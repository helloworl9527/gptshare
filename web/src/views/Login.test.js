import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it, vi } from 'vitest'
import Login from './Login.vue'

function response(body, status = 200, headers = {}) {
  return { ok: status >= 200 && status < 300, status, headers: new Headers(headers), json: async () => body }
}

async function render() {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/login', component: Login }, { path: '/', component: { template: '<div>dashboard</div>' } }] })
  await router.push('/login')
  await router.isReady()
  return { wrapper: mount(Login, { global: { plugins: [router] } }), router }
}

describe('Login', () => {
  it('runs password then TOTP and prevents repeated submit while busy', async () => {
    let resolvePassword
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: 'a'.repeat(43) }))
      .mockImplementationOnce(() => new Promise((resolve) => { resolvePassword = resolve }))
      .mockResolvedValueOnce(response(null, 204))
      .mockResolvedValueOnce(response({ csrf_token: 'b'.repeat(43) }))
    vi.stubGlobal('fetch', fetchMock)
    const { wrapper, router } = await render()
    expect(wrapper.text()).toContain('账号监控、业务库存、卡密交付与安全配置边界')
    await wrapper.find('#username').setValue('admin')
    await wrapper.find('#password').setValue('password')
    await wrapper.find('form').trigger('submit')
    await wrapper.find('form').trigger('submit')
    expect(fetchMock).toHaveBeenCalledTimes(2)
    resolvePassword(response({ challenge: 'opaque-challenge', expires_in: 120 }))
    await flushPromises()
    expect(wrapper.find('#totp-code').exists()).toBe(true)
    await wrapper.find('#totp-code').setValue('123456')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(router.currentRoute.value.path).toBe('/')
  })

  it('returns to password after a challenge expires', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: 'a'.repeat(43) }))
      .mockResolvedValueOnce(response({ challenge: 'opaque-challenge', expires_in: 1 })))
    const { wrapper } = await render()
    await wrapper.find('#username').setValue('admin')
    await wrapper.find('#password').setValue('password')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    vi.advanceTimersByTime(1100)
    await wrapper.vm.$nextTick()
    expect(wrapper.text()).toContain('验证码挑战已过期')
    await wrapper.find('.text-button').trigger('click')
    expect(wrapper.find('#password').exists()).toBe(true)
    vi.useRealTimers()
  })
})
