import { expect, test } from '@playwright/test'
import path from 'node:path'

const screenshots = path.resolve('..', '.workflow', 'screenshots')

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
  await page.getByLabel('新密码').fill('completed-password')
  await page.getByLabel('新 2FA Secret').fill('JBSWY3DPEHPK3PXP')
  await page.getByLabel('账号来源链接').fill('https://accounts.example.test/orders/42')
  await page.getByRole('dialog').getByRole('button', { name: '保存修改' }).click()
  await expect(page.getByText('账号已更新')).toBeVisible()
  await expect(page.getByRole('row', { name: /pulled-sync@example.test/ }).getByRole('link', { name: '打开来源' })).toHaveAttribute('href', 'https://accounts.example.test/orders/42')
  await page.screenshot({ path: path.join(screenshots, `mstep04-accounts-${testInfo.project.name}.png`), fullPage: true })

  await page.getByRole('link', { name: '卡密', exact: true }).click()
  await expect(page.getByRole('heading', { name: '卡密管理' })).toBeVisible()
  await page.getByRole('button', { name: '批量生成' }).click()
  await page.getByLabel('数量').fill('1')
  await page.getByLabel('有效期（天）').fill('5')
  await expect(page.getByText('可输入 1～30 天的任意整数')).toBeVisible()
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
