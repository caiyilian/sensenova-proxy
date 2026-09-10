package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateOpenCodeProviderPreservesOtherProvidersAndComments(t *testing.T) {
	original := []byte(`{
  // This comment must survive.
  "provider": {
    "sensenova-pool": {
      "options": { "baseURL": "http://old:18787/v1", "apiKey": "old-local-token" },
      "models": { "deepseek-v4-flash": { "name": "DeepSeek V4 Flash" } }
    },
    "agnes-proxy": {
      "options": { "baseURL": "http://127.0.0.1:18788/v1", "apiKey": "keep-agnes-token" }
    }
  }
}`)
	updated, err := updateOpenCodeProvider(original, "http://127.0.0.1:18787/v1", "new-local-token")
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	for _, wanted := range []string{"// This comment must survive.", "new-local-token", "http://127.0.0.1:18787/v1", "keep-agnes-token"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("updated config lost %q", wanted)
		}
	}
	if strings.Contains(text, "old-local-token") || strings.Contains(text, "http://old:18787/v1") {
		t.Fatal("old SenseNova connection settings remain")
	}
}

func TestSyncOpenCodeConfigCreatesBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	data, err := marshalOpenCodeConfig("http://old:18787/v1", "old-local-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := syncOpenCodeConfig(path, "http://127.0.0.1:18787/v1", "new-local-token")
	if err != nil {
		t.Fatal(err)
	}
	if backup == "" {
		t.Fatal("backup path is empty")
	}
	backupData, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backupData), "old-local-token") {
		t.Fatal("backup does not contain the previous config")
	}
}

func TestUpdateOpenCodeProviderAddsMissingProviderAndEnablesIt(t *testing.T) {
	original := []byte(`{
  // Keep the user's provider.
  "provider": {
    "agnes-proxy": {
      "options": { "baseURL": "http://127.0.0.1:18788/v1", "apiKey": "keep-agnes-token" }
    }
  },
  "enabled_providers": ["agnes-proxy"]
}`)
	updated, err := updateOpenCodeProvider(original, "http://127.0.0.1:18787/v1", "new-local-token")
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	for _, wanted := range []string{"// Keep the user's provider.", "keep-agnes-token", "\"sensenova-pool\"", "new-local-token"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("updated config lost %q", wanted)
		}
	}
	if count := strings.Count(text, `"sensenova-pool"`); count != 2 {
		t.Fatalf("sensenova-pool occurrence count = %d, want provider plus enabled entry", count)
	}
}

func TestUpdateOpenCodeProviderAddsProviderSectionWhenMissing(t *testing.T) {
	original := []byte("{\n  \"theme\": \"system\"\n}\n")
	updated, err := updateOpenCodeProvider(original, "http://127.0.0.1:18787/v1", "new-local-token")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), `"provider"`) || !strings.Contains(string(updated), `"enabled_providers"`) {
		t.Fatalf("missing inserted sections: %s", updated)
	}
	var decoded map[string]any
	if err := json.Unmarshal(updated, &decoded); err != nil {
		t.Fatalf("inserted config is not valid JSON: %v\n%s", err, updated)
	}
}

func TestNewOpenCodeConfigContainsOnlySenseNovaPool(t *testing.T) {
	data, err := marshalOpenCodeConfig("http://172.31.102.132:18787/v1", "local-test-token")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Model            string                      `json:"model"`
		Provider         map[string]openCodeProvider `json:"provider"`
		EnabledProviders []string                    `json:"enabled_providers"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Provider) != 1 || decoded.Provider["sensenova-pool"].Options["apiKey"] != "local-test-token" {
		t.Fatal("generated provider is incomplete")
	}
	if len(decoded.Provider["sensenova-pool"].Models) != len(supportedModels) {
		t.Fatalf("generated model count = %d, want %d", len(decoded.Provider["sensenova-pool"].Models), len(supportedModels))
	}
	if decoded.Model != "sensenova-pool/deepseek-v4-flash" || len(decoded.EnabledProviders) != 1 {
		t.Fatal("generated defaults are incorrect")
	}
}
