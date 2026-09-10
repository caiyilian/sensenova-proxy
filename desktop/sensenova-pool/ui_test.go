package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeKeyFilePathAcceptsAnyExtension(t *testing.T) {
	tempDir := t.TempDir()
	for _, name := range []string{"sensenova_apikeys", "accounts.json", "accounts.txt", "keys.custom"} {
		path := filepath.Join(tempDir, name)
		if err := os.WriteFile(path, []byte(fakeKey("i")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		normalized, err := normalizeKeyFilePath(path)
		if err != nil {
			t.Fatalf("normalize %q: %v", name, err)
		}
		if normalized != path {
			t.Fatalf("normalized path = %q, want %q", normalized, path)
		}
	}
}

func TestNormalizeKeyFilePathRejectsDirectory(t *testing.T) {
	if _, err := normalizeKeyFilePath(t.TempDir()); err == nil {
		t.Fatal("directory was accepted as a key file")
	}
}

func TestUnconfiguredKeyStoreIsNotAReadError(t *testing.T) {
	logger, err := NewJSONLLogger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewKeyStore("", logger, 0)
	snapshot := store.Reload("test")
	if snapshot.LastError != "" {
		t.Fatalf("unconfigured key store reported an error: %q", snapshot.LastError)
	}
	if len(snapshot.Accounts) != 0 {
		t.Fatalf("unconfigured account count = %d, want 0", len(snapshot.Accounts))
	}
}
