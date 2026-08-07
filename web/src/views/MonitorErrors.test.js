import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it, vi } from 'vitest'
import VitalCard from '../components/VitalCard.vue'
import AccountDetail from './AccountDetail.vue'

const abnormalAccount = {
  id: 7,
  provider_account_id: 'acct-7',
  label: 'Refresh account',
  email: 'refresh@example.test',
  plan: 'plus',
  status: 'alive',
  last_check_state: 'error',
  last_check_error_code: 'http_401',
  auth_expiry: '2026-08-19T00:00:00Z',
  last_authorized_at: '2026-07-27T00:00:00Z',
  credential: { type: 'refresh', configured: true },
}

const reauthorizationAccount = {
  ...abnormalAccount,
  last_check_state: 'reauthorization_required',
  last_check_error_code: 'oauth_refresh_token_reused',
  polling_paused: true,
}

function response(body) {
  return { ok: true, status: 200, headers: new Headers(), json: async () => body }
}

describe('monitor error presentation', () => {
  it('shows the actionable 401 summary on an account card', () => {
    const wrapper = mount(VitalCard, { props: { account: abnormalAccount } })

    expect(wrapper.text()).toContain('刷新授权 401')
    expect(wrapper.text()).toContain('OAuth 刷新被拒绝，需重新授权')
  })

  it('shows failure stage, likely cause, action, and raw code in account details', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(abnormalAccount)))
    const placeholder = { template: '<div />' }
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: '/monitor/accounts/:id', name: 'account-detail', component: AccountDetail },
        { path: '/monitor/import', name: 'import', component: placeholder },
        { path: '/monitor/accounts', component: placeholder },
        { path: '/login', component: placeholder },
        { path: '/', component: placeholder },
        { path: '/allocation/accounts', component: placeholder },
        { path: '/allocation/cards', component: placeholder },
        { path: '/allocation/allocations', component: placeholder },
        { path: '/settings', component: placeholder },
      ],
    })
    await router.push('/monitor/accounts/7')
    await router.isReady()
    const wrapper = mount(AccountDetail, { global: { plugins: [router] } })
    await flushPromises()

    expect(wrapper.text()).toContain('OAuth 令牌刷新被拒绝（HTTP 401）')
    expect(wrapper.text()).toContain('同一 refresh token 被其他程序刷新后发生轮换')
    expect(wrapper.text()).toContain('按原授权方式重新授权')
    expect(wrapper.text()).toContain('错误码：http_401')
  })

  it('shows detected token rotation competition and reauthorization entry points', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(reauthorizationAccount)))
    const placeholder = { template: '<div />' }
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: '/monitor/accounts/:id', component: AccountDetail },
        { path: '/monitor/import', name: 'import', component: placeholder },
        { path: '/monitor/accounts', component: placeholder },
        { path: '/login', component: placeholder },
        { path: '/', component: placeholder },
        { path: '/allocation/accounts', component: placeholder },
        { path: '/allocation/cards', component: placeholder },
        { path: '/allocation/allocations', component: placeholder },
        { path: '/settings', component: placeholder },
      ],
    })
    await router.push('/monitor/accounts/7')
    await router.isReady()
    const wrapper = mount(AccountDetail, { global: { plugins: [router] } })
    await flushPromises()

    expect(wrapper.text()).toContain('Refresh Token 已被重复使用')
    expect(wrapper.text()).toContain('令牌轮换')
    expect(wrapper.text()).toContain('令牌轮换竞争：已检测到')
    expect(wrapper.text()).toContain('错误码：oauth_refresh_token_reused')
    expect(wrapper.text()).toContain('OAuth 重新授权')
    expect(wrapper.text()).toContain('设备码重新授权')
  })
})
