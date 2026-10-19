import fs from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { hash as blake3hash } from 'blake3-wasm';
import { navigationBranding, rechargeWithoutIcon, shareGPTBranding } from './branding.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, '../..');
const OUT = path.resolve(process.argv[2] || path.join(ROOT, '.workflow/artifacts/cloudflare-pages'));
const SITE = 'https://redeem.baochai.cc';
const files = new Map();
const originals = new Map();
const pending = new Set(['/', '/recharge', '/recharge.js', '/recharge.css', '/ios/recharge', '/lz/recharge', '/baochai-ui.css', '/baochai-ui.js', '/baochai-entry.js']);

function discover(text, from) {
  // Collect same-origin public HTML, module imports, Vite lazy chunks and CSS assets.
  const pattern = /["'(]((?:\/?(?:ios\/portal-assets|lz\/assets|portal-assets|assets)\/|\.\/|\/baochai-|\/(?:ios|lz)\/)[^"'()\s<>]+\.(?:js|css|png|svg|ico|woff2?)(?:\?[^"'()\s<>]*)?)["')]/g;
  for (const match of text.matchAll(pattern)) {
    let candidate = match[1];
    if (candidate.startsWith('portal-assets/')) candidate = '/ios/' + candidate;
    if (candidate.startsWith('assets/')) candidate = '/lz/' + candidate;
    const url = new URL(candidate, SITE + from);
    if (url.origin === SITE && !files.has(url.pathname)) pending.add(url.pathname);
  }
}

while (pending.size) {
  const pathname = pending.values().next().value;
  pending.delete(pathname);
  if (files.has(pathname)) continue;
  if (files.size >= 100) throw new Error('Unexpected public asset count');
  const response = await fetch(SITE + pathname, { redirect: 'error', signal: AbortSignal.timeout(30000) });
  if (!response.ok) throw new Error(`Public asset ${pathname}: HTTP ${response.status}`);
  const data = Buffer.from(await response.arrayBuffer());
  if (data.length > 3 * 1024 * 1024) throw new Error(`Unexpected asset size: ${pathname}`);
  originals.set(pathname, data);
  files.set(pathname, data);
  if (/text|javascript|json/.test(response.headers.get('content-type') || '')) discover(data.toString('utf8'), pathname);
}

const index = files.get('/').toString('utf8');
if (!index.includes('href="/recharge"') || !index.includes('</nav>')) throw new Error('Navigation layout changed: review before deployment');
files.set('/index.html', Buffer.from(navigationBranding(index)));
files.delete('/');
files.set('/recharge.html', Buffer.from(rechargeWithoutIcon(files.get('/recharge').toString('utf8')))); files.delete('/recharge');
for (const provider of ['ios', 'lz']) {
  files.set(`/${provider}/index.html`, Buffer.from(rechargeWithoutIcon(files.get(`/${provider}/recharge`).toString('utf8')))); files.delete(`/${provider}/recharge`);
}
let html = await fs.readFile(path.join(ROOT, 'web/public/user.html'), 'utf8');
html = html.replace('<div class="wordmark">', '<a class="nav-home" href="/">返回服务导航</a><div class="wordmark">');
files.set('/sharegpt/index.html', Buffer.from(shareGPTBranding(html)));
const js = await fs.readFile(path.join(ROOT, 'web/public/static/user.js'), 'utf8');
if (!js.includes('"/api/redeem"') || !js.includes('"/api/cards/query"')) throw new Error('ShareGPT frontend API paths changed');
let routedJS = js;
for (const endpoint of ['/api/redeem', '/api/cards/query', '/api/cards/action-preview', '/api/cards/renew', '/api/redeem-different']) {
  if (!routedJS.includes(JSON.stringify(endpoint))) throw new Error(`Missing public API endpoint: ${endpoint}`);
  routedJS = routedJS.replaceAll(JSON.stringify(endpoint), JSON.stringify('/sharegpt' + endpoint));
}
files.set('/sharegpt/static/user.js', Buffer.from(routedJS));
files.set('/sharegpt/static/user.css', Buffer.from((await fs.readFile(path.join(ROOT, 'web/public/static/user.css'), 'utf8')) + '\n.nav-home { color:inherit; font-size:13px; text-decoration:none; }\n.nav-home:hover { text-decoration:underline; }\n'));
files.set('/sharegpt/static/change-timezone-ip-settings.jpg', await fs.readFile(path.join(ROOT, 'web/public/static/change-timezone-ip-settings.jpg')));

const manifest = {};
const assets = [];
const contentTypes = { html: 'text/html; charset=utf-8', js: 'application/javascript; charset=utf-8', css: 'text/css; charset=utf-8', png: 'image/png', jpg: 'image/jpeg', svg: 'image/svg+xml', ico: 'image/x-icon', woff: 'font/woff', woff2: 'font/woff2' };
for (const [name, data] of files) {
  const extension = path.extname(name).slice(1);
  const key = blake3hash(data.toString('base64') + extension).toString('hex').slice(0, 32);
  manifest[name] = key;
  assets.push({ key, value: data.toString('base64'), base64: true, metadata: { contentType: contentTypes[extension] || 'application/octet-stream' } });
  await fs.mkdir(path.dirname(path.join(OUT, 'public', name)), { recursive: true });
  await fs.writeFile(path.join(OUT, 'public', name), data);
}
for (const [name, data] of originals) {
  const filename = name === '/' ? '/index.html' : name;
  await fs.mkdir(path.dirname(path.join(OUT, 'original-public', filename)), { recursive: true });
  await fs.writeFile(path.join(OUT, 'original-public', filename), data);
}
await fs.writeFile(path.join(OUT, 'manifest.json'), JSON.stringify(manifest, null, 2));
await fs.writeFile(path.join(OUT, 'upload-assets.json'), JSON.stringify(assets));
await fs.copyFile(path.join(HERE, 'worker.mjs'), path.join(OUT, '_worker.js'));
await fs.writeFile(path.join(OUT, '_routes.json'), JSON.stringify({ version: 1, include: ['/*'], exclude: [] }));
console.log(JSON.stringify({ out: OUT, assets: assets.length, bytes: [...files.values()].reduce((total, data) => total + data.length, 0), preservedPublicAssets: originals.size }));
