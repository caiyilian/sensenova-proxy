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
	autoStartValueName    = "AgnesProxyDesktop"
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
	key, _, err := registry.CreateKey(registry.CURRENT_USER, autoStartRegistryPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open startup registry: %w", err)
	}
	defer key.Close()
	if !enabled {
		if err := key.DeleteValue(autoStartValueName); err != nil && err != registry.ErrNotExist {
			return fmt.Errorf("disable startup: %w", err)
		}
		return nil
	}
	if err := key.SetStringValue(autoStartValueName, fmt.Sprintf("\"%s\" --autostart", executable)); err != nil {
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
	return err == nil && strings.Contains(strings.ToLower(command), strings.ToLower(executable))
}
