package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	agnesConnectivityURL = "https://apihub.agnes-ai.com/v1/models"
	clashProxyURL        = "http://127.0.0.1:7890"
)

type RouteCheck struct {
	Online bool   `json:"online"`
	Detail string `json:"detail"`
}

type ConnectivityStatus struct {
	Known     bool       `json:"known"`
	Direct    RouteCheck `json:"direct"`
	Clash     RouteCheck `json:"clash"`
	Preferred string     `json:"preferred"`
	CheckedAt time.Time  `json:"checkedAt"`
}

type ConnectivityMonitor struct {
	logger   *JSONLLogger
	direct   *http.Client
	clash    *http.Client
	interval time.Duration
	url      string

	mu      sync.RWMutex
	status  ConnectivityStatus
	stop    chan struct{}
	done    chan struct{}
	start   sync.Once
	close   sync.Once
	started bool
}

func NewConnectivityMonitor(logger *JSONLLogger, interval time.Duration) (*ConnectivityMonitor, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	proxyURL, err := url.Parse(clashProxyURL)
	if err != nil {
		return nil, err
	}
	newTransport := func(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
		return &http.Transport{
			Proxy: proxy,
			DialContext: (&net.Dialer{
				Timeout:   4 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   4 * time.Second,
			ResponseHeaderTimeout: 4 * time.Second,
		}
	}
	newClient := func(transport *http.Transport) *http.Client {
		return &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &ConnectivityMonitor{
		logger:   logger,
		direct:   newClient(newTransport(nil)),
		clash:    newClient(newTransport(http.ProxyURL(proxyURL))),
		interval: interval,
		url:      agnesConnectivityURL,
		status: ConnectivityStatus{
			Direct:    RouteCheck{Detail: "正在检测…"},
			Clash:     RouteCheck{Detail: "正在检测…"},
			Preferred: "检测中",
		},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}, nil
}

func (monitor *ConnectivityMonitor) Start() {
	monitor.start.Do(func() {
		monitor.mu.Lock()
		monitor.started = true
		monitor.mu.Unlock()
		go monitor.loop()
	})
}

func (monitor *ConnectivityMonitor) Close() {
	monitor.close.Do(func() {
		monitor.mu.RLock()
		started := monitor.started
		monitor.mu.RUnlock()
		if started {
			close(monitor.stop)
			<-monitor.done
		}
		monitor.direct.CloseIdleConnections()
		monitor.clash.CloseIdleConnections()
	})
}

func (monitor *ConnectivityMonitor) Status() ConnectivityStatus {
	monitor.mu.RLock()
	defer monitor.mu.RUnlock()
	return monitor.status
}

func (monitor *ConnectivityMonitor) CheckNow(ctx context.Context) ConnectivityStatus {
	type result struct {
		name  string
		check RouteCheck
	}
	results := make(chan result, 2)
	go func() { results <- result{name: "direct", check: checkAgnesRoute(ctx, monitor.direct, monitor.url)} }()
	go func() { results <- result{name: "clash", check: checkAgnesRoute(ctx, monitor.clash, monitor.url)} }()
	next := ConnectivityStatus{Known: true, CheckedAt: time.Now()}
	for range 2 {
		item := <-results
		if item.name == "direct" {
			next.Direct = item.check
		} else {
			next.Clash = item.check
		}
	}
	switch {
	case next.Direct.Online:
		next.Preferred = "直连"
	case next.Clash.Online:
		next.Preferred = "Clash · 127.0.0.1:7890"
	default:
		next.Preferred = "当前无可用路线"
	}
	monitor.mu.Lock()
	previous := monitor.status
	monitor.status = next
	monitor.mu.Unlock()
	if !previous.Known || previous.Direct.Online != next.Direct.Online || previous.Clash.Online != next.Clash.Online {
		monitor.logger.Log("connectivity_changed", map[string]any{
			"directOnline": next.Direct.Online,
			"clashOnline":  next.Clash.Online,
			"preferred":    next.Preferred,
		})
	}
	return next
}

func (monitor *ConnectivityMonitor) loop() {
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

func checkAgnesRoute(ctx context.Context, client *http.Client, targetURL string) RouteCheck {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, targetURL, nil)
	if err != nil {
		return RouteCheck{Detail: "无法创建检测请求"}
	}
	response, err := client.Do(request)
	if err != nil {
		return RouteCheck{Detail: compactError(err)}
	}
	_, _ = io.CopyN(io.Discard, response.Body, 512)
	_ = response.Body.Close()
	return RouteCheck{Online: true, Detail: fmt.Sprintf("HTTP %d", response.StatusCode)}
}

func compactError(err error) string {
	if err == nil {
		return ""
	}
	if netError, ok := err.(net.Error); ok && netError.Timeout() {
		return "连接超时"
	}
	return redactText(err.Error())
}
