package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const expectedSenseNovaKeyLength = 35

var senseNovaKeyPattern = regexp.MustCompile(`^sk-[A-Za-z0-9._-]+$`)

type Account struct {
	ID          string `json:"id"`
	APIKey      string `json:"-"`
	Fingerprint string `json:"-"`
	KeyRef      string `json:"keyRef"`
	LineNumber  int    `json:"lineNumber"`
}

type KeyLineIssue struct {
	LineNumber int    `json:"lineNumber"`
	Reason     string `json:"reason"`
}

type DuplicateKeyLine struct {
	LineNumber      int `json:"lineNumber"`
	DuplicateOfLine int `json:"duplicateOfLine"`
}

type KeySnapshot struct {
	Path            string
	Accounts        []Account
	InvalidLines    []KeyLineIssue
	DuplicateLines  []DuplicateKeyLine
	Reloads         uint64
	LastReload      time.Time
	LastError       string
	BackgroundWatch bool
}

type parsedKeyFile struct {
	Accounts       []Account
	InvalidLines   []KeyLineIssue
	DuplicateLines []DuplicateKeyLine
}

type KeyStore struct {
	logger        *JSONLLogger
	watchInterval time.Duration

	mu              sync.RWMutex
	path            string
	accounts        []Account
	invalidLines    []KeyLineIssue
	duplicateLines  []DuplicateKeyLine
	signature       [sha256.Size]byte
	hasSignature    bool
	reloads         uint64
	lastReload      time.Time
	lastError       string
	lastLoggedError string

	updates chan KeySnapshot
	stop    chan struct{}
	done    chan struct{}
	start   sync.Once
	close   sync.Once
	started bool
}

func NewKeyStore(path string, logger *JSONLLogger, watchInterval time.Duration) *KeyStore {
	if watchInterval <= 0 {
		watchInterval = time.Second
	}
	return &KeyStore{
		path:          path,
		logger:        logger,
		watchInterval: watchInterval,
		updates:       make(chan KeySnapshot, 1),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
}

func (store *KeyStore) Start() {
	store.start.Do(func() {
		store.mu.Lock()
		store.started = true
		store.mu.Unlock()
		store.Reload("startup")
		go store.watchLoop()
	})
}

func (store *KeyStore) Close() {
	store.close.Do(func() {
		store.mu.RLock()
		started := store.started
		store.mu.RUnlock()
		if !started {
			return
		}
		close(store.stop)
		<-store.done
	})
}

func (store *KeyStore) Updates() <-chan KeySnapshot {
	return store.updates
}

func (store *KeyStore) SetPath(path string) error {
	if path == "" {
		return errors.New("key file path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve key file path: %w", err)
	}
	store.mu.Lock()
	changed := !strings.EqualFold(store.path, abs)
	store.path = abs
	if changed {
		store.hasSignature = false
		store.lastLoggedError = ""
	}
	store.mu.Unlock()
	store.Reload("path_selected")
	return nil
}

func (store *KeyStore) Reload(reason string) KeySnapshot {
	store.mu.RLock()
	keyPath := store.path
	store.mu.RUnlock()

	if keyPath == "" {
		return store.Snapshot()
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return store.setReadError(reason, err.Error())
	}
	signature := sha256.Sum256(data)

	store.mu.RLock()
	unchanged := store.hasSignature && store.signature == signature
	recovered := store.lastError != ""
	store.mu.RUnlock()
	if unchanged {
		if recovered {
			store.mu.Lock()
			store.lastError = ""
			store.lastLoggedError = ""
			store.mu.Unlock()
			store.logger.Log("key_reload_recovered", map[string]any{
				"reason":       reason,
				"accountCount": len(store.Accounts()),
			})
			store.publish()
		}
		return store.Snapshot()
	}

	parsed := parseKeyFile(data)
	store.mu.Lock()
	previous := append([]Account(nil), store.accounts...)
	previousFingerprints := make(map[string]struct{}, len(previous))
	for _, account := range previous {
		previousFingerprints[account.Fingerprint] = struct{}{}
	}
	nextFingerprints := make(map[string]struct{}, len(parsed.Accounts))
	addedRefs := make([]string, 0)
	for _, account := range parsed.Accounts {
		nextFingerprints[account.Fingerprint] = struct{}{}
		if _, exists := previousFingerprints[account.Fingerprint]; !exists {
			addedRefs = append(addedRefs, account.KeyRef)
		}
	}
	removedRefs := make([]string, 0)
	for _, account := range previous {
		if _, exists := nextFingerprints[account.Fingerprint]; !exists {
			removedRefs = append(removedRefs, account.KeyRef)
		}
	}

	store.accounts = parsed.Accounts
	store.invalidLines = parsed.InvalidLines
	store.duplicateLines = parsed.DuplicateLines
	store.signature = signature
	store.hasSignature = true
	store.reloads++
	store.lastReload = time.Now()
	store.lastError = ""
	store.lastLoggedError = ""
	snapshot := store.snapshotLocked()
	store.mu.Unlock()

	store.logger.Log("keys_reloaded", map[string]any{
		"reason":             reason,
		"accountCount":       len(snapshot.Accounts),
		"addedCount":         len(addedRefs),
		"removedCount":       len(removedRefs),
		"addedKeyRefs":       addedRefs,
		"removedKeyRefs":     removedRefs,
		"invalidLineCount":   len(snapshot.InvalidLines),
		"invalidLines":       snapshot.InvalidLines,
		"duplicateLineCount": len(snapshot.DuplicateLines),
		"duplicateLines":     snapshot.DuplicateLines,
		"reload":             snapshot.Reloads,
	})
	store.publishSnapshot(snapshot)
	return snapshot
}

func (store *KeyStore) Accounts() []Account {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return append([]Account(nil), store.accounts...)
}

func (store *KeyStore) Snapshot() KeySnapshot {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.snapshotLocked()
}

func (store *KeyStore) snapshotLocked() KeySnapshot {
	return KeySnapshot{
		Path:            store.path,
		Accounts:        append([]Account(nil), store.accounts...),
		InvalidLines:    append([]KeyLineIssue(nil), store.invalidLines...),
		DuplicateLines:  append([]DuplicateKeyLine(nil), store.duplicateLines...),
		Reloads:         store.reloads,
		LastReload:      store.lastReload,
		LastError:       store.lastError,
		BackgroundWatch: store.started,
	}
}

func (store *KeyStore) setReadError(reason, message string) KeySnapshot {
	safeMessage := redactText(message)
	store.mu.Lock()
	store.lastError = safeMessage
	shouldLog := safeMessage != store.lastLoggedError
	if shouldLog {
		store.lastLoggedError = safeMessage
	}
	snapshot := store.snapshotLocked()
	store.mu.Unlock()
	if shouldLog {
		store.logger.Log("key_reload_failed", map[string]any{
			"reason":               reason,
			"error":                safeMessage,
			"retainedAccountCount": len(snapshot.Accounts),
		})
	}
	store.publishSnapshot(snapshot)
	return snapshot
}

func (store *KeyStore) watchLoop() {
	defer close(store.done)
	ticker := time.NewTicker(store.watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			store.Reload("file_watch")
		case <-store.stop:
			return
		}
	}
}

func (store *KeyStore) publish() {
	store.publishSnapshot(store.Snapshot())
}

func (store *KeyStore) publishSnapshot(snapshot KeySnapshot) {
	select {
	case store.updates <- snapshot:
	default:
		select {
		case <-store.updates:
		default:
		}
		select {
		case store.updates <- snapshot:
		default:
		}
	}
}

func parseKeyFile(data []byte) parsedKeyFile {
	seen := make(map[string]int)
	parsed := parsedKeyFile{}
	scanner := bufio.NewScanner(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if reason := validateSenseNovaKey(line); reason != "" {
			parsed.InvalidLines = append(parsed.InvalidLines, KeyLineIssue{
				LineNumber: lineNumber,
				Reason:     reason,
			})
			continue
		}
		if originalLine, duplicate := seen[line]; duplicate {
			parsed.DuplicateLines = append(parsed.DuplicateLines, DuplicateKeyLine{
				LineNumber:      lineNumber,
				DuplicateOfLine: originalLine,
			})
			continue
		}
		seen[line] = lineNumber
		fingerprintBytes := sha256.Sum256([]byte(line))
		fingerprint := hex.EncodeToString(fingerprintBytes[:])
		parsed.Accounts = append(parsed.Accounts, Account{
			ID:          fmt.Sprintf("account-%d", len(parsed.Accounts)+1),
			APIKey:      line,
			Fingerprint: fingerprint,
			KeyRef:      fingerprint[:8],
			LineNumber:  lineNumber,
		})
	}
	return parsed
}

func validateSenseNovaKey(key string) string {
	if !strings.HasPrefix(key, "sk-") {
		return "missing_sk_prefix"
	}
	if !senseNovaKeyPattern.MatchString(key) {
		return "invalid_characters"
	}
	if len(key) != expectedSenseNovaKeyLength {
		return fmt.Sprintf("unexpected_length_%d", len(key))
	}
	return ""
}
