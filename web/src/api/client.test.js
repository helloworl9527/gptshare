import { describe, expect, it, vi } from 'vitest'
import { api, clearClientState, fetchCSRF } from './client.js'

function response(body, status = 200, headers = {}) {
  return { ok: status >= 200 && status < 300, status, headers: new Headers(headers), json: async () => body }
}

describe('API client', () => {
  it('uses in-memory CSRF and cookie credentials without browser token storage', async () => {
    const storage = vi.spyOn(Storage.prototype, 'setItem')
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({ challenge: 'challenge', expires_in: 120 }))
    vi.stubGlobal('fetch', fetchMock)
    await fetchCSRF()
    await api.password('admin', 'password')
    expect(fetchMock.mock.calls[1][1].credentials).toBe('include')
    expect(fetchMock.mock.calls[1][1].headers.get('X-CSRF-Token')).toBe('c'.repeat(43))
    expect(storage).not.toHaveBeenCalled()
    clearClientState()
  })

  it('clears presentation state and emits session-expired on 401', async () => {
    const listener = vi.fn()
    window.addEventListener('session-expired', listener, { once: true })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ code: 'unauthorized' }, 401)))
    await expect(api.monitorAccounts()).rejects.toMatchObject({ status: 401 })
    expect(listener).toHaveBeenCalledOnce()
  })

  it('explains duplicate account conflicts', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ code: 'provider_account_exists' }, 409)))
    await expect(api.monitorAccounts()).rejects.toMatchObject({
      status: 409,
      code: 'provider_account_exists',
      message: '账号已存在，无需重新导入',
    })
  })
})
