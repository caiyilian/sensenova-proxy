# 排坑记录：Lucky (Claude Code 中文版) 接入 SenseNova 代理

> 记录在 Windows 环境下配置 Lucky 使用 SenseNova Proxy 过程中遇到的各种问题及解决方案。

---

## 背景

现有 6 个 SenseNova Token Plan 免费 API Key，每个 key 有独立的频率限制（500 次/5 小时）。`sensenova-proxy` 实现了轮询代理，将多个 key 聚合为单一 Anthropic Messages API 端点。目标是将 Lucky（Claude Code 中文版）接入此代理，使用 `deepseek-v4-flash` 模型。

---

## 问题 1：npm.ps1 兼容性问题导致 Lucky 安装失败

### 现象

Lucky 的安装脚本在 Windows PowerShell 5.1 上运行失败，报错 `Unknown command: NpmCommand.Source`。原生 npm（`npm.ps1`）通过 PowerShell 调用运算符 `&` 配合数组展开（splatting）时，参数解析异常。

### 根因

Windows 上的 Node.js 安装程序将 npm 注册为 `ExternalScript`（`npm.ps1`）。安装脚本通过 `& $NpmCommand.Source @npmArgs` 调用 npm，但 `npm.ps1` 内部使用 `$MyInvocation` 解析参数，而数组展开（`@npmArgs`）导致 `$MyInvocation` 无法正确捕获参数，使 `npm.ps1` 错误地将 `NpmCommand.Source` 字面量解释为命令。

### 解决方案

绕过 `npm.ps1`，直接使用 Node.js 执行 npm CLI：

```powershell
# 下载安装包
Invoke-WebRequest -Uri "https://bridge.annealing.cn/download/thkj-claude-code.tgz" `
  -OutFile "$env:TEMP\lucky-install\thkj-claude-code.tgz"

# 用 node.exe 直接运行 npm-cli.js 安装
& "<node_install_dir>\node.exe" "<node_install_dir>\node_modules\npm\bin\npm-cli.js" `
  install --global --prefix "$env:LOCALAPPDATA\Programs\Lucky\channels\stable" `
  --no-audit --no-fund "$env:TEMP\lucky-install\thkj-claude-code.tgz"
```

---

## 问题 2：创建 `claude` 命令别名

### 现象

Lucky 安装后需要运行 `lucky` 命令启动，但用户习惯使用 `claude` 命令。

### 解决方案

在 `%LOCALAPPDATA%\Programs\Lucky\bin\` 目录下创建 `claude.cmd` 和 `claude.ps1` 包装脚本，内容与 `lucky.cmd`/`lucky.ps1` 一致，并将该目录添加到用户 PATH 环境变量中。这样 `claude` 和 `lucky` 两个命令都指向同一个 Lucky 安装。

---

## 问题 3：通过轮询代理转发 API 请求

### 现象

`/model` 命令显示默认模型为 `claude-opus-4-8`，且响应自称"我是 Claude Opus 4"。

### 根因

Lucky 通过内置桥接层（THKJ Bridge）连接 `bridge.annealing.cn`，桥接层转发到 Anthropic 官方 API，使用的是 Claude 模型。

### 解决方案

配置 `sensenova-proxy`（监听 `127.0.0.1:6790`）将请求轮询转发到 6 个 SenseNova API Key：

```json
// ~/.lucky/settings.json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:6790",
    "ANTHROPIC_AUTH_TOKEN": "sk-proxy",
    "ANTHROPIC_MODEL": "deepseek-v4-flash",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "deepseek-v4-flash",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "deepseek-v4-flask",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "deepseek-v4-flash"
  }
}
```

> **注意**：`ANTHROPIC_BASE_URL` 不要带 `/v1` 后缀，Claude Code/Lucky SDK 会自动追加 `/v1/messages`。

---

## 问题 4：Lucky 桥接层拦截 API 请求（关键问题）

### 现象

即使正确配置了 `ANTHROPIC_BASE_URL`，Lucky 仍然连接 Claude 模型，代理未收到任何请求。

### 根因

Lucky（`@thkj/claude-code`）是 Claude Code 的中文镜像版本，内置了名为 **THKJ Bridge** 的桥接层。该桥接层：

1. 通过 `THKJ_BRIDGE_MODE` 环境变量控制启停
2. 拦截所有 API 调用，优先发往 `bridge.annealing.cn` 的 Lucky API 服务器
3. 即使设置了 `ANTHROPIC_BASE_URL`，桥接层仍然会覆盖该配置，忽略自定义端点

Lucky 的默认启动脚本（`%LOCALAPPDATA%\Programs\Lucky\bin\lucky.cmd`）设置了 `THKJ_BRIDGE_MODE=1`，因此始终走桥接通道。

### 解决方案

创建独立的包装脚本，显式禁用桥接层，直接走自定义代理：

```batch
:: lucky-sensenova.cmd
@echo off
set THKJ_BRIDGE_MODE=0
set ANTHROPIC_BASE_URL=http://127.0.0.1:6790
set ANTHROPIC_AUTH_TOKEN=sk-proxy
set ANTHROPIC_DEFAULT_OPUS_MODEL=deepseek-v4-flash
set ANTHROPIC_DEFAULT_SONNET_MODEL=deepseek-v4-flash
set ANTHROPIC_DEFAULT_HAIKU_MODEL=deepseek-v4-flash
set CLAUDE_CONFIG_DIR=%USERPROFILE%\.lucky-sensenova
set CLAUDE_CODE_TMPDIR=%TEMP%\lucky-sensenova-tmp
"%LOCALAPPDATA%\Programs\Lucky\channels\stable\lucky.cmd" %*
```

关键改动：
- `THKJ_BRIDGE_MODE=0` — 禁用 Lucky 内置桥接层
- `ANTHROPIC_BASE_URL=http://127.0.0.1:6790` — 指向本地 SenseNova 代理
- `CLAUDE_CONFIG_DIR` — 使用独立配置目录，避免与原始 Lucky 配置冲突

同时创建对应的 PowerShell 版本 `lucky-sensenova.ps1`，都放在 `%LOCALAPPDATA%\Programs\Lucky\bin\` 目录下。

使用方式：
```cmd
lucky-sensenova
```

---

## 问题 5：SenseNova API 请求返回 `invalid arguments`

### 现象

通过 `curl.exe` 直接调用 SenseNova API 返回 `{"error":{"message":"invalid arguments","type":"invalid_request_error","code":"3"}}`。

### 根因

Windows 的 `curl.exe` 与 PowerShell 的 `curl` 别名（实际指向 `Invoke-WebRequest`）之间的混淆导致请求体编码异常。即使使用原生 `curl.exe`，JSON 请求体中的引号转义也可能出现问题。

### 解决方案

使用 PowerShell 的 `Invoke-RestMethod` 发送请求：

```powershell
$body = @{
  model = "deepseek-v4-flash"
  messages = @(@{ role = "user"; content = "Say hello" })
} | ConvertTo-Json -Compress

Invoke-RestMethod -Uri "https://token.sensenova.cn/v1/chat/completions" `
  -Method POST -ContentType "application/json" `
  -Headers @{ Authorization = "Bearer $key" } -Body $body
```

---

## 最终配置总结

| 组件 | 位置 |
|------|------|
| SenseNova Proxy | `node sensenova-proxy.js` → `http://127.0.0.1:6790` |
| Lucky 主程序 | `%LOCALAPPDATA%\Programs\Lucky\channels\stable\` |
| Lucky-SenseNova 启动脚本 | `%LOCALAPPDATA%\Programs\Lucky\bin\lucky-sensenova.cmd` |
| SenseNova 配置目录 | `%USERPROFILE%\.lucky-sensenova\settings.json` |
| API Key 文件 | `sensenova_apikeys`（每行一个 key，6 个轮询） |

### 启动步骤

```powershell
# 终端 1：启动代理（保持运行）
cd E:\projects\sensenova-proxy
node sensenova-proxy.js

# 终端 2：启动 Lucky（新窗口）
lucky-sensenova
```

### 架构示意

```
lucky-sensenova              sensenova-proxy                  SenseNova API
      │                            │                              │
      │  POST /v1/messages          │                              │
      │  (THKJ_BRIDGE_MODE=0)       │                              │
      │ ─────────────────────►      │                              │
      │                            │  POST /v1/messages (key 1)    │
      │                            │ ──────────────────────────►   │
      │                            │  POST /v1/messages (key 2)    │
      │                            │ ──────────────────────────►   │
      │                            │  ... (round-robin)            │
      │  ◄── deepseek-v4-flash ◄── │                              │
      │ ─────────────────────      │                              │
```