package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateAgnesOpenCodeProviderPreservesSenseNovaAndComments(t *testing.T) {
	input := []byte(`{
  // This comment and provider must survive Agnes synchronization.
  "model": "sensenova-pool/deepseek-v4-flash",
  "provider": {
    "sensenova-pool": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "SenseNova Pool",
      "options": { "baseURL": "http://127.0.0.1:18787/v1", "apiKey": "sense-token-preserved" },
      "models": { "deepseek-v4-flash": { "name": "DeepSeek V4 Flash" } }
    }
  },
  "enabled_providers": ["sensenova-pool"]
}
`)
	updated, err := updateAgnesOpenCodeProvider(input, "http://127.0.0.1:18788/v1", "agnes-local-token")
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	for _, wanted := range []string{
		"This comment and provider must survive",
		`"sensenova-pool"`,
		`"sense-token-preserved"`,
		`"agnes-proxy"`,
		`"agnes-local-token"`,
		`"agnes-2.0-flash"`,
		`"agnes-2.5-flash"`,
		`"agnes-3.0-flash"`,
	} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("updated OpenCode config does not contain %q:\n%s", wanted, text)
		}
	}
	if !strings.Contains(text, `"enabled_providers": ["agnes-proxy","sensenova-pool"]`) &&
		!strings.Contains(text, `"enabled_providers": ["agnes-proxy", "sensenova-pool"]`) {
		t.Fatalf("Agnes provider was not enabled without replacing SenseNova: %s", text)
	}
}

func TestSyncAgnesOpenCodeConfigBacksUpAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	original := []byte(`{
  "provider": {
    "sensenova-pool": { "options": { "baseURL": "http://127.0.0.1:18787/v1", "apiKey": "keep-sense" } },
    "agnes-proxy": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Agnes Resilient",
      "options": { "baseURL": "https://old.invalid/v1", "apiKey": "old-local" },
      "models": { "agnes-2.0-flash": { "name": "Custom Agnes 2.0" } }
    }
  },
  "enabled_providers": ["sensenova-pool"]
}
`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := syncAgnesOpenCodeConfig(path, "http://127.0.0.1:18788/v1", "new-local")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Updated || first.Created || first.Unchanged || first.BackupPath == "" {
		t.Fatalf("unexpected first result: %+v", first)
	}
	backup, err := os.ReadFile(first.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backup, original) {
		t.Fatal("OpenCode backup does not match the original file")
	}
	afterFirst, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(afterFirst, []byte(`"apiKey": "keep-sense"`)) || !bytes.Contains(afterFirst, []byte(`"name": "Custom Agnes 2.0"`)) {
		t.Fatal("synchronization changed SenseNova or an existing Agnes model customization")
	}

	second, err := syncAgnesOpenCodeConfig(path, "http://127.0.0.1:18788/v1", "new-local")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Unchanged || second.Created || second.Updated || second.BackupPath != "" {
		t.Fatalf("unexpected idempotent result: %+v", second)
	}
	afterSecond, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterFirst, afterSecond) {
		t.Fatal("idempotent OpenCode synchronization rewrote the file")
	}
}

func TestSyncAgnesOpenCodeConfigCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".config", "opencode", "opencode.jsonc")
	result, err := syncAgnesOpenCodeConfig(path, "http://127.0.0.1:18788/v1", "local-token")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.Updated || result.Unchanged || result.BackupPath != "" {
		t.Fatalf("unexpected create result: %+v", result)
	}
	var config agnesOpenCodeExport
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	provider, ok := config.Provider["agnes-proxy"]
	if !ok || len(provider.Models) != 3 || provider.Options["apiKey"] != "local-token" {
		t.Fatalf("unexpected generated config: %+v", config)
	}
}

func TestSyncAgnesWorkBuddyUpdatesAgnesAndPreservesSenseNovaArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	original := []byte(`[
  {
    "id": "deepseek-v4-flash",
    "name": "DeepSeek V4 Flash · SenseNova",
    "vendor": "SenseNova",
    "url": "http://127.0.0.1:18787/v1/chat/completions",
    "apiKey": "sense-token-preserved",
    "customField": "keep-me"
  },
  {
    "id": "agnes-2.0-flash",
    "name": "Agnes 2.0 Flash",
    "vendor": "Agnes",
    "url": "https://apihub.agnes-ai.com/v1",
    "apiKey": "old-token"
  }
]`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := syncAgnesWorkBuddyConfig(path, "http://127.0.0.1:18788/v1/chat/completions", "agnes-local-token")
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 2 || result.Updated != 1 || result.Unchanged != 0 || result.BackupPath == "" {
		t.Fatalf("unexpected first result: %+v", result)
	}
	models := readWorkBuddyModelsForTest(t, path)
	if len(models) != 4 {
		t.Fatalf("expected four models, got %d", len(models))
	}
	byID := make(map[string]map[string]any)
	for _, model := range models {
		id, _ := model["id"].(string)
		byID[id] = model
	}
	sense := byID["deepseek-v4-flash"]
	if sense["apiKey"] != "sense-token-preserved" || sense["customField"] != "keep-me" {
		t.Fatalf("SenseNova model was overwritten: %+v", sense)
	}
	for _, id := range []string{"agnes-2.0-flash", "agnes-2.5-flash", "agnes-3.0-flash"} {
		model := byID[id]
		if model["url"] != "http://127.0.0.1:18788/v1/chat/completions" || model["apiKey"] != "agnes-local-token" {
			t.Fatalf("Agnes model %s was not synchronized: %+v", id, model)
		}
	}

	second, err := syncAgnesWorkBuddyConfig(path, "http://127.0.0.1:18788/v1/chat/completions", "agnes-local-token")
	if err != nil {
		t.Fatal(err)
	}
	if second.Added != 0 || second.Updated != 0 || second.Unchanged != 3 || second.BackupPath != "" {
		t.Fatalf("unexpected idempotent result: %+v", second)
	}
}

func TestSyncAgnesWorkBuddyRefusesUnrelatedIDCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	original := []byte(`[{"id":"agnes-3.0-flash","name":"Unrelated model","vendor":"Other","url":"https://example.invalid/v1","apiKey":"keep"}]`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := syncAgnesWorkBuddyConfig(path, "http://127.0.0.1:18788/v1/chat/completions", "local-token"); err == nil {
		t.Fatal("expected an unrelated model ID collision to be rejected")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("WorkBuddy file changed despite a model ID conflict")
	}
}

func TestSyncDetectedLocalClientsHandlesClientsIndependently(t *testing.T) {
	root := t.TempDir()
	openCodePath := filepath.Join(root, ".config", "opencode", "opencode.jsonc")
	workBuddyPath := filepath.Join(root, ".workbuddy", "models.json")
	if err := os.MkdirAll(filepath.Dir(openCodePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(workBuddyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openCodePath, []byte(`{"provider":{"sensenova-pool":{"options":{"apiKey":"keep"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workBuddyPath, []byte(`[{"id":"deepseek-v4-flash","name":"SenseNova","vendor":"SenseNova","apiKey":"keep"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	result := syncDetectedLocalClients(openCodePath, workBuddyPath, 18788, "local-token")
	if result.DetectedCount() != 2 || result.Err() != nil {
		t.Fatalf("unexpected client synchronization result: %+v, error=%v", result, result.Err())
	}
	if result.OpenCode.Updated != 1 || result.WorkBuddy.Added != 3 {
		t.Fatalf("unexpected synchronization counts: %+v", result)
	}

	none := syncDetectedLocalClients(
		filepath.Join(root, "missing-open-code", "opencode.jsonc"),
		filepath.Join(root, "missing-workbuddy", "models.json"),
		18788,
		"local-token",
	)
	if none.DetectedCount() != 0 || none.Err() != nil {
		t.Fatalf("missing clients should be ignored: %+v", none)
	}
}

func readWorkBuddyModelsForTest(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var models []map[string]any
	if err := json.Unmarshal(data, &models); err != nil {
		t.Fatal(err)
	}
	return models
}
