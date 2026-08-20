import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { clearClientState } from '../api/client.js'
import Accounts from './Accounts.vue'
import Cards from './Cards.vue'
import Allocations from './Allocations.vue'

const accounts = [
  { id: 1, display_username: 'north@example.test', source_url: 'https://accounts.example.test/orders/42', account_expiry: '2026-08-19T00:00:00Z', max_concurrent_users: 3, current_allocations: 1, monitor_status: 'alive', status: 'available' },
  { id: 2, display_username: 'full@example.test', account_expiry: '2026-08-05T00:00:00Z', max_concurrent_users: 2, current_allocations: 2, monitor_status: 'unknown_monitor', status: 'full' },
]
const cards = [
  { id: 1, code_suffix: 'ABCD', duration_days: 7, status: 'unused', created_at: '2026-07-24T00:00:00Z' },
  { id: 2, code_suffix: 'EFGH', duration_days: 30, status: 'redeemed', redeemed_at: '2026-07-24T01:00:00Z', expires_at: '2026-08-23T00:00:00Z' },
]
const accountSettings = { settings: { default_account_capacity: 3 } }

function response(body, status = 200) {
  return { ok: status >= 200 && status < 300, status, headers: new Headers(), json: async () => body }
}

async function render(component, fetchMock) {
  vi.stubGlobal('fetch', fetchMock)
  const placeholder = { template: '<div />' }
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: placeholder },
      { path: '/accounts', component: Accounts },
      { path: '/cards', component: Cards },
      { path: '/allocations', component: Allocations },
      { path: '/login', component: placeholder },
      { path: '/monitor/accounts', component: placeholder },
      { path: '/allocation/accounts', component: placeholder },
      { path: '/allocation/cards', component: placeholder },
      { path: '/allocation/allocations', component: placeholder },
      { path: '/settings', component: placeholder },
    ],
  })
  const path = component === Accounts ? '/accounts' : component === Cards ? '/cards' : '/allocations'
  await router.push(path)
  await router.isReady()
  const wrapper = mount(component, { attachTo: document.body, global: { plugins: [router] } })
  await flushPromises()
  return wrapper
}

describe('P2 admin views', () => {
  beforeEach(() => {
    clearClientState()
  })

  it('renders account table without the retired phase-one pull action', async () => {
    const fetchMock = vi.fn().mockResolvedValue(response({ accounts, warnings: ['phase_one_monitor_unavailable'] }))
    const wrapper = await render(Accounts, fetchMock)
    expect(wrapper.text()).toContain('north@example.test')
    expect(wrapper.get('a.source-link').attributes('href')).toBe('https://accounts.example.test/orders/42')
    expect(wrapper.text()).toContain('一期监控暂时不可用')
    // 一期账号现在通过事件自动同步过来，手动拉取入口已下线。
    expect(wrapper.text()).not.toContain('从一期同步账号')
    expect(wrapper.find('button.compact-action').exists()).toBe(false)
    expect(fetchMock.mock.calls.map(([url]) => String(url))).not.toContain('/api/admin/accounts/pull-monitor')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('runs batch monitor sync from the accounts view', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ accounts }))
      .mockResolvedValueOnce(response(accountSettings))
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({ accounts, failed: 2, warnings: ['phase_one_monitor_unavailable'] }))
      .mockResolvedValueOnce(response({ accounts, warnings: ['phase_one_monitor_unavailable'] }))
      .mockResolvedValueOnce(response(accountSettings))
    const wrapper = await render(Accounts, fetchMock)
    await wrapper.findAll('button').find((button) => button.text() === '同步全部').trigger('click')
    await flushPromises()
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toContain('/api/admin/accounts/sync-status')
    expect(wrapper.text()).toContain('一期状态同步部分降级')
    wrapper.unmount()
  })

	it('edits account metadata and optional credentials without displaying stored secrets', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ accounts }))
      .mockResolvedValueOnce(response(accountSettings))
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({ account: { ...accounts[0], display_username: 'edited@example.test' } }))
      .mockResolvedValueOnce(response({ accounts: [{ ...accounts[0], display_username: 'edited@example.test' }] }))
      .mockResolvedValueOnce(response(accountSettings))
    const wrapper = await render(Accounts, fetchMock)
    await wrapper.findAll('button').find((button) => button.text() === '编辑').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
    expect(wrapper.find('#edit-display-password').attributes('placeholder')).toBe('留空不修改')
    expect(wrapper.find('#edit-display-totp').attributes('placeholder')).toBe('留空不修改')
    expect(wrapper.find('#edit-source-url').element.value).toBe('https://accounts.example.test/orders/42')
    expect(wrapper.text()).not.toContain('secret-password')
    await wrapper.find('#edit-display-username').setValue('edited@example.test')
    await wrapper.find('#edit-display-password').setValue('new-secret-password')
    await wrapper.find('#edit-display-totp').setValue('NEW-TOTP-SECRET')
    await wrapper.find('#edit-source-url').setValue('https://accounts.example.test/orders/84')
    await wrapper.find('.modal-form').trigger('submit')
    await flushPromises()
    const updateCall = fetchMock.mock.calls.find(([url, init]) => String(url) === '/api/admin/accounts/1' && init?.method === 'PUT')
    expect(updateCall).toBeTruthy()
    const body = JSON.parse(updateCall[1].body)
    expect(body).toMatchObject({
      display_username: 'edited@example.test',
      display_password: 'new-secret-password',
      display_2fa_secret: 'NEW-TOTP-SECRET',
      source_url: 'https://accounts.example.test/orders/84',
      max_concurrent_users: 3,
      status: 'available',
      monitor_status: 'alive',
	})
    expect(wrapper.text()).toContain('账号已更新')
    wrapper.unmount()
  })

  it('reveals credentials only on demand, corrects clock skew, updates TOTP, copies, and clears on close', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-08-11T12:00:05Z'))
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ accounts }))
      .mockResolvedValueOnce(response(accountSettings))
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({
        account_id: 1,
        password: 'revealed-password-value',
        totp: { secret: 'JBSWY3DPEHPK3PXP', period: 30, digits: 6, algorithm: 'SHA1' },
        server_time: '2026-08-11T12:00:25Z',
        request_id: 'reveal-request',
      }))
    const wrapper = await render(Accounts, fetchMock)
    await wrapper.findAll('button').find((button) => button.text() === '编辑').trigger('click')
    expect(fetchMock.mock.calls.map(([url]) => String(url))).not.toContain('/api/admin/accounts/1/credentials/reveal')

    await wrapper.findAll('button').find((button) => button.text() === '查看凭据').trigger('click')
    await flushPromises()
    const password = wrapper.get('#revealed-password')
    expect(password.attributes('type')).toBe('password')
    expect(password.element.value).toBe('revealed-password-value')
    expect(wrapper.get('#revealed-totp-secret').text()).toBe('JBSWY3DPEHPK3PXP')
    await vi.waitFor(() => expect(wrapper.get('#revealed-totp-code').text()).toMatch(/^\d{6}$/))
    expect(wrapper.text()).toContain('5 秒')

    await wrapper.get('[aria-label="显示密码"]').trigger('click')
    expect(wrapper.get('#revealed-password').attributes('type')).toBe('text')
    await wrapper.findAll('.credentials-panel button').find((button) => button.text() === '复制').trigger('click')
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('revealed-password-value')
    expect(wrapper.text()).toContain('密码已复制')

    const firstCode = wrapper.get('#revealed-totp-code').text()
    await vi.advanceTimersByTimeAsync(6_000)
    await vi.waitFor(() => expect(wrapper.get('#revealed-totp-code').text()).not.toBe(firstCode))
    expect(wrapper.get('#revealed-totp-code').text()).toMatch(/^\d{6}$/)

    await wrapper.get('[role="dialog"] .icon-button').trigger('click')
    expect(wrapper.html()).not.toContain('revealed-password-value')
    expect(wrapper.html()).not.toContain('JBSWY3DPEHPK3PXP')
    await vi.advanceTimersByTimeAsync(31_000)
    expect(wrapper.find('#revealed-totp-code').exists()).toBe(false)
    wrapper.unmount()
    vi.useRealTimers()
  })

  it('keeps an invalid Secret copyable, reports TOTP failure, and clears on session expiry', async () => {
    const writeText = vi.fn().mockRejectedValue(new Error('denied'))
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ accounts }))
      .mockResolvedValueOnce(response(accountSettings))
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({
        account_id: 1,
        password: 'temporary-password',
        totp: { secret: 'INVALID-SECRET!', period: 30, digits: 6, algorithm: 'SHA1' },
        server_time: new Date().toISOString(),
        request_id: 'invalid-secret-request',
      }))
    const wrapper = await render(Accounts, fetchMock)
    await wrapper.findAll('button').find((button) => button.text() === '编辑').trigger('click')
    await wrapper.findAll('button').find((button) => button.text() === '查看凭据').trigger('click')
    await flushPromises()
    expect(wrapper.get('#revealed-totp-secret').text()).toBe('INVALID-SECRET!')
    expect(wrapper.text()).toContain('2FA Secret 格式无效，无法生成动态验证码')
    const copyButtons = wrapper.findAll('.credentials-panel button').filter((button) => button.text() === '复制')
    await copyButtons[1].trigger('click')
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('INVALID-SECRET!')
    expect(wrapper.text()).toContain('2FA Secret复制失败，请手动选择')

    window.dispatchEvent(new CustomEvent('session-expired'))
    await flushPromises()
    expect(wrapper.html()).not.toContain('temporary-password')
    expect(wrapper.html()).not.toContain('INVALID-SECRET!')
    expect(wrapper.findAll('button').some((button) => button.text() === '查看凭据')).toBe(true)
    wrapper.unmount()
  })

  it('confirms safe retirement, reports migrations, and blocks repeated clicks', async () => {
    let finishRetirement
    const retirement = new Promise((resolve) => { finishRetirement = resolve })
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ accounts }))
      .mockResolvedValueOnce(response(accountSettings))
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockImplementationOnce(() => retirement)
      .mockResolvedValueOnce(response({ accounts: accounts.slice(1) }))
      .mockResolvedValueOnce(response(accountSettings))
    vi.stubGlobal('confirm', vi.fn(() => true))
    const wrapper = await render(Accounts, fetchMock)
    const retire = wrapper.findAll('button').find((button) => button.text() === '下线')
    await retire.trigger('click')
    await retire.trigger('click')
    expect(window.confirm).toHaveBeenCalledOnce()
    expect(window.confirm.mock.calls[0][0]).toContain('仍有效的卡密分配将自动迁移到备用账号')
    expect(fetchMock.mock.calls.filter(([url, init]) => String(url) === '/api/admin/accounts/1' && init?.method === 'DELETE')).toHaveLength(1)
    expect(retire.attributes('disabled')).toBeDefined()
    finishRetirement(response({ archived: true, replaced_allocations: 2, closed_allocations: 1, request_id: 'retire-request' }))
    await flushPromises()
    expect(wrapper.text()).toContain('账号已下线：迁移 2 个有效分配，结束 1 个无效或宽限分配')
    wrapper.unmount()
  })

  it('saves default capacity and applies it to all accounts', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ accounts }))
      .mockResolvedValueOnce(response(accountSettings))
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({ settings: { default_account_capacity: 6 } }))
      .mockResolvedValueOnce(response({ default_account_capacity: 6, updated_accounts: 2 }))
      .mockResolvedValueOnce(response({ accounts: accounts.map((account) => ({ ...account, max_concurrent_users: 6 })) }))
      .mockResolvedValueOnce(response({ settings: { default_account_capacity: 6 } }))
    vi.stubGlobal('confirm', vi.fn(() => true))
    const wrapper = await render(Accounts, fetchMock)
    await wrapper.find('#default-capacity').setValue(6)
    await wrapper.find('.capacity-form').trigger('submit')
    await flushPromises()
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toContain('/api/admin/account-settings')
    await wrapper.findAll('button').find((button) => button.text() === '应用到全部账号').trigger('click')
    await flushPromises()
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toContain('/api/admin/accounts/apply-default-capacity')
    expect(window.confirm).toHaveBeenCalled()
    wrapper.unmount()
  })

  it('generates cards and keeps one-time plaintext visible only in generated panel', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ cards }))
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({ cards: [
        { id: 5, code: '2345-6789-ABCD', code_suffix: 'ABCD', duration_days: 90, status: 'unused' },
        { id: 6, code: '2345-6789-EFGH', code_suffix: 'EFGH', duration_days: 90, status: 'unused' },
      ] }, 201))
      .mockResolvedValueOnce(response({ cards }))
    const wrapper = await render(Cards, fetchMock)
    await wrapper.get('button.compact-action').trigger('click')
    await wrapper.find('#quantity').setValue(1)
    await wrapper.find('#duration').setValue(90)
    await wrapper.find('.modal-form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('2345-6789-ABCD')
    await wrapper.findAll('button').find((button) => button.text() === '一键复制全部').trigger('click')
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('2345-6789-ABCD\n2345-6789-EFGH')
    expect(wrapper.text()).toContain('已复制 2 张卡密，每行一个。')
    const generateCall = fetchMock.mock.calls.find(([url]) => String(url) === '/api/admin/cards/generate')
    expect(JSON.parse(generateCall[1].body)).toMatchObject({ quantity: 1, duration_days: 90 })
    wrapper.unmount()
  })

  it('accepts custom duration and caps extension at ninety days from redemption', async () => {
    const extendable = [{
      id: 8,
      code_suffix: 'EXT5',
      duration_days: 5,
      status: 'redeemed',
      redeemed_at: '2026-07-24T00:00:00Z',
      expires_at: '2026-07-29T00:00:00Z',
    }]
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ cards: extendable }))
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({ cards: [{ id: 9, code: '2345-6789-ABCD', code_suffix: 'ABCD', duration_days: 31, status: 'unused' }] }, 201))
      .mockResolvedValueOnce(response({ cards: extendable }))
      .mockResolvedValueOnce(response({ card: { ...extendable[0], expires_at: '2026-10-22T00:00:00Z' } }))
      .mockResolvedValueOnce(response({ cards: extendable }))
    const wrapper = await render(Cards, fetchMock)

    await wrapper.get('button.compact-action').trigger('click')
    await wrapper.find('#duration').setValue(31)
    await wrapper.find('.modal-form').trigger('submit')
    await flushPromises()
    const generateCall = fetchMock.mock.calls.find(([url]) => String(url) === '/api/admin/cards/generate')
    expect(JSON.parse(generateCall[1].body)).toMatchObject({ duration_days: 31 })

    await wrapper.findAll('button').find((button) => button.text() === '延期').trigger('click')
    expect(wrapper.find('#extend-days').attributes('max')).toBe('85')
    await wrapper.find('#extend-days').setValue(85)
    await wrapper.find('.modal-form').trigger('submit')
    await flushPromises()
    const extendCall = fetchMock.mock.calls.find(([url]) => String(url) === '/api/admin/cards/8/extend')
    expect(JSON.parse(extendCall[1].body)).toEqual({ days: 85 })
    wrapper.unmount()
  })

  it('reveals a single card plaintext only after an explicit click', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ cards }))
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({ code: '2345-6789-ABCD', card: { id: 1, code_suffix: 'ABCD', plaintext_available: true } }))
    const wrapper = await render(Cards, fetchMock)
    expect(wrapper.text()).toContain('**** ABCD')
    expect(wrapper.text()).not.toContain('2345-6789-ABCD')
    await wrapper.findAll('button').find((button) => button.text() === '查看').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('2345-6789-ABCD')
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toContain('/api/admin/cards/1/reveal')
    wrapper.unmount()
  })

  it('shows legacy unavailable message when card plaintext cannot be restored', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ cards }))
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({ message: '明文不可用(旧批次)', card: { id: 1, code_suffix: 'ABCD', plaintext_available: false } }))
    const wrapper = await render(Cards, fetchMock)
    await wrapper.findAll('button').find((button) => button.text() === '查看').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('明文不可用(旧批次)')
    wrapper.unmount()
  })

  it('renders the real allocation relationships from the dedicated endpoint', async () => {
    const fetchMock = vi.fn().mockResolvedValue(response({ allocations: [
      { id: 28, card_id: 12, code_suffix: 'JUL28', duration_days: 8, account_id: 7, display_username: 'fund-outlier@example.test', account_expiry: '2026-08-20T00:00:00Z', allocation_state: 'primary', active: true, allocated_at: '2026-07-28T15:11:00Z', valid_until: '2026-08-05T15:11:00Z' },
      { id: 30, card_id: 14, code_suffix: 'JUL30', duration_days: 8, account_id: 9, display_username: 'flagon_snap@example.test', account_expiry: '2026-08-22T00:00:00Z', allocation_state: 'primary', active: true, allocated_at: '2026-07-30T13:48:00Z', valid_until: '2026-08-07T13:48:00Z' },
    ] }))
    const wrapper = await render(Allocations, fetchMock)
    expect(wrapper.text()).toContain('**** JUL28')
    expect(wrapper.text()).toContain('fund-outlier@example.test')
    expect(wrapper.text()).toContain('**** JUL30')
    expect(wrapper.text()).toContain('flagon_snap@example.test')
    expect(wrapper.text()).toContain('替换历史')
    expect(wrapper.text()).toContain('暂无替换记录')
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toEqual(['/api/admin/allocations'])
    wrapper.unmount()
  })

  it('renders replacement history with reasons, grace windows and retired accounts', async () => {
    const fetchMock = vi.fn().mockResolvedValue(response({
      allocations: [],
      replacements: [
        { id: 2, card_id: 58, code_suffix: 'CE58', old_account_id: 15, old_account_name: 'banned@example.test', old_account_gone: true, new_account_id: 20, new_account_name: 'fresh@example.test', new_account_gone: false, reason: 'banned', operator: 'system', detected_at: '2026-08-20T03:06:13Z', replaced_at: '2026-08-20T03:06:13Z' },
        { id: 1, card_id: 45, code_suffix: 'CE45', old_account_id: 5, old_account_name: 'aging@example.test', old_account_gone: false, new_account_id: 18, new_account_name: 'spare@example.test', new_account_gone: false, reason: 'account_expiring', operator: 'system', detected_at: '2026-08-18T19:36:13Z', replaced_at: '2026-08-18T19:36:13Z', grace_until: '2026-08-19T19:36:13Z' },
      ],
    }))
    const wrapper = await render(Allocations, fetchMock)
    expect(wrapper.text()).toContain('**** CE58')
    expect(wrapper.text()).toContain('账号封禁')
    expect(wrapper.text()).toContain('banned@example.test（已下线）')
    expect(wrapper.text()).toContain('fresh@example.test')
    expect(wrapper.text()).toContain('即时切换')
    expect(wrapper.text()).toContain('**** CE45')
    expect(wrapper.text()).toContain('账号临期')
    expect(wrapper.text()).toContain('系统自动')
    expect(wrapper.text()).not.toContain('暂无替换记录')
    wrapper.unmount()
  })
})
