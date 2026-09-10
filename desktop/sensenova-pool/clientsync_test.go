package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncDetectedLocalClientsDoesNothingWhenNeitherClientExists(t *testing.T) {
	root := t.TempDir()
	openCodePath := filepath.Join(root, "missing-opencode", "opencode.jsonc")
	workBuddyPath := filepath.Join(root, "missing-workbuddy", "models.json")

	result := syncDetectedLocalClients(openCodePath, workBuddyPath, 18787, "local-test-token")
	if result.DetectedCount() != 0 || result.Err() != nil {
		t.Fatalf("unexpected result: %+v, error: %v", result, result.Err())
	}
	for _, path := range []string{openCodePath, workBuddyPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("undetected client config was created: %s", path)
		}
	}
}

func TestSyncDetectedLocalClientsSynchronizesBothClients(t *testing.T) {
	root := t.TempDir()
	openCodePath := filepath.Join(root, "opencode", "opencode.jsonc")
	workBuddyPath := filepath.Join(root, "workbuddy", "models.json")
	if err := os.MkdirAll(filepath.Dir(openCodePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(workBuddyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	openCodeOriginal := `{
  // Preserve this provider.
  "provider": {
    "agnes-proxy": {
      "options": { "baseURL": "http://127.0.0.1:18788/v1", "apiKey": "keep-agnes-token" }
    }
  }
}`
	workBuddyOriginal := `{"models":[{"id":"agnes-2.0-flash","name":"Agnes 2.0 Flash","vendor":"Agnes","apiKey":"keep-agnes-token"}]}`
	if err := os.WriteFile(openCodePath, []byte(openCodeOriginal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workBuddyPath, []byte(workBuddyOriginal), 0o600); err != nil {
		t.Fatal(err)
	}

	result := syncDetectedLocalClients(openCodePath, workBuddyPath, 18787, "new-local-token")
	if result.DetectedCount() != 2 || result.Err() != nil {
		t.Fatalf("unexpected result: %+v, error: %v", result, result.Err())
	}
	if result.OpenCode.BackupPath == "" || result.WorkBuddy.BackupPath == "" {
		t.Fatalf("existing configs were not backed up: %+v", result)
	}
	if result.WorkBuddy.Added != len(supportedModels) {
		t.Fatalf("WorkBuddy added %d models, want %d", result.WorkBuddy.Added, len(supportedModels))
	}

	for _, path := range []string{openCodePath, workBuddyPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, "new-local-token") || !strings.Contains(text, "keep-agnes-token") {
			t.Fatalf("sync did not update SenseNova and preserve Agnes in %s", path)
		}
	}
}

func TestSyncDetectedLocalClientsCreatesConfigInsideDetectedDirectory(t *testing.T) {
	root := t.TempDir()
	openCodePath := filepath.Join(root, "opencode", "opencode.jsonc")
	workBuddyPath := filepath.Join(root, "missing-workbuddy", "models.json")
	if err := os.MkdirAll(filepath.Dir(openCodePath), 0o700); err != nil {
		t.Fatal(err)
	}

	result := syncDetectedLocalClients(openCodePath, workBuddyPath, 18787, "local-test-token")
	if result.DetectedCount() != 1 || !result.OpenCode.Created || result.WorkBuddy.Detected || result.Err() != nil {
		t.Fatalf("unexpected result: %+v, error: %v", result, result.Err())
	}
	if _, err := os.Stat(openCodePath); err != nil {
		t.Fatalf("OpenCode config was not created: %v", err)
	}
	if _, err := os.Stat(workBuddyPath); !os.IsNotExist(err) {
		t.Fatalf("WorkBuddy config was unexpectedly created: %v", err)
	}
}
