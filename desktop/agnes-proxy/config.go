package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	appName                = "Agnes Proxy"
	settingsVersion        = 2
	defaultProxyPort       = 18788
	defaultLocalProxyToken = "local-agnes-proxy"
)

type Settings struct {
	Version      int    `json:"version"`
	Port         int    `json:"port"`
	KeyFilePath  string `json:"keyFilePath,omitempty"`
	WindowWidth  int    `json:"windowWidth,omitempty"`
	WindowHeight int    `json:"windowHeight,omitempty"`
}

type AppPaths struct {
	DataDir            string
	SettingsFile       string
	LogDir             string
	RecoveryScriptFile string
}

func resolveAppPaths(override string) (AppPaths, error) {
	dataDir := override
	if dataDir == "" {
		dataDir = os.Getenv("AGNES_DESKTOP_DATA_DIR")
	}
	if dataDir == "" {
		dataDir = os.Getenv("LOCALAPPDATA")
		if dataDir != "" {
			dataDir = filepath.Join(dataDir, "AgnesProxy")
		}
	}
	if dataDir == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return AppPaths{}, fmt.Errorf("resolve user config directory: %w", err)
		}
		dataDir = filepath.Join(configDir, "AgnesProxy")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return AppPaths{}, fmt.Errorf("resolve data directory: %w", err)
	}
	return AppPaths{
		DataDir:            abs,
		SettingsFile:       filepath.Join(abs, "settings.json"),
		LogDir:             filepath.Join(abs, "logs"),
		RecoveryScriptFile: filepath.Join(abs, "clash-node-helper.ps1"),
	}, nil
}

func loadSettings(paths AppPaths) (Settings, error) {
	settings := Settings{Version: settingsVersion, Port: defaultProxyPort}
	data, err := os.ReadFile(paths.SettingsFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return Settings{}, fmt.Errorf("parse settings: %w", err)
		}
	}
	if settings.Port < 1 || settings.Port > 65535 {
		settings.Port = defaultProxyPort
	}
	settings.Version = settingsVersion
	return settings, nil
}

func saveSettings(paths AppPaths, settings Settings) error {
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	settings.Version = settingsVersion
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	if err := os.WriteFile(paths.SettingsFile, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}
