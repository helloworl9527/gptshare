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
  return fetchMock
    .mockResolvedValueOnce(response({ accounts: monitorAccounts }))
    .mockResolvedValueOnce(response({ dashboard: { capacity: 7, available_capacity: 5, warning_level: 'attention', days_to_exhaust: 12, redeemed_last_7_days: 8, daily_redemption_rate: 1.14 } }))
    .mockResolvedValueOnce(response({ accounts: allocationAccounts }))
    .mockResolvedValueOnce(response({ cards }))
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
  it('loads both domains from the single-origin API client and renders side-by-side vitals', async () => {
    const fetchMock = successResponses(vi.fn())
    const wrapper = await render(fetchMock)
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toEqual(['/api/accounts', '/api/admin/dashboard', '/api/admin/accounts', '/api/admin/cards'])
    expect(wrapper.text()).toContain('账号健康')
    expect(wrapper.text()).toContain('业务库存')
    expect(wrapper.text()).toContain('待补全账号')
    expect(wrapper.findAll('.domain-panel')).toHaveLength(2)
  })

  it('reports blocked capacity so a dashboard number can never promise seats redemption cannot allocate', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ accounts: monitorAccounts }))
      .mockResolvedValueOnce(response({
        dashboard: {
          capacity: 28, available_capacity: 0, blocked_capacity: 6, warning_level: 'exhausted', days_to_exhaust: 0,
          blocked_breakdown: [
            { reason: 'pending_credentials', accounts: 1, free_slots: 4 },
            { reason: 'full', accounts: 1, free_slots: 2 },
          ],
        },
      }))
      .mockResolvedValueOnce(response({ accounts: allocationAccounts }))
      .mockResolvedValueOnce(response({ cards }))
    const wrapper = await render(fetchMock)
    expect(wrapper.text()).toContain('另有 6 位受阻')
    expect(wrapper.text()).toContain('凭据未补全')
    expect(wrapper.text()).toContain('状态漂移（已纠偏中）')
    expect(wrapper.findAll('.blocked-breakdown li')).toHaveLength(2)
  })

  it('hides the blocked breakdown when every free seat is allocatable', async () => {
    const wrapper = await render(successResponses(vi.fn()))
    expect(wrapper.find('.blocked-breakdown').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('位受阻')
  })

  it('shows a recovery status after a failed aggregate request succeeds', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({}, 503))
      .mockResolvedValueOnce(response({ dashboard: {} }))
      .mockResolvedValueOnce(response({ accounts: [] }))
      .mockResolvedValueOnce(response({ cards: [] }))
    const wrapper = await render(fetchMock)
    expect(wrapper.text()).toContain('运营体征读取中断')
    successResponses(fetchMock)
    await wrapper.find('.error-panel button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('连接已恢复')
  })
})
