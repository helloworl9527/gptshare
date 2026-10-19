import test from 'node:test';
import assert from 'node:assert/strict';
import worker from './worker.mjs';

const base = 'https://redeem.baochai.cc';
const env = { ASSETS: { fetch: async request => new Response(new URL(request.url).pathname) }, REDEEM_PROXY: { fetch: async request => new Response('legacy:' + new URL(request.url).pathname) } };
const post = (path, body = '{}', headers = {}) => new Request(base + path, { method: 'POST', headers: { 'Content-Type': 'application/json', ...headers }, body });

test('ShareGPT page alias, assets and legacy routes remain accessible', async () => {
  for (const [pathname, expected] of [['/sharegpt/', '/sharegpt/'], ['/sharegpt/static/user.js', '/sharegpt/static/user.js'], ['/recharge', '/recharge'], ['/ios/recharge', '/ios/'], ['/lz/recharge', '/lz/'], ['/lz/history', '/lz/'], ['/ios/subscription', '/ios/']]) {
    assert.equal(await (await worker.fetch(new Request(base + pathname), env)).text(), expected);
  }
  const redirect = await worker.fetch(new Request(base + '/sharegpt'), env);
  assert.equal(redirect.status, 308);
  assert.equal(redirect.headers.get('Location'), base + '/sharegpt/');
});

test('old provider API service binding is preserved', async () => {
  for (const pathname of ['/ios/api/query-key', '/lz/api/v1/announcement']) assert.equal(await (await worker.fetch(post(pathname), env)).text(), 'legacy:' + pathname);
});

test('FAQ image uses the static binding without opening arbitrary image paths or writes', async () => {
  const pathname = '/sharegpt/static/change-timezone-ip-settings.jpg';
  for (const method of ['GET', 'HEAD']) {
    const response = await worker.fetch(new Request(base + pathname, { method }), env);
    assert.equal(response.status, 200);
    assert.equal(await response.text(), pathname);
    assert.equal(response.headers.get('X-Content-Type-Options'), 'nosniff');
    assert.match(response.headers.get('Content-Security-Policy'), /img-src 'self' data:/);
  }
  assert.equal((await worker.fetch(post(pathname), env)).status, 405);
  for (const path of ['/sharegpt/static/private.jpg', '/sharegpt/static/other.png', '/sharegpt/static/monitor.db']) {
    assert.equal((await worker.fetch(new Request(base + path), env)).status, 404);
  }
});

test('rejects admin, arbitrary endpoints, wrong methods, query strings and cross-site requests', async () => {
  const cases = [[post('/sharegpt/api/admin'), 404], [post('/sharegpt/admin'), 404], [new Request(base + '/sharegpt/api/redeem'), 405], [post('/sharegpt/api/redeem?code=x'), 400], [post('/sharegpt/api/redeem', '{}', { Origin: 'https://evil.example' }), 403], [post('/sharegpt/api/redeem', '{}', { 'Sec-Fetch-Site': 'cross-site' }), 403], [post('/sharegpt/api/redeem', '{}', { 'Content-Type': 'text/plain' }), 415], [post('/sharegpt/api/redeem', 'x'.repeat(8193)), 413], [post('/sharegpt/api/redeem', '{'), 400]];
  for (const [request, status] of cases) assert.equal((await worker.fetch(request, env)).status, status);
});

test('fixed backend, no browser credentials forwarded, captcha body/status and no-store retained', async () => {
  const original = globalThis.fetch;
  try {
    globalThis.fetch = async (url, options) => {
      assert.equal(url, 'https://gpt.baochai.cc/api/cards/query');
      assert.equal(options.headers.get('Cookie'), null);
      assert.equal(options.headers.get('Authorization'), null);
      assert.equal(options.headers.get('X-Real-IP'), '203.0.113.8');
      assert.equal(new TextDecoder().decode(options.body), '{"card_code":"example","captcha_answer":"5"}');
      return Response.json({ captcha_required: true, captcha_question: '2+3' }, { status: 403, headers: { 'Set-Cookie': 'secret=x' } });
    };
    const response = await worker.fetch(post('/sharegpt/api/cards/query', '{"card_code":"example","captcha_answer":"5"}', { Cookie: 'admin=x', Authorization: 'Bearer secret', Origin: base, 'CF-Connecting-IP': '203.0.113.8', 'X-Real-IP': '198.51.100.5' }), env);
    assert.equal(response.status, 403);
    assert.equal(response.headers.get('Cache-Control'), 'no-store');
    assert.equal(response.headers.get('Set-Cookie'), null);
    assert.equal((await response.json()).captcha_required, true);
  } finally { globalThis.fetch = original; }
});

test('upstream redirects, non-JSON errors and outages do not expose responses', async () => {
  const original = globalThis.fetch;
  try {
    for (const value of [Response.redirect('https://example.com'), new Response('<html>internal</html>', { status: 502 }), null]) {
      globalThis.fetch = async () => { if (!value) throw new Error('private detail'); return value; };
      const response = await worker.fetch(post('/sharegpt/api/redeem'), env);
      assert.equal(response.status, 502);
      assert.equal(response.headers.get('Cache-Control'), 'no-store');
      assert.ok(!(await response.text()).includes('private detail'));
    }
  } finally { globalThis.fetch = original; }
});

test('new card actions use only whitelisted fixed-origin POST routes and preserve replay results', async () => {
  const original = globalThis.fetch;
  try {
    for (const path of ['/api/cards/action-preview', '/api/cards/renew', '/api/redeem-different']) {
      globalThis.fetch = async (url, options) => {
        assert.equal(url, 'https://gpt.baochai.cc' + path);
        assert.equal(options.method, 'POST');
        assert.equal(options.redirect, 'manual');
        assert.equal(options.headers.get('Authorization'), null);
        assert.deepEqual(JSON.parse(new TextDecoder().decode(options.body)), { action: 'renew', original_code: 'TEST-2345-6789', new_code: 'TEST-6789-2345' });
        return Response.json({ operation: { replayed: true } });
      };
      const response = await worker.fetch(post('/sharegpt' + path, JSON.stringify({ action: 'renew', original_code: 'TEST-2345-6789', new_code: 'TEST-6789-2345' })), env);
      assert.equal(response.status, 200);
      assert.equal(response.headers.get('Cache-Control'), 'no-store');
      assert.equal((await response.json()).operation.replayed, true);
      assert.equal((await worker.fetch(new Request(base + '/sharegpt' + path), env)).status, 405);
    }
  } finally { globalThis.fetch = original; }
});
