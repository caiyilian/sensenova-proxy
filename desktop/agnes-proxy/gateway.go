package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	agnesUpstreamBaseURL  = "https://apihub.agnes-ai.com/v1"
	maximumRequestBytes   = 64 * 1024 * 1024
	maximumErrorBodyBytes = 8 * 1024 * 1024
)

var agnesModels = []string{
	"agnes-2.0-flash",
	"agnes-2.5-flash",
	"agnes-3.0-flash",
}

type AgnesRateLimit struct {
	Model         string `json:"model"`
	RetryAfterMS  int64  `json:"retryAfterMs"`
	ProbeInFlight bool   `json:"probeInFlight"`
}

type AgnesGatewayHealth struct {
	Status        string `json:"status"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
	Stats         struct {
		Requests       uint64 `json:"requests"`
		Successes      uint64 `json:"successes"`
		Failures       uint64 `json:"failures"`
		RateRetries    uint64 `json:"rateRetries"`
		RouteFallbacks uint64 `json:"routeFallbacks"`
		Recoveries     uint64 `json:"recoveries"`
		Canceled       uint64 `json:"canceled"`
	} `json:"stats"`
	Routes struct {
		Preferred           string         `json:"preferred"`
		LastSuccessfulRoute string         `json:"lastSuccessfulRoute"`
		DirectProbeAfterMS  int64          `json:"directProbeAfterMs"`
		TransportFailures   map[string]int `json:"transportFailures"`
	} `json:"routes"`
	RateLimits []AgnesRateLimit `json:"rateLimits"`
}

type gatewayCounters struct {
	requests       atomic.Uint64
	successes      atomic.Uint64
	failures       atomic.Uint64
	rateRetries    atomic.Uint64
	routeFallbacks atomic.Uint64
	recoveries     atomic.Uint64
	canceled       atomic.Uint64
}

type AgnesGateway struct {
	port         int
	keys         *AgnesKeyMonitor
	connectivity *ConnectivityMonitor
	logger       *JSONLLogger
	localToken   string
	upstream     *url.URL
	direct       *http.Client
	clash        *http.Client
	routes       *agnesRouteSelector
	rates        *agnesRateGate
	recovery     *ClashRecovery
	counters     gatewayCounters

	mu        sync.RWMutex
	server    *http.Server
	listener  net.Listener
	startedAt time.Time
	address   string
	lastError string
}

func NewAgnesGateway(port int, keys *AgnesKeyMonitor, connectivity *ConnectivityMonitor, logger *JSONLLogger, localToken string, recovery *ClashRecovery) (*AgnesGateway, error) {
	upstream, err := url.Parse(agnesUpstreamBaseURL)
	if err != nil {
		return nil, err
	}
	proxyURL, err := url.Parse(clashProxyURL)
	if err != nil {
		return nil, err
	}
	newTransport := func(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
		return &http.Transport{
			Proxy: proxy,
			DialContext: (&net.Dialer{
				Timeout:   8 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   12 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
		}
	}
	newClient := func(transport *http.Transport) *http.Client {
		return &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &AgnesGateway{
		port:         port,
		keys:         keys,
		connectivity: connectivity,
		logger:       logger,
		localToken:   localToken,
		upstream:     upstream,
		direct:       newClient(newTransport(nil)),
		clash:        newClient(newTransport(http.ProxyURL(proxyURL))),
		routes:       newAgnesRouteSelector(5 * time.Minute),
		rates:        newAgnesRateGate(time.Minute),
		recovery:     recovery,
	}, nil
}

func (gateway *AgnesGateway) Start() error {
	gateway.mu.Lock()
	if gateway.server != nil {
		gateway.mu.Unlock()
		return nil
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", gateway.port))
	if err != nil {
		gateway.lastError = redactText(err.Error())
		gateway.mu.Unlock()
		return err
	}
	server := &http.Server{
		Handler:           gateway,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	gateway.listener = listener
	gateway.server = server
	gateway.startedAt = time.Now()
	gateway.address = listener.Addr().String()
	gateway.lastError = ""
	address := gateway.address
	gateway.mu.Unlock()

	gateway.logger.Log("native_gateway_started", map[string]any{"address": address})
	go func() {
		err := server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			gateway.mu.Lock()
			if gateway.server == server {
				gateway.lastError = redactText(err.Error())
				gateway.server = nil
				gateway.listener = nil
			}
			gateway.mu.Unlock()
			gateway.logger.Log("native_gateway_stopped_unexpectedly", map[string]any{"error": err})
		}
	}()
	return nil
}

func (gateway *AgnesGateway) Restart() error {
	_ = gateway.stopServer()
	return gateway.Start()
}

func (gateway *AgnesGateway) Close() {
	_ = gateway.stopServer()
	gateway.direct.CloseIdleConnections()
	gateway.clash.CloseIdleConnections()
}

func (gateway *AgnesGateway) stopServer() error {
	gateway.mu.Lock()
	server := gateway.server
	gateway.server = nil
	gateway.listener = nil
	gateway.mu.Unlock()
	if server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := server.Shutdown(ctx)
	if err != nil {
		_ = server.Close()
	}
	gateway.logger.Log("native_gateway_stopped", map[string]any{})
	return err
}

func (gateway *AgnesGateway) Running() bool {
	gateway.mu.RLock()
	defer gateway.mu.RUnlock()
	return gateway.server != nil
}

func (gateway *AgnesGateway) Address() string {
	gateway.mu.RLock()
	defer gateway.mu.RUnlock()
	return gateway.address
}

func (gateway *AgnesGateway) LastError() string {
	gateway.mu.RLock()
	defer gateway.mu.RUnlock()
	return gateway.lastError
}

func (gateway *AgnesGateway) Snapshot() AgnesGatewayHealth {
	health := AgnesGatewayHealth{Status: "ok"}
	gateway.mu.RLock()
	if gateway.server == nil {
		health.Status = "stopped"
	}
	startedAt := gateway.startedAt
	gateway.mu.RUnlock()
	if !startedAt.IsZero() {
		health.UptimeSeconds = int64(time.Since(startedAt) / time.Second)
	}
	health.Stats.Requests = gateway.counters.requests.Load()
	health.Stats.Successes = gateway.counters.successes.Load()
	health.Stats.Failures = gateway.counters.failures.Load()
	health.Stats.RateRetries = gateway.counters.rateRetries.Load()
	health.Stats.RouteFallbacks = gateway.counters.routeFallbacks.Load()
	health.Stats.Recoveries = gateway.counters.recoveries.Load()
	health.Stats.Canceled = gateway.counters.canceled.Load()
	route := gateway.routes.snapshot()
	health.Routes.Preferred = route.preferred
	health.Routes.LastSuccessfulRoute = route.lastSuccessfulRoute
	health.Routes.DirectProbeAfterMS = route.directProbeAfter.Milliseconds()
	health.Routes.TransportFailures = route.failures
	health.RateLimits = gateway.rates.snapshot()
	return health
}

func (gateway *AgnesGateway) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	setAgnesCORS(response)
	if request.Method == http.MethodOptions {
		response.WriteHeader(http.StatusNoContent)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/health" {
		writeAgnesJSON(response, http.StatusOK, gateway.Snapshot())
		return
	}
	if !gateway.authorized(request) {
		writeAgnesJSON(response, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"type": "authentication_error", "message": "Invalid local gateway token."},
		})
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/v1/models" {
		data := make([]map[string]string, 0, len(agnesModels))
		for _, model := range agnesModels {
			data = append(data, map[string]string{"id": model, "object": "model", "owned_by": "agnes"})
		}
		writeAgnesJSON(response, http.StatusOK, map[string]any{"object": "list", "data": data})
		return
	}
	if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" {
		writeAgnesJSON(response, http.StatusNotFound, map[string]any{
			"error": map[string]any{"type": "not_found", "message": "Supported endpoint: POST /v1/chat/completions"},
		})
		return
	}
	gateway.handleChat(response, request)
}

func (gateway *AgnesGateway) handleChat(response http.ResponseWriter, request *http.Request) {
	gateway.counters.requests.Add(1)
	startedAt := time.Now()
	requestID := randomShortID()
	body, err := io.ReadAll(io.LimitReader(request.Body, maximumRequestBytes+1))
	if err != nil {
		gateway.counters.failures.Add(1)
		writeAgnesJSON(response, http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "Unable to read request body."}})
		return
	}
	if len(body) > maximumRequestBytes {
		gateway.counters.failures.Add(1)
		writeAgnesJSON(response, http.StatusRequestEntityTooLarge, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "Request body is too large."}})
		return
	}
	apiKey := gateway.keys.APIKey()
	if apiKey == "" {
		gateway.counters.failures.Add(1)
		writeAgnesJSON(response, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"type": "agnes_proxy_unavailable", "code": "api_key_missing", "message": "Choose a valid Agnes API key file in Agnes Proxy."},
		})
		return
	}
	model := extractAgnesModel(body)
	deadline := time.Now().Add(10 * time.Minute)
	rateRetries := 0
	upstreamRetries := 0
	networkRetries := 0

	for {
		resetAgnesUpstreamResponseHeaders(response.Header())
		permit, waitErr := gateway.rates.wait(request.Context(), model, deadline)
		if waitErr != nil {
			if request.Context().Err() != nil {
				gateway.counters.canceled.Add(1)
				return
			}
			gateway.counters.failures.Add(1)
			response.Header().Set("Retry-After", "60")
			response.Header().Set("X-Agnes-Rate-Retries", strconv.Itoa(rateRetries))
			writeAgnesJSON(response, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]any{"type": "agnes_proxy_unavailable", "code": "rate_queue_timeout", "message": "Agnes remained rate-limited beyond the local queue deadline; see the gateway log."},
			})
			return
		}

		result := gateway.fetchWithRoutes(request.Context(), request, body, apiKey, requestID, model)
		if result.response == nil {
			gateway.rates.releaseProbe(model, permit)
			if request.Context().Err() != nil {
				gateway.counters.canceled.Add(1)
				return
			}
			if result.safeToRetry {
				networkRetries++
				wait := agnesRetryBackoff(networkRetries)
				if time.Now().Add(wait).Before(deadline) {
					gateway.logger.Log("network_retry_scheduled", map[string]any{
						"requestId": requestID,
						"model":     model,
						"route":     result.route,
						"attempt":   networkRetries,
						"waitMs":    wait.Milliseconds(),
						"error":     result.err,
					})
					if sleepAgnesContext(request.Context(), wait) {
						continue
					}
					gateway.counters.canceled.Add(1)
					return
				}
			}
			gateway.counters.failures.Add(1)
			gateway.logger.Log("request_failed", map[string]any{
				"requestId": requestID,
				"model":     model,
				"route":     result.route,
				"category":  "network",
				"error":     result.err,
			})
			resetAgnesUpstreamResponseHeaders(response.Header())
			writeAgnesJSON(response, http.StatusBadGateway, map[string]any{
				"error": map[string]any{"type": "agnes_proxy_network_error", "code": "no_network_route", "message": "Neither the direct nor Clash route could reach Agnes; see the gateway log."},
			})
			return
		}

		upstream := result.response
		if upstream.StatusCode >= 200 && upstream.StatusCode < 300 {
			gateway.rates.markHealthy(model, permit)
			resetAgnesUpstreamResponseHeaders(response.Header())
			response.Header().Set("X-Agnes-Route", result.route)
			response.Header().Set("X-Agnes-Rate-Retries", strconv.Itoa(rateRetries))
			copyAgnesResponseHeaders(response.Header(), upstream.Header)
			bytesForwarded, streamErr := forwardAgnesStream(response, upstream.StatusCode, upstream.Body)
			_ = upstream.Body.Close()
			if streamErr != nil {
				if bytesForwarded == 0 {
					networkRetries++
					wait := agnesRetryBackoff(networkRetries)
					if networkRetries <= 6 && time.Now().Add(wait).Before(deadline) {
						gateway.logger.Log("response_stream_retry_scheduled", map[string]any{
							"requestId": requestID,
							"model":     model,
							"route":     result.route,
							"attempt":   networkRetries,
							"waitMs":    wait.Milliseconds(),
							"error":     streamErr,
						})
						if sleepAgnesContext(request.Context(), wait) {
							continue
						}
					}
				}
				if request.Context().Err() != nil {
					gateway.counters.canceled.Add(1)
				} else {
					gateway.counters.failures.Add(1)
				}
				gateway.logger.Log("response_stream_failed", map[string]any{
					"requestId":      requestID,
					"model":          model,
					"route":          result.route,
					"bytesForwarded": bytesForwarded,
					"error":          streamErr,
				})
				if bytesForwarded == 0 && request.Context().Err() == nil {
					resetAgnesUpstreamResponseHeaders(response.Header())
					writeAgnesJSON(response, http.StatusBadGateway, map[string]any{
						"error": map[string]any{
							"type":    "agnes_proxy_network_error",
							"code":    "response_stream_failed",
							"message": "The Agnes response stream disconnected before any content was delivered; retry attempts were exhausted.",
						},
					})
				}
				return
			}
			gateway.counters.successes.Add(1)
			gateway.logger.Log("request_succeeded", map[string]any{
				"requestId":  requestID,
				"model":      model,
				"route":      result.route,
				"status":     upstream.StatusCode,
				"durationMs": time.Since(startedAt).Milliseconds(),
			})
			return
		}

		failureBody := result.bufferedBody
		if failureBody == nil {
			failureBody, _ = io.ReadAll(io.LimitReader(upstream.Body, maximumErrorBodyBytes))
			_ = upstream.Body.Close()
		}
		category, retryable := classifyAgnesFailure(upstream.StatusCode, failureBody)
		if category == "rate_limit" {
			cooldown := gateway.rates.markLimited(model, parseAgnesRetryAfter(upstream.Header.Get("Retry-After")))
			rateRetries++
			gateway.counters.rateRetries.Add(1)
			gateway.logger.Log("rate_limit_queued", map[string]any{
				"requestId": requestID,
				"model":     model,
				"route":     result.route,
				"status":    upstream.StatusCode,
				"attempt":   rateRetries,
				"waitMs":    cooldown.Milliseconds(),
			})
			continue
		}
		gateway.rates.markHealthy(model, permit)
		maxRetries := 2
		if isAgnesRouteFallbackStatus(upstream.StatusCode) {
			maxRetries = 12
		}
		if retryable && upstreamRetries < maxRetries {
			upstreamRetries++
			wait := 5 * time.Second
			if isAgnesRouteFallbackStatus(upstream.StatusCode) {
				wait = agnesRetryBackoff(upstreamRetries)
			}
			if !time.Now().Add(wait).Before(deadline) {
				retryable = false
			} else {
				gateway.logger.Log("upstream_retry", map[string]any{
					"requestId": requestID,
					"model":     model,
					"route":     result.route,
					"status":    upstream.StatusCode,
					"category":  category,
					"attempt":   upstreamRetries,
					"waitMs":    wait.Milliseconds(),
				})
				if !sleepAgnesContext(request.Context(), wait) {
					gateway.counters.canceled.Add(1)
					return
				}
				continue
			}
		}

		gateway.counters.failures.Add(1)
		response.Header().Set("X-Agnes-Route", result.route)
		response.Header().Set("X-Agnes-Rate-Retries", strconv.Itoa(rateRetries))
		copyAgnesResponseHeaders(response.Header(), upstream.Header)
		response.Header().Set("Content-Length", strconv.Itoa(len(failureBody)))
		response.WriteHeader(upstream.StatusCode)
		_, _ = response.Write(failureBody)
		gateway.logger.Log("request_rejected", map[string]any{
			"requestId":  requestID,
			"model":      model,
			"route":      result.route,
			"status":     upstream.StatusCode,
			"category":   category,
			"durationMs": time.Since(startedAt).Milliseconds(),
		})
		return
	}
}

type agnesUpstreamResult struct {
	response     *http.Response
	bufferedBody []byte
	route        string
	err          error
	safeToRetry  bool
}

func (gateway *AgnesGateway) fetchWithRoutes(ctx context.Context, incoming *http.Request, body []byte, apiKey, requestID, model string) agnesUpstreamResult {
	var buffered []agnesUpstreamResult
	var last agnesUpstreamResult
	clashFailed := false
	for _, route := range gateway.routeOrder() {
		result := gateway.fetchRoute(ctx, route, incoming, body, apiKey)
		if result.response != nil {
			if !isAgnesRouteFallbackStatus(result.response.StatusCode) {
				return result
			}
			result.bufferedBody, _ = io.ReadAll(io.LimitReader(result.response.Body, maximumErrorBodyBytes))
			_ = result.response.Body.Close()
			buffered = append(buffered, result)
			gateway.routes.markTransportFailure(route)
			gateway.counters.routeFallbacks.Add(1)
			gateway.logger.Log("route_http_fallback", map[string]any{"requestId": requestID, "model": model, "route": route, "status": result.response.StatusCode})
			if route == "clash" {
				clashFailed = true
			}
			continue
		}
		safe := isSafeAgnesConnectFailure(result.err)
		result.safeToRetry = ctx.Err() == nil
		last = result
		event := "route_connect_failed"
		if !safe {
			event = "route_ambiguous_failure"
		}
		gateway.logger.Log(event, map[string]any{
			"requestId":   requestID,
			"model":       model,
			"route":       route,
			"safeFailure": safe,
			"willRetry":   ctx.Err() == nil,
			"error":       result.err,
		})
		gateway.routes.markTransportFailure(route)
		gateway.counters.routeFallbacks.Add(1)
		if route == "clash" {
			clashFailed = true
		}
	}
	if clashFailed && gateway.recovery != nil {
		recovery := gateway.recovery.Run(ctx, requestID, model)
		if recovery.Attempted {
			gateway.counters.recoveries.Add(1)
		}
		gateway.logger.Log("clash_recovery_result", map[string]any{"requestId": requestID, "model": model, "ok": recovery.OK, "reason": recovery.Reason})
		if recovery.OK && ctx.Err() == nil {
			if result := gateway.fetchRoute(ctx, "clash", incoming, body, apiKey); result.response != nil {
				return result
			} else {
				result.safeToRetry = ctx.Err() == nil
				last = result
			}
		}
	}
	if len(buffered) > 0 {
		return buffered[len(buffered)-1]
	}
	return last
}

func (gateway *AgnesGateway) fetchRoute(ctx context.Context, route string, incoming *http.Request, body []byte, apiKey string) agnesUpstreamResult {
	attemptContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	target := *gateway.upstream
	basePath := strings.TrimSuffix(gateway.upstream.Path, "/")
	incomingPath := incoming.URL.Path
	if basePath != "" && strings.HasPrefix(incomingPath, basePath+"/") {
		target.Path = incomingPath
	} else {
		target.Path = basePath + "/" + strings.TrimPrefix(incomingPath, "/")
	}
	target.RawQuery = incoming.URL.RawQuery
	request, err := http.NewRequestWithContext(attemptContext, incoming.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return agnesUpstreamResult{route: route, err: err}
	}
	copyAgnesRequestHeaders(request.Header, incoming.Header)
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Del("X-Api-Key")
	request.Header.Set("Accept-Encoding", "identity")
	client := gateway.direct
	if route == "clash" {
		client = gateway.clash
	}
	upstream, err := client.Do(request)
	if err != nil {
		return agnesUpstreamResult{route: route, err: err}
	}
	gateway.routes.markConnected(route)
	return agnesUpstreamResult{response: upstream, route: route}
}

func (gateway *AgnesGateway) routeOrder() []string {
	if gateway.connectivity != nil {
		status := gateway.connectivity.Status()
		if status.Known && !status.Direct.Online && status.Clash.Online {
			return []string{"clash", "direct"}
		}
	}
	return gateway.routes.order()
}

func (gateway *AgnesGateway) authorized(request *http.Request) bool {
	credential := strings.TrimSpace(request.Header.Get("X-Api-Key"))
	authorization := strings.TrimSpace(request.Header.Get("Authorization"))
	if len(authorization) >= 7 && strings.EqualFold(authorization[:7], "Bearer ") {
		credential = strings.TrimSpace(authorization[7:])
	}
	expected := []byte(gateway.localToken)
	actual := []byte(credential)
	return len(expected) == len(actual) && subtle.ConstantTimeCompare(expected, actual) == 1
}

type agnesRatePermit struct {
	probe bool
}

type agnesRateState struct {
	blockedUntil  time.Time
	probeInFlight bool
}

type agnesRateGate struct {
	mu              sync.Mutex
	defaultCooldown time.Duration
	states          map[string]*agnesRateState
}

func newAgnesRateGate(defaultCooldown time.Duration) *agnesRateGate {
	return &agnesRateGate{defaultCooldown: defaultCooldown, states: make(map[string]*agnesRateState)}
}

func (gate *agnesRateGate) wait(ctx context.Context, model string, deadline time.Time) (agnesRatePermit, error) {
	for {
		now := time.Now()
		if !now.Before(deadline) {
			return agnesRatePermit{}, errors.New("rate-limit queue timeout")
		}
		gate.mu.Lock()
		state := gate.states[model]
		if state == nil {
			gate.mu.Unlock()
			return agnesRatePermit{}, nil
		}
		if !now.Before(state.blockedUntil) && !state.probeInFlight {
			state.probeInFlight = true
			gate.mu.Unlock()
			return agnesRatePermit{probe: true}, nil
		}
		wait := 250 * time.Millisecond
		if remaining := time.Until(state.blockedUntil); remaining > 0 && remaining < wait {
			wait = remaining
		}
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		gate.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		if !sleepAgnesContext(ctx, wait) {
			return agnesRatePermit{}, ctx.Err()
		}
	}
}

func (gate *agnesRateGate) markLimited(model string, retryAfter time.Duration) time.Duration {
	cooldown := gate.defaultCooldown
	if retryAfter > cooldown {
		cooldown = retryAfter
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	state := gate.states[model]
	if state == nil {
		state = &agnesRateState{}
		gate.states[model] = state
	}
	until := time.Now().Add(cooldown)
	if until.After(state.blockedUntil) {
		state.blockedUntil = until
	}
	state.probeInFlight = false
	return cooldown
}

func (gate *agnesRateGate) markHealthy(model string, permit agnesRatePermit) {
	if !permit.probe {
		return
	}
	gate.mu.Lock()
	delete(gate.states, model)
	gate.mu.Unlock()
}

func (gate *agnesRateGate) releaseProbe(model string, permit agnesRatePermit) {
	if !permit.probe {
		return
	}
	gate.mu.Lock()
	if state := gate.states[model]; state != nil {
		state.probeInFlight = false
	}
	gate.mu.Unlock()
}

func (gate *agnesRateGate) snapshot() []AgnesRateLimit {
	now := time.Now()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	result := make([]AgnesRateLimit, 0, len(gate.states))
	for model, state := range gate.states {
		retryAfter := state.blockedUntil.Sub(now)
		if retryAfter < 0 {
			retryAfter = 0
		}
		result = append(result, AgnesRateLimit{Model: model, RetryAfterMS: retryAfter.Milliseconds(), ProbeInFlight: state.probeInFlight})
	}
	return result
}

type agnesRouteSnapshot struct {
	preferred           string
	lastSuccessfulRoute string
	directProbeAfter    time.Duration
	failures            map[string]int
}

type agnesRouteSelector struct {
	mu                  sync.Mutex
	preferClashDuration time.Duration
	directRetryAt       time.Time
	lastSuccessful      string
	failures            map[string]int
}

func newAgnesRouteSelector(preferClash time.Duration) *agnesRouteSelector {
	return &agnesRouteSelector{preferClashDuration: preferClash, failures: map[string]int{"direct": 0, "clash": 0}}
}

func (selector *agnesRouteSelector) order() []string {
	selector.mu.Lock()
	defer selector.mu.Unlock()
	if time.Now().Before(selector.directRetryAt) {
		return []string{"clash", "direct"}
	}
	return []string{"direct", "clash"}
}

func (selector *agnesRouteSelector) markTransportFailure(route string) {
	selector.mu.Lock()
	selector.failures[route]++
	if route == "direct" {
		selector.directRetryAt = time.Now().Add(selector.preferClashDuration)
	}
	selector.mu.Unlock()
}

func (selector *agnesRouteSelector) markConnected(route string) {
	selector.mu.Lock()
	selector.lastSuccessful = route
	selector.failures[route] = 0
	if route == "direct" {
		selector.directRetryAt = time.Time{}
	}
	selector.mu.Unlock()
}

func (selector *agnesRouteSelector) snapshot() agnesRouteSnapshot {
	selector.mu.Lock()
	defer selector.mu.Unlock()
	preferred := "direct"
	remaining := time.Until(selector.directRetryAt)
	if remaining > 0 {
		preferred = "clash"
	} else {
		remaining = 0
	}
	return agnesRouteSnapshot{
		preferred:           preferred,
		lastSuccessfulRoute: selector.lastSuccessful,
		directProbeAfter:    remaining,
		failures:            map[string]int{"direct": selector.failures["direct"], "clash": selector.failures["clash"]},
	}
}

func copyAgnesRequestHeaders(target, source http.Header) {
	for name, values := range source {
		lower := strings.ToLower(name)
		if isAgnesHopByHop(lower) || lower == "host" || lower == "content-length" || lower == "authorization" || lower == "x-api-key" {
			continue
		}
		for _, value := range values {
			target.Add(name, value)
		}
	}
}

func copyAgnesResponseHeaders(target, source http.Header) {
	for name, values := range source {
		if isAgnesHopByHop(strings.ToLower(name)) || strings.EqualFold(name, "Content-Length") {
			continue
		}
		target[name] = append([]string(nil), values...)
	}
}

func resetAgnesUpstreamResponseHeaders(header http.Header) {
	for name := range header {
		if !strings.HasPrefix(strings.ToLower(name), "access-control-") {
			header.Del(name)
		}
	}
}

func isAgnesHopByHop(name string) bool {
	switch name {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func isAgnesRouteFallbackStatus(status int) bool {
	switch status {
	case 502, 503, 504, 522, 523, 524:
		return true
	default:
		return false
	}
}

func isSafeAgnesConnectFailure(err error) bool {
	if err == nil {
		return false
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return true
	}
	var operationError *net.OpError
	if errors.As(err, &operationError) && operationError.Op == "dial" {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{"connection refused", "no such host", "network is unreachable", "host is unreachable", "proxyconnect tcp"} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func classifyAgnesFailure(status int, body []byte) (category string, retryable bool) {
	normalized := strings.ToLower(string(body))
	rateLimited := status == http.StatusTooManyRequests ||
		strings.Contains(normalized, "too many requests") ||
		strings.Contains(normalized, "resource exhausted") ||
		((strings.Contains(normalized, "tpm") || strings.Contains(normalized, "rpm") || strings.Contains(normalized, "rate")) &&
			(strings.Contains(normalized, "limit") || strings.Contains(normalized, "exceed")))
	switch {
	case rateLimited:
		return "rate_limit", true
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "auth", false
	case status == http.StatusRequestTimeout:
		return "upstream_timeout", true
	case status >= 500:
		return "upstream", true
	default:
		return "request", false
	}
}

func parseAgnesRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	if date, err := http.ParseTime(value); err == nil {
		duration := time.Until(date)
		if duration > 0 {
			return duration
		}
	}
	return 0
}

func extractAgnesModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &payload) == nil && strings.TrimSpace(payload.Model) != "" {
		return payload.Model
	}
	return "unknown"
}

func forwardAgnesStream(response http.ResponseWriter, status int, body io.Reader) (int64, error) {
	buffer := make([]byte, 32*1024)
	flusher, canFlush := response.(http.Flusher)
	var total int64
	committed := false
	for {
		count, readErr := body.Read(buffer)
		if count > 0 {
			if !committed {
				response.WriteHeader(status)
				committed = true
			}
			if _, writeErr := response.Write(buffer[:count]); writeErr != nil {
				return total, writeErr
			}
			total += int64(count)
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if !committed {
					response.WriteHeader(status)
				}
				return total, nil
			}
			return total, readErr
		}
	}
}

func agnesRetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	wait := 2 * time.Second
	for index := 1; index < attempt && wait < 30*time.Second; index++ {
		wait *= 2
	}
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	return wait
}

func writeAgnesJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func setAgnesCORS(response http.ResponseWriter) {
	response.Header().Set("Access-Control-Allow-Origin", "*")
	response.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	response.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key")
}

func randomShortID() string {
	value := make([]byte, 4)
	if _, err := rand.Read(value); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(value)
}

func sleepAgnesContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
