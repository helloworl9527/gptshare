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
    await expect(api.accounts()).rejects.toMatchObject({ status: 401 })
    expect(listener).toHaveBeenCalledOnce()
  })

	it('shows a specific phase-one contract error', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ code: 'phase_one_contract_changed' }, 503)))
		await expect(api.accounts()).rejects.toMatchObject({
			code: 'phase_one_contract_changed',
			message: '一期返回的数据格式已变化，请检查一期服务版本。',
		})
	})

  it('sends CSRF for reveal GET without writing secrets to storage', async () => {
    const storage = vi.spyOn(Storage.prototype, 'setItem')
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ csrf_token: 'c'.repeat(43) }))
      .mockResolvedValueOnce(response({ code: '2345-6789-ABCD', card: { id: 7, plaintext_available: true } }))
    vi.stubGlobal('fetch', fetchMock)
    const result = await api.revealCard(7)
    expect(result.code).toBe('2345-6789-ABCD')
    expect(fetchMock.mock.calls[1][0]).toBe('/api/admin/cards/7/reveal')
    expect(fetchMock.mock.calls[1][1].method).toBe('GET')
    expect(fetchMock.mock.calls[1][1].headers.get('X-CSRF-Token')).toBe('c'.repeat(43))
    expect(storage).not.toHaveBeenCalled()
    clearClientState()
  })
})
