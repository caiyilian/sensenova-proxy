# Agnes Proxy 便携桌面版

`AgnesProxy.exe` 是面向个人多台 Windows 电脑的单文件托盘程序。代理核心已改为内置 Go 实现，目标电脑不需要本仓库、Node.js、npm、Python 或 .NET；准备好以下两项即可：

- 本机 Clash HTTP 代理监听 `127.0.0.1:7890`（当前按良心云 / Clash Verge Rev 的使用方式优化）。
- 一个包含 Agnes API Key 的本地文件。

首次运行先显示主界面，不会直接弹出文件选择框。点击“选择文件”或把文件拖进窗口即可；路径会保存到 `%LOCALAPPDATA%\AgnesProxy\settings.json`，以后启动自动沿用。支持任意扩展名和无后缀文件，并识别：

- 纯文本：第一行有效内容就是 Key；
- dotenv：`AGNES_API_KEY=...`；
- JSON：`apiKey`、`agnesApiKey`、`key`、`token` 或 `accessToken` 字段。

程序每 3 秒重读一次文件。手动修改或替换 Key 后不需要重启；如果保存过程中出现短暂空文件、格式错误或文件被占用，代理继续使用上一次有效 Key，并在界面和日志中提示，不会退出。

## 网络韧性

- 每 5 秒分别检测 Agnes 直连和 `127.0.0.1:7890`，检测请求不携带 API Key，也不调用模型。
- 安全的建连失败会以 `2 / 4 / 8 / 16 / 30… 秒`退避重试，最长受单次请求的 10 分钟本地等待窗口约束。
- 代理节点返回 `502 / 503 / 504 / 522 / 523 / 524` 时会切换路线并重试。
- Clash 路线连接失败或返回上述网关错误时，会调用随 EXE 内置释放的 PowerShell 恢复组件，通过 Clash Verge Rev 的 `verge-mihomo` 控制管道检测节点，优先选择良心云/代理选择组中的低延迟可用节点，然后重新请求。
- 上游返回 TPM/RPM/429 时按模型进入本地限流队列，不把指数退避错误直接抛给客户端。
- 响应流在尚未输出任何字节前断开时可以自动重试；已经向客户端输出部分内容后不会把第二次回答拼接上去，而是中止并记录 `bytesForwarded`，避免响应损坏。

所有请求结果、重试次数、等待时间、路线、节点恢复结果和流中断位置均写入 `%LOCALAPPDATA%\AgnesProxy\logs\desktop-YYYY-MM-DD.jsonl`。日志不记录请求正文、响应正文或 API Key。

## 客户端信息

- Base URL：`http://127.0.0.1:18788/v1`
- Chat Completions URL：`http://127.0.0.1:18788/v1/chat/completions`
- 本地 API Key：`local-agnes-proxy`
- 模型：`agnes-2.0-flash`、`agnes-2.5-flash`、`agnes-3.0-flash`

为了兼容这台电脑原有配置，若用户环境变量中已有 `AGNES_API_KEY`，尚未选择文件时仍可继续使用；文件一旦配置便优先于环境变量。若设置了 `AGNES_PROXY_LOCAL_TOKEN`，客户端连接令牌也会继续沿用该值。

窗口右上角 `×` 只隐藏到托盘；真正退出请右键托盘图标选择“退出”。开机自启时直接进入托盘，但首次尚未配置文件时仍会显示界面。

## 构建

```powershell
.\build.ps1 -Version 0.2.0
```

成品位于 `dist\AgnesProxy.exe`。
