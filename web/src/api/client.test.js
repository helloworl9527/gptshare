import { describe, expect, it, vi } from 'vitest'
import { api, clearClientState, fetchCSRF, request } from './client.js'

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

	it('shows a specific phase-one contract error', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ code: 'phase_one_contract_changed' }, 503)))
		await expect(api.allocationAccounts()).rejects.toMatchObject({
			code: 'phase_one_contract_changed',
			message: '一期返回的数据格式已变化，请检查一期服务版本。',
		})
	})

	it('shows dedicated safe-retirement conflict messages', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ code: 'account_replacement_unavailable' }, 409)))
		await expect(api.allocationAccounts()).rejects.toMatchObject({
			code: 'account_replacement_unavailable',
			message: '备用账号容量不足，无法安全下线；本次操作未产生任何变更。',
		})
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ code: 'account_allocated' }, 409)))
		await expect(api.allocationAccounts()).rejects.toMatchObject({
			code: 'account_allocated',
			message: '该账号仍有分配，暂时无法下线。',
		})
	})

  it('keeps the generic message for non-OAuth validation errors and exposes the request ID', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({
      code: 'validation_failed',
      message: 'untrusted upstream detail',
      request_id: 'request-422',
    }, 422)))
    await expect(request('/api/example')).rejects.toMatchObject({
      status: 422,
      code: 'validation_failed',
      requestId: 'request-422',
      message: '请求参数未通过校验。',
    })
  })
})
