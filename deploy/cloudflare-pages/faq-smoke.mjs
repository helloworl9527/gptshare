import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
import { createRequire } from 'node:module';
import worker from './worker.mjs';
import { shareGPTBranding } from './branding.mjs';

const require = createRequire(new URL('../../web/package.json', import.meta.url));
const { chromium } = require('@playwright/test');
const root = new URL('../../web/public/', import.meta.url);
let server;
let base = process.argv[2];
if (!base) {
  const files = new Map([
    ['/sharegpt/', ['user.html', 'text/html; charset=utf-8']],
    ['/sharegpt/static/user.css', ['static/user.css', 'text/css; charset=utf-8']],
    ['/sharegpt/static/user.js', ['static/user.js', 'application/javascript; charset=utf-8']],
    ['/sharegpt/static/change-timezone-ip-settings.jpg', ['static/change-timezone-ip-settings.jpg', 'image/jpeg']],
  ]);
  const env = { ASSETS: { fetch: async request => {
    const entry = files.get(new URL(request.url).pathname);
    if (!entry) return new Response('Not found', { status: 404 });
    const [filename, contentType] = entry;
    let data = await fs.readFile(new URL(filename, root));
    if (filename === 'user.html') data = shareGPTBranding(data.toString('utf8'));
    return new Response(request.method === 'HEAD' ? null : data, { headers: { 'Content-Type': contentType } });
  } } };
  server = http.createServer(async (request, response) => {
    try {
      const url = `http://127.0.0.1:${server.address().port}${request.url}`;
      const result = await worker.fetch(new Request(url, { method: request.method }), env);
      response.writeHead(result.status, Object.fromEntries(result.headers));
      response.end(Buffer.from(await result.arrayBuffer()));
    } catch {
      response.writeHead(500);
      response.end('Local test server error');
    }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  base = `http://127.0.0.1:${server.address().port}/sharegpt/`;
} else if (!/^https:\/\/[a-z0-9.-]+\/sharegpt\/$/.test(base)) {
  throw new Error('Explicit HTTPS /sharegpt/ frontend URL required');
}

let browser;
try {
  browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const errors = [];
  const unexpectedRequests = [];
  let analyticsRequests = 0;
  page.on('pageerror', error => errors.push(error.message));
  page.on('request', request => {
    const url = new URL(request.url());
    // Cloudflare may inject its existing zone analytics script on the custom
    // domain, but not on pages.dev previews. It is unrelated to FAQ assets.
    if (url.origin === 'https://static.cloudflareinsights.com' && /^\/beacon\.min\.js(?:\/|$)/.test(url.pathname)) {
      analyticsRequests++;
      return;
    }
    if (url.origin !== new URL(base).origin || /\/api\//.test(url.pathname)) unexpectedRequests.push(url.pathname);
  });
  page.on('response', response => {
    if (response.status() >= 400 && /\.(js|css|jpg)(\?|$)/.test(response.url())) errors.push(`${response.status()} ${new URL(response.url()).pathname}`);
  });
  await page.goto(base, { waitUntil: 'networkidle' });
  assert.equal(await page.title(), 'GPT共享账号兑换');
  assert.equal(await page.locator('#query-form').isVisible(), true);
  assert.equal(await page.locator('#faq').isVisible(), true);
  assert.equal(await page.getByRole('heading', { name: '常见问题', exact: true }).count(), 1);
  const item = page.locator('#faq details');
  const summary = item.locator('summary');
  assert.equal(await item.getAttribute('open'), '');
  assert.match(await summary.textContent(), /model not found/);
  const faqText = await page.locator('#faq').textContent();
  assert.ok(faqText.includes('现在 OpenAI 增加了时区的检测，假如浏览器时区和梯子时区不一致会出现 model not found。'));
  for (const removed of ['可以尝试检查浏览器时区是否与当前代理', '时区设置只是排查方向之一', '插件为第三方工具']) {
    assert.ok(!faqText.includes(removed));
  }
  const extension = page.locator('#faq a[href^="https://chromewebstore.google.com/"]');
  assert.equal(await extension.count(), 1);
  assert.equal(await extension.getAttribute('target'), '_blank');
  assert.equal(await extension.getAttribute('rel'), 'noopener noreferrer');
  assert.ok((await extension.getAttribute('href')).includes('/cejckpjfigehbeecbbfhangbknpidcnh?'));
  const image = page.locator('#faq img');
  await image.scrollIntoViewIfNeeded();
  await page.waitForFunction(() => {
    const image = document.querySelector('#faq img');
    return image.complete && image.naturalWidth === 1162 && image.naturalHeight === 1076;
  });
  const imageResponse = await page.request.head(new URL(await image.getAttribute('src'), base).href);
  assert.equal(imageResponse.status(), 200);
  assert.match(imageResponse.headers()['content-type'], /^image\/jpeg/);
  assert.equal(imageResponse.headers()['x-content-type-options'], 'nosniff');
  assert.ok(imageResponse.headers()['content-security-policy'].includes("img-src 'self' data:"));
  const out = new URL('../../.workflow/artifacts/sharegpt-faq-20261015/', import.meta.url);
  await fs.mkdir(out, { recursive: true });
  await page.screenshot({ path: path.join(out.pathname, 'desktop.png'), fullPage: true });
  await summary.click();
  assert.equal(await page.locator('#faq .faq-answer').isVisible(), false);
  await summary.focus();
  await page.keyboard.press('Enter');
  assert.equal(await page.locator('#faq .faq-answer').isVisible(), true);
  for (const width of [375, 320]) {
    await page.setViewportSize({ width, height: 812 });
    await image.scrollIntoViewIfNeeded();
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
    const bounds = await image.boundingBox();
    assert.ok(bounds.width > 0 && bounds.x >= 0 && bounds.x + bounds.width <= width);
    await page.locator('button[data-mode="renew"]').click();
    assert.equal(await page.locator('#action-panel').isVisible(), true);
    assert.equal(await page.locator('#faq').isVisible(), true);
    await page.locator('button[data-mode="query"]').click();
    assert.equal(await page.locator('#query-panel').isVisible(), true);
    await page.screenshot({ path: path.join(out.pathname, `mobile-${width}.png`), fullPage: true });
  }
  assert.deepEqual(errors, []);
  assert.deepEqual(unexpectedRequests, []);
  console.log(JSON.stringify({ browser: 'passed', checks: ['faq-before-redemption', 'native-collapse-and-keyboard', 'extension-link', 'local-jpeg-and-security-headers', 'mobile-375-and-320', 'existing-tabs', 'no-api-or-unexpected-third-party-requests'], analyticsRequests, pageErrors: errors }));
} finally {
  if (browser) await browser.close();
  if (server) await new Promise(resolve => server.close(resolve));
}
