package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	autoStartRegistryPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	autoStartValueName    = "SenseNovaPool"
)

func setAutoStart(enabled bool) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	key, _, err := registry.CreateKey(
		registry.CURRENT_USER,
		autoStartRegistryPath,
		registry.QUERY_VALUE|registry.SET_VALUE,
	)
	if err != nil {
		return fmt.Errorf("open startup registry: %w", err)
	}
	defer key.Close()
	if !enabled {
		err = key.DeleteValue(autoStartValueName)
		if err == registry.ErrNotExist {
			return nil
		}
		if err != nil {
			return fmt.Errorf("disable startup: %w", err)
		}
		return nil
	}
	command := fmt.Sprintf("\"%s\" --autostart", executable)
	if err := key.SetStringValue(autoStartValueName, command); err != nil {
		return fmt.Errorf("enable startup: %w", err)
	}
	return nil
}

func autoStartEnabledForCurrentExecutable() bool {
	executable, err := os.Executable()
	if err != nil {
		return false
	}
	executable, _ = filepath.Abs(executable)
	key, err := registry.OpenKey(registry.CURRENT_USER, autoStartRegistryPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	command, _, err := key.GetStringValue(autoStartValueName)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(command), strings.ToLower(executable))
}
