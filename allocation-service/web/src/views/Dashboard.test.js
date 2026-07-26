import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it, vi } from 'vitest'
import Dashboard from './Dashboard.vue'

const accounts = [
  { id: 1, display_username: 'north@example.test', account_expiry: '2026-08-19T00:00:00Z', max_concurrent_users: 3, current_allocations: 1, monitor_status: 'alive', status: 'available' },
  { id: 2, display_username: 'amber@example.test', account_expiry: '2026-07-25T00:00:00Z', max_concurrent_users: 2, current_allocations: 2, monitor_status: 'unknown_monitor', status: 'full' },
]
const cards = [
  { id: 1, code_suffix: 'ABCD', duration_days: 7, status: 'unused', created_at: '2026-07-24T00:00:00Z' },
  { id: 2, code_suffix: 'EFGH', duration_days: 30, status: 'redeemed', redeemed_at: '2026-07-24T01:00:00Z', expires_at: '2026-07-26T00:00:00Z' },
  { id: 3, code_suffix: 'JKLM', duration_days: 90, status: 'revoked', revoked_at: '2026-07-24T02:00:00Z' },
]
const dashboard = {
  capacity: 5,
  used: 3,
  available_capacity: 2,
  redeemed_last_7_days: 7,
  daily_redemption_rate: 1,
  days_to_exhaust: 2,
  days_to_exhaust_label: '2.0',
  recommended_account_add: 3,
  warning_level: 'urgent',
  warning_label: '紧急',
}

function response(body, status = 200) {
  return { ok: status >= 200 && status < 300, status, headers: new Headers(), json: async () => body }
}

async function render(fetchMock = vi.fn()
  .mockResolvedValueOnce(response({ dashboard }))
  .mockResolvedValueOnce(response({ accounts }))
  .mockResolvedValueOnce(response({ cards }))) {
  vi.stubGlobal('fetch', fetchMock)
  const placeholder = { template: '<div />' }
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: Dashboard },
      { path: '/accounts', component: placeholder },
      { path: '/cards', component: placeholder },
      { path: '/allocations', component: placeholder },
      { path: '/login', component: placeholder },
    ],
  })
  await router.push('/')
  await router.isReady()
  const wrapper = mount(Dashboard, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, fetchMock }
}

describe('Dashboard', () => {
  it('computes allocator KPIs from admin accounts and cards', async () => {
    const { wrapper, fetchMock } = await render()
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toEqual(['/api/admin/dashboard', '/api/admin/accounts', '/api/admin/cards'])
    const kpis = wrapper.findAll('.kpi-card strong').map((node) => node.text())
    expect(kpis).toEqual(['紧急', '2', '1.0', '3'])
    expect(wrapper.text()).toContain('账号池时间分布')
    expect(wrapper.text()).toContain('**** EFGH')
  })

  it('shows loading, error, and recovery states', async () => {
    const fetchMock = vi.fn()
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValueOnce(response({ accounts: [] }))
      .mockResolvedValueOnce(response({ cards: [] }))
      .mockResolvedValueOnce(response({ dashboard }))
      .mockResolvedValueOnce(response({ accounts }))
      .mockResolvedValueOnce(response({ cards }))
    const { wrapper } = await render(fetchMock)
    expect(wrapper.text()).toContain('后台读取中断')
    await wrapper.find('.error-panel button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('连接已恢复')
    expect(wrapper.findAll('.kpi-card strong').map((node) => node.text())).toContain('紧急')
  })
})
