package main

import (
	"sync"
	"time"
)

type ControllerStatus struct {
	Running         bool
	StartedAt       time.Time
	LastError       string
	HealthKnown     bool
	HealthOK        bool
	Health          AgnesGatewayHealth
	LastHealthCheck time.Time
}

type ProxyController struct {
	gateway *AgnesGateway
	logger  *JSONLLogger

	mu      sync.RWMutex
	status  ControllerStatus
	opMu    sync.Mutex
	stop    chan struct{}
	done    chan struct{}
	start   sync.Once
	close   sync.Once
	started bool
}

func NewProxyController(settings Settings, keys *AgnesKeyMonitor, connectivity *ConnectivityMonitor, logger *JSONLLogger, localToken string, recovery *ClashRecovery) (*ProxyController, error) {
	gateway, err := NewAgnesGateway(settings.Port, keys, connectivity, logger, localToken, recovery)
	if err != nil {
		return nil, err
	}
	return &ProxyController{
		gateway: gateway,
		logger:  logger,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}, nil
}

func (controller *ProxyController) Start() error {
	controller.opMu.Lock()
	defer controller.opMu.Unlock()
	err := controller.gateway.Start()
	controller.updateStatus(err)
	return err
}

func (controller *ProxyController) RunMonitor() {
	controller.start.Do(func() {
		controller.mu.Lock()
		controller.started = true
		controller.mu.Unlock()
		go controller.monitorLoop()
	})
}

func (controller *ProxyController) Restart() error {
	controller.opMu.Lock()
	defer controller.opMu.Unlock()
	err := controller.gateway.Restart()
	controller.updateStatus(err)
	if err == nil {
		controller.logger.Log("native_gateway_restarted", map[string]any{})
	}
	return err
}

func (controller *ProxyController) Close() {
	controller.close.Do(func() {
		controller.mu.RLock()
		started := controller.started
		controller.mu.RUnlock()
		if started {
			close(controller.stop)
			<-controller.done
		}
		controller.opMu.Lock()
		controller.gateway.Close()
		controller.opMu.Unlock()
		controller.updateStatus(nil)
	})
}

func (controller *ProxyController) Status() ControllerStatus {
	controller.mu.RLock()
	defer controller.mu.RUnlock()
	status := controller.status
	status.Health.RateLimits = append([]AgnesRateLimit(nil), controller.status.Health.RateLimits...)
	if controller.status.Health.Routes.TransportFailures != nil {
		status.Health.Routes.TransportFailures = make(map[string]int, len(controller.status.Health.Routes.TransportFailures))
		for name, count := range controller.status.Health.Routes.TransportFailures {
			status.Health.Routes.TransportFailures[name] = count
		}
	}
	return status
}

func (controller *ProxyController) RecoveryStatus() ClashRecoverySnapshot {
	if controller.gateway.recovery == nil {
		return ClashRecoverySnapshot{}
	}
	return controller.gateway.recovery.Snapshot()
}

func (controller *ProxyController) monitorLoop() {
	defer close(controller.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	controller.updateStatus(nil)
	for {
		select {
		case <-ticker.C:
			controller.updateStatus(nil)
		case <-controller.stop:
			return
		}
	}
}

func (controller *ProxyController) updateStatus(operationError error) {
	running := controller.gateway.Running()
	health := controller.gateway.Snapshot()
	lastError := controller.gateway.LastError()
	if operationError != nil {
		lastError = redactText(operationError.Error())
	}
	controller.mu.Lock()
	if running && !controller.status.Running {
		controller.status.StartedAt = time.Now()
	}
	controller.status.Running = running
	controller.status.HealthKnown = true
	controller.status.HealthOK = running && health.Status == "ok"
	controller.status.Health = health
	controller.status.LastHealthCheck = time.Now()
	controller.status.LastError = lastError
	controller.mu.Unlock()
}
