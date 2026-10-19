import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';

const html = await fs.readFile(new URL('../../web/public/user.html', import.meta.url), 'utf8');
const css = await fs.readFile(new URL('../../web/public/static/user.css', import.meta.url), 'utf8');

test('FAQ remains accessible before redemption and preserves the existing card controls', () => {
  assert.match(html, /<section id="faq" class="panel faq" aria-labelledby="faq-title">/);
  assert.match(html, /<h2 id="faq-title">常见问题<\/h2>/);
  assert.match(html, /<details class="faq-item" open>/);
  assert.match(html, /<summary>出现「model not found」怎么办？<\/summary>/);
  assert.ok(html.indexOf('id="faq"') > html.indexOf('id="result"'));
  assert.ok(html.indexOf('id="faq"') < html.indexOf('</main>'));
  for (const id of ['query-form', 'card-code', 'card-action-form', 'saved-cards', 'result']) {
    assert.match(html, new RegExp(`id="${id}"`));
  }
  for (const removed of ['可以尝试检查浏览器时区是否与当前代理', '时区设置只是排查方向之一', '插件为第三方工具']) {
    assert.ok(!html.includes(removed));
  }
  assert.match(html, /现在 OpenAI 增加了时区的检测，假如浏览器时区和梯子时区不一致会出现 model not found。/);
});

test('FAQ includes the supplied extension, accurate settings instructions and responsive local image', async () => {
  const url = html.match(/<a href="(https:\/\/chromewebstore\.google\.com[^\"]+)"[^>]+>/);
  assert.ok(url);
  assert.ok(url[1].includes('/cejckpjfigehbeecbbfhangbknpidcnh?'));
  assert.match(url[0], /target="_blank" rel="noopener noreferrer"/);
  assert.match(html, /Automatically update timezone based on my IP address/);
  assert.match(html, /「Save」保存/);
  assert.match(html, /<img src="\.\/static\/change-timezone-ip-settings\.jpg" width="1162" height="1076" loading="lazy" decoding="async" alt="[^"]+">/);
  assert.match(css, /\.faq-figure img\s*\{[^}]*width: 100%; height: auto;/);
  const image = await fs.readFile(new URL('../../web/public/static/change-timezone-ip-settings.jpg', import.meta.url));
  assert.equal(image.readUInt16BE(0), 0xffd8);
  assert.ok(image.length > 1000 && image.length < 200000);
});
