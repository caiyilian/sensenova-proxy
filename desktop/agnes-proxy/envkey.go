package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"
)

type AgnesKeySnapshot struct {
	Present     bool
	Source      string
	Fingerprint string
	KeyRef      string
	ChangedAt   time.Time
}

type AgnesKeyMonitor struct {
	logger   *JSONLLogger
	interval time.Duration
	reader   func() (string, string)

	mu       sync.RWMutex
	apiKey   string
	snapshot AgnesKeySnapshot
	stop     chan struct{}
	done     chan struct{}
	start    sync.Once
	close    sync.Once
	started  bool
}

func NewAgnesKeyMonitor(logger *JSONLLogger, interval time.Duration) *AgnesKeyMonitor {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	return &AgnesKeyMonitor{
		logger:   logger,
		interval: interval,
		reader:   readAgnesKey,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (monitor *AgnesKeyMonitor) Start() {
	monitor.start.Do(func() {
		monitor.mu.Lock()
		monitor.started = true
		monitor.mu.Unlock()
		monitor.Refresh()
		go monitor.loop()
	})
}

func (monitor *AgnesKeyMonitor) Close() {
	monitor.close.Do(func() {
		monitor.mu.RLock()
		started := monitor.started
		monitor.mu.RUnlock()
		if !started {
			return
		}
		close(monitor.stop)
		<-monitor.done
	})
}

func (monitor *AgnesKeyMonitor) Refresh() AgnesKeySnapshot {
	apiKey, source := monitor.reader()
	apiKey = strings.TrimSpace(apiKey)
	fingerprint := ""
	keyRef := ""
	if apiKey != "" {
		sum := sha256.Sum256([]byte(apiKey))
		fingerprint = hex.EncodeToString(sum[:])
		keyRef = fingerprint[:8]
	}

	monitor.mu.Lock()
	changed := fingerprint != monitor.snapshot.Fingerprint || (apiKey != "") != monitor.snapshot.Present
	if changed {
		monitor.snapshot = AgnesKeySnapshot{
			Present:     apiKey != "",
			Source:      source,
			Fingerprint: fingerprint,
			KeyRef:      keyRef,
			ChangedAt:   time.Now(),
		}
	}
	monitor.apiKey = apiKey
	snapshot := monitor.snapshot
	monitor.mu.Unlock()

	monitor.logger.SetSecrets(apiKey)
	if changed {
		monitor.logger.Log("agnes_key_changed", map[string]any{
			"present": snapshot.Present,
			"source":  snapshot.Source,
			"keyRef":  snapshot.KeyRef,
		})
	}
	return snapshot
}

func (monitor *AgnesKeyMonitor) APIKey() string {
	monitor.mu.RLock()
	defer monitor.mu.RUnlock()
	return monitor.apiKey
}

func (monitor *AgnesKeyMonitor) Snapshot() AgnesKeySnapshot {
	monitor.mu.RLock()
	defer monitor.mu.RUnlock()
	return monitor.snapshot
}

func (monitor *AgnesKeyMonitor) loop() {
	defer close(monitor.done)
	ticker := time.NewTicker(monitor.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			monitor.Refresh()
		case <-monitor.stop:
			return
		}
	}
}

func readAgnesKey() (string, string) {
	if value := readUserEnvironmentVariable("AGNES_API_KEY"); value != "" {
		return value, "用户环境变量"
	}
	if value := strings.TrimSpace(os.Getenv("AGNES_API_KEY")); value != "" {
		return value, "进程环境变量"
	}
	return "", "未检测到 AGNES_API_KEY"
}

func readAgnesLocalToken() (string, string) {
	return selectAgnesLocalToken(
		readUserEnvironmentVariable("AGNES_PROXY_LOCAL_TOKEN"),
		os.Getenv("AGNES_PROXY_LOCAL_TOKEN"),
	)
}

func selectAgnesLocalToken(userValue, processValue string) (string, string) {
	if value := strings.TrimSpace(userValue); value != "" {
		return value, "用户环境变量"
	}
	if value := strings.TrimSpace(processValue); value != "" {
		return value, "进程环境变量"
	}
	return defaultLocalProxyToken, "程序默认值"
}

func readUserEnvironmentVariable(name string) string {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer key.Close()
	value, _, err := key.GetStringValue(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}
