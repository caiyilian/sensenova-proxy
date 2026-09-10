# SenseNova Proxy

Lightweight local gateways for SenseNova and Agnes:

- `sensenova-proxy.js` keeps the original Anthropic Messages API endpoint for Claude Code/Lucky.
- `opencode-pool-proxy.js` exposes an OpenAI-compatible endpoint for OpenCode, WorkBuddy, and similar clients, with automatic failover and per-model cooldowns.
- `agnes-proxy.js` exposes an OpenAI-compatible Agnes endpoint with direct/Clash failover, guarded node recovery, and local rate-limit queuing.
- `desktop/sensenova-pool/` contains the standalone Windows tray application for sharing the SenseNova pool with users who do not have Node.js installed.
- `desktop/agnes-proxy/` contains a separate Windows tray controller that runs the tested Agnes gateway in the background without a terminal window.

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
- after one request has exhausted the whole pool, spaces later probes by a fixed 5 seconds instead of repeatedly bursting across every account;
- watches `sensenova_apikeys` and hot-reloads additions, removals, and replacements within about one second, without a restart;
- logs reload totals, affected line numbers, and short one-way key references to both the Node terminal and JSONL log, never the keys themselves;
- ignores malformed key lines and quarantines a key that receives an upstream 401/403 while other accounts continue working;
- keeps the last successfully loaded account list if the key file is temporarily missing, locked, or unreadable;
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

Edit `sensenova_apikeys` normally, one key per line. A successful change prints a message like `keys_reloaded total=7 added=1 removed=0 invalid=0`; the optional `addedRefs`/`removedRefs` values are only the first eight characters of a SHA-256 fingerprint. Invalid lines are reported by line number without printing their contents. A same-length typo can only be detected when SenseNova rejects that account; it is then logged as `account_quarantined` and the request moves to another account.

Optional environment variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `SENSENOVA_POOL_PORT` | `18787` | Local listening port |
| `SENSENOVA_POOL_HOST` | `127.0.0.1` | Listening interface; use a LAN address only with firewall controls |
| `SENSENOVA_POOL_LOCAL_TOKEN` | `local-sensenova-pool` | Credential accepted from local clients |
| `SENSENOVA_POOL_MAX_QUEUE_MS` | `600000` | Maximum time a request may wait for a cooled-down account |
| `SENSENOVA_POOL_PROBE_INTERVAL_MS` | `5000` | Fixed spacing between probes after a full-pool failure |
| `SENSENOVA_POOL_REQUEST_TIMEOUT_MS` | `300000` | Time allowed for upstream response headers |
| `SENSENOVA_POOL_EXPECTED_KEY_LENGTH` | `35` | Expected SenseNova key length; use `0` to disable only the length check |
| `SENSENOVA_POOL_KEY_WATCH_MS` | `1000` | Key-file polling interval; use `0` to disable background watching |
| `SENSENOVA_POOL_KEY_DEBOUNCE_MS` | `250` | Delay before loading a detected file change |
| `SENSENOVA_POOL_KEY_FILE` | `sensenova_apikeys` | Alternate key-file path |
| `SENSENOVA_POOL_LOG_DIR` | `logs/openai-pool` | Alternate JSONL log directory |

## Agnes resilient gateway

The Agnes gateway listens on `127.0.0.1:18788`. It preserves the original OpenAI-compatible request and response format for these models:

- `agnes-2.0-flash`
- `agnes-2.5-flash`
- `agnes-3.0-flash`

Its routing policy is deliberately conservative:

1. Use a direct connection while it is healthy.
2. On a definite DNS/connect failure, retry through the local Clash HTTP proxy at `127.0.0.1:7890`.
3. If Clash also cannot connect, run `../android-install/tools/clash-node-helper.ps1` once to select a healthy node, then retry through Clash.
4. Do not change Clash nodes for an Agnes TPM/RPM response. Queue that model locally for 60 seconds (or the server's longer `Retry-After`) and let one request probe first, which avoids a retry stampede.
5. Do not automatically resend an ambiguous connection failure such as a socket reset after transmission, because Agnes may already be processing it.

The real upstream key is read only from `AGNES_API_KEY`. Clients send the unrelated local token `local-agnes-proxy`, so the upstream key is not stored in OpenCode configuration, task arguments, source code, or logs.

Install and start it:

```powershell
npm install
npm run start:agnes
```

Or register the hidden Windows logon task:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\register-agnes-proxy-task.ps1
```

OpenCode provider settings:

```json
{
  "npm": "@ai-sdk/openai-compatible",
  "name": "Agnes Resilient",
  "options": {
    "baseURL": "http://127.0.0.1:18788/v1",
    "apiKey": "local-agnes-proxy"
  }
}
```

Health information is at `http://127.0.0.1:18788/health`. Metadata-only JSONL logs are written to `logs/agnes-proxy/`; request bodies, responses, and credentials are never logged.

Optional environment variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `AGNES_PROXY_PORT` | `18788` | Listening port |
| `AGNES_PROXY_HOST` | `127.0.0.1` | Listening interface; use a LAN address only with firewall controls |
| `AGNES_PROXY_LOCAL_TOKEN` | `local-agnes-proxy` | Credential accepted from local clients |
| `AGNES_PROXY_CLASH_URL` | `http://127.0.0.1:7890` | Clash HTTP proxy URL |
| `AGNES_CLASH_RECOVERY_SCRIPT` | sibling `android-install` helper | Alternate node-recovery script path |
| `AGNES_PROXY_MAX_QUEUE_MS` | `600000` | Maximum local wait for rate limits |
| `AGNES_PROXY_RATE_COOLDOWN_MS` | `60000` | Minimum fixed rate-limit cooldown |
| `AGNES_PROXY_LOG_DIR` | `logs/agnes-proxy` | Alternate JSONL log directory |

## LAN deployment

Both OpenAI-compatible gateways can run on an always-on LAN server. Set `SENSENOVA_POOL_HOST` and `AGNES_PROXY_HOST` to the server's LAN address (or `0.0.0.0`), replace the default local tokens with strong random values, and allow the ports only from trusted client IP addresses in the server firewall. Then point clients to `http://SERVER_IP:18787/v1` and `http://SERVER_IP:18788/v1`.

Keep `sensenova_apikeys` and `AGNES_API_KEY` only on the server. The Agnes Clash fallback and automatic node switch must also run on that server: install Clash Verge/mihomo there and place the `android-install` repository beside this one, or set `AGNES_CLASH_RECOVERY_SCRIPT` explicitly. Plain HTTP exposes prompts and responses to anyone able to observe the LAN; use a trusted LAN, a private overlay such as WireGuard/Tailscale, or a TLS reverse proxy when that matters.

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
| `agnes-proxy.js` | Agnes direct/Clash resilient gateway (port 18788) |
| `lib/agnes-gateway.js` | Agnes routing, guarded retry, streaming, and safe logging logic |
| `lib/clash-recovery.js` | Bounded invocation of the existing Clash node helper |
| `scripts/register-agnes-proxy-task.ps1` | Register/start the Agnes Windows logon task |
| `sensenova_apikeys` | **Your real API keys (gitignored)** |
| `sensenova_apikeys.example` | Example key file template |

## Requirements

- Node.js 18.17+
- SenseNova API key(s) from [platform.sensenova.cn](https://platform.sensenova.cn)

## License

MIT
