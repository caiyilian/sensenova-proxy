package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type LocalClientSyncOutcome struct {
	Name       string
	Path       string
	Detected   bool
	Created    bool
	BackupPath string
	Added      int
	Updated    int
	Unchanged  int
	Err        error
}

type LocalClientsSyncResult struct {
	OpenCode  LocalClientSyncOutcome
	WorkBuddy LocalClientSyncOutcome
}

func syncDetectedLocalClients(openCodePath, workBuddyPath string, port int, localToken string) LocalClientsSyncResult {
	result := LocalClientsSyncResult{
		OpenCode:  LocalClientSyncOutcome{Name: "OpenCode", Path: openCodePath},
		WorkBuddy: LocalClientSyncOutcome{Name: "WorkBuddy", Path: workBuddyPath},
	}

	result.OpenCode.Detected, result.OpenCode.Err = localClientConfigDetected(openCodePath)
	if result.OpenCode.Detected && result.OpenCode.Err == nil {
		openCodeResult, err := syncAgnesOpenCodeConfig(openCodePath, agnesLocalBaseURL(port), localToken)
		result.OpenCode.Err = err
		result.OpenCode.Created = openCodeResult.Created
		result.OpenCode.BackupPath = openCodeResult.BackupPath
		switch {
		case openCodeResult.Created:
			result.OpenCode.Added = 1
		case openCodeResult.Updated:
			result.OpenCode.Updated = 1
		case openCodeResult.Unchanged:
			result.OpenCode.Unchanged = 1
		}
	}

	result.WorkBuddy.Detected, result.WorkBuddy.Err = localClientConfigDetected(workBuddyPath)
	if result.WorkBuddy.Detected && result.WorkBuddy.Err == nil {
		workBuddyResult, err := syncAgnesWorkBuddyConfig(workBuddyPath, agnesWorkBuddyChatURL(port), localToken)
		result.WorkBuddy.Err = err
		result.WorkBuddy.Created = workBuddyResult.Created
		result.WorkBuddy.BackupPath = workBuddyResult.BackupPath
		result.WorkBuddy.Added = workBuddyResult.Added
		result.WorkBuddy.Updated = workBuddyResult.Updated
		result.WorkBuddy.Unchanged = workBuddyResult.Unchanged
	}

	return result
}

func localClientConfigDetected(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			return false, fmt.Errorf("客户端配置路径是目录：%s", path)
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("检查客户端配置：%w", err)
	}

	parent := filepath.Dir(path)
	info, err = os.Stat(parent)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("客户端配置目录路径不是目录：%s", parent)
		}
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("检查客户端配置目录：%w", err)
}

func (result LocalClientsSyncResult) DetectedCount() int {
	count := 0
	if result.OpenCode.Detected {
		count++
	}
	if result.WorkBuddy.Detected {
		count++
	}
	return count
}

func (result LocalClientsSyncResult) Err() error {
	var failures []error
	if result.OpenCode.Err != nil {
		failures = append(failures, fmt.Errorf("OpenCode：%w", result.OpenCode.Err))
	}
	if result.WorkBuddy.Err != nil {
		failures = append(failures, fmt.Errorf("WorkBuddy：%w", result.WorkBuddy.Err))
	}
	return errors.Join(failures...)
}
