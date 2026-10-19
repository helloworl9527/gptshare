const UPSTREAM = 'https://gpt.baochai.cc';
const API_ROUTES = new Set(['/sharegpt/api/redeem', '/sharegpt/api/cards/query', '/sharegpt/api/cards/action-preview', '/sharegpt/api/cards/renew', '/sharegpt/api/redeem-different']);
const SHAREGPT_ASSETS = new Set(['/sharegpt/', '/sharegpt/static/user.css', '/sharegpt/static/user.js', '/sharegpt/static/change-timezone-ip-settings.jpg']);
const SECURITY = {
  'Cache-Control': 'no-store',
  'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'",
  'X-Content-Type-Options': 'nosniff',
  'X-Frame-Options': 'DENY',
  'Referrer-Policy': 'no-referrer',
};

function secure(response) {
  const headers = new Headers(response.headers);
  for (const [key, value] of Object.entries(SECURITY)) headers.set(key, value);
  headers.delete('set-cookie');
  return new Response(response.body, { status: response.status, headers });
}

function error(status, message) {
  return secure(Response.json({ error: message }, { status }));
}

async function proxyShareGPT(request, url) {
  if (!API_ROUTES.has(url.pathname)) return error(404, '接口不存在');
  if (request.method !== 'POST') return error(405, '请使用 POST 请求');
  if (url.search) return error(400, '请勿在网址中传入卡密');
  const origin = request.headers.get('Origin');
  if (origin && origin !== url.origin) return error(403, '请求来源不允许');
  if (request.headers.get('Sec-Fetch-Site') === 'cross-site') return error(403, '请求来源不允许');
  if (!/^application\/json(?:\s*;|$)/i.test(request.headers.get('Content-Type') || '')) return error(415, '请提交 JSON 格式');
  if (Number(request.headers.get('Content-Length')) > 8192) return error(413, '请求内容过长');
  const chunks = [];
  let bytes = 0;
  if (request.body) {
    const reader = request.body.getReader();
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        bytes += value.byteLength;
        if (bytes > 8192) {
          await reader.cancel();
          return error(413, '请求内容过长');
        }
        chunks.push(value);
      }
    } finally { reader.releaseLock(); }
  }
  const body = new Uint8Array(bytes);
  let offset = 0;
  for (const chunk of chunks) { body.set(chunk, offset); offset += chunk.byteLength; }
  try { JSON.parse(new TextDecoder().decode(body)); }
  catch { return error(400, '请求参数错误，请重新输入'); }
  const headers = new Headers({ 'Content-Type': 'application/json', Accept: 'application/json' });
  // Cloudflare supplies this header; never forward the browser's X-Real-IP.
  // Same-zone subrequests use X-Real-IP to preserve the visitor's address.
  const visitorIP = request.headers.get('CF-Connecting-IP');
  if (visitorIP && visitorIP.length <= 45 && /^[0-9a-f:.]+$/i.test(visitorIP)) headers.set('X-Real-IP', visitorIP);
  try {
    const upstream = await fetch(UPSTREAM + url.pathname.slice('/sharegpt'.length), {
      method: 'POST', headers, body, redirect: 'manual', signal: AbortSignal.timeout(20000),
    });
    if (upstream.status >= 300 && upstream.status < 400) return error(502, '兑换服务暂时不可用，请稍后重试');
    if (!upstream.headers.get('Content-Type')?.includes('application/json')) return error(502, '兑换服务响应异常，请稍后重试');
    return secure(upstream);
  } catch { return error(502, '暂时无法连接兑换服务，请稍后重试'); }
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === '/sharegpt') return secure(Response.redirect(url.origin + '/sharegpt/', 308));
    if (url.pathname.startsWith('/sharegpt/api/')) return proxyShareGPT(request, url);
    if (url.pathname.startsWith('/sharegpt/')) {
      if (!SHAREGPT_ASSETS.has(url.pathname)) return error(404, '页面不存在');
      if (!['GET', 'HEAD'].includes(request.method)) return error(405, '请求方法不允许');
      return secure(await env.ASSETS.fetch(new Request(url, request)));
    }
    // Preserve the existing two recharge providers through the existing service binding.
    if (url.pathname.startsWith('/ios/api/') || url.pathname.startsWith('/lz/api/')) {
      if (!env.REDEEM_PROXY) return error(503, '充值服务暂时不可用');
      return env.REDEEM_PROXY.fetch(request);
    }
    if (!['GET', 'HEAD'].includes(request.method)) return new Response('Not found', { status: 404 });
    if (url.pathname === '/recharge/') url.pathname = '/recharge';
    for (const provider of ['ios', 'lz']) {
      const route = url.pathname.slice(provider.length + 2);
      if (url.pathname.startsWith(`/${provider}/`) && ['recharge', 'history', 'subscription', 'lookup', 'query', 'agent-exchange', 'cancel'].includes(route)) url.pathname = `/${provider}/`;
    }
    return env.ASSETS.fetch(new Request(url, request));
  },
};
