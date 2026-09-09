# SenseNova Proxy

A pair of lightweight local gateways that pool multiple SenseNova API keys:

- `sensenova-proxy.js` keeps the original Anthropic Messages API endpoint for Claude Code/Lucky.
- `opencode-pool-proxy.js` exposes an OpenAI-compatible endpoint for OpenCode, WorkBuddy, and similar clients, with automatic failover and per-model cooldowns.

## Why

SenseNova ([商汤日日新](https://platform.sensenova.cn)) provides free API access to models like `deepseek-v4-flash` via its [Token Plan](https://platform.sensenova.cn/token-plan), but each key has a rate limit (e.g. 150–500 requests per 5 hours). This proxy lets you pool multiple keys and rotate them automatically, effectively multiplying your quota.

## How It Works

```
Claude Code / Lucky         SenseNova Proxy              SenseNova API
       │                         │                            │
       │  POST /v1/messages       │                            │
       │ ──────────────────────►  │                            │
       │                         │  POST /v1/messages (key 1)  │
       │                         │ ──────────────────────────► │
       │                         │  POST /v1/messages (key 2)  │
       │                         │ ──────────────────────────► │
       │                         │  ... (round-robin)          │
       │  ◄── Anthropic response  │                            │
       │ ──────────────────────  │                            │
```

- Accepts **Anthropic Messages API** format (`/v1/messages`)
- Rotates through your API keys on each request
- Proxies both streaming (SSE) and non-streaming responses
- No protocol translation needed — SenseNova natively supports the Anthropic format

## OpenCode / WorkBuddy gateway

The OpenAI-compatible gateway listens on `127.0.0.1:18787` and forwards `POST /v1/chat/completions` to SenseNova. It uses the same gitignored `sensenova_apikeys` file, one key per line.

```text
OpenCode / WorkBuddy       Local pool gateway             SenseNova
        |               127.0.0.1:18787/v1                   |
        |  chat/completions          |                        |
        | -------------------------> | -- account 1 --------> |
        |                            | <- TPM/RPM limit ------ |
        |                            | -- account 2 --------> |
        | <------------------------- | <- successful stream - |
```

The gateway:

- rotates across any number of keys and prefers the least-busy account;
- switches accounts immediately on TPM/RPM limits, exhausted entitlements, authentication failures, network failures, and upstream 5xx responses;
- uses a fixed 60-second TPM/RPM cooldown (or a longer server-provided `Retry-After`) instead of exponential backoff;
- tracks cooldowns separately per model, so a limited model does not unnecessarily disable the same account for other models;
- waits locally only when every account is cooling down (up to 10 minutes by default);
- hot-reloads `sensenova_apikeys` after it changes, without a restart;
- writes metadata-only JSONL logs to `logs/openai-pool/` and never logs API keys, prompts, or response bodies.

Start it in a terminal:

```powershell
npm run start:opencode
```

Or register the included hidden, per-user scheduled task so it starts at Windows logon:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\register-opencode-pool-task.ps1
```

OpenCode provider settings:

```json
{
  "npm": "@ai-sdk/openai-compatible",
  "name": "SenseNova Pool",
  "options": {
    "baseURL": "http://127.0.0.1:18787/v1",
    "apiKey": "local-sensenova-pool"
  }
}
```

For WorkBuddy, use `http://127.0.0.1:18787/v1/chat/completions` with API key `local-sensenova-pool`.

Health information is available locally at `http://127.0.0.1:18787/health`. It contains account numbers and cooldown state, but no credentials.

Optional environment variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `SENSENOVA_POOL_PORT` | `18787` | Local listening port |
| `SENSENOVA_POOL_LOCAL_TOKEN` | `local-sensenova-pool` | Credential accepted from local clients |
| `SENSENOVA_POOL_MAX_QUEUE_MS` | `600000` | Maximum time a request may wait for a cooled-down account |
| `SENSENOVA_POOL_REQUEST_TIMEOUT_MS` | `300000` | Time allowed for upstream response headers |
| `SENSENOVA_POOL_KEY_FILE` | `sensenova_apikeys` | Alternate key-file path |
| `SENSENOVA_POOL_LOG_DIR` | `logs/openai-pool` | Alternate JSONL log directory |

## Usage

### 1. Start the proxy

```bash
node sensenova-proxy.js
```

Or in the background (Windows):

```cmd
start /B node sensenova-proxy.js
```

### 2. Configure your API keys

Copy the example file and fill in your keys:

```bash
cp sensenova_apikeys.example sensenova_apikeys
```

One key per line. Get keys from [platform.sensenova.cn](https://platform.sensenova.cn) → Token Plan.

### 3. Point Claude Code / Lucky to the proxy

**Environment variables:**

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:6790
ANTHROPIC_AUTH_TOKEN=sk-proxy       # any value works, proxy replaces it
ANTHROPIC_MODEL=deepseek-v4-flash
```

**~/.claude/settings.json (or ~/.lucky/settings.json):**

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:6790",
    "ANTHROPIC_AUTH_TOKEN": "sk-proxy",
    "ANTHROPIC_MODEL": "deepseek-v4-flash",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "deepseek-v4-flash",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "deepseek-v4-flash",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "deepseek-v4-flash"
  }
}
```

> **Note:** `ANTHROPIC_BASE_URL` must NOT include `/v1` — Claude Code automatically appends `/v1/messages`.

### 4. Run

```bash
claude
# or
lucky
```

## Available Models

| Model ID | Description |
|----------|-------------|
| `deepseek-v4-flash` | High-performance, 1M context, thinking mode |

The proxy passes the model name through to SenseNova as-is. Any model available on SenseNova's Anthropic endpoint will work.

## Files

| File | Purpose |
|------|---------|
| `sensenova-proxy.js` | Original Anthropic-compatible proxy server (port 6790) |
| `opencode-pool-proxy.js` | OpenAI-compatible failover gateway (port 18787) |
| `lib/openai-pool.js` | Key reload, scheduling, cooldown, retry, streaming, and logging logic |
| `scripts/register-opencode-pool-task.ps1` | Register/start the Windows logon task |
| `sensenova_apikeys` | **Your real API keys (gitignored)** |
| `sensenova_apikeys.example` | Example key file template |

## Requirements

- Node.js 18+
- SenseNova API key(s) from [platform.sensenova.cn](https://platform.sensenova.cn)

## License

MIT
