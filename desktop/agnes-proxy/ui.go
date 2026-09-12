package main

import (
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	d "github.com/lxn/walk/declarative"
	"golang.org/x/sys/windows"
)

type DesktopUI struct {
	paths        AppPaths
	settings     Settings
	keys         *AgnesKeyMonitor
	controller   *ProxyController
	connectivity *ConnectivityMonitor
	logger       *JSONLLogger
	localToken   string

	mainWindow       *walk.MainWindow
	notifyIcon       *walk.NotifyIcon
	appIcon          *walk.Icon
	serviceLabel     *walk.Label
	keyLabel         *walk.Label
	directLabel      *walk.Label
	clashLabel       *walk.Label
	routeLabel       *walk.Label
	recoveryLabel    *walk.Label
	keyPathEdit      *walk.LineEdit
	baseURLEdit      *walk.LineEdit
	localTokenEdit   *walk.LineEdit
	logPathEdit      *walk.LineEdit
	autoStartCheck   *walk.CheckBox
	restartButton    *walk.PushButton
	refreshKeyButton *walk.PushButton
	openKeyButton    *walk.PushButton

	exiting           bool
	autoStartUpdating bool
	refreshStop       chan struct{}
	refreshDone       chan struct{}
	refreshMu         sync.Mutex
	refreshStarted    bool
	stopRefresh       sync.Once
}

func NewDesktopUI(paths AppPaths, settings Settings, keys *AgnesKeyMonitor, controller *ProxyController, connectivity *ConnectivityMonitor, logger *JSONLLogger, localToken string) *DesktopUI {
	return &DesktopUI{
		paths:        paths,
		settings:     settings,
		keys:         keys,
		controller:   controller,
		connectivity: connectivity,
		logger:       logger,
		localToken:   localToken,
		refreshStop:  make(chan struct{}),
		refreshDone:  make(chan struct{}),
	}
}

func (ui *DesktopUI) Run(startHidden bool) error {
	icon, err := createApplicationIcon()
	if err != nil {
		return err
	}
	ui.appIcon = icon
	window := d.MainWindow{
		AssignTo: &ui.mainWindow,
		Title:    appName,
		Icon:     icon,
		Visible:  false,
		MinSize:  d.Size{Width: 740, Height: 600},
		Size:     d.Size{Width: max(ui.settings.WindowWidth, 810), Height: max(ui.settings.WindowHeight, 670)},
		Layout: d.VBox{
			Margins: d.Margins{Left: 16, Top: 14, Right: 16, Bottom: 14},
			Spacing: 10,
		},
		OnDropFiles: ui.dropKeyFiles,
		Children: []d.Widget{
			d.Label{Text: "Agnes 弹性代理", Font: d.Font{Family: "Microsoft YaHei UI", PointSize: 14, Bold: true}},
			d.Label{Text: "单文件便携版；代理核心已内置，无需 Node.js 或仓库。支持直连、Clash 7890、断联重试与良心云节点自动恢复。"},
			d.GroupBox{
				Title:  "运行状态",
				Layout: d.VBox{Margins: d.Margins{Left: 10, Top: 8, Right: 10, Bottom: 9}, Spacing: 5},
				Children: []d.Widget{
					d.Label{AssignTo: &ui.serviceLabel, Text: "代理服务：正在启动…"},
					d.Label{AssignTo: &ui.keyLabel, Text: "AGNES_API_KEY：正在检测…"},
					d.Label{AssignTo: &ui.directLabel, Text: "直连：正在检测…"},
					d.Label{AssignTo: &ui.clashLabel, Text: "Clash：正在检测…"},
					d.Label{AssignTo: &ui.routeLabel, Text: "请求路线：等待代理启动…"},
					d.Label{AssignTo: &ui.recoveryLabel, Text: "节点自动恢复：正在检查…"},
				},
			},
			d.GroupBox{
				Title:  "API Key 文件与代理控制",
				Layout: d.VBox{Margins: d.Margins{Left: 10, Top: 8, Right: 10, Bottom: 9}, Spacing: 7},
				Children: []d.Widget{
					d.Composite{
						Layout: d.HBox{MarginsZero: true, Spacing: 7},
						Children: []d.Widget{
							d.LineEdit{AssignTo: &ui.keyPathEdit, ReadOnly: true, StretchFactor: 1},
							d.PushButton{Text: "选择文件", MinSize: d.Size{Width: 88}, OnClicked: ui.chooseKeyFile},
							d.PushButton{AssignTo: &ui.openKeyButton, Text: "打开文件", MinSize: d.Size{Width: 88}, OnClicked: ui.openKeyFile},
						},
					},
					d.Composite{
						Layout: d.HBox{MarginsZero: true, Spacing: 7},
						Children: []d.Widget{
							d.PushButton{AssignTo: &ui.refreshKeyButton, Text: "立即重读", MinSize: d.Size{Width: 88}, OnClicked: ui.refreshKey},
							d.PushButton{AssignTo: &ui.restartButton, Text: "重启代理", MinSize: d.Size{Width: 88}, OnClicked: ui.restartProxy},
							d.HSpacer{},
						},
					},
					d.Label{Text: "支持任意扩展名、无后缀文件和拖入窗口；保存路径后每 3 秒自动重读，临时写坏时保留上一次有效 Key。"},
				},
			},
			d.GroupBox{
				Title:  "客户端连接信息",
				Layout: d.Grid{Columns: 3, Margins: d.Margins{Left: 10, Top: 8, Right: 10, Bottom: 9}, Spacing: 7},
				Children: []d.Widget{
					d.Label{Text: "Base URL"},
					d.LineEdit{AssignTo: &ui.baseURLEdit, ReadOnly: true, StretchFactor: 1},
					d.PushButton{Text: "复制", MinSize: d.Size{Width: 62}, OnClicked: func() { ui.copyText(ui.baseURLEdit.Text(), "Base URL") }},
					d.Label{Text: "本地 API Key"},
					d.LineEdit{AssignTo: &ui.localTokenEdit, ReadOnly: true, StretchFactor: 1},
					d.PushButton{Text: "复制", MinSize: d.Size{Width: 62}, OnClicked: func() { ui.copyText(ui.localTokenEdit.Text(), "本地 API Key") }},
				},
			},
			d.Label{Text: "模型：agnes-2.0-flash · agnes-2.5-flash · agnes-3.0-flash"},
			d.GroupBox{
				Title:  "程序设置",
				Layout: d.VBox{Margins: d.Margins{Left: 10, Top: 8, Right: 10, Bottom: 9}, Spacing: 7},
				Children: []d.Widget{
					d.CheckBox{
						AssignTo:         &ui.autoStartCheck,
						Text:             "开机自动启动（自动启动时直接最小化到托盘）",
						Checked:          autoStartEnabledForCurrentExecutable(),
						OnCheckedChanged: ui.autoStartChanged,
					},
					d.Composite{
						Layout: d.HBox{MarginsZero: true, Spacing: 7},
						Children: []d.Widget{
							d.LineEdit{AssignTo: &ui.logPathEdit, ReadOnly: true, StretchFactor: 1},
							d.PushButton{Text: "打开日志目录", MinSize: d.Size{Width: 108}, OnClicked: ui.openLogFolder},
						},
					},
				},
			},
			d.Label{Text: "提示：右上角 × 只会隐藏到托盘；真正退出请右键托盘图标选择“退出”。", TextColor: walk.RGB(95, 99, 104)},
		},
	}
	if err := window.Create(); err != nil {
		return err
	}
	ui.mainWindow.Closing().Attach(ui.windowClosing)
	if err := ui.createTrayIcon(); err != nil {
		ui.mainWindow.Dispose()
		return err
	}
	ui.refreshUI()
	ui.startRefreshLoop()
	if startHidden && ui.keys.Snapshot().Present {
		ui.mainWindow.Hide()
	} else {
		ui.showWindow()
	}
	ui.mainWindow.Run()
	ui.stopRefreshLoop()
	return nil
}

func (ui *DesktopUI) Dispose() {
	ui.stopRefreshLoop()
	if ui.notifyIcon != nil {
		_ = ui.notifyIcon.Dispose()
		ui.notifyIcon = nil
	}
	if ui.appIcon != nil {
		ui.appIcon.Dispose()
		ui.appIcon = nil
	}
}

func (ui *DesktopUI) createTrayIcon() error {
	icon, err := walk.NewNotifyIcon(ui.mainWindow)
	if err != nil {
		return err
	}
	ui.notifyIcon = icon
	if err := icon.SetIcon(ui.appIcon); err != nil {
		return err
	}
	_ = icon.SetToolTip(appName + " · 正在启动")
	icon.MouseUp().Attach(func(_ int, _ int, button walk.MouseButton) {
		if button == walk.LeftButton {
			ui.showWindow()
		}
	})
	openAction := walk.NewAction()
	_ = openAction.SetText("打开主界面")
	openAction.Triggered().Attach(ui.showWindow)
	if err := icon.ContextMenu().Actions().Add(openAction); err != nil {
		return err
	}
	if err := icon.ContextMenu().Actions().Add(walk.NewSeparatorAction()); err != nil {
		return err
	}
	restartAction := walk.NewAction()
	_ = restartAction.SetText("重启代理")
	restartAction.Triggered().Attach(ui.restartProxy)
	if err := icon.ContextMenu().Actions().Add(restartAction); err != nil {
		return err
	}
	exitAction := walk.NewAction()
	_ = exitAction.SetText("退出")
	exitAction.Triggered().Attach(ui.exitFromTray)
	if err := icon.ContextMenu().Actions().Add(exitAction); err != nil {
		return err
	}
	return icon.SetVisible(true)
}

func (ui *DesktopUI) restartProxy() {
	ui.restartButton.SetEnabled(false)
	go func() {
		err := ui.controller.Restart()
		ui.mainWindow.Synchronize(func() {
			ui.restartButton.SetEnabled(true)
			ui.refreshUI()
			if err != nil {
				walk.MsgBox(ui.mainWindow, appName, "代理重启失败：\n"+redactText(err.Error()), walk.MsgBoxIconError)
			} else if ui.notifyIcon != nil {
				_ = ui.notifyIcon.ShowInfo(appName, "后台代理已重启")
			}
		})
	}()
}

func (ui *DesktopUI) chooseKeyFile() {
	dialog := walk.FileDialog{
		Title:  "选择 Agnes API Key 文件",
		Filter: "所有文件 (*)|*",
	}
	if ui.settings.KeyFilePath != "" {
		dialog.FilePath = ui.settings.KeyFilePath
		dialog.InitialDirPath = filepath.Dir(ui.settings.KeyFilePath)
	}
	accepted, err := dialog.ShowOpen(ui.mainWindow)
	if err != nil {
		walk.MsgBox(ui.mainWindow, appName, "无法打开文件选择器：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	if accepted {
		ui.useKeyFile(dialog.FilePath, "file_dialog")
	}
}

func (ui *DesktopUI) dropKeyFiles(files []string) {
	if len(files) == 0 {
		return
	}
	if len(files) > 1 && ui.notifyIcon != nil {
		_ = ui.notifyIcon.ShowWarning(appName, "一次只能使用一个 API Key 文件，已选择拖入的第一个文件。")
	}
	ui.useKeyFile(files[0], "drag_drop")
}

func (ui *DesktopUI) useKeyFile(path, source string) {
	normalized, err := normalizeAgnesKeyFilePath(path)
	if err != nil {
		walk.MsgBox(ui.mainWindow, appName, "无法使用该文件：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	if err := ui.keys.SetPath(normalized); err != nil {
		walk.MsgBox(ui.mainWindow, appName, "无法加载该文件：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	ui.settings.KeyFilePath = normalized
	if err := saveSettings(ui.paths, ui.settings); err != nil {
		walk.MsgBox(ui.mainWindow, appName, "API Key 已加载，但无法保存设置：\n"+redactText(err.Error()), walk.MsgBoxIconWarning)
	}
	ui.logger.Log("key_file_selected", map[string]any{"source": source, "fileName": filepath.Base(normalized)})
	ui.refreshUI()
	if ui.notifyIcon != nil {
		_ = ui.notifyIcon.ShowInfo(appName, "API Key 文件已加载；后续修改会自动生效")
	}
}

func (ui *DesktopUI) openKeyFile() {
	path := ui.keys.Path()
	if path == "" {
		walk.MsgBox(ui.mainWindow, appName, "尚未选择 API Key 文件。", walk.MsgBoxIconInformation)
		return
	}
	if err := openPath(path); err != nil {
		walk.MsgBox(ui.mainWindow, appName, "无法打开 API Key 文件：\n"+redactText(err.Error()), walk.MsgBoxIconError)
	}
}

func (ui *DesktopUI) refreshKey() {
	before := ui.keys.Snapshot().Fingerprint
	after := ui.keys.Refresh()
	ui.refreshUI()
	if after.Fingerprint != before {
		if ui.notifyIcon != nil {
			_ = ui.notifyIcon.ShowInfo(appName, "已加载新的 Agnes API Key，无需重启代理")
		}
	} else if ui.notifyIcon != nil {
		_ = ui.notifyIcon.ShowInfo(appName, "API Key 文件已重新读取，内容未发生变化")
	}
}

func (ui *DesktopUI) autoStartChanged() {
	if ui.autoStartUpdating {
		return
	}
	enabled := ui.autoStartCheck.Checked()
	if err := setAutoStart(enabled); err != nil {
		ui.autoStartUpdating = true
		ui.autoStartCheck.SetChecked(!enabled)
		ui.autoStartUpdating = false
		walk.MsgBox(ui.mainWindow, appName, "无法修改开机自启：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	ui.logger.Log("autostart_changed", map[string]any{"enabled": enabled})
}

func (ui *DesktopUI) openLogFolder() {
	if err := os.MkdirAll(ui.paths.LogDir, 0o700); err != nil {
		walk.MsgBox(ui.mainWindow, appName, redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	if err := openPath(ui.paths.LogDir); err != nil {
		walk.MsgBox(ui.mainWindow, appName, redactText(err.Error()), walk.MsgBoxIconError)
	}
}

func (ui *DesktopUI) copyText(value, label string) {
	if err := walk.Clipboard().SetText(value); err != nil {
		walk.MsgBox(ui.mainWindow, appName, "复制失败：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	if ui.notifyIcon != nil {
		_ = ui.notifyIcon.ShowInfo(appName, label+" 已复制")
	}
}

func (ui *DesktopUI) windowClosing(canceled *bool, _ walk.CloseReason) {
	if ui.exiting {
		return
	}
	*canceled = true
	ui.mainWindow.Hide()
}

func (ui *DesktopUI) exitFromTray() {
	ui.exiting = true
	ui.mainWindow.Close()
}

func (ui *DesktopUI) showWindow() {
	ui.mainWindow.Show()
	_ = ui.mainWindow.Activate()
}

func (ui *DesktopUI) startRefreshLoop() {
	ui.refreshMu.Lock()
	ui.refreshStarted = true
	ui.refreshMu.Unlock()
	go func() {
		defer close(ui.refreshDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if ui.mainWindow != nil {
					ui.mainWindow.Synchronize(ui.refreshUI)
				}
			case <-ui.refreshStop:
				return
			}
		}
	}()
}

func (ui *DesktopUI) stopRefreshLoop() {
	ui.stopRefresh.Do(func() {
		ui.refreshMu.Lock()
		started := ui.refreshStarted
		ui.refreshMu.Unlock()
		if !started {
			return
		}
		close(ui.refreshStop)
		<-ui.refreshDone
	})
}

func (ui *DesktopUI) refreshUI() {
	key := ui.keys.Snapshot()
	controller := ui.controller.Status()
	network := ui.connectivity.Status()
	baseURL := fmt.Sprintf("http://127.0.0.1:%d/v1", ui.settings.Port)
	ui.baseURLEdit.SetText(baseURL)
	ui.localTokenEdit.SetText(ui.localToken)
	ui.logPathEdit.SetText(ui.paths.LogDir)
	ui.keyPathEdit.SetText(ui.keys.Path())
	ui.openKeyButton.SetEnabled(ui.keys.Path() != "")

	if controller.Running && controller.HealthOK {
		ui.serviceLabel.SetText(fmt.Sprintf("代理服务：内置引擎运行中 · %s", baseURL))
		ui.serviceLabel.SetTextColor(walk.RGB(25, 126, 65))
	} else if controller.Running {
		ui.serviceLabel.SetText("代理服务：内置引擎已启动 · 等待健康检查")
		ui.serviceLabel.SetTextColor(walk.RGB(190, 103, 0))
	} else {
		message := controller.LastError
		if message == "" {
			message = "尚未启动"
		}
		ui.serviceLabel.SetText("代理服务：未运行 · " + message)
		ui.serviceLabel.SetTextColor(walk.RGB(190, 48, 48))
	}

	if key.Present && key.LastError == "" {
		ui.keyLabel.SetText(fmt.Sprintf("AGNES_API_KEY：已加载 · %s · Key Ref %s", key.Source, key.KeyRef))
		ui.keyLabel.SetTextColor(walk.RGB(25, 126, 65))
	} else if key.RetainedLastGood {
		ui.keyLabel.SetText("AGNES_API_KEY：文件当前不可用，仍在使用上一次有效 Key · " + key.LastError)
		ui.keyLabel.SetTextColor(walk.RGB(190, 103, 0))
	} else {
		ui.keyLabel.SetText("AGNES_API_KEY：尚未加载 · 请点击“选择文件”或把文件拖入窗口")
		ui.keyLabel.SetTextColor(walk.RGB(190, 48, 48))
	}

	if !network.Known {
		ui.directLabel.SetText("直连：正在检测 Agnes 网站…")
		ui.clashLabel.SetText("Clash：正在检测 127.0.0.1:7890…")
	} else {
		setRouteLabel(ui.directLabel, "直连", network.Direct)
		setRouteLabel(ui.clashLabel, "Clash 127.0.0.1:7890", network.Clash)
	}

	if controller.HealthOK {
		preferred := translateRoute(controller.Health.Routes.Preferred)
		last := translateRoute(controller.Health.Routes.LastSuccessfulRoute)
		if last == "" {
			last = "尚无成功请求"
		}
		rateText := ""
		if len(controller.Health.RateLimits) > 0 {
			items := make([]string, 0, len(controller.Health.RateLimits))
			for _, limit := range controller.Health.RateLimits {
				items = append(items, fmt.Sprintf("%s 约 %ds", limit.Model, (limit.RetryAfterMS+999)/1000))
			}
			rateText = " · 限流队列：" + strings.Join(items, "，")
		}
		ui.routeLabel.SetText(fmt.Sprintf(
			"请求路线：优先 %s · 上次成功 %s · 成功 %d/%d · 路线切换/断联 %d · 限流重试 %d · 节点恢复 %d%s",
			preferred,
			last,
			controller.Health.Stats.Successes,
			controller.Health.Stats.Requests,
			controller.Health.Stats.RouteFallbacks,
			controller.Health.Stats.RateRetries,
			controller.Health.Stats.Recoveries,
			rateText,
		))
	} else {
		ui.routeLabel.SetText("请求路线：" + network.Preferred)
	}

	recovery := ui.controller.RecoveryStatus()
	if recovery.Enabled && recovery.Running {
		ui.recoveryLabel.SetText("节点自动恢复：正在检测良心云节点并切换，请稍候…")
		ui.recoveryLabel.SetTextColor(walk.RGB(190, 103, 0))
	} else if recovery.Enabled {
		detail := "已内置 · Clash 路线失败时自动检测并切换良心云可用节点"
		if !recovery.LastResult.Finished.IsZero() {
			if recovery.LastResult.OK {
				detail += " · 上次切换成功"
			} else {
				detail += " · 上次结果 " + recovery.LastResult.Reason
			}
		}
		ui.recoveryLabel.SetText("节点自动恢复：" + detail)
		ui.recoveryLabel.SetTextColor(walk.RGB(25, 126, 65))
	} else {
		ui.recoveryLabel.SetText("节点自动恢复：内置恢复组件不可用，请查看日志")
		ui.recoveryLabel.SetTextColor(walk.RGB(190, 103, 0))
	}

	if ui.notifyIcon != nil {
		tooltip := appName
		if controller.HealthOK {
			tooltip += " · 运行中"
		} else {
			tooltip += " · 需要检查"
		}
		_ = ui.notifyIcon.SetToolTip(tooltip)
	}
}

func setRouteLabel(label *walk.Label, name string, check RouteCheck) {
	if check.Online {
		label.SetText(name + "：在线 · " + check.Detail)
		label.SetTextColor(walk.RGB(25, 126, 65))
	} else {
		label.SetText(name + "：不可用 · " + check.Detail)
		label.SetTextColor(walk.RGB(190, 48, 48))
	}
}

func translateRoute(route string) string {
	switch strings.ToLower(route) {
	case "direct":
		return "直连"
	case "clash":
		return "Clash"
	default:
		return route
	}
}

func normalizeAgnesKeyFilePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("文件路径为空")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析文件路径：%w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("读取文件：%w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("拖入的是文件夹，请选择一个 API Key 文件")
	}
	return absolute, nil
}

func openPath(path string) error {
	operation, _ := windows.UTF16PtrFromString("open")
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, operation, file, nil, nil, 1)
}

func createApplicationIcon() (*walk.Icon, error) {
	const size = 32
	canvas := image.NewNRGBA(image.Rect(0, 0, size, size))
	center := float64(size-1) / 2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)-center, float64(y)-center
			if dx*dx+dy*dy <= 14.5*14.5 {
				canvas.Set(x, y, color.NRGBA{R: 116, G: 70, B: 185, A: 255})
			}
		}
	}
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	for y := 9; y <= 23; y++ {
		for x := 8; x <= 23; x++ {
			if (x == 8 || x == 23 || y == 9 || y == 23) && !(x < 12 && y > 18) {
				canvas.Set(x, y, white)
			}
		}
	}
	return walk.NewIconFromImage(canvas)
}
