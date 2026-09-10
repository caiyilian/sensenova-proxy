package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var credentialPattern = regexp.MustCompile(`(?i)(?:sk-|Bearer\s+)[A-Za-z0-9._-]+`)

type JSONLLogger struct {
	dir string
	mu  sync.Mutex
}

func NewJSONLLogger(dir string) (*JSONLLogger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	return &JSONLLogger{dir: dir}, nil
}

func (logger *JSONLLogger) Dir() string {
	return logger.dir
}

func (logger *JSONLLogger) Log(event string, fields map[string]any) {
	record := make(map[string]any, len(fields)+2)
	record["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	record["event"] = event
	for key, value := range fields {
		record[key] = sanitizeLogValue(value)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	// Redact the final serialized record as a last line of defense. This also
	// covers credentials nested inside maps or slices that a future log event
	// might add.
	encoded = credentialPattern.ReplaceAll(encoded, []byte("[redacted]"))

	logger.mu.Lock()
	defer logger.mu.Unlock()
	filePath := filepath.Join(logger.dir, "gateway-"+time.Now().Format("2006-01-02")+".jsonl")
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(encoded, '\n'))
	_ = file.Close()
}

func sanitizeLogValue(value any) any {
	switch typed := value.(type) {
	case error:
		return redactText(typed.Error())
	case string:
		return redactText(typed)
	case []string:
		copyValues := make([]string, len(typed))
		for index, item := range typed {
			copyValues[index] = redactText(item)
		}
		return copyValues
	default:
		return value
	}
}

func redactText(value string) string {
	redacted := credentialPattern.ReplaceAllString(value, "[redacted]")
	return strings.TrimSpace(redacted)
}
