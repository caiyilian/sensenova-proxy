# SenseNova Pool 桌面版

这是一个不依赖 Node.js、Python 或 .NET 运行时的 Windows 桌面程序。一个 EXE 同时提供托盘界面、SenseNova 多账号轮询代理、密钥热更新、网络检测和开机自启。

## 第一次使用

1. 把 `SenseNovaPool.exe` 放到一个固定位置后双击运行。开启自启后不要再随意移动 EXE，否则 Windows 中保存的启动路径会失效。
2. 第一次运行只会显示主界面，不会直接弹出文件窗口。点击“选择任意文件…”或把文件拖到主窗口即可；文件可以是 `.txt`、`.json` 或没有扩展名，但内容统一为每行一个 SenseNova Key。空行和以 `#` 开头的注释会被忽略。
3. 主界面会实时显示“可调度的 API Key”数量；上游明确拒绝鉴权的 Key 会被隔离并从这个数字中扣除。格式错误或重复行不会让程序崩溃；已加载的文件暂时被删除或锁定时，程序会保留上一次成功结果。
4. 点击“同步本机客户端”后，程序会自动检测 OpenCode 和 WorkBuddy：有哪个就同步哪个，两者都有就一次全部同步，两者都没有则只提示没有需要同步的客户端，不会创建多余配置。WorkBuddy 的 `models.json` 支持顶层模型数组和 `{ "models": [...] }` 两种格式，并会保持原格式。OpenCode 的其他 provider 与注释会保留；WorkBuddy 中的 Agnes 和其他自定义模型也不会被修改。修改已有配置前会创建 `.sensenova-pool.bak` 备份。
5. 需要时勾选“开机自动启动”。开机启动会直接进入托盘；窗口右上角的 `×` 也只隐藏到托盘。真正退出需要右键托盘图标选择“退出”。

默认监听地址是 `http://127.0.0.1:18787/v1`，只允许本机访问。确实需要从可信局域网连接时，可以勾选“允许局域网访问”；代理会立即改为监听所有 IPv4 网卡，界面同时显示应填写到远端客户端的局域网 Base URL。该设置默认关闭，因此发给朋友的同一个 EXE 不会自动开放端口。

Windows 防火墙可能为新 EXE 自动创建入站“阻止”规则。远端能 ping 通但连接 `18787` 超时时，应先禁用该程序的旧阻止规则，再仅向需要的远端 IP 放行 TCP `18787`；显式阻止规则的优先级高于允许规则。

## WorkBuddy

在 `%USERPROFILE%\.workbuddy\models.json` 中，每个模型使用：

- `url`: `http://127.0.0.1:18787/v1/chat/completions`
- `apiKey`: 主界面显示的“本地 API Key”
- `useCustomProtocol`: `false`

桌面版提供以下模型 ID：

- `deepseek-v4-flash`
- `sensenova-6.7-flash-lite`
- `glm-5.2`
- `deepseek-v4-pro`
- `kimi-k3`
- `sensenova-6.8-flash-lite`

## 联网检测与日志

程序每 5 秒使用 `HEAD https://platform.sensenova.cn/` 检测一次，不调用模型、不消耗任何账号额度。代理请求与联网检测共用同一套联网路线：标准代理环境变量优先，其次是 Windows 当前用户的系统代理，最后是直连。因此开机后 Clash 尚未启动或遗留的 `127.0.0.1:7890` 不可用时，界面会显示离线；Clash 恢复后会自动变为在线。

日志位于 `%LOCALAPPDATA%\SenseNovaPool\logs`，主界面可直接打开。日志只记录请求结果、模型、账号序号、Key 的单向短指纹和冷却状态，不记录完整 Key、提示词或模型回复。

“关于 / 许可”窗口包含随单个 EXE 一起嵌入的第三方许可说明，因此分发时不需要额外附带许可文件。

## 本地构建

在 Windows PowerShell 中运行：

```powershell
.\build.ps1 -Version 0.2.2
```

构建脚本会先执行测试，再嵌入 Windows Common Controls 清单，最后生成 `dist\SenseNovaPool.exe`。需要 Go 1.24 或兼容版本；最终 EXE 本身不要求目标电脑安装 Go。
