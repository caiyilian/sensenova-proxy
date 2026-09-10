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
	gatewayStartError := gateway.Start(settings.Port)
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
