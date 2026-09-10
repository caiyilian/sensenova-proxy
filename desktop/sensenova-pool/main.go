package main

import (
	"context"
	"flag"
	"fmt"
	"runtime"
	"time"

	"github.com/lxn/walk"
)

var version = "dev"

type commandLineOptions struct {
	autoStart        bool
	hidden           bool
	allowLAN         bool
	localOnly        bool
	syncLocalClients bool
	syncOpenCode     bool
	dataDir          string
	keyFile          string
	port             int
}

func main() {
	runtime.LockOSThread()
	options := parseCommandLine()
	if err := runApplication(options); err != nil {
		walk.MsgBox(nil, appName, "程序无法启动：\n"+redactText(err.Error()), walk.MsgBoxIconError)
	}
}

func parseCommandLine() commandLineOptions {
	options := commandLineOptions{port: -1}
	flag.BoolVar(&options.autoStart, "autostart", false, "start hidden from the current-user startup entry")
	flag.BoolVar(&options.hidden, "hidden", false, "start hidden in the notification area")
	flag.BoolVar(&options.allowLAN, "allow-lan", false, "allow clients on the local network")
	flag.BoolVar(&options.localOnly, "local-only", false, "listen on loopback only")
	flag.BoolVar(&options.syncLocalClients, "sync-local-clients", false, "detect and synchronize installed OpenCode and WorkBuddy clients")
	flag.BoolVar(&options.syncOpenCode, "sync-opencode", false, "synchronize the current local token to the user's OpenCode config")
	flag.StringVar(&options.dataDir, "data-dir", "", "override the settings and log directory")
	flag.StringVar(&options.keyFile, "key-file", "", "override the API key file")
	flag.IntVar(&options.port, "port", -1, "override the local listening port")
	flag.Parse()
	return options
}

func runApplication(options commandLineOptions) error {
	lock, acquired, err := acquireInstanceLock()
	if err != nil {
		return err
	}
	if !acquired {
		walk.MsgBox(nil, appName, "SenseNova Pool 已在运行，请查看系统托盘。", walk.MsgBoxIconInformation)
		return nil
	}
	defer lock.Close()

	paths, err := resolveAppPaths(options.dataDir)
	if err != nil {
		return err
	}
	settings, _, err := loadSettings(paths)
	if err != nil {
		return err
	}
	if options.port >= 0 {
		if options.port > 65535 {
			return fmt.Errorf("invalid port %d", options.port)
		}
		settings.Port = options.port
	}
	if options.keyFile != "" {
		settings.KeyFilePath = options.keyFile
	}
	if options.allowLAN && options.localOnly {
		return fmt.Errorf("allow-lan and local-only cannot be used together")
	}
	if options.allowLAN {
		settings.AllowLAN = true
	}
	if options.localOnly {
		settings.AllowLAN = false
	}
	if err := saveSettings(paths, settings); err != nil {
		return err
	}

	logger, err := NewJSONLLogger(paths.LogDir)
	if err != nil {
		return err
	}
	logger.Log("application_started", map[string]any{
		"version":   version,
		"autostart": options.autoStart,
		"hidden":    options.hidden,
		"port":      settings.Port,
		"allowLAN":  settings.AllowLAN,
	})
	if options.syncOpenCode {
		configPath, pathErr := defaultOpenCodeConfigPath()
		if pathErr != nil {
			return pathErr
		}
		backupPath, syncErr := syncOpenCodeConfig(configPath, localBaseURL(settings.Port), settings.LocalToken)
		if syncErr != nil {
			return syncErr
		}
		logger.Log("opencode_config_synced", map[string]any{
			"created": backupPath == "",
			"source":  "command_line",
		})
	}
	if options.syncLocalClients {
		openCodePath, pathErr := defaultOpenCodeConfigPath()
		if pathErr != nil {
			return pathErr
		}
		modelsPath, pathErr := defaultWorkBuddyModelsPath()
		if pathErr != nil {
			return pathErr
		}
		result := syncDetectedLocalClients(openCodePath, modelsPath, settings.Port, settings.LocalToken)
		logger.Log("local_clients_synced", map[string]any{
			"opencode_detected":  result.OpenCode.Detected,
			"opencode_success":   result.OpenCode.Detected && result.OpenCode.Err == nil,
			"workbuddy_detected": result.WorkBuddy.Detected,
			"workbuddy_success":  result.WorkBuddy.Detected && result.WorkBuddy.Err == nil,
			"workbuddy_added":    result.WorkBuddy.Added,
			"workbuddy_updated":  result.WorkBuddy.Updated,
			"source":             "command_line",
		})
		if syncErr := result.Err(); syncErr != nil {
			return syncErr
		}
	}

	keyStore := NewKeyStore(settings.KeyFilePath, logger, time.Second)
	keyStore.Start()
	defer keyStore.Close()

	resolver := &ProxyResolver{}
	network := NewNetworkMonitor(logger, resolver, 5*time.Second)
	network.Start()
	defer network.Close()

	pool := NewAccountPool()
	gateway, err := NewGateway(keyStore, pool, logger, network, resolver, settings.LocalToken)
	if err != nil {
		return err
	}
	gatewayStartError := gateway.Start(gatewayBindHost(settings.AllowLAN), settings.Port)
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = gateway.Close(shutdownContext)
	}()

	ui := NewDesktopUI(paths, settings, keyStore, pool, network, gateway, logger, gatewayStartError)
	defer ui.Dispose()
	startHidden := options.autoStart || options.hidden
	if err := ui.Run(startHidden); err != nil {
		return fmt.Errorf("create desktop window: %w", err)
	}
	logger.Log("application_stopped", map[string]any{})
	return nil
}
