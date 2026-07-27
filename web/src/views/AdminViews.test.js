import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { clearClientState } from '../api/client.js'
import Accounts from './Accounts.vue'
import Cards from './Cards.vue'
import Allocations from './Allocations.vue'

const accounts = [
  { id: 1, display_username: 'north@example.test', account_expiry: '2026-08-19T00:00:00Z', max_concurrent_users: 3, current_allocations: 1, monitor_status: 'alive', status: 'available' },
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

  it('renders account table and pulls pending accounts from phase one', async () => {
    const fetchMock = vi.fn().mockResolvedValue(response({ accounts, warnings: ['phase_one_monitor_unavailable'] }))
    const wrapper = await render(Accounts, fetchMock)
    expect(wrapper.text()).toContain('north@example.test')
    expect(wrapper.text()).toContain('一期监控暂时不可用')
    await wrapper.get('button.compact-action').trigger('click')
    await flushPromises()
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toContain('/api/admin/accounts/pull-monitor')
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
    expect(wrapper.text()).not.toContain('secret-password')
    await wrapper.find('#edit-display-username').setValue('edited@example.test')
    await wrapper.find('#edit-display-password').setValue('new-secret-password')
    await wrapper.find('#edit-display-totp').setValue('NEW-TOTP-SECRET')
    await wrapper.find('.modal-form').trigger('submit')
    await flushPromises()
    const updateCall = fetchMock.mock.calls.find(([url, init]) => String(url) === '/api/admin/accounts/1' && init?.method === 'PUT')
    expect(updateCall).toBeTruthy()
    const body = JSON.parse(updateCall[1].body)
    expect(body).toMatchObject({
      display_username: 'edited@example.test',
      display_password: 'new-secret-password',
      display_2fa_secret: 'NEW-TOTP-SECRET',
      max_concurrent_users: 3,
      status: 'available',
      monitor_status: 'alive',
    })
    expect(wrapper.text()).toContain('账号已更新')
    wrapper.unmount()
  })

  it('saves default capacity, pulls with implicit default, and applies it to all accounts', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ accounts }))
      .mockResolvedValueOnce(response(accountSettings))
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({ settings: { default_account_capacity: 6 } }))
      .mockResolvedValueOnce(response({ accounts: [{ ...accounts[0], id: 9, display_username: 'implicit-default@example.test', max_concurrent_users: 6, monitor_account_id: 'mon-implicit', status: 'pending_credentials' }], created: 1, updated: 0 }))
      .mockResolvedValueOnce(response({ accounts: [{ ...accounts[0], id: 9, display_username: 'implicit-default@example.test', max_concurrent_users: 6, monitor_account_id: 'mon-implicit' }] }))
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
    await wrapper.get('button.compact-action').trigger('click')
    await flushPromises()
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toContain('/api/admin/accounts/pull-monitor')
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
        { id: 5, code: '2345-6789-ABCD', code_suffix: 'ABCD', duration_days: 30, status: 'unused' },
        { id: 6, code: '2345-6789-EFGH', code_suffix: 'EFGH', duration_days: 30, status: 'unused' },
      ] }, 201))
      .mockResolvedValueOnce(response({ cards }))
    const wrapper = await render(Cards, fetchMock)
    await wrapper.get('button.compact-action').trigger('click')
    await wrapper.find('#quantity').setValue(1)
    await wrapper.find('.modal-form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('2345-6789-ABCD')
    await wrapper.findAll('button').find((button) => button.text() === '一键复制全部').trigger('click')
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('2345-6789-ABCD\n2345-6789-EFGH')
    expect(wrapper.text()).toContain('已复制 2 张卡密，每行一个。')
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toContain('/api/admin/cards/generate')
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

  it('renders allocation records from approved admin data without requesting unknown endpoints', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ accounts }))
      .mockResolvedValueOnce(response({ cards: cards.filter((item) => item.status === 'redeemed') }))
    const wrapper = await render(Allocations, fetchMock)
    expect(wrapper.text()).toContain('**** EFGH')
    expect(wrapper.text()).toContain('替换历史')
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toEqual(['/api/admin/accounts', '/api/admin/cards?status=redeemed'])
    wrapper.unmount()
  })
})
