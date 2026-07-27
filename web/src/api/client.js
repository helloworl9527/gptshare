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
  if (!['GET', 'HEAD', 'OPTIONS'].includes(method) || options.requireCSRF) {
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

function publicMessage(status) {
  if (status === 401) return '登录状态已失效，请重新登录。'
  if (status === 403) return '安全校验未通过，请刷新页面后重试。'
  if (status === 404) return '请求的账号或运行记录不存在。'
  if (status === 409) return '当前操作与账号状态冲突，请刷新后重试。'
  if (status === 422) return '请求参数未通过校验。'
  if (status === 503) return '上游服务暂时不可用，请稍后重试。'
  if (status >= 500) return '服务暂时不可用，请稍后重试。'
  return '请求未完成，请检查输入后重试。'
}

export const api = {
  me: () => request('/api/me'),
  password: (username, password) => request('/api/auth/password', { method: 'POST', body: { username, password }, authRedirect: false }),
  totp: (challenge, code) => request('/api/auth/totp', { method: 'POST', body: { challenge, code }, authRedirect: false }),
  logout: () => request('/api/auth/logout', { method: 'POST' }),
	monitorAccounts: () => request('/api/accounts'),
	monitorAccount: (id) => request(`/api/accounts/${encodeURIComponent(id)}`),
	removeMonitorAccount: (id) => request(`/api/accounts/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  importToken: (body) => request('/api/accounts/import/token', { method: 'POST', body }),
  importTokenBatch: (items) => request('/api/accounts/import/token/batch', { method: 'POST', body: { items } }),
  reauthorizeToken: (id, body) => request(`/api/accounts/${encodeURIComponent(id)}/reauthorize/token`, { method: 'POST', body }),
  startOAuth: (label) => request('/api/accounts/import/oauth/start', { method: 'POST', body: { label } }),
  startOAuthReauthorization: (id) => request(`/api/accounts/${encodeURIComponent(id)}/reauthorize/oauth/start`, { method: 'POST', body: {} }),
  completeOAuth: (id, callbackURL) => request(`/api/accounts/oauth/${encodeURIComponent(id)}/complete`, { method: 'POST', body: { callback_url: callbackURL } }),
  startDevice: (label) => request('/api/accounts/import/device/start', { method: 'POST', body: { label } }),
  startDeviceReauthorization: (id) => request(`/api/accounts/${encodeURIComponent(id)}/reauthorize/device/start`, { method: 'POST', body: {} }),
  pollDevice: (id) => request(`/api/accounts/import/device/${encodeURIComponent(id)}/poll`, { method: 'POST', body: {} }),
  refreshAccount: (id) => request(`/api/accounts/${encodeURIComponent(id)}/refresh`, { method: 'POST', body: {} }),
  pollRun: (id) => request(`/api/poll-runs/${encodeURIComponent(id)}`),
  settings: () => request('/api/settings'),
  updateSettings: (body) => request('/api/settings', { method: 'PUT', body }),
  clearChannelSecret: (channel) => request(`/api/settings/channels/${encodeURIComponent(channel)}/secret`, { method: 'DELETE' }),
	securityBoundaries: () => request('/api/admin/config/security-boundaries'),
	allocationDashboard: () => request('/api/admin/dashboard'),
	allocationAccounts: () => request('/api/admin/accounts'),
	allocationAccount: (id) => request(`/api/admin/accounts/${encodeURIComponent(id)}`),
	accountSettings: () => request('/api/admin/account-settings'),
	updateAccountSettings: (body) => request('/api/admin/account-settings', { method: 'PUT', body }),
	pullMonitorAccounts: () => request('/api/admin/accounts/pull-monitor', { method: 'POST', body: {} }),
	updateAllocationAccount: (id, body) => request(`/api/admin/accounts/${encodeURIComponent(id)}`, { method: 'PUT', body }),
	deleteAllocationAccount: (id) => request(`/api/admin/accounts/${encodeURIComponent(id)}`, { method: 'DELETE' }),
	applyDefaultCapacity: () => request('/api/admin/accounts/apply-default-capacity', { method: 'POST', body: {} }),
	syncAllocationAccounts: () => request('/api/admin/accounts/sync-status', { method: 'POST', body: {} }),
	syncAllocationAccount: (id) => request(`/api/admin/accounts/${encodeURIComponent(id)}/sync-status`, { method: 'POST', body: {} }),
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
}
