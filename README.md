# SenseNova Proxy

A lightweight reverse proxy that round-robins multiple SenseNova API keys behind a single Anthropic Messages API endpoint — designed for [Claude Code](https://claude.ai/code), [Lucky](https://claude-zh.cn), and any tool that speaks the Anthropic API format.

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
| `sensenova-proxy.js` | The proxy server |
| `sensenova_apikeys` | **Your real API keys (gitignored)** |
| `sensenova_apikeys.example` | Example key file template |

## Requirements

- Node.js 18+
- SenseNova API key(s) from [platform.sensenova.cn](https://platform.sensenova.cn)

## License

MIT