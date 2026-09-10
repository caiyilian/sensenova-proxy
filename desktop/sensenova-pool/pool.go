package main

import (
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type failureClass struct {
	Category  string
	Retryable bool
	Scope     string
}

type cooldownSettings struct {
	Base        time.Duration
	Max         time.Duration
	Exponential bool
}

type cooldownState struct {
	Category    string
	Consecutive int
	Until       time.Time
	LastFailure time.Time
}

var (
	rateLimitPattern = regexp.MustCompile(`(?i)tpm|rpm|rate.?limit|too many requests|inference exceeds|request frequency|限流|频率`)
	quotaPattern     = regexp.MustCompile(`(?i)token plan entitlement exhausted|entitlement exhausted|quota|insufficient (?:balance|credit)|balance exhausted|credits? exhausted|额度|余额不足`)
	authPattern      = regexp.MustCompile(`(?i)invalid api.?key|authentication|unauthori[sz]ed|forbidden|鉴权|认证失败`)
)

type AccountPool struct {
	mu           sync.Mutex
	cursor       int
	inFlight     map[string]int
	failureState map[string]cooldownState
	cooldowns    map[string]cooldownSettings
}

func NewAccountPool() *AccountPool {
	return &AccountPool{
		inFlight:     make(map[string]int),
		failureState: make(map[string]cooldownState),
		cooldowns: map[string]cooldownSettings{
			"rate_limit": {Base: time.Minute, Max: 5 * time.Minute, Exponential: false},
			"quota":      {Base: 30 * time.Minute, Max: 5 * time.Hour, Exponential: true},
			"auth":       {Base: 24 * time.Hour, Max: 24 * time.Hour, Exponential: false},
			"upstream":   {Base: 5 * time.Second, Max: 2 * time.Minute, Exponential: true},
		},
	}
}

func (pool *AccountPool) Pick(accounts []Account, model string, excluded map[string]struct{}) (Account, bool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(accounts) == 0 {
		return Account{}, false
	}
	now := time.Now()
	candidates := make([]Account, 0, len(accounts))
	minimumInFlight := math.MaxInt
	for _, account := range accounts {
		if _, skip := excluded[account.Fingerprint]; skip || pool.cooldownForLocked(account, model, now) != nil {
			continue
		}
		busy := pool.inFlight[account.Fingerprint]
		if busy < minimumInFlight {
			minimumInFlight = busy
			candidates = candidates[:0]
			candidates = append(candidates, account)
		} else if busy == minimumInFlight {
			candidates = append(candidates, account)
		}
	}
	if len(candidates) == 0 {
		return Account{}, false
	}

	eligible := make(map[string]Account, len(candidates))
	for _, account := range candidates {
		eligible[account.Fingerprint] = account
	}
	for offset := 0; offset < len(accounts); offset++ {
		index := (pool.cursor + offset) % len(accounts)
		if account, ok := eligible[accounts[index].Fingerprint]; ok {
			pool.cursor = (index + 1) % len(accounts)
			return account, true
		}
	}
	return candidates[0], true
}

func (pool *AccountPool) Begin(account Account) {
	pool.mu.Lock()
	pool.inFlight[account.Fingerprint]++
	pool.mu.Unlock()
}

func (pool *AccountPool) End(account Account) {
	pool.mu.Lock()
	if count := pool.inFlight[account.Fingerprint]; count <= 1 {
		delete(pool.inFlight, account.Fingerprint)
	} else {
		pool.inFlight[account.Fingerprint] = count - 1
	}
	pool.mu.Unlock()
}

func (pool *AccountPool) MarkFailure(account Account, model string, class failureClass, retryAfter time.Duration) time.Duration {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	settings, ok := pool.cooldowns[class.Category]
	if !ok {
		return 0
	}
	key := pool.stateKey(account, model, class.Scope)
	previous := pool.failureState[key]
	consecutive := previous.Consecutive + 1
	duration := settings.Base
	if settings.Exponential {
		exponent := min(consecutive-1, 8)
		duration = settings.Base * time.Duration(1<<exponent)
	}
	if duration > settings.Max {
		duration = settings.Max
	}
	if retryAfter > duration {
		duration = retryAfter
	}
	if duration > settings.Max {
		duration = settings.Max
	}
	now := time.Now()
	pool.failureState[key] = cooldownState{
		Category:    class.Category,
		Consecutive: consecutive,
		Until:       now.Add(duration),
		LastFailure: now,
	}
	return duration
}

func (pool *AccountPool) MarkSuccess(account Account, model string) {
	pool.mu.Lock()
	delete(pool.failureState, pool.stateKey(account, model, "model"))
	pool.mu.Unlock()
}

func (pool *AccountPool) NextReadyAt(accounts []Account, model string) (time.Time, bool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	now := time.Now()
	var earliest time.Time
	found := false
	for _, account := range accounts {
		state := pool.cooldownForLocked(account, model, now)
		if state == nil {
			return now, true
		}
		if !found || state.Until.Before(earliest) {
			earliest = state.Until
			found = true
		}
	}
	return earliest, found
}

func (pool *AccountPool) Snapshot(accounts []Account) []map[string]any {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	now := time.Now()
	result := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		states := make([]map[string]any, 0)
		for key, state := range pool.failureState {
			if strings.HasPrefix(key, account.Fingerprint+":") && state.Until.After(now) {
				states = append(states, map[string]any{
					"category": state.Category,
					"until":    state.Until.UTC().Format(time.RFC3339),
				})
			}
		}
		sort.Slice(states, func(left, right int) bool {
			return states[left]["until"].(string) < states[right]["until"].(string)
		})
		result = append(result, map[string]any{
			"id":          account.ID,
			"keyRef":      account.KeyRef,
			"lineNumber":  account.LineNumber,
			"inFlight":    pool.inFlight[account.Fingerprint],
			"cooldowns":   states,
			"quarantined": pool.authQuarantinedLocked(account, now),
		})
	}
	return result
}

func (pool *AccountPool) AuthQuarantinedCount(accounts []Account) int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	now := time.Now()
	count := 0
	for _, account := range accounts {
		if pool.authQuarantinedLocked(account, now) {
			count++
		}
	}
	return count
}

func (pool *AccountPool) stateKey(account Account, model, scope string) string {
	if scope == "account" {
		return account.Fingerprint + ":*"
	}
	return account.Fingerprint + ":" + model
}

func (pool *AccountPool) cooldownForLocked(account Account, model string, now time.Time) *cooldownState {
	modelState, hasModel := pool.failureState[pool.stateKey(account, model, "model")]
	accountState, hasAccount := pool.failureState[pool.stateKey(account, model, "account")]
	var selected *cooldownState
	if hasModel && modelState.Until.After(now) {
		copyState := modelState
		selected = &copyState
	}
	if hasAccount && accountState.Until.After(now) && (selected == nil || accountState.Until.After(selected.Until)) {
		copyState := accountState
		selected = &copyState
	}
	return selected
}

func (pool *AccountPool) authQuarantinedLocked(account Account, now time.Time) bool {
	state, ok := pool.failureState[pool.stateKey(account, "", "account")]
	return ok && state.Category == "auth" && state.Until.After(now)
}

func classifyFailure(statusCode int, body string) failureClass {
	if rateLimitPattern.MatchString(body) {
		return failureClass{Category: "rate_limit", Retryable: true, Scope: "model"}
	}
	if quotaPattern.MatchString(body) {
		return failureClass{Category: "quota", Retryable: true, Scope: "model"}
	}
	if statusCode == 429 {
		return failureClass{Category: "rate_limit", Retryable: true, Scope: "model"}
	}
	if statusCode == 401 || statusCode == 403 || authPattern.MatchString(body) {
		return failureClass{Category: "auth", Retryable: true, Scope: "account"}
	}
	if statusCode == 408 || statusCode == 425 || statusCode >= 500 {
		return failureClass{Category: "upstream", Retryable: true, Scope: "model"}
	}
	return failureClass{Category: "request", Retryable: false, Scope: "none"}
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil && seconds >= 0 {
		return seconds
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}
