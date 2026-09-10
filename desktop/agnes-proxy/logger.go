package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var bearerPattern = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._-]+`)

type JSONLLogger struct {
	dir string
	mu  sync.Mutex

	secretsMu sync.RWMutex
	secrets   [][]byte
}

func NewJSONLLogger(dir string) (*JSONLLogger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	return &JSONLLogger{dir: dir}, nil
}

func (logger *JSONLLogger) Dir() string { return logger.dir }

func (logger *JSONLLogger) SetSecrets(values ...string) {
	secrets := make([][]byte, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			secrets = append(secrets, []byte(value))
		}
	}
	logger.secretsMu.Lock()
	logger.secrets = secrets
	logger.secretsMu.Unlock()
}

func (logger *JSONLLogger) Log(event string, fields map[string]any) {
	record := make(map[string]any, len(fields)+2)
	record["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	record["event"] = event
	for key, value := range fields {
		if err, ok := value.(error); ok {
			record[key] = err.Error()
		} else {
			record[key] = value
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	encoded = bearerPattern.ReplaceAll(encoded, []byte("Bearer [redacted]"))
	logger.secretsMu.RLock()
	for _, secret := range logger.secrets {
		encoded = bytes.ReplaceAll(encoded, secret, []byte("[redacted]"))
	}
	logger.secretsMu.RUnlock()

	logger.mu.Lock()
	defer logger.mu.Unlock()
	path := filepath.Join(logger.dir, "desktop-"+time.Now().Format("2006-01-02")+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(encoded, '\n'))
	_ = file.Close()
}

func redactText(value string) string {
	return strings.TrimSpace(string(bearerPattern.ReplaceAll([]byte(value), []byte("Bearer [redacted]"))))
}
