import test from 'node:test';
import assert from 'node:assert/strict';
import { navigationBranding, rechargeWithoutIcon, shareGPTBranding } from './branding.mjs';

test('renames navigation and existing entry without duplication', () => {
  const html = '<title>宝柴 · 服务导航</title><h1>服务导航</h1><nav aria-label="服务导航"><a href="/recharge">GPT充值</a><a href="/sharegpt/">ShareGPT 账号兑换</a></nav>';
  const result = navigationBranding(html);
  assert.ok(result.includes('<title>宝钗服务导航</title>'));
  assert.ok(result.includes('<h1>宝钗服务导航</h1>'));
  assert.ok(result.includes('GPT共享账号兑换'));
  assert.equal((result.match(/href="\/sharegpt\/"/g) || []).length, 1);
  assert.ok(result.includes('href="/recharge"'));
  assert.equal(navigationBranding(result), result);
});

test('creates correctly named entry when building the original navigation', () => {
  const result = navigationBranding('<title>宝柴 · 服务导航</title><nav><a href="/recharge">GPT充值</a></nav>');
  assert.ok(result.includes('<strong>GPT共享账号兑换</strong>'));
});

test('removes favicons and touch icons while preserving scripts, styles and page content', () => {
  const html = '<head><link rel="icon" href="/ios/lz-icon.png"><link href="/lz/lz-icon.png" rel="apple-touch-icon" /><link rel="stylesheet" href="/style.css"><script src="/app.js"></script></head><body>GPT充值</body>';
  const result = rechargeWithoutIcon(html);
  assert.ok(!result.includes('lz-icon.png'));
  assert.ok(!result.includes('apple-touch-icon'));
  assert.ok(result.includes('rel="icon" href="data:,"'));
  assert.ok(result.includes('href="/style.css"'));
  assert.ok(result.includes('src="/app.js"'));
  assert.ok(result.includes('<body>GPT充值</body>'));
  assert.equal(rechargeWithoutIcon(result), result);
});

test('child page uses the new name and retains card query controls', () => {
  const result = shareGPTBranding('<title>卡密查询</title><div class="wordmark">ChatGPT 账号服务</div><input id="card-code">');
  assert.ok(result.includes('<title>GPT共享账号兑换</title>'));
  assert.ok(result.includes('<div class="wordmark">GPT共享账号兑换</div>'));
  assert.ok(result.includes('id="card-code"'));
});
