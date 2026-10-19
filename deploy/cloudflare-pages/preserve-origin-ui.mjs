// Keep the server's currently served public UI unchanged. The new canonical
// frontend under web/public is published only to Cloudflare Pages.
import fs from 'node:fs/promises';
import path from 'node:path';
const root = new URL('../../', import.meta.url);
const snapshot = new URL('.workflow/artifacts/card-actions-origin-ui/', root);
for (const [pathname, filename] of [['/', 'user.html'], ['/static/user.css', 'static/user.css'], ['/static/user.js', 'static/user.js']]) {
  const response = await fetch('https://gpt.baochai.cc' + pathname, { redirect: 'error', signal: AbortSignal.timeout(20000) });
  if (!response.ok) throw new Error(`Origin UI HTTP ${response.status}`);
  const body = await response.text();
  if (body.includes('card-action-form') || body.includes('action-preview-button')) throw new Error('Origin UI already changed: review snapshot before overwriting');
  const target = new URL(filename, snapshot);
  await fs.mkdir(path.dirname(target.pathname), { recursive: true });
  await fs.writeFile(target, body);
  for (const prefix of ['internal/unifiedui/static/', 'allocation-service/internal/httpapi/static/']) {
    const embeddedFilename = prefix.startsWith('allocation-service/') ? filename.replace(/^static\//, '') : filename;
    const destination = new URL(embeddedFilename, new URL(prefix, root));
    await fs.mkdir(path.dirname(destination.pathname), { recursive: true });
    await fs.copyFile(target, destination);
  }
}
console.log(JSON.stringify({ originUI: 'preserved', frontendTarget: 'Cloudflare Pages only' }));
