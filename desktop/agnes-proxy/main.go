package main

import (
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
	flag.BoolVar(&options.syncLocalClients, "sync-local-clients", false, "detect and synchronize installed OpenCode and WorkBuddy clients")
	flag.BoolVar(&options.syncOpenCode, "sync-opencode", false, "synchronize the current local token to the user's OpenCode config")
	flag.StringVar(&options.dataDir, "data-dir", "", "override the settings and log directory")
	flag.StringVar(&options.keyFile, "key-file", "", "override the Agnes API key file")
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
		walk.MsgBox(nil, appName, "Agnes Proxy 已在运行，请查看系统托盘。", walk.MsgBoxIconInformation)
		return nil
	}
	defer lock.Close()

	paths, err := resolveAppPaths(options.dataDir)
	if err != nil {
		return err
	}
	settings, err := loadSettings(paths)
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
	})
	localToken, localTokenSource := readAgnesLocalToken()
	logger.Log("local_gateway_token_selected", map[string]any{"source": localTokenSource})
	if options.syncLocalClients {
		openCodePath, pathErr := defaultOpenCodeConfigPath()
		if pathErr != nil {
			return pathErr
		}
		modelsPath, pathErr := defaultWorkBuddyModelsPath()
		if pathErr != nil {
			return pathErr
		}
		result := syncDetectedLocalClients(openCodePath, modelsPath, settings.Port, localToken)
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
	} else if options.syncOpenCode {
		configPath, pathErr := defaultOpenCodeConfigPath()
		if pathErr != nil {
			return pathErr
		}
		result, syncErr := syncAgnesOpenCodeConfig(configPath, agnesLocalBaseURL(settings.Port), localToken)
		if syncErr != nil {
			return syncErr
		}
		logger.Log("opencode_config_synced", map[string]any{
			"created": result.Created,
			"updated": result.Updated,
			"source":  "command_line",
		})
	}

	keys := NewAgnesKeyMonitor(logger, 3*time.Second, settings.KeyFilePath)
	keys.Start()
	defer keys.Close()
	connectivity, err := NewConnectivityMonitor(logger, 5*time.Second)
	if err != nil {
		return err
	}
	connectivity.Start()
	defer connectivity.Close()
	recovery, recoveryErr := NewClashRecovery(paths, logger)
	if recoveryErr != nil {
		logger.Log("clash_recovery_unavailable", map[string]any{"error": recoveryErr})
	}
	controller, err := NewProxyController(settings, keys, connectivity, logger, localToken, recovery)
	if err != nil {
		return err
	}
	_ = controller.Start()
	controller.RunMonitor()
	defer controller.Close()

	ui := NewDesktopUI(paths, settings, keys, controller, connectivity, logger, localToken)
	defer ui.Dispose()
	if err := ui.Run(options.autoStart || options.hidden); err != nil {
		return fmt.Errorf("create desktop window: %w", err)
	}
	logger.Log("application_stopped", map[string]any{})
	return nil
}
