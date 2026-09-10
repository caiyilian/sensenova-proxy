package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type AgnesRateLimit struct {
	Model         string `json:"model"`
	RetryAfterMS  int64  `json:"retryAfterMs"`
	ProbeInFlight bool   `json:"probeInFlight"`
}

type AgnesGatewayHealth struct {
	Status string `json:"status"`
	Stats  struct {
		Requests       uint64 `json:"requests"`
		Successes      uint64 `json:"successes"`
		Failures       uint64 `json:"failures"`
		RateRetries    uint64 `json:"rateRetries"`
		RouteFallbacks uint64 `json:"routeFallbacks"`
		Recoveries     uint64 `json:"recoveries"`
	} `json:"stats"`
	Routes struct {
		Preferred           string `json:"preferred"`
		LastSuccessfulRoute string `json:"lastSuccessfulRoute"`
		DirectProbeAfterMS  int64  `json:"directProbeAfterMs"`
	} `json:"routes"`
	RateLimits []AgnesRateLimit `json:"rateLimits"`
}

type ControllerStatus struct {
	Running          bool
	PID              int
	StartedAt        time.Time
	LastExitAt       time.Time
	LastError        string
	NodePath         string
	ScriptPath       string
	ChildKeyRef      string
	HealthKnown      bool
	HealthOK         bool
	Health           AgnesGatewayHealth
	LastHealthCheck  time.Time
	ConsecutiveExits int
}

type runningChild struct {
	cmd          *exec.Cmd
	done         chan struct{}
	keyRef       string
	expectedStop bool
}

type ProxyController struct {
	settings   Settings
	paths      AppPaths
	keys       *AgnesKeyMonitor
	logger     *JSONLLogger
	client     *http.Client
	localToken string

	mu               sync.RWMutex
	status           ControllerStatus
	child            *runningChild
	closing          bool
	lastStartAttempt time.Time
	opMu             sync.Mutex

	stop  chan struct{}
	done  chan struct{}
	close sync.Once
}

func NewProxyController(settings Settings, paths AppPaths, keys *AgnesKeyMonitor, logger *JSONLLogger, localToken string) *ProxyController {
	return &ProxyController{
		settings:   settings,
		paths:      paths,
		keys:       keys,
		logger:     logger,
		client:     &http.Client{Timeout: 1500 * time.Millisecond},
		localToken: localToken,
		status: ControllerStatus{
			NodePath:   settings.NodePath,
			ScriptPath: settings.ProxyScriptPath,
		},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

func (controller *ProxyController) Start() error {
	controller.opMu.Lock()
	defer controller.opMu.Unlock()
	return controller.startChild()
}

func (controller *ProxyController) RunMonitor() {
	go controller.monitorLoop()
}

func (controller *ProxyController) Restart() error {
	controller.opMu.Lock()
	defer controller.opMu.Unlock()
	controller.stopChild()
	return controller.startChild()
}

func (controller *ProxyController) Close() {
	controller.close.Do(func() {
		controller.mu.Lock()
		controller.closing = true
		controller.mu.Unlock()
		close(controller.stop)
		<-controller.done
		controller.opMu.Lock()
		controller.stopChild()
		controller.opMu.Unlock()
		controller.client.CloseIdleConnections()
	})
}

func (controller *ProxyController) Status() ControllerStatus {
	controller.mu.RLock()
	defer controller.mu.RUnlock()
	status := controller.status
	status.Health.RateLimits = append([]AgnesRateLimit(nil), controller.status.Health.RateLimits...)
	return status
}

func (controller *ProxyController) startChild() error {
	controller.mu.RLock()
	if controller.child != nil {
		controller.mu.RUnlock()
		return nil
	}
	closing := controller.closing
	controller.mu.RUnlock()
	if closing {
		return errors.New("controller is closing")
	}

	keySnapshot := controller.keys.Snapshot()
	apiKey := controller.keys.APIKey()
	if apiKey == "" {
		return controller.setStartError("未检测到 AGNES_API_KEY")
	}
	nodePath, scriptPath, err := validateRuntime(controller.settings.NodePath, controller.settings.ProxyScriptPath)
	if err != nil {
		return controller.setStartError(err.Error())
	}
	if err := os.MkdirAll(controller.paths.LogDir, 0o700); err != nil {
		return controller.setStartError(err.Error())
	}

	command := exec.Command(nodePath, scriptPath)
	command.Dir = filepath.Dir(scriptPath)
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		"AGNES_API_KEY":           apiKey,
		"AGNES_PROXY_HOST":        "127.0.0.1",
		"AGNES_PROXY_PORT":        strconv.Itoa(controller.settings.Port),
		"AGNES_PROXY_LOCAL_TOKEN": controller.localToken,
		"AGNES_PROXY_LOG_DIR":     controller.paths.LogDir,
	})
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return controller.setStartError(err.Error())
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return controller.setStartError(err.Error())
	}

	controller.mu.Lock()
	controller.lastStartAttempt = time.Now()
	controller.mu.Unlock()
	if err := command.Start(); err != nil {
		return controller.setStartError(err.Error())
	}
	child := &runningChild{cmd: command, done: make(chan struct{}), keyRef: keySnapshot.KeyRef}
	controller.mu.Lock()
	controller.child = child
	controller.status.Running = true
	controller.status.PID = command.Process.Pid
	controller.status.StartedAt = time.Now()
	controller.status.LastError = ""
	controller.status.NodePath = nodePath
	controller.status.ScriptPath = scriptPath
	controller.status.ChildKeyRef = keySnapshot.KeyRef
	controller.status.HealthKnown = false
	controller.status.HealthOK = false
	controller.mu.Unlock()
	controller.logger.Log("proxy_process_started", map[string]any{
		"pid":    command.Process.Pid,
		"node":   nodePath,
		"script": scriptPath,
		"keyRef": keySnapshot.KeyRef,
		"port":   controller.settings.Port,
	})
	go controller.captureOutput(stdout, "stdout")
	go controller.captureOutput(stderr, "stderr")
	go controller.waitForChild(child)
	return nil
}

func (controller *ProxyController) stopChild() {
	controller.mu.Lock()
	child := controller.child
	if child != nil {
		child.expectedStop = true
	}
	controller.mu.Unlock()
	if child == nil {
		return
	}
	if child.cmd.Process != nil {
		_ = child.cmd.Process.Kill()
	}
	select {
	case <-child.done:
	case <-time.After(3 * time.Second):
		controller.logger.Log("proxy_process_stop_timeout", map[string]any{"pid": child.cmd.Process.Pid})
	}
}

func (controller *ProxyController) waitForChild(child *runningChild) {
	err := child.cmd.Wait()
	controller.mu.Lock()
	if controller.child != child {
		controller.mu.Unlock()
		close(child.done)
		return
	}
	controller.child = nil
	controller.status.Running = false
	controller.status.PID = 0
	controller.status.HealthOK = false
	controller.status.LastExitAt = time.Now()
	expected := child.expectedStop || controller.closing
	if !expected {
		controller.status.ConsecutiveExits++
		controller.status.LastError = redactText(fmt.Sprintf("后台代理意外退出：%v", err))
	} else {
		controller.status.ConsecutiveExits = 0
	}
	controller.mu.Unlock()
	close(child.done)
	controller.logger.Log("proxy_process_exited", map[string]any{
		"expected": expected,
		"error":    err,
	})
}

func (controller *ProxyController) captureOutput(reader io.Reader, stream string) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			controller.logger.Log("proxy_output", map[string]any{"stream": stream, "message": line})
		}
	}
}

func (controller *ProxyController) monitorLoop() {
	defer close(controller.done)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	controller.checkHealth()
	for {
		select {
		case <-ticker.C:
			controller.reconcile()
			controller.checkHealth()
		case <-controller.stop:
			return
		}
	}
}

func (controller *ProxyController) reconcile() {
	key := controller.keys.Snapshot()
	controller.mu.RLock()
	child := controller.child
	lastAttempt := controller.lastStartAttempt
	closing := controller.closing
	controller.mu.RUnlock()
	if closing {
		return
	}
	if child != nil && (!key.Present || child.keyRef != key.KeyRef) {
		_ = controller.Restart()
		return
	}
	if child == nil && key.Present && time.Since(lastAttempt) >= 5*time.Second {
		_ = controller.Start()
	}
}

func (controller *ProxyController) checkHealth() {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", controller.settings.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	response, err := controller.client.Do(request)
	now := time.Now()
	if err != nil {
		controller.mu.Lock()
		controller.status.HealthKnown = true
		controller.status.HealthOK = false
		controller.status.LastHealthCheck = now
		controller.mu.Unlock()
		return
	}
	defer response.Body.Close()
	var health AgnesGatewayHealth
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&health)
	ok := decodeErr == nil && response.StatusCode == http.StatusOK && health.Status == "ok"
	controller.mu.Lock()
	controller.status.HealthKnown = true
	controller.status.HealthOK = ok
	controller.status.LastHealthCheck = now
	if ok {
		controller.status.Health = health
	}
	controller.mu.Unlock()
}

func (controller *ProxyController) setStartError(message string) error {
	err := errors.New(redactText(message))
	controller.mu.Lock()
	controller.lastStartAttempt = time.Now()
	controller.status.Running = false
	controller.status.LastError = err.Error()
	controller.mu.Unlock()
	controller.logger.Log("proxy_process_start_failed", map[string]any{"error": err})
	return err
}

func validateRuntime(configuredNode, configuredScript string) (string, string, error) {
	nodePath := strings.TrimSpace(configuredNode)
	if nodePath == "" {
		var err error
		nodePath, err = exec.LookPath("node.exe")
		if err != nil {
			return "", "", errors.New("找不到 node.exe；请先安装 Node.js 或把它加入 PATH")
		}
	}
	if info, err := os.Stat(nodePath); err != nil || info.IsDir() {
		return "", "", fmt.Errorf("Node.js 路径不可用：%s", nodePath)
	}
	scriptPath := strings.TrimSpace(configuredScript)
	if scriptPath == "" {
		scriptPath = discoverProxyScript()
	}
	if info, err := os.Stat(scriptPath); err != nil || info.IsDir() {
		return "", "", fmt.Errorf("找不到 Agnes 代理脚本：%s", scriptPath)
	}
	nodePath, _ = filepath.Abs(nodePath)
	scriptPath, _ = filepath.Abs(scriptPath)
	return nodePath, scriptPath, nil
}

func discoverProxyScript() string {
	if configured := strings.TrimSpace(os.Getenv("AGNES_PROXY_SCRIPT")); configured != "" {
		if abs, err := filepath.Abs(configured); err == nil {
			return abs
		}
	}
	candidates := make([]string, 0, 10)
	if executable, err := os.Executable(); err == nil {
		directory := filepath.Dir(executable)
		for range 7 {
			candidates = append(candidates, filepath.Join(directory, "agnes-proxy.js"))
			parent := filepath.Dir(directory)
			if parent == directory {
				break
			}
			directory = parent
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "agnes-proxy.js"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			abs, _ := filepath.Abs(candidate)
			return abs
		}
	}
	return ""
}

func replaceEnvironment(base []string, values map[string]string) []string {
	result := make([]string, 0, len(base)+len(values))
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, replace := values[strings.ToUpper(name)]; replace {
			continue
		}
		result = append(result, entry)
	}
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}
