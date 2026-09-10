package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeKey(character string) string {
	return "sk-" + strings.Repeat(character, 32)
}

func TestParseKeyFileIgnoresInvalidAndDuplicateLines(t *testing.T) {
	first := fakeKey("a")
	second := fakeKey("b")
	parsed := parseKeyFile([]byte("\xef\xbb\xbf# friend keys\n" + first + "\nnot-a-key\n" + first + "\n\n" + second + "\n"))

	if got, want := len(parsed.Accounts), 2; got != want {
		t.Fatalf("account count = %d, want %d", got, want)
	}
	if got, want := len(parsed.InvalidLines), 1; got != want {
		t.Fatalf("invalid line count = %d, want %d", got, want)
	}
	if parsed.InvalidLines[0].LineNumber != 3 {
		t.Fatalf("invalid line = %d, want 3", parsed.InvalidLines[0].LineNumber)
	}
	if got, want := len(parsed.DuplicateLines), 1; got != want {
		t.Fatalf("duplicate line count = %d, want %d", got, want)
	}
	if parsed.DuplicateLines[0].LineNumber != 4 || parsed.DuplicateLines[0].DuplicateOfLine != 2 {
		t.Fatalf("unexpected duplicate metadata: %+v", parsed.DuplicateLines[0])
	}

	encoded, err := json.Marshal(parsed.Accounts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), first) || strings.Contains(string(encoded), second) {
		t.Fatal("account JSON exposed a full API key")
	}
}

func TestKeyStoreHotReloadsAndRetainsLastGoodRead(t *testing.T) {
	tempDir := t.TempDir()
	keyPath := filepath.Join(tempDir, "keys.txt")
	first := fakeKey("c")
	second := fakeKey("d")
	if err := os.WriteFile(keyPath, []byte(first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, err := NewJSONLLogger(filepath.Join(tempDir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewKeyStore(keyPath, logger, 15*time.Millisecond)
	store.Start()
	defer store.Close()

	eventually(t, time.Second, func() bool { return len(store.Accounts()) == 1 })
	if err := os.WriteFile(keyPath, []byte(first+"\n"+second+"\nwrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool {
		snapshot := store.Snapshot()
		return len(snapshot.Accounts) == 2 && len(snapshot.InvalidLines) == 1 && snapshot.Reloads >= 2
	})

	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return store.Snapshot().LastError != "" })
	if got := len(store.Accounts()); got != 2 {
		t.Fatalf("retained account count = %d, want 2", got)
	}

	entries, err := os.ReadDir(logger.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		contents, readErr := os.ReadFile(filepath.Join(logger.Dir(), entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(contents), first) || strings.Contains(string(contents), second) {
			t.Fatal("log file exposed a full API key")
		}
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
