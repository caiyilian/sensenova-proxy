package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var supportedModels = []string{
	"deepseek-v4-flash",
	"sensenova-6.7-flash-lite",
	"glm-5.2",
	"deepseek-v4-pro",
	"kimi-k3",
	"sensenova-6.8-flash-lite",
}

var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"content-length":      {},
	"host":                {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"proxy-connection":    {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

type networkStateProvider interface {
	Status() NetworkStatus
}

type gatewayStats struct {
	requests      atomic.Uint64
	successes     atomic.Uint64
	failures      atomic.Uint64
	retries       atomic.Uint64
	queued        atomic.Uint64
	canceled      atomic.Uint64
	pacedProbes   atomic.Uint64
	networkBlocks atomic.Uint64
}

func (stats *gatewayStats) Snapshot() map[string]uint64 {
	return map[string]uint64{
		"requests":      stats.requests.Load(),
		"successes":     stats.successes.Load(),
		"failures":      stats.failures.Load(),
		"retries":       stats.retries.Load(),
		"queued":        stats.queued.Load(),
		"canceled":      stats.canceled.Load(),
		"pacedProbes":   stats.pacedProbes.Load(),
		"networkBlocks": stats.networkBlocks.Load(),
	}
}

type Gateway struct {
	keyStore *KeyStore
	pool     *AccountPool
	logger   *JSONLLogger
	network  networkStateProvider
	resolver *ProxyResolver
	client   *http.Client

	localToken    string
	upstreamBase  *url.URL
	maxQueue      time.Duration
	probeInterval time.Duration
	startedAt     time.Time
	stats         gatewayStats

	mu       sync.RWMutex
	server   *http.Server
	listener net.Listener
	address  string
}

func NewGateway(
	keyStore *KeyStore,
	pool *AccountPool,
	logger *JSONLLogger,
	network networkStateProvider,
	resolver *ProxyResolver,
	localToken string,
) (*Gateway, error) {
	upstreamBase, err := url.Parse("https://token.sensenova.cn/v1/")
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = resolver.Proxy
	transport.MaxIdleConns = 32
	transport.MaxIdleConnsPerHost = 16
	return &Gateway{
		keyStore:      keyStore,
		pool:          pool,
		logger:        logger,
		network:       network,
		resolver:      resolver,
		client:        &http.Client{Transport: transport},
		localToken:    localToken,
		upstreamBase:  upstreamBase,
		maxQueue:      10 * time.Minute,
		probeInterval: 5 * time.Second,
		startedAt:     time.Now(),
	}, nil
}

func (gateway *Gateway) Start(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", port, err)
	}
	server := &http.Server{
		Handler:           gateway,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	gateway.mu.Lock()
	gateway.listener = listener
	gateway.server = server
	gateway.address = "http://" + listener.Addr().String()
	gateway.startedAt = time.Now()
	gateway.mu.Unlock()
	gateway.logger.Log("gateway_started", map[string]any{
		"address": gateway.address,
		"models":  len(supportedModels),
	})
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			gateway.logger.Log("gateway_serve_failed", map[string]any{"error": serveErr})
		}
	}()
	return nil
}

func (gateway *Gateway) Close(ctx context.Context) error {
	gateway.mu.RLock()
	server := gateway.server
	gateway.mu.RUnlock()
	if server == nil {
		return nil
	}
	gateway.client.CloseIdleConnections()
	err := server.Shutdown(ctx)
	gateway.logger.Log("gateway_stopped", map[string]any{})
	return err
}

func (gateway *Gateway) Address() string {
	gateway.mu.RLock()
	defer gateway.mu.RUnlock()
	return gateway.address
}

func (gateway *Gateway) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	setCORS(response)
	if request.Method == http.MethodOptions {
		response.WriteHeader(http.StatusNoContent)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/health" {
		gateway.handleHealth(response)
		return
	}
	if !gateway.authorized(request) {
		writeJSON(response, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{
				"type":    "authentication_error",
				"message": "Invalid local gateway token.",
			},
		})
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/v1/models" {
		models := make([]map[string]string, 0, len(supportedModels))
		for _, model := range supportedModels {
			models = append(models, map[string]string{
				"id":       model,
				"object":   "model",
				"owned_by": "sensenova",
			})
		}
		writeJSON(response, http.StatusOK, map[string]any{"object": "list", "data": models})
		return
	}
	if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" {
		writeJSON(response, http.StatusNotFound, map[string]any{
			"error": map[string]any{
				"type":    "not_found",
				"message": "Supported endpoint: POST /v1/chat/completions",
			},
		})
		return
	}
	gateway.stats.requests.Add(1)
	if status := gateway.networkStatus(); status.Known && !status.Online {
		gateway.stats.failures.Add(1)
		gateway.stats.networkBlocks.Add(1)
		gateway.logger.Log("request_blocked_offline", map[string]any{
			"route": status.Route,
		})
		response.Header().Set("Retry-After", "5")
		writeJSON(response, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"type":    "network_unavailable",
				"code":    "network_unavailable",
				"message": "SenseNova website is currently unreachable; the desktop app will check again automatically.",
			},
		})
		return
	}
	gateway.handleChat(response, request)
}

func (gateway *Gateway) handleHealth(response http.ResponseWriter) {
	keys := gateway.keyStore.Snapshot()
	network := gateway.networkStatus()
	invalidLines := keys.InvalidLines
	if invalidLines == nil {
		invalidLines = []KeyLineIssue{}
	}
	duplicateLines := keys.DuplicateLines
	if duplicateLines == nil {
		duplicateLines = []DuplicateKeyLine{}
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"status":            gatewayStatus(keys, network),
		"accountCount":      len(keys.Accounts),
		"invalidKeyLines":   invalidLines,
		"duplicateKeyLines": duplicateLines,
		"keyFileReadError":  keys.LastError != "",
		"keyReloads":        keys.Reloads,
		"network":           network,
		"uptimeSeconds":     int(time.Since(gateway.startedAt).Seconds()),
		"stats":             gateway.stats.Snapshot(),
		"accounts":          gateway.pool.Snapshot(keys.Accounts),
	})
}

func gatewayStatus(keys KeySnapshot, network NetworkStatus) string {
	if len(keys.Accounts) == 0 {
		return "no_accounts"
	}
	if network.Known && !network.Online {
		return "offline"
	}
	return "ok"
}

func (gateway *Gateway) handleChat(response http.ResponseWriter, request *http.Request) {
	requestID := randomID()
	startedAt := time.Now()
	request.Body = http.MaxBytesReader(response, request.Body, 64*1024*1024)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		gateway.stats.failures.Add(1)
		writeJSON(response, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"type": "invalid_request_error", "message": "Unable to read request body."},
		})
		return
	}
	model := extractModel(body)
	attempted := make(map[string]struct{})
	attempts := 0
	queued := false
	completedSweep := false
	nextProbeAt := time.Time{}
	lastCategory := "pool_unavailable"

	for request.Context().Err() == nil {
		if completedSweep && !nextProbeAt.IsZero() && time.Now().Before(nextProbeAt) {
			remaining := gateway.maxQueue - time.Since(startedAt)
			if remaining <= 0 {
				gateway.failUnavailable(response, requestID, model, attempts, lastCategory, startedAt)
				return
			}
			if !sleepContext(request.Context(), min(time.Until(nextProbeAt), min(2*time.Second, remaining))) {
				gateway.stats.canceled.Add(1)
				return
			}
			continue
		}

		accounts := gateway.keyStore.Accounts()
		if len(accounts) == 0 {
			gateway.stats.failures.Add(1)
			gateway.logger.Log("request_failed", map[string]any{
				"requestId": requestID,
				"model":     model,
				"category":  "no_accounts",
			})
			writeJSON(response, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]any{
					"type":    "sensenova_pool_unavailable",
					"code":    "no_accounts",
					"message": "No valid SenseNova API keys are configured.",
				},
			})
			return
		}

		account, available := gateway.pool.Pick(accounts, model, attempted)
		if !available {
			if attempts > 0 {
				completedSweep = true
			}
			clear(attempted)
			nextReady, hasNext := gateway.pool.NextReadyAt(accounts, model)
			remaining := gateway.maxQueue - time.Since(startedAt)
			wait := remaining + time.Millisecond
			if hasNext {
				wait = max(25*time.Millisecond, time.Until(nextReady))
			}
			if remaining <= 0 || wait > remaining {
				gateway.failUnavailable(response, requestID, model, attempts, lastCategory, startedAt)
				return
			}
			if !queued {
				queued = true
				gateway.stats.queued.Add(1)
				gateway.logger.Log("request_queued", map[string]any{
					"requestId": requestID,
					"model":     model,
					"attempts":  attempts,
					"waitMs":    wait.Milliseconds(),
				})
			}
			if !sleepContext(request.Context(), min(wait, 2*time.Second)) {
				gateway.stats.canceled.Add(1)
				return
			}
			continue
		}

		attempted[account.Fingerprint] = struct{}{}
		attempts++
		gateway.pool.Begin(account)
		attemptStarted := time.Now()
		upstream, upstreamErr := gateway.sendUpstream(request, body, account)
		gateway.pool.End(account)
		if upstreamErr != nil {
			if request.Context().Err() != nil {
				gateway.stats.canceled.Add(1)
				return
			}
			class := failureClass{Category: "upstream", Retryable: true, Scope: "model"}
			cooldown := gateway.pool.MarkFailure(account, model, class, 0)
			gateway.stats.retries.Add(1)
			lastCategory = class.Category
			gateway.logger.Log("upstream_retry", accountLogFields(account, map[string]any{
				"requestId":  requestID,
				"model":      model,
				"category":   class.Category,
				"attempt":    attempts,
				"cooldownMs": cooldown.Milliseconds(),
				"durationMs": time.Since(attemptStarted).Milliseconds(),
				"error":      upstreamErr,
			}))
			if completedSweep {
				nextProbeAt = time.Now().Add(gateway.probeInterval)
				gateway.stats.pacedProbes.Add(1)
			}
			continue
		}

		if upstream.StatusCode >= 200 && upstream.StatusCode < 300 {
			gateway.pool.MarkSuccess(account, model)
			copyResponseHeaders(response.Header(), upstream.Header)
			response.Header().Set("X-SenseNova-Pool-Account", account.ID)
			response.Header().Set("X-SenseNova-Pool-Attempts", strconv.Itoa(attempts))
			response.WriteHeader(upstream.StatusCode)
			streamErr := copyStream(response, upstream.Body)
			_ = upstream.Body.Close()
			if streamErr != nil {
				gateway.stats.canceled.Add(1)
				gateway.logger.Log("response_stream_failed", accountLogFields(account, map[string]any{
					"requestId": requestID,
					"model":     model,
					"status":    upstream.StatusCode,
					"error":     streamErr,
				}))
				return
			}
			gateway.stats.successes.Add(1)
			gateway.logger.Log("request_succeeded", accountLogFields(account, map[string]any{
				"requestId":  requestID,
				"model":      model,
				"status":     upstream.StatusCode,
				"attempts":   attempts,
				"durationMs": time.Since(startedAt).Milliseconds(),
			}))
			return
		}

		failureBody, readErr := io.ReadAll(io.LimitReader(upstream.Body, 8*1024*1024))
		_ = upstream.Body.Close()
		if readErr != nil {
			failureBody = []byte(`{"error":{"message":"Unable to read upstream error response."}}`)
		}
		class := classifyFailure(upstream.StatusCode, string(failureBody))
		if !class.Retryable {
			gateway.stats.failures.Add(1)
			gateway.logger.Log("request_rejected", accountLogFields(account, map[string]any{
				"requestId":  requestID,
				"model":      model,
				"status":     upstream.StatusCode,
				"category":   class.Category,
				"attempts":   attempts,
				"durationMs": time.Since(startedAt).Milliseconds(),
			}))
			copyResponseHeaders(response.Header(), upstream.Header)
			response.Header().Set("X-SenseNova-Pool-Account", account.ID)
			response.Header().Set("X-SenseNova-Pool-Attempts", strconv.Itoa(attempts))
			response.WriteHeader(upstream.StatusCode)
			_, _ = response.Write(failureBody)
			return
		}

		cooldown := gateway.pool.MarkFailure(
			account,
			model,
			class,
			parseRetryAfter(upstream.Header.Get("Retry-After")),
		)
		gateway.stats.retries.Add(1)
		lastCategory = class.Category
		event := "upstream_retry"
		if class.Category == "auth" {
			event = "account_quarantined"
		}
		gateway.logger.Log(event, accountLogFields(account, map[string]any{
			"requestId":  requestID,
			"model":      model,
			"status":     upstream.StatusCode,
			"category":   class.Category,
			"attempt":    attempts,
			"cooldownMs": cooldown.Milliseconds(),
			"durationMs": time.Since(attemptStarted).Milliseconds(),
		}))
		if completedSweep {
			nextProbeAt = time.Now().Add(gateway.probeInterval)
			gateway.stats.pacedProbes.Add(1)
		}
	}
	gateway.stats.canceled.Add(1)
}

func (gateway *Gateway) sendUpstream(request *http.Request, body []byte, account Account) (*http.Response, error) {
	target := *gateway.upstreamBase
	relativePath := strings.TrimPrefix(request.URL.Path, "/v1")
	target.Path = strings.TrimSuffix(gateway.upstreamBase.Path, "/") + relativePath
	target.RawQuery = request.URL.RawQuery
	upstreamRequest, err := http.NewRequestWithContext(
		request.Context(),
		request.Method,
		target.String(),
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(upstreamRequest.Header, request.Header)
	upstreamRequest.Header.Set("Authorization", "Bearer "+account.APIKey)
	upstreamRequest.Header.Del("X-Api-Key")
	return gateway.client.Do(upstreamRequest)
}

func (gateway *Gateway) failUnavailable(
	response http.ResponseWriter,
	requestID, model string,
	attempts int,
	category string,
	startedAt time.Time,
) {
	gateway.stats.failures.Add(1)
	gateway.logger.Log("request_failed", map[string]any{
		"requestId": requestID,
		"model":     model,
		"attempts":  attempts,
		"category":  category,
		"queuedMs":  time.Since(startedAt).Milliseconds(),
	})
	response.Header().Set("Retry-After", "5")
	response.Header().Set("X-SenseNova-Pool-Attempts", strconv.Itoa(attempts))
	writeJSON(response, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{
			"type":    "sensenova_pool_unavailable",
			"code":    category,
			"message": "All SenseNova accounts are temporarily unavailable; see the desktop log.",
		},
	})
}

func (gateway *Gateway) authorized(request *http.Request) bool {
	credential := strings.TrimSpace(request.Header.Get("X-Api-Key"))
	if credential == "" {
		authorization := strings.TrimSpace(request.Header.Get("Authorization"))
		if len(authorization) > 7 && strings.EqualFold(authorization[:7], "Bearer ") {
			credential = strings.TrimSpace(authorization[7:])
		}
	}
	if len(credential) != len(gateway.localToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(credential), []byte(gateway.localToken)) == 1
}

func extractModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &payload) != nil || strings.TrimSpace(payload.Model) == "" {
		return "unknown"
	}
	return strings.TrimSpace(payload.Model)
}

func copyRequestHeaders(destination, source http.Header) {
	for name, values := range source {
		if _, skip := hopByHopHeaders[strings.ToLower(name)]; skip || strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "X-Api-Key") || strings.EqualFold(name, "Accept-Encoding") {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func (gateway *Gateway) networkStatus() NetworkStatus {
	if gateway.network == nil {
		return NetworkStatus{Detail: "未启用联网检测", Route: "未知"}
	}
	return gateway.network.Status()
}

func copyResponseHeaders(destination, source http.Header) {
	for name, values := range source {
		if _, skip := hopByHopHeaders[strings.ToLower(name)]; skip {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func copyStream(response http.ResponseWriter, body io.Reader) error {
	buffer := make([]byte, 32*1024)
	flusher, canFlush := response.(http.Flusher)
	for {
		count, readErr := body.Read(buffer)
		if count > 0 {
			if _, writeErr := response.Write(buffer[:count]); writeErr != nil {
				return writeErr
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func accountLogFields(account Account, fields map[string]any) map[string]any {
	fields["account"] = account.ID
	fields["keyLine"] = account.LineNumber
	fields["keyRef"] = account.KeyRef
	return fields
}

func setCORS(response http.ResponseWriter) {
	response.Header().Set("Access-Control-Allow-Origin", "*")
	response.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	response.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key")
}

func writeJSON(response http.ResponseWriter, status int, payload any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(payload)
}

func randomID() string {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buffer)
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return true
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
