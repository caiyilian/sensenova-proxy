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
	autoStart bool
	hidden    bool
	dataDir   string
	keyFile   string
	port      int
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
