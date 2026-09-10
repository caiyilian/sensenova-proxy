package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"
)

const defaultConnectivityURL = "https://platform.sensenova.cn/"

type ProxyResolver struct{}

func (resolver *ProxyResolver) Proxy(request *http.Request) (*url.URL, error) {
	proxyURL, _, err := resolver.Resolve(request)
	return proxyURL, err
}

func (resolver *ProxyResolver) Resolve(request *http.Request) (*url.URL, string, error) {
	if hasStandardProxyEnvironment() {
		proxyURL, err := http.ProxyFromEnvironment(request)
		if err != nil {
			return nil, "环境代理配置错误", err
		}
		if proxyURL == nil {
			return nil, "直连（环境变量绕过）", nil
		}
		return proxyURL, "环境代理 " + safeProxyURL(proxyURL), nil
	}
	if raw := firstEnvironment("ALL_PROXY", "all_proxy"); raw != "" {
		proxyURL, err := parseProxyURL(raw)
		if err != nil {
			return nil, "ALL_PROXY 配置错误", err
		}
		return proxyURL, "环境代理 " + safeProxyURL(proxyURL), nil
	}

	proxyURL, enabled, err := windowsSystemProxy(request.URL.Scheme)
	if err != nil {
		return nil, "系统代理配置错误", err
	}
	if !enabled {
		return nil, "直连", nil
	}
	return proxyURL, "系统代理 " + safeProxyURL(proxyURL), nil
}

func hasStandardProxyEnvironment() bool {
	return firstEnvironment("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy") != ""
}

func firstEnvironment(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func windowsSystemProxy(scheme string) (*url.URL, bool, error) {
	key, err := registry.OpenKey(
		registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`,
		registry.QUERY_VALUE,
	)
	if err != nil {
		return nil, false, nil
	}
	defer key.Close()
	enabled, _, err := key.GetIntegerValue("ProxyEnable")
	if err != nil || enabled == 0 {
		return nil, false, nil
	}
	raw, _, err := key.GetStringValue("ProxyServer")
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, false, nil
	}

	selected := strings.TrimSpace(raw)
	if strings.Contains(selected, "=") {
		selected = ""
		for _, item := range strings.Split(raw, ";") {
			name, value, found := strings.Cut(item, "=")
			if found && strings.EqualFold(strings.TrimSpace(name), scheme) {
				selected = strings.TrimSpace(value)
				break
			}
		}
	}
	if selected == "" {
		return nil, false, nil
	}
	proxyURL, err := parseProxyURL(selected)
	if err != nil {
		return nil, false, err
	}
	return proxyURL, true, nil
}

func parseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL")
	}
	return parsed, nil
}

func safeProxyURL(proxyURL *url.URL) string {
	copyURL := *proxyURL
	copyURL.User = nil
	return copyURL.String()
}

type NetworkStatus struct {
	Known     bool      `json:"known"`
	Online    bool      `json:"online"`
	Detail    string    `json:"detail"`
	Route     string    `json:"route"`
	CheckedAt time.Time `json:"checkedAt"`
}

type NetworkMonitor struct {
	logger   *JSONLLogger
	resolver *ProxyResolver
	client   *http.Client
	url      string
	interval time.Duration

	mu      sync.RWMutex
	status  NetworkStatus
	updates chan NetworkStatus
	stop    chan struct{}
	done    chan struct{}
	start   sync.Once
	close   sync.Once
	started bool
}

func NewNetworkMonitor(logger *JSONLLogger, resolver *ProxyResolver, interval time.Duration) *NetworkMonitor {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = resolver.Proxy
	transport.DisableCompression = true
	return &NetworkMonitor{
		logger:   logger,
		resolver: resolver,
		client: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
		},
		url:      defaultConnectivityURL,
		interval: interval,
		status: NetworkStatus{
			Detail: "正在检测网络…",
			Route:  "检测中",
		},
		updates: make(chan NetworkStatus, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (monitor *NetworkMonitor) Start() {
	monitor.start.Do(func() {
		monitor.mu.Lock()
		monitor.started = true
		monitor.mu.Unlock()
		go monitor.loop()
	})
}

func (monitor *NetworkMonitor) Close() {
	monitor.close.Do(func() {
		monitor.mu.RLock()
		started := monitor.started
		monitor.mu.RUnlock()
		if !started {
			monitor.client.CloseIdleConnections()
			return
		}
		close(monitor.stop)
		<-monitor.done
		monitor.client.CloseIdleConnections()
	})
}

func (monitor *NetworkMonitor) Status() NetworkStatus {
	monitor.mu.RLock()
	defer monitor.mu.RUnlock()
	return monitor.status
}

func (monitor *NetworkMonitor) Updates() <-chan NetworkStatus {
	return monitor.updates
}

func (monitor *NetworkMonitor) CheckNow(ctx context.Context) NetworkStatus {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, monitor.url, nil)
	if err != nil {
		return monitor.updateStatus(false, "检测请求创建失败", "未知", err)
	}
	_, route, routeErr := monitor.resolver.Resolve(request)
	if routeErr != nil {
		return monitor.updateStatus(false, "代理配置不可用", route, routeErr)
	}
	response, err := monitor.client.Do(request)
	if err != nil {
		return monitor.updateStatus(false, "无法访问 SenseNova 网站", route, err)
	}
	_, _ = io.CopyN(io.Discard, response.Body, 1024)
	_ = response.Body.Close()
	detail := fmt.Sprintf("SenseNova 网站可访问（HTTP %d）", response.StatusCode)
	return monitor.updateStatus(true, detail, route, nil)
}

func (monitor *NetworkMonitor) loop() {
	defer close(monitor.done)
	monitor.CheckNow(context.Background())
	ticker := time.NewTicker(monitor.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			monitor.CheckNow(context.Background())
		case <-monitor.stop:
			return
		}
	}
}

func (monitor *NetworkMonitor) updateStatus(online bool, detail, route string, checkError error) NetworkStatus {
	if checkError != nil {
		detail += "：" + redactText(checkError.Error())
	}
	next := NetworkStatus{
		Known:     true,
		Online:    online,
		Detail:    detail,
		Route:     route,
		CheckedAt: time.Now(),
	}
	monitor.mu.Lock()
	previous := monitor.status
	monitor.status = next
	monitor.mu.Unlock()
	if !previous.Known || previous.Online != next.Online || previous.Route != next.Route {
		monitor.logger.Log("network_status_changed", map[string]any{
			"online": next.Online,
			"detail": next.Detail,
			"route":  next.Route,
		})
	}
	select {
	case monitor.updates <- next:
	default:
		select {
		case <-monitor.updates:
		default:
		}
		select {
		case monitor.updates <- next:
		default:
		}
	}
	return next
}
