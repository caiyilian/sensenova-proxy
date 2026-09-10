package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

func TestProxyControllerStartsNodeAndReloadsChangedEnvironmentKey(t *testing.T) {
	nodePath, err := exec.LookPath("node.exe")
	if err != nil {
		t.Skip("node.exe is required for the Agnes desktop controller")
	}
	tempDir := t.TempDir()
	scriptPath := filepath.Join(tempDir, "fake-agnes.js")
	script := `
const http = require('http');
console.log(process.env.AGNES_API_KEY);
const health = {status:'ok',stats:{requests:0,successes:0,failures:0,rateRetries:0,routeFallbacks:0,recoveries:0},routes:{preferred:'clash',lastSuccessfulRoute:'clash',directProbeAfterMs:1000},rateLimits:[]};
http.createServer((req,res) => {res.setHeader('content-type','application/json');res.end(JSON.stringify(health));}).listen(Number(process.env.AGNES_PROXY_PORT),'127.0.0.1');
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	port := reservePort(t)
	paths := AppPaths{DataDir: tempDir, SettingsFile: filepath.Join(tempDir, "settings.json"), LogDir: filepath.Join(tempDir, "logs")}
	logger, err := NewJSONLLogger(paths.LogDir)
	if err != nil {
		t.Fatal(err)
	}
	keys := NewAgnesKeyMonitor(logger, time.Second)
	var keyMu sync.RWMutex
	keyValue := "first-agnes-controller-test-secret"
	keys.reader = func() (string, string) {
		keyMu.RLock()
		defer keyMu.RUnlock()
		return keyValue, "test"
	}
	keys.Refresh()
	controller := NewProxyController(Settings{Port: port, NodePath: nodePath, ProxyScriptPath: scriptPath}, paths, keys, logger, "local-controller-test-token")
	controller.RunMonitor()
	defer controller.Close()
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool { return controller.Status().HealthOK })
	firstPID := controller.Status().PID
	if firstPID == 0 {
		t.Fatal("Node proxy PID is missing")
	}

	keyMu.Lock()
	keyValue = "second-agnes-controller-test-secret"
	keyMu.Unlock()
	keys.Refresh()
	eventually(t, 7*time.Second, func() bool {
		status := controller.Status()
		return status.HealthOK && status.PID != 0 && status.PID != firstPID
	})

	controller.Close()
	contents := readLogs(t, paths.LogDir)
	if strings.Contains(contents, "first-agnes-controller-test-secret") || strings.Contains(contents, "second-agnes-controller-test-secret") {
		t.Fatal("controller log exposed an Agnes API key")
	}
}

func TestReplaceEnvironmentIsCaseInsensitive(t *testing.T) {
	result := replaceEnvironment([]string{"Path=one", "agnes_api_key=old", "OTHER=value"}, map[string]string{"AGNES_API_KEY": "new"})
	joined := strings.Join(result, "\n")
	if strings.Contains(joined, "old") || !strings.Contains(joined, "AGNES_API_KEY=new") {
		t.Fatalf("unexpected environment replacement: %v", result)
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
