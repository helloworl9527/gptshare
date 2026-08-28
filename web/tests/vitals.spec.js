import { expect, test } from '@playwright/test'
import { createHmac } from 'node:crypto'
import path from 'node:path'

const screenshots = path.resolve('..', '.workflow', 'screenshots')

function totpAt(secret, timestamp) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  const clean = secret.toUpperCase().replace(/=+$/, '').replace(/\s+/g, '')
  let bits = ''
  for (const character of clean) bits += alphabet.indexOf(character).toString(2).padStart(5, '0')
  const key = Buffer.from(Array.from({ length: Math.floor(bits.length / 8) }, (_, index) => Number.parseInt(bits.slice(index * 8, index * 8 + 8), 2)))
  const counter = Math.floor(timestamp / 1000 / 30)
  const message = Buffer.alloc(8)
  message.writeUInt32BE(Math.floor(counter / 0x100000000), 0)
  message.writeUInt32BE(counter >>> 0, 4)
  const digest = createHmac('sha1', key).update(message).digest()
  const offset = digest[digest.length - 1] & 0x0f
  const binary = digest.readUInt32BE(offset) & 0x7fffffff
  return String(binary % 1_000_000).padStart(6, '0')
}

async function scenario(request, name, reset = false) {
  const response = await request.post('http://127.0.0.1:4174/__test/scenario', { data: { name, reset } })
  expect(response.ok()).toBeTruthy()
}

async function login(page) {
  await page.goto('/admin/login')
  await page.waitForLoadState('networkidle')
  await page.getByLabel('管理员账号').fill('admin')
  await page.getByLabel('密码').fill('controlled-test-password')
  await page.getByRole('button', { name: '验证密码' }).click()
  await page.getByLabel('动态验证码').fill('123456')
  await page.getByRole('button', { name: '完成登录' }).click()
  await expect(page.getByRole('heading', { name: '运营总览' })).toBeVisible()
}

test.beforeEach(async ({ request }) => { await scenario(request, 'default', true) })

test('unified admin covers both domains, credential completion and reveal', async ({ page }, testInfo) => {
  const consoleErrors = []
  page.on('console', (message) => { if (message.type() === 'error') consoleErrors.push(message.text()) })
  await login(page)
  await expect(page.locator('.domain-panel')).toHaveCount(2)
  for (const name of ['运营总览', '账号体征', '账号池', '卡密', '分配记录', '配置']) await expect(page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name, exact: true })).toBeVisible()
  await page.screenshot({ path: path.join(screenshots, `mstep04-unified-${testInfo.project.name}.png`), fullPage: true })

  await page.getByRole('link', { name: '账号池', exact: true }).click()
  await expect(page.getByRole('heading', { name: '账号池' })).toBeVisible()
  await page.getByRole('button', { name: '从一期同步账号' }).click()
  await expect(page.getByText('pulled-sync@example.test')).toBeVisible()
  await expect(page.getByText('pending_credentials')).toBeVisible()
  await page.getByRole('row', { name: /pulled-sync@example.test/ }).getByRole('button', { name: '编辑' }).click()
  await page.getByLabel('替换密码').fill('completed-password')
  await page.getByLabel('替换 2FA Secret').fill('JBSWY3DPEHPK3PXP')
  await page.getByLabel('账号来源链接').fill('https://accounts.example.test/orders/42')
  await page.getByRole('dialog').getByRole('button', { name: '保存修改' }).click()
  await expect(page.getByText('账号已更新')).toBeVisible()
  await expect(page.getByRole('row', { name: /pulled-sync@example.test/ }).getByRole('link', { name: '打开来源' })).toHaveAttribute('href', 'https://accounts.example.test/orders/42')

  await page.getByRole('row', { name: /pulled-sync@example.test/ }).getByRole('button', { name: '编辑' }).click()
  const firstRevealResponse = page.waitForResponse((response) => response.url().includes('/credentials/reveal') && response.status() === 200)
  await page.getByRole('button', { name: '查看凭据' }).click()
  const firstReveal = await (await firstRevealResponse).json()
  await expect(page.getByLabel('密码', { exact: true })).toHaveAttribute('type', 'password')
  await expect(page.getByLabel('密码', { exact: true })).toHaveValue('completed-password')
  await expect(page.locator('#revealed-totp-secret')).toHaveText('JBSWY3DPEHPK3PXP')
  await expect(page.locator('#revealed-totp-code')).toHaveText(totpAt(firstReveal.totp.secret, Date.parse(firstReveal.server_time)))
  await page.getByLabel('账号来源链接').fill('https://accounts.example.test/orders/84')
  await page.getByRole('dialog').getByRole('button', { name: '保存修改' }).click()
  await expect(page.getByText('账号已更新')).toBeVisible()

  await page.getByRole('row', { name: /pulled-sync@example.test/ }).getByRole('button', { name: '编辑' }).click()
  const secondRevealResponse = page.waitForResponse((response) => response.url().includes('/credentials/reveal') && response.status() === 200)
  await page.getByRole('button', { name: '查看凭据' }).click()
  const secondReveal = await (await secondRevealResponse).json()
  await expect(page.getByLabel('密码', { exact: true })).toHaveValue('completed-password')
  await expect(page.locator('#revealed-totp-secret')).toHaveText('JBSWY3DPEHPK3PXP')
  await expect(page.locator('#revealed-totp-code')).toHaveText(totpAt(secondReveal.totp.secret, Date.parse(secondReveal.server_time)))
  await page.getByRole('dialog').getByRole('button', { name: '关闭弹窗' }).click()
  await page.screenshot({ path: path.join(screenshots, `mstep04-accounts-${testInfo.project.name}.png`), fullPage: true })

  await page.getByRole('link', { name: '卡密', exact: true }).click()
  await expect(page.getByRole('heading', { name: '卡密管理' })).toBeVisible()
  await page.getByRole('button', { name: '批量生成' }).click()
  await page.getByLabel('数量').fill('1')
  await page.getByLabel('有效期（天）').fill('45')
  await expect(page.getByText('可输入 1～90 天的任意整数')).toBeVisible()
  await page.getByRole('dialog').getByRole('button', { name: '生成卡密' }).click()
  await expect(page.getByText('已生成 1 张卡密')).toBeVisible()
  await page.getByRole('row', { name: /ABCD/ }).getByRole('button', { name: '查看' }).click()
  await expect(page.getByText('2345-6789-ABCD')).toBeVisible()
  await expect(page.getByText(/查看明文会写入.*审计/)).toBeVisible()
  await page.screenshot({ path: path.join(screenshots, `mstep04-cards-${testInfo.project.name}.png`), fullPage: true })

  await page.getByRole('link', { name: '配置' }).click()
  await expect(page.getByRole('heading', { name: '密钥用途边界' })).toBeVisible()
  await expect(page.getByText('统一管理员认证')).toBeVisible()
  await expect(page.getByText('监控数据加密')).toBeVisible()
  await expect(page.getByText('分配数据加密')).toBeVisible()
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await page.emulateMedia({ reducedMotion: 'reduce' })
  const duration = await page.locator('.wordmark-pulse').evaluate((node) => getComputedStyle(node).transitionDuration)
  expect(Number.parseFloat(duration)).toBeLessThan(0.001)
  expect(consoleErrors).toEqual([])
})

test('monitor 401 explains the failed stage and next action', async ({ page }, testInfo) => {
  const consoleErrors = []
  page.on('console', (message) => { if (message.type() === 'error') consoleErrors.push(message.text()) })
  await login(page)
  await page.getByRole('link', { name: '账号体征', exact: true }).click()
  await expect(page.getByRole('heading', { name: '账号体征', exact: true })).toBeVisible()
  const accountCard = page.locator('.vital-card').filter({ hasText: 'Device Refresh Alert' })
  await expect(accountCard).toContainText('刷新授权 401')
  await expect(accountCard).toContainText('OAuth 刷新被拒绝，需重新授权')
  await accountCard.getByRole('button', { name: /Device Refresh Alert/ }).click()
  await expect(page.getByText('OAuth 令牌刷新被拒绝（HTTP 401）')).toBeVisible()
  await expect(page.getByText(/refresh token 被其他程序刷新后发生轮换/)).toBeVisible()
  await expect(page.getByText(/按原授权方式重新授权/)).toBeVisible()
  await expect(page.getByText('错误码：http_401')).toBeVisible()
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await page.screenshot({ path: path.join(screenshots, `monitor-http-401-${testInfo.project.name}.png`), fullPage: true })
  expect(consoleErrors).toEqual([])
})

test('public card flow persists redeemed card codes without account credentials', async ({ page }) => {
  const consoleErrors = []
  page.on('console', (message) => { if (message.type() === 'error') consoleErrors.push(message.text()) })
  await page.addInitScript(() => {
    window.__testNow = Date.now()
    Date.now = () => window.__testNow
  })
  await page.goto('/admin/user.html')
  await page.waitForLoadState('networkidle')
  await expect(page.getByRole('heading', { name: '卡密查询', exact: true })).toBeVisible()
  await page.getByLabel('卡密', { exact: true }).fill('2345-6789-ABCD')
  await page.getByRole('button', { name: '查询或兑换卡密' }).click()
  await expect(page.getByText('public@example.test')).toBeVisible()
  await expect(page.locator('#totp-code')).not.toHaveText('------')
  const initialTOTP = await page.locator('#totp-code').textContent()
  await page.evaluate(() => { window.__testNow += 31_000 })
  await expect(page.locator('#totp-timer')).toHaveText('当前验证码已过期，请手动刷新')
  await expect(page.locator('#totp-code')).toHaveText(initialTOTP)
  await page.getByRole('button', { name: '刷新验证码' }).click()
  await expect(page.locator('#totp-timer')).toHaveText(/s 后进入下一周期/)
  await page.getByRole('button', { name: '复制账号' }).click()
  await expect(page.locator('#copy-status')).toHaveText(/已复制|无法复制/)
  await expect(page.getByRole('heading', { name: '已兑换卡密' })).toBeVisible()
  await expect(page.locator('#saved-cards-list')).toContainText('2345-6789-ABCD')
  const storage = await page.evaluate(() => ({
    saved: localStorage.getItem('vitals.redeemed-cards.v1'),
    sessionLength: sessionStorage.length,
  }))
  expect(storage.saved).toContain('2345-6789-ABCD')
  expect(storage.saved).not.toContain('public@example.test')
  expect(storage.saved).not.toContain('public-test-password')
  expect(storage.saved).not.toContain('JBSWY3DPEHPK3PXP')
  expect(storage.sessionLength).toBe(0)
  await page.reload()
  await expect(page.getByRole('heading', { name: '已兑换卡密' })).toBeVisible()
  await page.locator('#saved-cards-list').getByRole('button', { name: '查询' }).click()
  await expect(page.getByText('public@example.test')).toBeVisible()
  expect(consoleErrors).toEqual([])
})

test('public card flow identifies a previously saved expired card', async ({ page, request }) => {
  await scenario(request, 'expired-card', true)
  await page.addInitScript(() => {
    localStorage.setItem('vitals.redeemed-cards.v1', JSON.stringify([{
      code: 'STUV-WXYZ-2345',
      saved_at: '2026-07-01T00:00:00Z',
      valid_until: '2026-07-08T00:00:00Z',
    }]))
  })
  await page.goto('/admin/user.html')
  await page.getByLabel('卡密', { exact: true }).fill('STUV-WXYZ-2345')
  await page.getByRole('button', { name: '查询或兑换卡密' }).click()

  await expect(page.locator('#error')).toHaveText('卡密已过期。')
  await expect(page.locator('#error')).toHaveClass(/visible/)
  await expect(page.locator('#result')).not.toHaveClass(/visible/)
  await expect(page.locator('#saved-cards-list')).toContainText('STUV-WXYZ-2345')
})

test('public card flow displays pickup-only credentials without requiring 2FA', async ({ page, request }) => {
  const consoleErrors = []
  page.on('console', (message) => { if (message.type() === 'error') consoleErrors.push(message.text()) })
  await scenario(request, 'pickup-only', true)
  await page.goto('/admin/user.html')
  await page.getByLabel('卡密', { exact: true }).fill('22X9-Q5KQ-ED5R')
  await page.getByRole('button', { name: '查询或兑换卡密' }).click()

  await expect(page.locator('#result')).toHaveClass(/visible/)
  await expect(page.locator('#account')).toHaveText('pickup-only@example.test')
  await expect(page.locator('#password')).toHaveText('pickup-only-password')
  await expect(page.locator('#pickup-address')).toHaveAttribute('href', 'https://pickup.example.test/order/42')
  await expect(page.locator('#totp-code')).toHaveText('未提供')
  await expect(page.locator('#totp-timer')).toHaveText('请使用取件地址')
  await expect(page.getByRole('button', { name: '刷新验证码' })).toBeDisabled()
  expect(consoleErrors).toEqual([])
})
