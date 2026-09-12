package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

//go:embed clash-node-helper.ps1
var embeddedClashNodeHelper []byte

type ClashRecoveryResult struct {
	Attempted bool
	OK        bool
	Reason    string
	Finished  time.Time
}

type ClashRecoverySnapshot struct {
	Enabled             bool
	Running             bool
	ScriptPath          string
	CooldownRemainingMS int64
	LastResult          ClashRecoveryResult
}

type ClashRecovery struct {
	scriptPath string
	logger     *JSONLLogger
	cooldown   time.Duration

	mu          sync.Mutex
	running     bool
	lastStarted time.Time
	lastResult  ClashRecoveryResult
}

func NewClashRecovery(paths AppPaths, logger *JSONLLogger) (*ClashRecovery, error) {
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create Agnes data directory: %w", err)
	}
	desired := make([]byte, 0, len(embeddedClashNodeHelper)+3)
	desired = append(desired, 0xEF, 0xBB, 0xBF)
	desired = append(desired, embeddedClashNodeHelper...)
	current, err := os.ReadFile(paths.RecoveryScriptFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read embedded Clash helper: %w", err)
	}
	if !bytes.Equal(current, desired) {
		if err := os.WriteFile(paths.RecoveryScriptFile, desired, 0o600); err != nil {
			return nil, fmt.Errorf("extract embedded Clash helper: %w", err)
		}
		logger.Log("clash_recovery_helper_extracted", map[string]any{"path": paths.RecoveryScriptFile})
	}
	return &ClashRecovery{
		scriptPath: paths.RecoveryScriptFile,
		logger:     logger,
		cooldown:   5 * time.Minute,
	}, nil
}

func (recovery *ClashRecovery) Run(parent context.Context, requestID, model string) ClashRecoveryResult {
	recovery.mu.Lock()
	if recovery.running {
		recovery.mu.Unlock()
		return ClashRecoveryResult{Reason: "recovery_in_progress"}
	}
	if !recovery.lastStarted.IsZero() && time.Since(recovery.lastStarted) < recovery.cooldown {
		recovery.mu.Unlock()
		return ClashRecoveryResult{Reason: "recovery_cooldown"}
	}
	if _, err := os.Stat(recovery.scriptPath); err != nil {
		recovery.mu.Unlock()
		return ClashRecoveryResult{Reason: "recovery_script_missing"}
	}
	recovery.running = true
	recovery.lastStarted = time.Now()
	recovery.mu.Unlock()

	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-File",
		recovery.scriptPath,
		"-Action",
		"recover",
		"-Url",
		agnesConnectivityURL,
		"-TimeoutMs",
		strconv.Itoa(3000),
	)
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	err := command.Run()
	result := ClashRecoveryResult{Attempted: true, OK: err == nil, Finished: time.Now()}
	switch {
	case err == nil:
		result.Reason = "node_switched"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.Reason = "recovery_timeout"
	case errors.Is(ctx.Err(), context.Canceled):
		result.Reason = "client_disconnected"
	default:
		result.Reason = "recovery_failed"
	}

	recovery.mu.Lock()
	recovery.running = false
	recovery.lastResult = result
	recovery.mu.Unlock()
	recovery.logger.Log("clash_recovery_process_finished", map[string]any{
		"requestId": requestID,
		"model":     model,
		"ok":        result.OK,
		"reason":    result.Reason,
	})
	return result
}

func (recovery *ClashRecovery) Snapshot() ClashRecoverySnapshot {
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	remaining := recovery.cooldown - time.Since(recovery.lastStarted)
	if recovery.lastStarted.IsZero() || remaining < 0 {
		remaining = 0
	}
	_, err := os.Stat(recovery.scriptPath)
	return ClashRecoverySnapshot{
		Enabled:             err == nil,
		Running:             recovery.running,
		ScriptPath:          recovery.scriptPath,
		CooldownRemainingMS: remaining.Milliseconds(),
		LastResult:          recovery.lastResult,
	}
}
