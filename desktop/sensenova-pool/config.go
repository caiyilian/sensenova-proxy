package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	appName          = "SenseNova Pool"
	settingsVersion  = 1
	defaultProxyPort = 18787
)

type Settings struct {
	Version      int    `json:"version"`
	KeyFilePath  string `json:"keyFilePath"`
	Port         int    `json:"port"`
	LocalToken   string `json:"localToken"`
	WindowWidth  int    `json:"windowWidth,omitempty"`
	WindowHeight int    `json:"windowHeight,omitempty"`
}

type AppPaths struct {
	DataDir      string
	SettingsFile string
	LogDir       string
}

func resolveAppPaths(override string) (AppPaths, error) {
	dataDir := override
	if dataDir == "" {
		dataDir = os.Getenv("SENSENOVA_POOL_DATA_DIR")
	}
	if dataDir == "" {
		dataDir = os.Getenv("LOCALAPPDATA")
		if dataDir != "" {
			dataDir = filepath.Join(dataDir, "SenseNovaPool")
		}
	}
	if dataDir == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return AppPaths{}, fmt.Errorf("resolve user config directory: %w", err)
		}
		dataDir = filepath.Join(configDir, "SenseNovaPool")
	}

	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return AppPaths{}, fmt.Errorf("resolve data directory: %w", err)
	}
	return AppPaths{
		DataDir:      abs,
		SettingsFile: filepath.Join(abs, "settings.json"),
		LogDir:       filepath.Join(abs, "logs"),
	}, nil
}

func loadSettings(paths AppPaths) (Settings, bool, error) {
	settings := Settings{
		Version: settingsVersion,
		Port:    defaultProxyPort,
	}
	data, err := os.ReadFile(paths.SettingsFile)
	firstRun := errors.Is(err, os.ErrNotExist)
	if err != nil && !firstRun {
		return Settings{}, false, fmt.Errorf("read settings: %w", err)
	}
	if err == nil {
		if unmarshalErr := json.Unmarshal(data, &settings); unmarshalErr != nil {
			return Settings{}, false, fmt.Errorf("parse settings: %w", unmarshalErr)
		}
	}

	if settings.Version == 0 {
		settings.Version = settingsVersion
	}
	if settings.Port < 1 || settings.Port > 65535 {
		settings.Port = defaultProxyPort
	}
	if settings.LocalToken == "" {
		token, tokenErr := generateLocalToken()
		if tokenErr != nil {
			return Settings{}, false, tokenErr
		}
		settings.LocalToken = token
	}
	if settings.KeyFilePath != "" {
		settings.KeyFilePath, _ = filepath.Abs(settings.KeyFilePath)
	}
	return settings, firstRun, nil
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
	data = append(data, '\n')
	if err := os.WriteFile(paths.SettingsFile, data, 0o600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}

func generateLocalToken() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate local token: %w", err)
	}
	return "local-" + hex.EncodeToString(buffer), nil
}
