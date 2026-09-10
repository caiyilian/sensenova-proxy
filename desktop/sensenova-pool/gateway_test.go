package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixedNetworkStatus struct {
	status NetworkStatus
}

func (provider fixedNetworkStatus) Status() NetworkStatus { return provider.status }

func TestGatewayRetriesAnotherAccountWithoutLoggingSecrets(t *testing.T) {
	tempDir := t.TempDir()
	firstKey := fakeKey("e")
	secondKey := fakeKey("f")
	keyPath := filepath.Join(tempDir, "keys.txt")
	if err := os.WriteFile(keyPath, []byte(firstKey+"\n"+secondKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, err := NewJSONLLogger(filepath.Join(tempDir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewKeyStore(keyPath, logger, time.Second)
	store.Reload("test")
	pool := NewAccountPool()

	var requests atomic.Int32
	var mu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		mu.Unlock()
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q", request.URL.Path)
		}
		if requests.Add(1) == 1 {
			response.WriteHeader(http.StatusTooManyRequests)
			_, _ = response.Write([]byte(`{"error":{"message":"inference exceeds tpm/rpm limit"}}`))
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	gateway, err := NewGateway(store, pool, logger, fixedNetworkStatus{NetworkStatus{Known: true, Online: true, Route: "test"}}, &ProxyResolver{}, "local-test-token")
	if err != nil {
		t.Fatal(err)
	}
	upstreamURL, _ := url.Parse(upstream.URL + "/v1/")
	gateway.upstreamBase = upstreamURL
	gateway.client = upstream.Client()
	if err := gateway.Start("127.0.0.1", 0); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = gateway.Close(ctx)
	}()

	secretPrompt := "do-not-log-this-prompt"
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"` + secretPrompt + `"}]}`)
	request, err := http.NewRequest(http.MethodPost, gateway.Address()+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-test-token")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if response.Header.Get("X-SenseNova-Pool-Attempts") != "2" {
		t.Fatalf("attempt header = %q, want 2", response.Header.Get("X-SenseNova-Pool-Attempts"))
	}
	if requests.Load() != 2 {
		t.Fatalf("upstream request count = %d, want 2", requests.Load())
	}
	mu.Lock()
	gotAuthorizations := append([]string(nil), authorizations...)
	mu.Unlock()
	if len(gotAuthorizations) != 2 || gotAuthorizations[0] != "Bearer "+firstKey || gotAuthorizations[1] != "Bearer "+secondKey {
		t.Fatalf("unexpected account order (values intentionally omitted), count=%d", len(gotAuthorizations))
	}

	logText := readAllLogs(t, logger.Dir())
	for _, forbidden := range []string{firstKey, secondKey, secretPrompt, "Bearer local-test-token"} {
		if strings.Contains(logText, forbidden) {
			t.Fatal("logs exposed forbidden request content or credential")
		}
	}
}

func TestGatewayBlocksWhileConnectivityMonitorIsOffline(t *testing.T) {
	tempDir := t.TempDir()
	logger, err := NewJSONLLogger(filepath.Join(tempDir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(tempDir, "keys.txt")
	if err := os.WriteFile(keyPath, []byte(fakeKey("g")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewKeyStore(keyPath, logger, time.Second)
	store.Reload("test")
	gateway, err := NewGateway(store, NewAccountPool(), logger, fixedNetworkStatus{NetworkStatus{Known: true, Online: false, Route: "stale proxy"}}, &ProxyResolver{}, "local-test-token")
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash"}`))
	request.Header.Set("X-Api-Key", "local-test-token")
	gateway.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "network_unavailable") {
		t.Fatalf("unexpected body: %s", recorder.Body.String())
	}
}

func TestLoggerRedactsCredentialsInsideNestedValues(t *testing.T) {
	tempDir := t.TempDir()
	logger, err := NewJSONLLogger(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	key := fakeKey("h")
	logger.Log("nested", map[string]any{
		"outer": map[string]any{
			"credential": key,
			"header":     "Bearer " + key,
		},
	})
	contents := readAllLogs(t, tempDir)
	if strings.Contains(contents, key) || strings.Contains(contents, "Bearer ") {
		t.Fatal("nested credential was written to the log")
	}
	if !strings.Contains(contents, "[redacted]") {
		t.Fatal("redaction marker is missing")
	}
}

func readAllLogs(t *testing.T, directory string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var contents strings.Builder
	for _, entry := range entries {
		data, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		contents.Write(data)
	}
	return contents.String()
}
