import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import { createRequire } from 'node:module';
const require = createRequire(new URL('../../web/package.json', import.meta.url));
const { chromium } = require('@playwright/test');
const base = process.argv[2];
if (!/^https:\/\/[a-z0-9.-]+$/.test(base || '')) throw new Error('Explicit HTTPS origin required');
const browser = await chromium.launch({ headless: true });
const page = await browser.newPage();
const errors = [];
page.on('pageerror', error => errors.push(error.message));
page.on('response', response => { if (response.status() >= 400 && /\.(js|css)(\?|$)/.test(response.url())) errors.push(response.status() + ' ' + response.url()); });
try {
  await page.goto(base + '/', { waitUntil: 'networkidle' });
  assert.equal(await page.title(), '宝钗服务导航');
  assert.equal(await page.locator('h1').textContent(), '宝钗服务导航');
  await page.getByRole('link', { name: /GPT共享账号兑换/ }).click();
  await page.waitForURL(base + '/sharegpt/');
  await page.locator('#card-code').waitFor();
  assert.equal(await page.title(), 'GPT共享账号兑换');
  const purchase = page.locator('.topbar').getByRole('link', { name: '自助购买' });
  assert.equal(await purchase.getAttribute('href'), 'https://wzyp.cn/item/l2c6m1');
  assert.equal(await purchase.getAttribute('rel'), 'noopener noreferrer');
  await page.setViewportSize({ width: 375, height: 812 });
  assert.equal(await purchase.isVisible(), true);
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
  await page.setViewportSize({ width: 1280, height: 800 });
  assert.ok((await page.locator('#clock').textContent()).length > 0);
  await page.route('**/sharegpt/api/redeem', route => route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ error: { code: 'card_not_found' } }) }));
  let queried = 0;
  await page.route('**/sharegpt/api/cards/query', async route => {
    const payload = route.request().postDataJSON();
    queried++;
    assert.equal(payload.code, 'TEST-2345-6789');
    if (!payload.captcha_answer) {
      await route.fulfill({ status: 403, contentType: 'application/json', body: JSON.stringify({ code: 'captcha_required', captcha: { id: 123, question: '2 + 3 = ?' } }) });
    } else {
      assert.equal(payload.captcha_id, 123);
      assert.equal(payload.captcha_answer, '5');
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ result: { card: { valid_until: new Date(Date.now() + 86400000).toISOString() }, account: { display_username: 'test@example.invalid', password: 'synthetic-test-password', pickup_address: '' }, replacement_notice: { state: 'normal' } } }) });
    }
  });
  await page.locator('#card-code').fill('TEST23456789');
  await page.locator('#query-button').click();
  await page.locator('#captcha-box.visible').waitFor();
  assert.equal(await page.locator('#captcha-question').textContent(), '2 + 3 = ?');
  await page.locator('#captcha-answer').fill('5');
  await page.locator('#query-button').click();
  await page.locator('#result.visible').waitFor();
  assert.equal(await page.locator('#account').textContent(), 'test@example.invalid');
  assert.equal(await page.locator('#password').textContent(), 'synthetic-test-password');
  assert.equal(queried, 2);
  await page.getByRole('link', { name: '返回服务导航' }).click();
  for (const provider of ['ios', 'lz']) {
    await page.goto(base + `/${provider}/recharge`, { waitUntil: 'domcontentloaded' });
    await page.locator('input.exchange-input.mono[type="text"]').first().waitFor();
    assert.equal(await page.locator('link[rel="icon"]').getAttribute('href'), 'data:,');
    assert.equal(await page.locator('link[rel="apple-touch-icon"]').count(), 0);
  }
  // Prefix routing tests use synthetic codes; navigation only, never submit to provider APIs.
  for (const [code, provider] of [['LZ-TEST2345', 'ios'], ['PLUS-PP-TEST-2345-6789', 'lz']]) {
    await page.route(`**/${provider}/api/**`, route => route.fulfill({ status: 200, contentType: 'application/json', body: '{}' }));
    await page.goto(base + '/recharge');
    assert.equal(await page.locator('link[rel="icon"]').getAttribute('href'), 'data:,');
    await page.locator('#redeem-code').fill(code);
    await page.locator('button[type="submit"]').click();
    await page.waitForURL(base + `/${provider}/recharge`);
    await page.waitForFunction(expected => document.querySelector('input.exchange-input.mono[type="text"]')?.value === expected, code);
  }
  assert.deepEqual(errors, []);
  await fs.mkdir('/root/gptshare/.workflow/artifacts/cloudflare-pages-20261013', { recursive: true });
  await page.goto(base + '/sharegpt/');
  await page.screenshot({ path: '/root/gptshare/.workflow/artifacts/cloudflare-pages-20261013/sharegpt-preview.png', fullPage: true });
  console.log(JSON.stringify({ browser: 'passed', checks: ['navigation', 'sharegpt', 'mocked-captcha', 'mocked-result', 'existing-provider-pages', 'code-prefix-routing'], pageErrors: errors }));
} finally { await browser.close(); }
