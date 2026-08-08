import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api, APIError, clearClientState } from '../api/client.js'
import ImportWizard from './ImportWizard.vue'

const firstOAuth = {
  session_id: 'session-current',
  authorization_url: 'https://auth.openai.test/current',
  expires_at: '2026-08-08T12:15:00Z',
}
const nextOAuth = {
  session_id: 'session-next',
  authorization_url: 'https://auth.openai.test/next',
  expires_at: '2026-08-08T12:20:00Z',
}
const sensitiveCallback = 'http://localhost:1455/auth/callback?code=authorization-secret&state=session-secret'

async function renderWizard() {
  const placeholder = { template: '<div />' }
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/monitor/import', name: 'import', component: ImportWizard },
      { path: '/monitor/accounts/:id', name: 'account-detail', component: placeholder },
    ],
  })
  await router.push('/monitor/import?mode=oauth')
  await router.isReady()
  return mount(ImportWizard, {
    attachTo: document.body,
    global: {
      plugins: [router],
      stubs: { AdminShell: { template: '<main><slot /></main>' } },
    },
  })
}

async function startCurrentOAuth(wrapper) {
  await wrapper.findAll('button').find((button) => button.text().includes('打开 OAuth 授权页')).trigger('click')
  await flushPromises()
}

async function submitCallback(wrapper) {
  await wrapper.get('#oauth-callback').setValue(sensitiveCallback)
  await wrapper.get('#oauth-callback').element.closest('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
  await flushPromises()
}

function storageValues(storage) {
  return Array.from({ length: storage.length }, (_, index) => storage.getItem(storage.key(index)))
}

describe('ImportWizard OAuth callback recovery', () => {
  beforeEach(() => {
    clearClientState()
    vi.stubGlobal('open', vi.fn())
  })

  it('shows a state mismatch without retaining callback secrets', async () => {
    const reason = new APIError('untrusted callback detail', {
      status: 422,
      code: 'oauth_state_mismatch',
      requestId: 'request-state-1',
    })
    vi.spyOn(api, 'startOAuth').mockResolvedValue(firstOAuth)
    vi.spyOn(api, 'completeOAuth').mockRejectedValue(reason)
    const wrapper = await renderWizard()
    await startCurrentOAuth(wrapper)
    await submitCallback(wrapper)

    const alert = wrapper.get('#oauth-error')
    expect(alert.text()).toContain('此回调不属于当前授权会话')
    expect(alert.text()).toContain('oauth_state_mismatch')
    expect(alert.text()).toContain('request-state-1')
    expect(document.activeElement).toBe(alert.element)
    expect(wrapper.get('#oauth-callback').element.value).toBe('')
    expect(wrapper.get('a.button-link').attributes('href')).toBe(firstOAuth.authorization_url)
    expect(api.startOAuth).toHaveBeenCalledTimes(1)
    expect(wrapper.html()).not.toContain(sensitiveCallback)
    expect(wrapper.html()).not.toContain('authorization-secret')
    expect(wrapper.html()).not.toContain('session-secret')
    expect(JSON.stringify(reason)).not.toContain(sensitiveCallback)
    expect(storageValues(localStorage)).not.toContain(sensitiveCallback)
    expect(storageValues(sessionStorage)).not.toContain(sensitiveCallback)
    wrapper.unmount()
  })

  it('keeps the current session for an invalid callback and returns focus to the input', async () => {
    vi.spyOn(api, 'startOAuth').mockResolvedValue(firstOAuth)
    vi.spyOn(api, 'completeOAuth').mockRejectedValue(new APIError('ignored', {
      status: 422,
      code: 'oauth_callback_invalid',
    }))
    const wrapper = await renderWizard()
    await startCurrentOAuth(wrapper)
    await submitCallback(wrapper)

    expect(wrapper.get('#oauth-error').text()).toContain('回调 URL 格式不正确')
    expect(api.startOAuth).toHaveBeenCalledTimes(1)
    await wrapper.findAll('.oauth-error-actions button').find((button) => button.text() === '重新粘贴回调 URL').trigger('click')
    expect(document.activeElement).toBe(wrapper.get('#oauth-callback').element)
    wrapper.unmount()
  })

  it('replaces an expired session with a newly generated authorization link', async () => {
    vi.spyOn(api, 'startOAuth')
      .mockResolvedValueOnce(firstOAuth)
      .mockResolvedValueOnce(nextOAuth)
    vi.spyOn(api, 'completeOAuth').mockRejectedValue(new APIError('ignored', {
      status: 422,
      code: 'oauth_session_expired',
    }))
    const wrapper = await renderWizard()
    await startCurrentOAuth(wrapper)
    await submitCallback(wrapper)
    await wrapper.findAll('.oauth-error-actions button').find((button) => button.text() === '生成新的授权链接').trigger('click')
    await flushPromises()

    expect(api.startOAuth).toHaveBeenCalledTimes(2)
    expect(wrapper.find('#oauth-error').exists()).toBe(false)
    expect(wrapper.get('a.button-link').attributes('href')).toBe(nextOAuth.authorization_url)
    expect(window.open).toHaveBeenLastCalledWith(nextOAuth.authorization_url, '_blank', 'noopener,noreferrer')
    wrapper.unmount()
  })

  it('reopens the current link after authorization is denied', async () => {
    vi.spyOn(api, 'startOAuth').mockResolvedValue(firstOAuth)
    vi.spyOn(api, 'completeOAuth').mockRejectedValue(new APIError('ignored', {
      status: 422,
      code: 'oauth_authorization_denied',
    }))
    const wrapper = await renderWizard()
    await startCurrentOAuth(wrapper)
    await submitCallback(wrapper)
    await wrapper.findAll('.oauth-error-actions button').find((button) => button.text() === '重新打开当前授权页').trigger('click')

    expect(api.startOAuth).toHaveBeenCalledTimes(1)
    expect(window.open).toHaveBeenCalledTimes(2)
    expect(window.open).toHaveBeenLastCalledWith(firstOAuth.authorization_url, '_blank', 'noopener,noreferrer')
    wrapper.unmount()
  })

  it('shows only safe fallback identifiers for an unknown callback error', async () => {
    vi.spyOn(api, 'startOAuth').mockResolvedValue(firstOAuth)
    vi.spyOn(api, 'completeOAuth').mockRejectedValue(new APIError('secret upstream response', {
      status: 422,
      code: 'upstream-secret-code',
      requestId: 'request-unknown-9',
    }))
    const wrapper = await renderWizard()
    await startCurrentOAuth(wrapper)
    await submitCallback(wrapper)

    const alertText = wrapper.get('#oauth-error').text()
    expect(alertText).toContain('授权未完成')
    expect(alertText).toContain('oauth_unknown_error')
    expect(alertText).toContain('request-unknown-9')
    expect(alertText).not.toContain('secret upstream response')
    expect(alertText).not.toContain('upstream-secret-code')
    wrapper.unmount()
  })
})
