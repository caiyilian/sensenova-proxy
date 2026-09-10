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
	paths    AppPaths
	settings Settings
	keyStore *KeyStore
	pool     *AccountPool
	network  *NetworkMonitor
	gateway  *Gateway
	logger   *JSONLLogger

	mainWindow       *walk.MainWindow
	notifyIcon       *walk.NotifyIcon
	appIcon          *walk.Icon
	proxyStatusLabel *walk.Label
	networkLabel     *walk.Label
	keyCountLabel    *walk.Label
	lastReloadLabel  *walk.Label
	keyHintLabel     *walk.Label
	keyPathEdit      *walk.LineEdit
	openKeyButton    *walk.PushButton
	baseURLEdit      *walk.LineEdit
	lanURLEdit       *walk.LineEdit
	localTokenEdit   *walk.LineEdit
	logPathEdit      *walk.LineEdit
	autoStartCheck   *walk.CheckBox
	allowLANCheck    *walk.CheckBox

	gatewayStartError error
	exiting           bool
	autoStartUpdating bool
	allowLANUpdating  bool
	refreshStop       chan struct{}
	refreshDone       chan struct{}
	refreshMu         sync.Mutex
	refreshStarted    bool
	stopRefresh       sync.Once
}

func NewDesktopUI(
	paths AppPaths,
	settings Settings,
	keyStore *KeyStore,
	pool *AccountPool,
	network *NetworkMonitor,
	gateway *Gateway,
	logger *JSONLLogger,
	gatewayStartError error,
) *DesktopUI {
	return &DesktopUI{
		paths:             paths,
		settings:          settings,
		keyStore:          keyStore,
		pool:              pool,
		network:           network,
		gateway:           gateway,
		logger:            logger,
		gatewayStartError: gatewayStartError,
		allowLANUpdating:  true,
		refreshStop:       make(chan struct{}),
		refreshDone:       make(chan struct{}),
	}
}

func (ui *DesktopUI) Run(startHidden bool) error {
	icon, err := createApplicationIcon()
	if err != nil {
		return fmt.Errorf("create application icon: %w", err)
	}
	ui.appIcon = icon

	window := d.MainWindow{
		AssignTo: &ui.mainWindow,
		Title:    appName,
		Icon:     icon,
		Visible:  false,
		MinSize:  d.Size{Width: 740, Height: 600},
		Size:     d.Size{Width: max(ui.settings.WindowWidth, 790), Height: max(ui.settings.WindowHeight, 650)},
		Layout: d.VBox{
			Margins: d.Margins{Left: 16, Top: 14, Right: 16, Bottom: 14},
			Spacing: 10,
		},
		OnDropFiles: ui.dropKeyFiles,
		Children: []d.Widget{
			d.Label{
				Text: "SenseNova 多账号轮询代理",
				Font: d.Font{Family: "Microsoft YaHei UI", PointSize: 14, Bold: true},
			},
			d.Label{
				Text: "程序运行时代理始终在后台工作；关闭窗口会隐藏到系统托盘。",
			},
			d.GroupBox{
				Title:  "运行状态",
				Layout: d.VBox{Margins: d.Margins{Left: 10, Top: 8, Right: 10, Bottom: 9}, Spacing: 5},
				Children: []d.Widget{
					d.Label{AssignTo: &ui.proxyStatusLabel, Text: "代理服务：正在启动…"},
					d.Label{AssignTo: &ui.networkLabel, Text: "联网状态：正在检测…"},
					d.Label{AssignTo: &ui.keyCountLabel, Text: "格式有效的 API Key：0"},
					d.Label{AssignTo: &ui.lastReloadLabel, Text: "密钥文件：尚未加载"},
				},
			},
			d.GroupBox{
				Title:  "API Key 文件",
				Layout: d.VBox{Margins: d.Margins{Left: 10, Top: 8, Right: 10, Bottom: 9}, Spacing: 7},
				Children: []d.Widget{
					d.Label{
						AssignTo:  &ui.keyHintLabel,
						Text:      "尚未选择 API Key 文件。请点击“选择任意文件…”或把文件拖到此窗口。",
						TextColor: walk.RGB(190, 103, 0),
					},
					d.Composite{
						Layout: d.HBox{MarginsZero: true, Spacing: 7},
						Children: []d.Widget{
							d.LineEdit{AssignTo: &ui.keyPathEdit, ReadOnly: true, StretchFactor: 1},
							d.PushButton{Text: "选择任意文件…", MinSize: d.Size{Width: 108}, OnClicked: ui.chooseKeyFile},
							d.PushButton{AssignTo: &ui.openKeyButton, Text: "打开文件", MinSize: d.Size{Width: 82}, OnClicked: ui.openKeyFile},
						},
					},
					d.Label{Text: "文件扩展名不限；内容仍为每行一个 API Key。外部保存后约 1 秒自动重载。"},
				},
			},
			d.GroupBox{
				Title:  "客户端连接信息",
				Layout: d.Grid{Columns: 3, Margins: d.Margins{Left: 10, Top: 8, Right: 10, Bottom: 9}, Spacing: 7},
				Children: []d.Widget{
					d.Label{Text: "本机 Base URL"},
					d.LineEdit{AssignTo: &ui.baseURLEdit, ReadOnly: true, ColumnSpan: 1, StretchFactor: 1},
					d.PushButton{Text: "复制", MinSize: d.Size{Width: 62}, OnClicked: func() { ui.copyText(ui.baseURLEdit.Text(), "Base URL") }},
					d.Label{Text: "局域网 Base URL"},
					d.LineEdit{AssignTo: &ui.lanURLEdit, ReadOnly: true, ColumnSpan: 1, StretchFactor: 1},
					d.PushButton{Text: "复制", MinSize: d.Size{Width: 62}, OnClicked: func() { ui.copyText(ui.lanURLEdit.Text(), "局域网 Base URL") }},
					d.Label{Text: "本地 API Key"},
					d.LineEdit{AssignTo: &ui.localTokenEdit, ReadOnly: true, ColumnSpan: 1, StretchFactor: 1},
					d.PushButton{Text: "复制", MinSize: d.Size{Width: 62}, OnClicked: func() { ui.copyText(ui.localTokenEdit.Text(), "本地 API Key") }},
					d.Label{Text: "OpenCode 配置"},
					d.Composite{
						ColumnSpan:    2,
						Layout:        d.HBox{MarginsZero: true, Spacing: 7},
						StretchFactor: 1,
						Children: []d.Widget{
							d.PushButton{Text: "同步到本机 OpenCode", MinSize: d.Size{Width: 152}, OnClicked: ui.syncLocalOpenCode},
							d.HSpacer{},
						},
					},
				},
			},
			d.GroupBox{
				Title:  "程序设置",
				Layout: d.VBox{Margins: d.Margins{Left: 10, Top: 8, Right: 10, Bottom: 9}, Spacing: 7},
				Children: []d.Widget{
					d.CheckBox{
						AssignTo:         &ui.allowLANCheck,
						Text:             "允许局域网访问（仅在可信局域网中开启；切换后代理会自动重新监听）",
						Checked:          ui.settings.AllowLAN,
						OnCheckedChanged: ui.allowLANChanged,
					},
					d.Label{
						Text:      "若远端仍连接超时，请检查 Windows 防火墙；已有的“阻止”规则会覆盖允许规则。",
						TextColor: walk.RGB(95, 99, 104),
					},
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
			d.Composite{
				Layout: d.HBox{MarginsZero: true, Spacing: 8},
				Children: []d.Widget{
					d.Label{
						Text:          "提示：窗口右上角 × 只会最小化到托盘；如需真正退出，请右键托盘图标选择“退出”。",
						TextColor:     walk.RGB(95, 99, 104),
						StretchFactor: 1,
					},
					d.PushButton{Text: "关于 / 许可", MinSize: d.Size{Width: 86}, OnClicked: ui.showAbout},
				},
			},
		},
	}
	if err := window.Create(); err != nil {
		return err
	}
	ui.allowLANUpdating = false
	ui.mainWindow.Closing().Attach(ui.windowClosing)
	if err := ui.createTrayIcon(); err != nil {
		ui.mainWindow.Dispose()
		return err
	}
	ui.refreshUI()
	ui.startRefreshLoop()

	keyConfigured := ui.settings.KeyFilePath != ""
	if startHidden && keyConfigured {
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
	notifyIcon, err := walk.NewNotifyIcon(ui.mainWindow)
	if err != nil {
		return err
	}
	ui.notifyIcon = notifyIcon
	if err := notifyIcon.SetIcon(ui.appIcon); err != nil {
		return err
	}
	if err := notifyIcon.SetToolTip(appName + " · 正在启动"); err != nil {
		return err
	}
	notifyIcon.MouseUp().Attach(func(_ int, _ int, button walk.MouseButton) {
		if button == walk.LeftButton {
			ui.showWindow()
		}
	})

	openAction := walk.NewAction()
	_ = openAction.SetText("打开主界面")
	openAction.Triggered().Attach(ui.showWindow)
	if err := notifyIcon.ContextMenu().Actions().Add(openAction); err != nil {
		return err
	}
	separator := walk.NewSeparatorAction()
	if err := notifyIcon.ContextMenu().Actions().Add(separator); err != nil {
		return err
	}
	exitAction := walk.NewAction()
	_ = exitAction.SetText("退出")
	exitAction.Triggered().Attach(ui.exitFromTray)
	if err := notifyIcon.ContextMenu().Actions().Add(exitAction); err != nil {
		return err
	}
	return notifyIcon.SetVisible(true)
}

func (ui *DesktopUI) chooseKeyFile() {
	dialog := walk.FileDialog{
		Title:  "选择 SenseNova API Key 文件",
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
	if !accepted {
		return
	}
	ui.useKeyFile(dialog.FilePath, "file_dialog")
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
	normalized, err := normalizeKeyFilePath(path)
	if err != nil {
		walk.MsgBox(ui.mainWindow, appName, "无法使用该文件：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	if err := ui.keyStore.SetPath(normalized); err != nil {
		walk.MsgBox(ui.mainWindow, appName, "无法加载该文件：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	ui.settings.KeyFilePath = normalized
	if err := saveSettings(ui.paths, ui.settings); err != nil {
		walk.MsgBox(ui.mainWindow, appName, "密钥文件已加载，但无法保存设置：\n"+redactText(err.Error()), walk.MsgBoxIconWarning)
	}
	ui.logger.Log("key_file_selected", map[string]any{
		"source":   source,
		"fileName": filepath.Base(normalized),
	})
	ui.refreshUI()
}

func (ui *DesktopUI) openKeyFile() {
	path := ui.keyStore.Snapshot().Path
	if path == "" {
		return
	}
	if err := openPath(path); err != nil {
		walk.MsgBox(ui.mainWindow, appName, "无法打开文件：\n"+redactText(err.Error()), walk.MsgBoxIconError)
	}
}

func (ui *DesktopUI) openLogFolder() {
	if err := os.MkdirAll(ui.paths.LogDir, 0o700); err != nil {
		walk.MsgBox(ui.mainWindow, appName, "无法创建日志目录：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	if err := openPath(ui.paths.LogDir); err != nil {
		walk.MsgBox(ui.mainWindow, appName, "无法打开日志目录：\n"+redactText(err.Error()), walk.MsgBoxIconError)
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

func (ui *DesktopUI) allowLANChanged() {
	if ui.allowLANUpdating {
		return
	}
	desired := ui.allowLANCheck.Checked()
	previous := ui.settings.AllowLAN
	if desired == previous {
		return
	}
	if err := ui.gateway.Restart(gatewayBindHost(desired), ui.settings.Port); err != nil {
		fallbackErr := ui.gateway.Restart(gatewayBindHost(previous), ui.settings.Port)
		ui.allowLANUpdating = true
		ui.allowLANCheck.SetChecked(previous)
		ui.allowLANUpdating = false
		message := "无法切换局域网监听：\n" + redactText(err.Error())
		if fallbackErr != nil {
			ui.gatewayStartError = fallbackErr
			message += "\n恢复原监听也失败：\n" + redactText(fallbackErr.Error())
		}
		walk.MsgBox(ui.mainWindow, appName, message, walk.MsgBoxIconError)
		ui.refreshUI()
		return
	}
	ui.settings.AllowLAN = desired
	if err := saveSettings(ui.paths, ui.settings); err != nil {
		_ = ui.gateway.Restart(gatewayBindHost(previous), ui.settings.Port)
		ui.settings.AllowLAN = previous
		ui.allowLANUpdating = true
		ui.allowLANCheck.SetChecked(previous)
		ui.allowLANUpdating = false
		walk.MsgBox(ui.mainWindow, appName, "代理已经切换，但无法保存设置，已恢复原状态：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		ui.refreshUI()
		return
	}
	ui.gatewayStartError = nil
	ui.logger.Log("lan_access_changed", map[string]any{"enabled": desired})
	ui.refreshUI()
}

func (ui *DesktopUI) syncLocalOpenCode() {
	path, err := defaultOpenCodeConfigPath()
	if err != nil {
		walk.MsgBox(ui.mainWindow, appName, "无法确定 OpenCode 配置位置：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	backup, err := syncOpenCodeConfig(path, localBaseURL(ui.settings.Port), ui.settings.LocalToken)
	if err != nil {
		walk.MsgBox(ui.mainWindow, appName, "同步 OpenCode 配置失败：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	ui.logger.Log("opencode_config_synced", map[string]any{
		"created": backup == "",
	})
	message := "已同步本机 OpenCode 配置：\n" + path
	if backup != "" {
		message += "\n\n修改前的配置已备份到：\n" + backup
	}
	message += "\n\n如果 OpenCode 已经打开，请重新启动一次 OpenCode。"
	walk.MsgBox(ui.mainWindow, appName, message, walk.MsgBoxIconInformation)
}

func (ui *DesktopUI) copyText(text, label string) {
	if err := walk.Clipboard().SetText(text); err != nil {
		walk.MsgBox(ui.mainWindow, appName, "复制失败：\n"+redactText(err.Error()), walk.MsgBoxIconError)
		return
	}
	if ui.notifyIcon != nil {
		_ = ui.notifyIcon.ShowInfo(appName, label+" 已复制")
	}
}

func (ui *DesktopUI) showAbout() {
	var dialog *walk.Dialog
	var closeButton *walk.PushButton
	_, err := d.Dialog{
		AssignTo:     &dialog,
		Title:        "关于 SenseNova Pool",
		CancelButton: &closeButton,
		MinSize:      d.Size{Width: 650, Height: 480},
		Size:         d.Size{Width: 700, Height: 560},
		Layout:       d.VBox{Margins: d.Margins{Left: 12, Top: 12, Right: 12, Bottom: 12}, Spacing: 8},
		Children: []d.Widget{
			d.Label{Text: fmt.Sprintf("SenseNova Pool %s", version), Font: d.Font{Family: "Microsoft YaHei UI", PointSize: 12, Bold: true}},
			d.Label{Text: "独立 Windows 多账号轮询代理。第三方组件许可如下："},
			d.TextEdit{Text: thirdPartyNotices, ReadOnly: true, VScroll: true, HScroll: true, StretchFactor: 1},
			d.Composite{
				Layout: d.HBox{MarginsZero: true},
				Children: []d.Widget{
					d.HSpacer{},
					d.PushButton{AssignTo: &closeButton, Text: "关闭", OnClicked: func() { dialog.Accept() }},
				},
			},
		},
	}.Run(ui.mainWindow)
	if err != nil {
		walk.MsgBox(ui.mainWindow, appName, "无法打开关于窗口：\n"+redactText(err.Error()), walk.MsgBoxIconError)
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
	keys := ui.keyStore.Snapshot()
	network := ui.network.Status()
	address := ui.gateway.Address()
	if address == "" {
		address = fmt.Sprintf("http://127.0.0.1:%d", ui.settings.Port)
	}
	baseURL := strings.TrimSuffix(address, "/") + "/v1"
	ui.baseURLEdit.SetText(baseURL)
	lanURL, lanErr := lanBaseURL(ui.settings.Port)
	if lanErr != nil {
		ui.lanURLEdit.SetText("未检测到局域网 IPv4 地址")
	} else {
		ui.lanURLEdit.SetText(lanURL)
	}
	ui.localTokenEdit.SetText(ui.settings.LocalToken)
	ui.logPathEdit.SetText(ui.paths.LogDir)

	if ui.gatewayStartError != nil {
		ui.proxyStatusLabel.SetText("代理服务：启动失败 · " + redactText(ui.gatewayStartError.Error()))
		ui.proxyStatusLabel.SetTextColor(walk.RGB(190, 48, 48))
	} else {
		listenScope := "仅本机"
		if ui.settings.AllowLAN {
			listenScope = "本机与局域网"
		}
		ui.proxyStatusLabel.SetText("代理服务：运行中 · " + listenScope + " · " + baseURL)
		ui.proxyStatusLabel.SetTextColor(walk.RGB(25, 126, 65))
	}

	if !network.Known {
		ui.networkLabel.SetText("联网状态：正在检测 SenseNova 网站…")
		ui.networkLabel.SetTextColor(walk.RGB(120, 120, 120))
	} else if network.Online {
		ui.networkLabel.SetText("联网状态：在线 · " + network.Route)
		ui.networkLabel.SetTextColor(walk.RGB(25, 126, 65))
	} else {
		ui.networkLabel.SetText("联网状态：离线 · " + network.Route + " · " + network.Detail)
		ui.networkLabel.SetTextColor(walk.RGB(190, 48, 48))
	}

	quarantined := ui.pool.AuthQuarantinedCount(keys.Accounts)
	available := max(0, len(keys.Accounts)-quarantined)
	countText := fmt.Sprintf("可调度的 API Key：%d", available)
	if len(keys.InvalidLines) > 0 || len(keys.DuplicateLines) > 0 || quarantined > 0 {
		countText += fmt.Sprintf("（格式有效 %d，格式错误 %d，重复 %d，鉴权隔离 %d）", len(keys.Accounts), len(keys.InvalidLines), len(keys.DuplicateLines), quarantined)
	}
	ui.keyCountLabel.SetText(countText)
	if available > 0 && keys.LastError == "" {
		ui.keyCountLabel.SetTextColor(walk.RGB(25, 126, 65))
	} else {
		ui.keyCountLabel.SetTextColor(walk.RGB(190, 48, 48))
	}

	pathText := keys.Path
	if pathText == "" {
		pathText = "尚未选择"
		ui.keyHintLabel.SetText("尚未选择 API Key 文件。请点击“选择任意文件…”或把文件拖到此窗口。")
		ui.keyHintLabel.SetTextColor(walk.RGB(190, 103, 0))
	} else {
		ui.keyHintLabel.SetText("正在使用此文件；也可以把另一个文件拖到窗口中进行更换。")
		ui.keyHintLabel.SetTextColor(walk.RGB(25, 126, 65))
	}
	ui.keyPathEdit.SetText(pathText)
	ui.openKeyButton.SetEnabled(keys.Path != "")
	if keys.LastError != "" {
		ui.lastReloadLabel.SetText("密钥文件错误（保留上一次结果）：" + keys.LastError)
		ui.lastReloadLabel.SetTextColor(walk.RGB(190, 48, 48))
	} else if keys.LastReload.IsZero() {
		ui.lastReloadLabel.SetText("密钥文件：尚未加载")
		ui.lastReloadLabel.SetTextColor(walk.RGB(120, 120, 120))
	} else {
		ui.lastReloadLabel.SetText("最近加载：" + keys.LastReload.Format("2006-01-02 15:04:05"))
		ui.lastReloadLabel.SetTextColor(walk.RGB(80, 80, 80))
	}

	if ui.notifyIcon != nil {
		status := fmt.Sprintf("%s · %d 个 Key", appName, len(keys.Accounts))
		if network.Known && !network.Online {
			status += " · 离线"
		}
		_ = ui.notifyIcon.SetToolTip(status)
	}
}

func normalizeKeyFilePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("文件路径为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析文件路径：%w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("读取文件：%w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("拖入的是文件夹，请选择一个 API Key 文件")
	}
	return abs, nil
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
			dx := float64(x) - center
			dy := float64(y) - center
			if dx*dx+dy*dy <= 14.5*14.5 {
				canvas.Set(x, y, color.NRGBA{R: 28, G: 137, B: 220, A: 255})
			}
		}
	}
	drawDot := func(cx, cy, radius int, value color.NRGBA) {
		for y := cy - radius; y <= cy+radius; y++ {
			for x := cx - radius; x <= cx+radius; x++ {
				if x >= 0 && x < size && y >= 0 && y < size && (x-cx)*(x-cx)+(y-cy)*(y-cy) <= radius*radius {
					canvas.Set(x, y, value)
				}
			}
		}
	}
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	for x := 10; x <= 22; x++ {
		canvas.Set(x, 16, white)
	}
	drawDot(9, 16, 3, white)
	drawDot(16, 16, 3, white)
	drawDot(23, 16, 3, white)
	return walk.NewIconFromImage(canvas)
}
