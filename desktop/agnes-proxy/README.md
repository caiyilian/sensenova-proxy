# Agnes Proxy 桌面控制器

这是仅供本机使用的独立 Windows 托盘程序。`AgnesProxy.exe` 会在后台无窗口启动仓库中现有的 `agnes-proxy.js`，因此沿用已经验证过的直连 → Clash → 自动节点恢复和本地限流排队逻辑，同时不再保留一个终端窗口。

## 运行条件

- 用户环境变量 `AGNES_API_KEY` 已设置；界面只显示它的短指纹，不显示完整值。
- 本机已安装 Node.js。
- EXE 默认从自身父目录向上寻找 `agnes-proxy.js`，所以放在本仓库的 `desktop\agnes-proxy\dist` 中即可。也可使用 `--script` 和 `--node` 指定路径。
- 自动节点恢复继续使用相邻项目 `E:\projects\android-install\tools\clash-node-helper.ps1`；代理请求的所有网络路线都失败时由原代理核心自动调用。

默认客户端配置保持不变。如果设置了用户环境变量 `AGNES_PROXY_LOCAL_TOKEN`，界面与后台代理会优先沿用它，以兼容已有的 OpenCode / WorkBuddy 配置；未设置时才使用下面的默认值。

- URL：`http://127.0.0.1:18788/v1/chat/completions`
- 本地 API Key：`local-agnes-proxy`
- 模型：`agnes-2.0-flash`、`agnes-2.5-flash`、`agnes-3.0-flash`

界面每 5 秒分别检测 Agnes 的直连和 `127.0.0.1:7890` Clash 路线，检测不携带 `AGNES_API_KEY`，也不会调用模型。环境变量被修改后，程序会自动重新读取并重启隐藏的代理进程。

窗口右上角 `×` 只隐藏到托盘；真正退出、重启代理均可从托盘右键菜单完成。开机自启是独立的当前用户设置，不会与 SenseNova Pool 的自启项互相覆盖。

## 构建

```powershell
.\build.ps1 -Version 0.1.0
```

成品位于 `dist\AgnesProxy.exe`。目标电脑不需要 Go，但此自用控制器仍需要本仓库、Node.js 与已有 npm 依赖。
