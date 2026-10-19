export function navigationBranding(html) {
  let result = html.replaceAll('宝柴', '宝钗').replaceAll('ShareGPT 账号兑换', 'GPT共享账号兑换');
  result = result.replace(/<title>[^<]*<\/title>/, '<title>宝钗服务导航</title>');
  result = result.replace('<h1>服务导航</h1>', '<h1>宝钗服务导航</h1>');
  result = result.replace('aria-label="服务导航"', 'aria-label="宝钗服务导航"');
  if (!result.includes('href="/sharegpt/"')) {
    result = result.replace('</nav>', '<a href="/sharegpt/"><span><strong>GPT共享账号兑换</strong><small>兑换或查询卡密，查看账号信息和有效时间</small></span><span class="arrow" aria-hidden="true">→</span></a>\n    </nav>');
  }
  return result;
}

export function rechargeWithoutIcon(html) {
  // An empty icon also suppresses automatic /favicon.ico discovery and stale branding.
  const withoutIcons = html.replace(/<link\b(?=[^>]*\brel\s*=\s*["'][^"']*\b(?:icon|apple-touch-icon|mask-icon)\b[^"']*["'])[^>]*>\s*/gi, '');
  return withoutIcons.replace(/<\/head>/i, '<link rel="icon" href="data:,">\n</head>');
}

export function shareGPTBranding(html) {
  return html.replace(/<title>[^<]*<\/title>/, '<title>GPT共享账号兑换</title>')
    .replace('<div class="wordmark">ChatGPT 账号服务</div>', '<div class="wordmark">GPT共享账号兑换</div>');
}
