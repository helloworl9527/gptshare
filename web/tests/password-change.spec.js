import { expect, test } from '@playwright/test'

async function scenario(request, name, reset = false) {
  const response = await request.post('http://127.0.0.1:4174/__test/scenario', { data: { name, reset } })
  expect(response.ok()).toBeTruthy()
}

async function login(page) {
  await page.goto('/admin/login')
  await page.getByLabel('管理员账号').fill('admin')
  await page.getByLabel('密码').fill('controlled-test-password')
  await page.getByRole('button', { name: '验证密码' }).click()
  await page.getByLabel('动态验证码').fill('123456')
  await page.getByRole('button', { name: '完成登录' }).click()
  await expect(page.getByRole('heading', { name: '运营总览' })).toBeVisible()
}

test.beforeEach(async ({ request }) => { await scenario(request, 'default', true) })

test('password reminders confirm handling and persist through refresh', async ({ page }, testInfo) => {
  await login(page)
  const panel = page.getByRole('region', { name: /待更改密码/ })
  await expect(panel.getByText('north@example.test', { exact: true })).toBeVisible()
  await expect(panel.getByText(/账号剩余 8 天/)).toBeVisible()
  await expect(page.locator('.domain-panel')).toHaveCount(1)
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await page.screenshot({ path: testInfo.outputPath('password-reminders.png'), fullPage: true })
  await panel.getByRole('button', { name: '标记已更改密码' }).click()
  const dialog = page.getByRole('dialog', { name: '确认已更改密码' })
  await expect(dialog.getByText(/不会实际修改密码或登出设备/)).toBeVisible()
  await dialog.getByRole('button', { name: '取消', exact: true }).click()
  await expect(panel.getByText('north@example.test', { exact: true })).toBeVisible()
  await panel.getByRole('button', { name: '标记已更改密码' }).click()
  await dialog.getByRole('button', { name: '确认已处理' }).click()
  await expect(panel.getByText('暂无待更改密码账号')).toBeVisible()
  await expect(panel.getByRole('status').filter({ hasText: '已记录 north@example.test' })).toBeVisible()
  await page.getByRole('button', { name: '刷新数据', exact: true }).click()
  await expect(panel.getByText('暂无待更改密码账号')).toBeVisible()
  await page.reload()
  await expect(panel.getByText('暂无待更改密码账号')).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

test('password list failure does not hide capacity and can recover independently', async ({ page, request }) => {
  await scenario(request, 'password-error')
  await login(page)
  const panel = page.getByRole('region', { name: /待更改密码/ })
  await expect(panel.getByText('密码清单读取失败')).toBeVisible()
  await expect(page.getByRole('heading', { name: '当前可用容量' })).toBeVisible()
  await scenario(request, 'default')
  await panel.getByRole('button', { name: '重试', exact: true }).click()
  await expect(panel.getByText('north@example.test', { exact: true })).toBeVisible()
})

test('failed password mark keeps the reminder until a successful retry', async ({ page, request }) => {
  await login(page)
  const panel = page.getByRole('region', { name: /待更改密码/ })
  await scenario(request, 'password-mark-error')
  await panel.getByRole('button', { name: '标记已更改密码' }).click()
  const dialog = page.getByRole('dialog', { name: '确认已更改密码' })
  await dialog.getByRole('button', { name: '确认已处理' }).click()
  await expect(dialog.getByRole('alert')).toBeVisible()
  await expect(panel.locator('.password-row')).toHaveCount(1)
  await scenario(request, 'default')
  await dialog.getByRole('button', { name: '确认已处理' }).click()
  await expect(panel.getByText('暂无待更改密码账号')).toBeVisible()
})
