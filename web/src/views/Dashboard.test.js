import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it, vi } from 'vitest'
import Dashboard from './Dashboard.vue'

const monitorAccounts = [
  { id: 1, status: 'alive', near_expiry: false, last_check_state: 'ok' },
  { id: 2, status: 'alive', near_expiry: true, last_check_state: 'ok' },
  { id: 3, status: 'dead_banned', last_check_state: 'verification_required', banned_survival_days: 18 },
]
const allocationAccounts = [
  { id: 10, status: 'available', max_concurrent_users: 4, current_allocations: 2 },
  { id: 11, status: 'pending_credentials', max_concurrent_users: 3, current_allocations: 0 },
]
const cards = [{ id: 20, status: 'redeemed' }, { id: 21, status: 'unused' }]

function response(body, status = 200) {
  return { ok: status >= 200 && status < 300, status, headers: new Headers(), json: async () => body }
}

function successResponses(fetchMock) {
  return fetchMock.mockImplementation(async (url) => response({
    '/api/accounts': { accounts: monitorAccounts },
    '/api/admin/dashboard': { dashboard: { capacity: 7, available_capacity: 5, warning_level: 'attention', days_to_exhaust: 12, redeemed_last_7_days: 8, daily_redemption_rate: 1.14 } },
    '/api/admin/accounts': { accounts: allocationAccounts },
    '/api/admin/cards': { cards },
    '/api/admin/dashboard/password-change-due': { accounts: [] },
  }[url]))
}

async function render(fetchMock) {
  vi.stubGlobal('fetch', fetchMock)
  const placeholder = { template: '<div />' }
  const routes = [
    { path: '/', component: Dashboard }, { path: '/login', component: placeholder },
    { path: '/monitor/accounts', component: placeholder }, { path: '/allocation/accounts', component: placeholder },
    { path: '/allocation/cards', component: placeholder }, { path: '/allocation/allocations', component: placeholder },
    { path: '/settings', component: placeholder },
  ]
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push('/'); await router.isReady()
  const wrapper = mount(Dashboard, { global: { plugins: [router] } })
  await flushPromises()
  return wrapper
}

describe('Unified dashboard', () => {
  it('keeps the compact capacity overview and independently loads password reminders', async () => {
    const fetchMock = successResponses(vi.fn())
    const wrapper = await render(fetchMock)
    expect(fetchMock.mock.calls.map(([url]) => String(url)).sort()).toEqual(['/api/accounts', '/api/admin/dashboard', '/api/admin/accounts', '/api/admin/cards', '/api/admin/dashboard/password-change-due'].sort())
    // 账号健康、业务库存和过期卡密面板保持隐藏。
    expect(wrapper.text()).not.toContain('账号健康')
    expect(wrapper.text()).not.toContain('业务库存')
    expect(wrapper.text()).not.toContain('待补全账号')
    expect(wrapper.text()).toContain('当前可用容量')
    expect(wrapper.findAll('.domain-panel')).toHaveLength(1)
    const capacity = wrapper.find('.domain-kpi strong')
    expect(capacity.text()).toBe('5')
    expect(wrapper.text()).toContain('暂无待更改密码账号')
    expect(wrapper.text()).not.toContain('近 3 天已过期卡密及对应账号')
    wrapper.unmount()
  })

  it('highlights the capacity card when nothing is allocatable', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url) => response(url === '/api/admin/dashboard'
      ? { dashboard: { capacity: 28, available_capacity: 0, warning_level: 'exhausted' } }
      : { accounts: [], cards: [] }))
    const wrapper = await render(fetchMock)
    expect(wrapper.find('.domain-kpi').classes()).toContain('tone-red')
    expect(wrapper.find('.domain-kpi strong').text()).toBe('0')
    // 藏起来的受阻容量归因不再渲染。
    expect(wrapper.find('.blocked-breakdown').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows a recovery status after a failed aggregate request succeeds', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url) => response({ accounts: [], cards: [] }, url === '/api/accounts' ? 503 : 200))
    const wrapper = await render(fetchMock)
    expect(wrapper.text()).toContain('运营体征读取中断')
    successResponses(fetchMock)
    await wrapper.find('.error-panel button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('连接已恢复')
    wrapper.unmount()
  })

  it('isolates password API failure from capacity and refreshes both from the overview button', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url) => response(
      url === '/api/admin/dashboard' ? { dashboard: { available_capacity: 4 } } : { accounts: [], cards: [] },
      url === '/api/admin/dashboard/password-change-due' ? 503 : 200,
    ))
    const wrapper = await render(fetchMock)
    expect(wrapper.text()).toContain('密码清单读取失败')
    expect(wrapper.text()).not.toContain('运营体征读取中断')
    expect(wrapper.find('.domain-kpi strong').text()).toBe('4')
    successResponses(fetchMock)
    await wrapper.find('.overview-refresh').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('暂无待更改密码账号')
    expect(wrapper.find('.domain-kpi strong').text()).toBe('5')
    expect(fetchMock.mock.calls.filter(([url]) => url === '/api/admin/dashboard/password-change-due')).toHaveLength(2)
    wrapper.unmount()
  })
})
