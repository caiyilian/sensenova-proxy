/**
 * SenseNova Key Round-Robin Proxy
 * 
 * 单模型 deepseek-v4-flash，6 个 API Key 轮询，突破单 key 限频。
 * 
 * 用法：node sensenova-proxy.js
 * 监听：http://127.0.0.1:6790
 */

const http = require('http');
const https = require('https');
const fs = require('fs');
const path = require('path');

const PORT = 6790;
const HOST = 'token.sensenova.cn';

const KEYS = fs.readFileSync(path.join(__dirname, 'sensenova_apikeys'), 'utf-8')
  .trim().split('\n').map(k => k.trim()).filter(Boolean);
console.log(`Loaded ${KEYS.length} API keys`);

let idx = 0;
function nextKey() { const k = KEYS[idx]; idx = (idx + 1) % KEYS.length; return k; }

function proxy(method, path, headers, body, apiKey) {
  return new Promise((resolve, reject) => {
    const opts = {
      hostname: HOST, path, method,
      headers: { ...headers, 'Authorization': `Bearer ${apiKey}`, 'Host': HOST },
      timeout: 300000,
    };
    delete opts.headers['x-api-key'];
    delete opts.headers['authorization'];
    const req = https.request(opts, resolve);
    req.on('error', reject);
    req.on('timeout', () => { req.destroy(); reject(new Error('timeout')); });
    if (body) req.write(body);
    req.end();
  });
}

function pipe(res, dst) {
  return new Promise((resolve, reject) => {
    const c = []; res.on('data', d => c.push(d));
    res.on('end', () => { dst.writeHead(res.statusCode, res.headers); dst.end(Buffer.concat(c)); resolve(); });
    res.on('error', reject);
  });
}

function pipeStream(res, dst) {
  return new Promise((resolve, reject) => {
    dst.writeHead(res.statusCode, res.headers);
    res.on('data', d => dst.write(d));
    res.on('end', () => { dst.end(); resolve(); });
    res.on('error', reject);
  });
}

function log(method, path, status, key, note) {
  const k = key ? key.slice(0, 10) + '...' : '-';
  console.log(`[${new Date().toISOString().slice(11,19)}] ${method} ${path} → ${status} key:${k}${note ? ' '+note : ''}`);
}

http.createServer((req, res) => {
  res.setHeader('Access-Control-Allow-Origin', '*');
  res.setHeader('Access-Control-Allow-Methods', 'GET, POST, OPTIONS');
  res.setHeader('Access-Control-Allow-Headers', 'Content-Type, Authorization, x-api-key, anthropic-version');
  if (req.method === 'OPTIONS') { res.writeHead(204); res.end(); return; }

  const url = new URL(req.url, `http://${req.headers.host}`);

  if (req.method === 'GET' && url.pathname === '/v1/models') {
    log('GET', '/v1/models', 200, '');
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ object: "list", data: [{ id: "deepseek-v4-flash", object: "model" }] }));
    return;
  }

  const chunks = [];
  req.on('data', c => chunks.push(c));
  req.on('end', async () => {
    const body = Buffer.concat(chunks);
    const apiKey = nextKey();
    try {
      const pr = await proxy(req.method, url.pathname + url.search, req.headers, body, apiKey);
      if (pr.headers['content-type']?.includes('text/event-stream')) {
        await pipeStream(pr, res);
        log(req.method, url.pathname, pr.statusCode, apiKey, 'stream');
      } else {
        await pipe(pr, res);
        log(req.method, url.pathname, pr.statusCode, apiKey, '');
      }
    } catch (e) {
      log(req.method, url.pathname, 502, apiKey, e.message);
      res.writeHead(502, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ type: "error", error: { type: "proxy_error", message: e.message } }));
    }
  });
}).listen(PORT, '127.0.0.1', () => {
  console.log(`SenseNova Proxy → http://127.0.0.1:${PORT}`);
  console.log(`ANTHROPIC_BASE_URL=http://127.0.0.1:${PORT}`);
});