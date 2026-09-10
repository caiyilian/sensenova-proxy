package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncWorkBuddyUpdatesAddsAndPreservesOtherModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	original := `{
  "models": [
    {
      "id": "deepseek-v4-flash",
      "name": "DeepSeek V4 Flash · SenseNova",
      "vendor": "SenseNova",
      "url": "http://old:18787/v1/chat/completions",
      "apiKey": "old-local-token",
      "customField": "keep-me"
    },
    {
      "id": "agnes-2.0-flash",
      "name": "Agnes 2.0 Flash",
      "vendor": "Agnes",
      "url": "http://127.0.0.1:18788/v1/chat/completions",
      "apiKey": "keep-agnes-token"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := syncWorkBuddyConfig(path, workBuddyChatURL(18787), "new-local-token")
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 || result.Added != 5 || result.BackupPath == "" {
		t.Fatalf("unexpected sync result: %+v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Models) != 7 {
		t.Fatalf("model count = %d, want 7", len(document.Models))
	}
	for _, model := range document.Models {
		id, _ := model["id"].(string)
		if id == "agnes-2.0-flash" {
			if model["apiKey"] != "keep-agnes-token" {
				t.Fatal("Agnes model was modified")
			}
			continue
		}
		if model["apiKey"] != "new-local-token" || model["url"] != workBuddyChatURL(18787) {
			t.Fatalf("SenseNova model %s has stale connection settings", id)
		}
		if id == "deepseek-v4-flash" && model["customField"] != "keep-me" {
			t.Fatal("custom model field was not preserved")
		}
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backup), "old-local-token") {
		t.Fatal("backup does not contain the previous config")
	}
}

func TestSyncWorkBuddyCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".workbuddy", "models.json")
	result, err := syncWorkBuddyConfig(path, workBuddyChatURL(18787), "local-test-token")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.Added != len(supportedModels) || result.BackupPath != "" {
		t.Fatalf("unexpected create result: %+v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Models) != len(supportedModels) {
		t.Fatalf("created model count = %d, want %d", len(document.Models), len(supportedModels))
	}
}

func TestSyncWorkBuddyRefusesUnrelatedIDCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	original := `{"models":[{"id":"deepseek-v4-flash","name":"Another provider","vendor":"Other","url":"https://example.invalid/v1","apiKey":"other-token"}]}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := syncWorkBuddyConfig(path, workBuddyChatURL(18787), "local-test-token"); err == nil {
		t.Fatal("unrelated ID collision was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatal("conflicting file was modified")
	}
}

func TestSyncWorkBuddyIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	first, err := syncWorkBuddyConfig(path, workBuddyChatURL(18787), "local-test-token")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created {
		t.Fatalf("first sync did not create the config: %+v", first)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := syncWorkBuddyConfig(path, workBuddyChatURL(18787), "local-test-token")
	if err != nil {
		t.Fatal(err)
	}
	if second.Added != 0 || second.Updated != 0 || second.Unchanged != len(supportedModels) || second.BackupPath != "" {
		t.Fatalf("unexpected second sync result: %+v", second)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("idempotent sync rewrote the config")
	}
}
