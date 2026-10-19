import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import http from 'node:http';
import fs from 'node:fs/promises';
const require = createRequire(new URL('../../web/package.json', import.meta.url));
const { chromium } = require('@playwright/test');
let server;
let base = process.argv[2];
if (!base) {
  const root = new URL('../../web/public/', import.meta.url);
  server = http.createServer(async (request, response) => {
    const pathname = new URL(request.url, 'http://localhost').pathname;
    const filename = pathname === '/' ? 'user.html' : ({ '/static/user.js': 'static/user.js', '/static/user.css': 'static/user.css' })[pathname];
    if (!filename) { response.writeHead(404); response.end(); return; }
    response.setHeader('Content-Type', filename.endsWith('.js') ? 'application/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html');
    response.end(await fs.readFile(new URL(filename, root)));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  base = `http://127.0.0.1:${server.address().port}`;
} else if (!/^https:\/\/[a-z0-9.-]+(?:\/sharegpt\/?)?$/.test(base)) throw new Error('Explicit HTTPS frontend URL required');
const browser = await chromium.launch({ headless: true });
const page = await browser.newPage();
const errors = [];
page.on('pageerror', error => errors.push(error.message));
const original = '2345-6789-ABCD';
const next = 'EFGH-JKMN-PQRS';
const third = 'TUVA-WXYZ-2345';
let normalRedemptions = 0;
let queries = 0;
let previews = 0;
let confirmations = 0;
let mode = 'unused';
let expired = false;
let capacityFailure = false;
let challenge = false;
let responseLost = false;
const expiry = new Date(Date.now() + 86400000 * 120).toISOString();
const result = action => ({ operation: { action, mode: action === 'different' ? 'different' : mode, added_days: 30, duration_days: 120, status: action === 'renew' && mode === 'unused' ? 'unused' : 'redeemed', expires_at: action === 'renew' && mode === 'unused' ? null : expiry, account_changed: expired, replayed: false } });
try {
  // Every card API is intercepted. No real code or transaction reaches a backend.
  await page.route('**/api/**', async route => {
    const pathname = new URL(route.request().url()).pathname.replace('/sharegpt', '');
    const payload = route.request().postDataJSON();
    if (pathname === '/api/redeem') { normalRedemptions++; throw new Error('Card actions must never auto-redeem ordinary codes'); }
    if (pathname === '/api/cards/query') {
      queries++;
      assert.equal(payload.code, mode === 'different' ? next : original);
      return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ result: { card: { valid_until: expiry }, account: { display_username: 'synthetic@example.invalid', password: 'synthetic-only-password', pickup_address: '' }, replacement_notice: { state: 'normal' } } }) });
    }
    assert.equal(payload.original_code, original);
    assert.equal(payload.new_code, next);
    if (pathname === '/api/cards/action-preview') {
      previews++;
      if (challenge && !payload.captcha_answer) return route.fulfill({ status: 403, contentType: 'application/json', body: JSON.stringify({ code: 'captcha_required', message: '需要完成验证码后继续', captcha: { id: 55, question: '2 + 3 = ?' } }) });
      if (challenge) { assert.equal(payload.captcha_id, 55); assert.equal(payload.captcha_answer, '5'); challenge = false; }
      if (capacityFailure) return route.fulfill({ status: 409, contentType: 'application/json', body: JSON.stringify({ error: { code: 'no_account_capacity', message: '暂无不同账号可分配，新兑换码未消耗' } }) });
      return route.fulfill({ contentType: 'application/json', body: JSON.stringify(result(payload.action)) });
    }
    assert.ok(['/api/cards/renew', '/api/redeem-different'].includes(pathname));
    confirmations++;
    if (responseLost) { responseLost = false; return route.abort('failed'); }
    return route.fulfill({ contentType: 'application/json', body: JSON.stringify(result(payload.action)) });
  });
  await page.goto(base, { waitUntil: 'networkidle' });
  await page.locator('button[data-mode="renew"]').click();
  await page.locator('#original-card-code').fill(original);
  await page.locator('#new-card-code').fill(next);
  assert.equal(await page.locator('#action-confirm-button').isDisabled(), true);
  await page.locator('#action-preview-button').click();
  await page.locator('#action-preview:not([hidden])').waitFor();
  assert.match(await page.locator('#action-preview').textContent(), /120 天.*仍未激活/);
  assert.equal(confirmations, 0);
  await page.locator('#new-card-code').fill(third);
  assert.equal(await page.locator('#action-confirm-button').isDisabled(), true);
  await page.locator('#new-card-code').fill(next);
  await page.locator('#action-preview-button').click();
  await page.locator('#action-confirm-button:enabled').waitFor();
  await page.locator('#action-confirm-button').click();
  await page.waitForFunction(() => document.querySelector('#action-success').textContent.includes('尚未开始计时'));
  assert.equal(queries, 0);
  assert.equal(await page.evaluate(() => localStorage.getItem('vitals.redeemed-cards.v1')), null);
  mode = 'active';
  challenge = true;
  await page.locator('#action-preview-button').click();
  await page.locator('#action-captcha.visible').waitFor();
  await page.locator('#action-captcha-answer').fill('5');
  await page.locator('#action-preview-button').click();
  await page.locator('#action-confirm-button:enabled').waitFor();
  responseLost = true;
  await page.locator('#action-confirm-button').click();
  await page.waitForFunction(() => document.querySelector('#action-error').textContent.includes('相同操作不会重复续费'));
  await page.locator('#action-confirm-button:enabled').waitFor();
  await page.locator('#action-confirm-button').click();
  await page.locator('#result.visible').waitFor();
  assert.equal(await page.locator('#account').textContent(), 'synthetic@example.invalid');
  let saved = await page.evaluate(() => JSON.parse(localStorage.getItem('vitals.redeemed-cards.v1')));
  assert.deepEqual(saved.map(card => card.code), [original]);
  assert.equal(saved[0].valid_until, expiry);
  mode = 'reactivated'; expired = true;
  await page.locator('#saved-cards-list button[data-card-action="renew"]').click();
  await page.locator('#action-preview-button').click();
  await page.locator('#action-confirm-button:enabled').waitFor();
  assert.match(await page.locator('#action-preview').textContent(), /重新分配账号/);
  await page.locator('#action-confirm-button').click();
  await page.locator('#result.visible').waitFor();
  mode = 'different'; expired = false; capacityFailure = true;
  await page.locator('#saved-cards-list button[data-card-action="different"]').click();
  assert.equal(await page.locator('#original-card-code').inputValue(), original);
  await page.locator('#action-preview-button').click();
  await page.waitForFunction(() => document.querySelector('#action-error').textContent.includes('新兑换码未消耗'));
  assert.equal(await page.locator('#action-confirm-button').isDisabled(), true);
  capacityFailure = false;
  await page.locator('#action-preview-button').click();
  await page.locator('#action-confirm-button:enabled').waitFor();
  await page.locator('#action-confirm-button').click();
  await page.locator('#result.visible').waitFor();
  saved = await page.evaluate(() => JSON.parse(localStorage.getItem('vitals.redeemed-cards.v1')));
  assert.deepEqual(saved.map(card => card.code), [next, original]);
  await page.setViewportSize({ width: 375, height: 812 });
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
  await page.locator('button[data-mode="query"]').click();
  assert.equal(await page.locator('#query-panel').isVisible(), true);
  assert.deepEqual(errors, []);
  assert.equal(normalRedemptions, 0);
  await page.screenshot({ path: '/root/gptshare/.workflow/artifacts/card-actions-mobile.png', fullPage: true });
  console.log(JSON.stringify({ browser: 'passed', previews, confirmations, queries, checks: ['preview-only', 'edit-invalidates-preview', 'unused-merge', 'captcha', 'lost-response-retry', 'active-renew', 'expired-renew', 'no-capacity', 'different-account', 'local-history', 'mobile'], pageErrors: errors }));
} finally { await browser.close(); if (server) await new Promise(resolve => server.close(resolve)); }
