package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"
)

const maximumAgnesKeyFileBytes = 4 * 1024 * 1024

type AgnesKeySnapshot struct {
	Present          bool
	Source           string
	Fingerprint      string
	KeyRef           string
	ChangedAt        time.Time
	Path             string
	LastError        string
	RetainedLastGood bool
}

type AgnesKeyMonitor struct {
	logger   *JSONLLogger
	interval time.Duration
	reader   func() (string, string)

	mu              sync.RWMutex
	keyFilePath     string
	apiKey          string
	snapshot        AgnesKeySnapshot
	lastLoggedError string
	stop            chan struct{}
	done            chan struct{}
	start           sync.Once
	close           sync.Once
	started         bool
}

func NewAgnesKeyMonitor(logger *JSONLLogger, interval time.Duration, initialPath ...string) *AgnesKeyMonitor {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	monitor := &AgnesKeyMonitor{
		logger:   logger,
		interval: interval,
		reader:   readAgnesKey,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if len(initialPath) > 0 && strings.TrimSpace(initialPath[0]) != "" {
		if absolute, err := filepath.Abs(strings.TrimSpace(initialPath[0])); err == nil {
			monitor.keyFilePath = absolute
		} else {
			monitor.keyFilePath = strings.TrimSpace(initialPath[0])
		}
	}
	return monitor
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

func (monitor *AgnesKeyMonitor) SetPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("API Key 文件路径为空")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("解析 API Key 文件路径：%w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return fmt.Errorf("读取 API Key 文件：%w", err)
	}
	if info.IsDir() {
		return errors.New("选择的是文件夹，请选择一个 API Key 文件")
	}
	if _, err := readAgnesKeyFile(absolute); err != nil {
		return err
	}

	monitor.mu.Lock()
	monitor.keyFilePath = absolute
	monitor.lastLoggedError = ""
	monitor.mu.Unlock()
	monitor.Refresh()
	return nil
}

func (monitor *AgnesKeyMonitor) Path() string {
	monitor.mu.RLock()
	defer monitor.mu.RUnlock()
	return monitor.keyFilePath
}

func (monitor *AgnesKeyMonitor) Refresh() AgnesKeySnapshot {
	apiKey, source, path, readErr := monitor.readCurrent()
	apiKey = strings.TrimSpace(apiKey)
	if readErr == nil && apiKey == "" {
		readErr = errors.New("未在文件中识别到 Agnes API Key")
	}

	monitor.mu.Lock()
	previous := monitor.snapshot
	if readErr == nil {
		sum := sha256.Sum256([]byte(apiKey))
		fingerprint := hex.EncodeToString(sum[:])
		monitor.apiKey = apiKey
		monitor.snapshot = AgnesKeySnapshot{
			Present:     true,
			Source:      source,
			Fingerprint: fingerprint,
			KeyRef:      fingerprint[:8],
			ChangedAt:   previous.ChangedAt,
			Path:        path,
		}
		if fingerprint != previous.Fingerprint || previous.LastError != "" || previous.Source != source || previous.Path != path {
			monitor.snapshot.ChangedAt = time.Now()
		}
	} else {
		message := redactText(readErr.Error())
		monitor.snapshot = AgnesKeySnapshot{
			Present:          monitor.apiKey != "",
			Source:           source,
			Fingerprint:      previous.Fingerprint,
			KeyRef:           previous.KeyRef,
			ChangedAt:        previous.ChangedAt,
			Path:             path,
			LastError:        message,
			RetainedLastGood: monitor.apiKey != "",
		}
		if previous.LastError != message || previous.Path != path {
			monitor.snapshot.ChangedAt = time.Now()
		}
	}
	snapshot := monitor.snapshot
	currentKey := monitor.apiKey
	changed := snapshot.Fingerprint != previous.Fingerprint || snapshot.LastError != previous.LastError || snapshot.Source != previous.Source || snapshot.Path != previous.Path
	logError := snapshot.LastError != "" && snapshot.LastError != monitor.lastLoggedError
	if snapshot.LastError == "" {
		monitor.lastLoggedError = ""
	} else if logError {
		monitor.lastLoggedError = snapshot.LastError
	}
	monitor.mu.Unlock()

	monitor.logger.SetSecrets(currentKey)
	if changed {
		monitor.logger.Log("agnes_key_changed", map[string]any{
			"present":          snapshot.Present,
			"source":           snapshot.Source,
			"keyRef":           snapshot.KeyRef,
			"fileName":         filepath.Base(snapshot.Path),
			"retainedLastGood": snapshot.RetainedLastGood,
		})
	}
	if logError {
		monitor.logger.Log("agnes_key_file_error", map[string]any{
			"fileName": filepath.Base(snapshot.Path),
			"error":    snapshot.LastError,
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

func (monitor *AgnesKeyMonitor) readCurrent() (apiKey, source, path string, err error) {
	monitor.mu.RLock()
	path = monitor.keyFilePath
	reader := monitor.reader
	monitor.mu.RUnlock()
	if path != "" {
		apiKey, err = readAgnesKeyFile(path)
		return apiKey, "API Key 文件", path, err
	}
	apiKey, source = reader()
	if strings.TrimSpace(apiKey) == "" {
		err = errors.New("尚未选择 API Key 文件，也未检测到 AGNES_API_KEY 环境变量")
	}
	return apiKey, source, "", err
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

func readAgnesKeyFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("读取 API Key 文件：%w", err)
	}
	if info.IsDir() {
		return "", errors.New("API Key 路径指向文件夹")
	}
	if info.Size() > maximumAgnesKeyFileBytes {
		return "", fmt.Errorf("API Key 文件过大（上限 %d MiB）", maximumAgnesKeyFileBytes/(1024*1024))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取 API Key 文件：%w", err)
	}
	if key := parseAgnesKeyFile(data); key != "" {
		return key, nil
	}
	return "", errors.New("未在文件中识别到 Agnes API Key；支持纯文本、AGNES_API_KEY=... 或 JSON")
}

func parseAgnesKeyFile(data []byte) string {
	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}))
	if len(data) == 0 {
		return ""
	}
	var decoded any
	if json.Unmarshal(data, &decoded) == nil {
		if key := findAgnesKeyInJSON(decoded); key != "" {
			return key
		}
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "//") {
			continue
		}
		if name, value, found := strings.Cut(line, "="); found {
			if isAgnesKeyField(name) {
				if key := normalizeAgnesKeyCandidate(value); key != "" {
					return key
				}
			}
			continue
		}
		if key := normalizeAgnesKeyCandidate(line); key != "" {
			return key
		}
	}
	return ""
}

func findAgnesKeyInJSON(value any) string {
	switch typed := value.(type) {
	case string:
		return normalizeAgnesKeyCandidate(typed)
	case []any:
		for _, item := range typed {
			if key := findAgnesKeyInJSON(item); key != "" {
				return key
			}
		}
	case map[string]any:
		for name, item := range typed {
			if !isAgnesKeyField(name) {
				continue
			}
			if key := findAgnesKeyInJSON(item); key != "" {
				return key
			}
		}
	}
	return ""
}

func isAgnesKeyField(name string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.TrimSpace(name)))
	switch normalized {
	case "agnesapikey", "apikey", "key", "token", "accesstoken":
		return true
	default:
		return false
	}
}

func normalizeAgnesKeyCandidate(value string) string {
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), ","))
	value = strings.Trim(value, "\"'`")
	if len(value) >= 7 && strings.EqualFold(value[:7], "Bearer ") {
		value = strings.TrimSpace(value[7:])
	}
	if len(value) < 8 || strings.ContainsAny(value, " \t\r\n{}[]\"") {
		return ""
	}
	return value
}

func readAgnesKey() (string, string) {
	if value := readUserEnvironmentVariable("AGNES_API_KEY"); value != "" {
		return value, "用户环境变量（兼容模式）"
	}
	if value := strings.TrimSpace(os.Getenv("AGNES_API_KEY")); value != "" {
		return value, "进程环境变量（兼容模式）"
	}
	return "", "未配置"
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
