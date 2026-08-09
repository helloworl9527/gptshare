let csrfToken = ''

export class APIError extends Error {
  constructor(message, { status = 0, code = 'network_error', retryAfter = 0 } = {}) {
    super(message)
    this.name = 'APIError'
    this.status = status
    this.code = code
    this.retryAfter = retryAfter
  }
}

export function clearClientState() {
  csrfToken = ''
}

export async function fetchCSRF() {
  const result = await request('/api/auth/csrf', { authRedirect: false })
  csrfToken = result.csrf_token
  return csrfToken
}

export async function request(path, options = {}) {
  const method = options.method || 'GET'
  const headers = new Headers(options.headers || {})
  const init = { method, headers, credentials: 'include' }
  if (options.body !== undefined) {
    headers.set('Content-Type', 'application/json')
    init.body = JSON.stringify(options.body)
  }
  if (options.requireCSRF || !['GET', 'HEAD', 'OPTIONS'].includes(method)) {
    if (!csrfToken) await fetchCSRF()
    headers.set('X-CSRF-Token', csrfToken)
  }
  let response
  try {
    response = await fetch(path, init)
  } catch {
    throw new APIError('服务暂时不可达，请检查本机服务状态后重试。')
  }
  if (response.status === 401 && options.authRedirect !== false) {
    clearClientState()
    window.dispatchEvent(new CustomEvent('session-expired'))
  }
  if (!response.ok) {
    let payload = {}
    try { payload = await response.json() } catch { /* ignore non-json error bodies */ }
    throw new APIError(publicMessage(response.status, payload.code), {
      status: response.status,
      code: payload.code || 'request_failed',
      retryAfter: Number(response.headers.get('Retry-After') || 0),
    })
  }
  if (response.status === 204) return null
  return response.json()
}

function publicMessage(status, code) {
  if (code === 'phase_one_contract_changed') return '一期返回的数据格式已变化，请检查一期服务版本。'
  if (code === 'phase_one_monitor_timeout') return '一期响应超时，请稍后重试。'
  if (code === 'phase_one_monitor_unavailable') return '一期监控暂时不可用，请稍后重试。'
  if (status === 401) return '登录状态已失效，请重新登录。'
  if (status === 403) return '安全校验未通过，请刷新页面后重试。'
  if (status === 409 && code === 'account_replacement_unavailable') return '备用账号容量不足，无法安全下线；本次操作未产生任何变更。'
  if (status === 409 && code === 'account_allocated') return '该账号仍有分配，暂时无法下线。'
  if (status === 409 && code === 'card_state_conflict') return '当前卡密状态不允许执行该操作。'
  if (status === 422) return '请求参数未通过校验。'
  if (status >= 500) return '服务暂时不可用，请稍后重试。'
  return '请求未完成，请检查输入后重试。'
}

export const api = {
  me: () => request('/api/me'),
  password: (username, password) => request('/api/auth/password', { method: 'POST', body: { username, password }, authRedirect: false }),
  totp: (challenge, code) => request('/api/auth/totp', { method: 'POST', body: { challenge, code }, authRedirect: false }),
  logout: () => request('/api/auth/logout', { method: 'POST' }),
  adminPing: () => request('/api/admin/ping'),
  dashboard: () => request('/api/admin/dashboard'),
  allocations: () => request('/api/admin/allocations'),
  accounts: () => request('/api/admin/accounts'),
  account: (id) => request(`/api/admin/accounts/${encodeURIComponent(id)}`),
  accountSettings: () => request('/api/admin/account-settings'),
  updateAccountSettings: (body) => request('/api/admin/account-settings', { method: 'PUT', body }),
  createAccount: (body) => request('/api/admin/accounts', { method: 'POST', body }),
  pullMonitorAccounts: () => request('/api/admin/accounts/pull-monitor', { method: 'POST', body: {} }),
  updateAccount: (id, body) => request(`/api/admin/accounts/${encodeURIComponent(id)}`, { method: 'PUT', body }),
  deleteAccount: (id) => request(`/api/admin/accounts/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  applyDefaultCapacity: () => request('/api/admin/accounts/apply-default-capacity', { method: 'POST', body: {} }),
  syncAccounts: () => request('/api/admin/accounts/sync-status', { method: 'POST', body: {} }),
  syncAccount: (id) => request(`/api/admin/accounts/${encodeURIComponent(id)}/sync-status`, { method: 'POST', body: {} }),
  cards: (filter = {}) => {
    const params = new URLSearchParams()
    if (filter.status) params.set('status', filter.status)
    if (filter.duration_days) params.set('duration_days', String(filter.duration_days))
    const suffix = params.toString()
    return request(`/api/admin/cards${suffix ? `?${suffix}` : ''}`)
  },
  generateCards: (body) => request('/api/admin/cards/generate', { method: 'POST', body }),
  exportCards: (body) => request('/api/admin/cards/export', { method: 'POST', body }),
  revealCard: (id) => request(`/api/admin/cards/${encodeURIComponent(id)}/reveal`, { requireCSRF: true }),
  revokeCard: (id) => request(`/api/admin/cards/${encodeURIComponent(id)}/revoke`, { method: 'POST', body: {} }),
  extendCard: (id, days) => request(`/api/admin/cards/${encodeURIComponent(id)}/extend`, { method: 'POST', body: { days } }),
  expireDueCards: () => request('/api/admin/cards/expire-due', { method: 'POST', body: {} }),
}
