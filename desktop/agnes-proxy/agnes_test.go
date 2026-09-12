package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgnesKeyMonitorTracksChangesWithoutLoggingKey(t *testing.T) {
	tempDir := t.TempDir()
	logger, err := NewJSONLLogger(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	monitor := NewAgnesKeyMonitor(logger, time.Second)
	value := "agnes-test-secret-value-which-must-not-appear"
	monitor.reader = func() (string, string) { return value, "test" }
	first := monitor.Refresh()
	if !first.Present || first.KeyRef == "" || first.Source != "test" {
		t.Fatalf("unexpected first snapshot: %+v", first)
	}
	logger.Log("nested_secret_test", map[string]any{"nested": map[string]any{"value": value}})
	value = "agnes-replacement-secret-value"
	second := monitor.Refresh()
	if second.KeyRef == first.KeyRef {
		t.Fatal("key change did not update the fingerprint")
	}
	contents := readLogs(t, tempDir)
	if strings.Contains(contents, "agnes-test-secret") || strings.Contains(contents, value) {
		t.Fatal("Agnes API key was written to a log")
	}
}

func TestConnectivityMonitorReportsClashOnlyRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	logger, err := NewJSONLLogger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	monitor, err := NewConnectivityMonitor(logger, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	monitor.url = server.URL
	monitor.direct = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("direct route unavailable")
	})}
	monitor.clash = server.Client()
	status := monitor.CheckNow(context.Background())
	if status.Direct.Online || !status.Clash.Online || !strings.HasPrefix(status.Preferred, "Clash") {
		t.Fatalf("unexpected route status: %+v", status)
	}
}

func TestNativeProxyUsesChangedKeyWithoutRestart(t *testing.T) {
	tempDir := t.TempDir()
	firstKey := "first-agnes-controller-test-secret"
	secondKey := "second-agnes-controller-test-secret"
	keyPath := filepath.Join(tempDir, "agnes-key.data")
	if err := os.WriteFile(keyPath, []byte(firstKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		received <- request.Header.Get("Authorization")
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"id":"test","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	port := reservePort(t)
	paths := AppPaths{DataDir: tempDir, SettingsFile: filepath.Join(tempDir, "settings.json"), LogDir: filepath.Join(tempDir, "logs")}
	logger, err := NewJSONLLogger(paths.LogDir)
	if err != nil {
		t.Fatal(err)
	}
	keys := NewAgnesKeyMonitor(logger, time.Second, keyPath)
	keys.Refresh()
	controller, err := NewProxyController(Settings{Port: port}, keys, nil, logger, "local-controller-test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(upstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	controller.gateway.upstream = target
	controller.RunMonitor()
	defer controller.Close()
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool { return controller.Status().HealthOK })
	callGateway := func() {
		request, requestErr := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), strings.NewReader(`{"model":"agnes-2.0-flash","messages":[]}`))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.Header.Set("Authorization", "Bearer local-controller-test-token")
		request.Header.Set("Content-Type", "application/json")
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("unexpected gateway status: %d", response.StatusCode)
		}
	}
	callGateway()
	if authorization := <-received; authorization != "Bearer "+firstKey {
		t.Fatalf("gateway used unexpected first credential: %q", authorization)
	}

	if err := os.WriteFile(keyPath, []byte(secondKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys.Refresh()
	callGateway()
	if authorization := <-received; authorization != "Bearer "+secondKey {
		t.Fatalf("gateway did not hot-reload the credential: %q", authorization)
	}

	controller.Close()
	contents := readLogs(t, paths.LogDir)
	if strings.Contains(contents, firstKey) || strings.Contains(contents, secondKey) {
		t.Fatal("controller log exposed an Agnes API key")
	}
}

func TestParseAgnesKeyFileFormats(t *testing.T) {
	expected := "agnes-portable-test-key"
	for name, data := range map[string]string{
		"plain":  expected + "\n",
		"dotenv": "AGNES_API_KEY='" + expected + "'\n",
		"json":   `{"apiKey":"` + expected + `"}`,
		"array":  `[{"token":"` + expected + `"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if actual := parseAgnesKeyFile([]byte(data)); actual != expected {
				t.Fatalf("unexpected key: %q", actual)
			}
		})
	}
}

func TestAgnesKeyFileHotReloadRetainsLastGoodValue(t *testing.T) {
	tempDir := t.TempDir()
	keyPath := filepath.Join(tempDir, "key-without-extension")
	firstKey := "agnes-retained-first-secret"
	secondKey := "agnes-retained-second-secret"
	if err := os.WriteFile(keyPath, []byte(firstKey), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, err := NewJSONLLogger(filepath.Join(tempDir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	monitor := NewAgnesKeyMonitor(logger, time.Second, keyPath)
	first := monitor.Refresh()
	if !first.Present || monitor.APIKey() != firstKey {
		t.Fatalf("unexpected first key snapshot: %+v", first)
	}
	if err := os.WriteFile(keyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	retained := monitor.Refresh()
	if !retained.Present || !retained.RetainedLastGood || retained.LastError == "" || monitor.APIKey() != firstKey {
		t.Fatalf("last good key was not retained: %+v", retained)
	}
	if err := os.WriteFile(keyPath, []byte(secondKey), 0o600); err != nil {
		t.Fatal(err)
	}
	second := monitor.Refresh()
	if second.LastError != "" || second.KeyRef == first.KeyRef || monitor.APIKey() != secondKey {
		t.Fatalf("replacement key was not loaded: %+v", second)
	}
	contents := readLogs(t, logger.Dir())
	if strings.Contains(contents, firstKey) || strings.Contains(contents, secondKey) {
		t.Fatal("key hot-reload log exposed an Agnes API key")
	}
}

func TestGatewayRetriesDisconnectBeforeReturningSuccess(t *testing.T) {
	logger, err := NewJSONLLogger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := NewAgnesKeyMonitor(logger, time.Second)
	keys.reader = func() (string, string) { return "agnes-network-retry-secret", "test" }
	keys.Refresh()
	gateway, err := NewAgnesGateway(0, keys, nil, logger, "local-retry-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	gateway.direct = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	clashAttempts := 0
	gateway.clash = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		clashAttempts++
		if clashAttempts == 1 {
			return nil, errors.New("timeout awaiting response headers")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"recovered"}}]}`)),
			Request:    request,
		}, nil
	})}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"agnes-3.0-flash","messages":[]}`))
	request.Header.Set("Authorization", "Bearer local-retry-token")
	response := httptest.NewRecorder()
	started := time.Now()
	gateway.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "recovered") {
		t.Fatalf("unexpected response: status=%d body=%s", response.Code, response.Body.String())
	}
	if clashAttempts != 2 || time.Since(started) < 1900*time.Millisecond {
		t.Fatalf("disconnect was not retried with backoff: attempts=%d duration=%s", clashAttempts, time.Since(started))
	}
	contents := readLogs(t, logger.Dir())
	if !strings.Contains(contents, "network_retry_scheduled") || strings.Contains(contents, keys.APIKey()) {
		t.Fatal("retry was not logged safely")
	}
}

func TestEmbeddedClashRecoveryExtractsWithoutExternalRepository(t *testing.T) {
	tempDir := t.TempDir()
	logger, err := NewJSONLLogger(filepath.Join(tempDir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	paths := AppPaths{
		DataDir:            tempDir,
		RecoveryScriptFile: filepath.Join(tempDir, "clash-node-helper.ps1"),
	}
	recovery, err := NewClashRecovery(paths, logger)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := recovery.Snapshot()
	if !snapshot.Enabled || snapshot.ScriptPath != paths.RecoveryScriptFile {
		t.Fatalf("embedded recovery helper is unavailable: %+v", snapshot)
	}
	data, err := os.ReadFile(paths.RecoveryScriptFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("recovery helper must include a UTF-8 BOM for Windows PowerShell 5.1")
	}
	if !strings.Contains(string(data), "良心云|选择|Proxy") {
		t.Fatal("embedded recovery helper is not the Liangxinyun-aware version")
	}
}

func TestSelectAgnesLocalTokenPrefersUserEnvironment(t *testing.T) {
	token, source := selectAgnesLocalToken(" user-local-token ", "process-local-token")
	if token != "user-local-token" || source != "用户环境变量" {
		t.Fatalf("unexpected selection: token=%q source=%q", token, source)
	}
	token, source = selectAgnesLocalToken("", " process-local-token ")
	if token != "process-local-token" || source != "进程环境变量" {
		t.Fatalf("unexpected process selection: token=%q source=%q", token, source)
	}
	token, source = selectAgnesLocalToken("", "")
	if token != defaultLocalProxyToken || source != "程序默认值" {
		t.Fatalf("unexpected default selection: token=%q source=%q", token, source)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func reservePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func readLogs(t *testing.T, directory string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var contents strings.Builder
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		contents.Write(data)
	}
	return contents.String()
}
