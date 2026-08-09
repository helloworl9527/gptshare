import http from 'node:http'

const csrf = 'test-csrf-token-with-sufficient-entropy-0000001'
let scenario = 'default'
let accounts = []
let cards = []
let defaultAccountCapacity = 3
const monitorAccounts = [
  { id: 101, label: 'North Star', provider_account_id: 'acct-north', email: 'north@example.test', plan: 'plus', auth_expiry: '2026-08-19T00:00:00Z', current_expiry: '2026-08-19T00:00:00Z', last_authorized_at: '2026-07-20T00:00:00Z', last_alive_at: '2026-07-24T00:00:00Z', last_check_state: 'ok', status: 'alive', near_expiry: false, credential: { type: 'session_token', configured: true }, polling_paused: false },
  { id: 102, label: 'Amber Lab', provider_account_id: 'acct-amber', email: 'amber@example.test', plan: 'team', auth_expiry: '2026-07-26T00:00:00Z', current_expiry: '2026-07-26T00:00:00Z', last_check_state: 'ok', status: 'alive', near_expiry: true },
  { id: 103, label: 'Banned', provider_account_id: 'acct-banned', email: 'banned@example.test', plan: 'plus', last_check_state: 'verification_required', status: 'dead_banned', banned_survival_days: 21 },
  { id: 104, label: 'Device Refresh Alert', provider_account_id: 'acct-device-alert', email: 'device-alert@example.test', plan: 'plus', auth_expiry: '2026-08-26T00:00:00Z', current_expiry: '2026-08-26T00:00:00Z', last_authorized_at: '2026-07-26T00:00:00Z', last_alive_at: '2026-08-05T00:00:00Z', last_check_state: 'error', last_check_error_code: 'http_401', status: 'alive', near_expiry: false, credential: { type: 'device', configured: true }, polling_paused: false },
]
const monitorSettings = { poll_interval: 3600, near_expiry_days: 3, channels: { telegram: { enabled: false, configured: false }, wecom: { enabled: false, configured: true }, feishu: { enabled: false, configured: false } } }

function resetData() {
  defaultAccountCapacity = 3
  accounts = [
    { id: 1, display_username: 'north@example.test', account_expiry: '2026-08-19T00:00:00Z', max_concurrent_users: 3, current_allocations: 1, monitor_status: 'alive', status: 'available', monitor_account_id: 'mon-north' },
    { id: 2, display_username: 'amber-allocation@example.test', account_expiry: '2026-07-28T00:00:00Z', max_concurrent_users: 2, current_allocations: 2, monitor_status: 'unknown_monitor', status: 'full' },
    { id: 3, display_username: 'banned@example.test', account_expiry: '2026-08-03T00:00:00Z', max_concurrent_users: 2, current_allocations: 0, monitor_status: 'dead_banned', status: 'available' },
  ]
  cards = [
    { id: 1, code_suffix: 'ABCD', duration_days: 7, status: 'unused', created_at: '2026-07-24T00:00:00Z', updated_at: '2026-07-24T00:00:00Z' },
    { id: 2, code_suffix: 'EFGH', duration_days: 30, status: 'redeemed', redeemed_at: '2026-07-24T01:00:00Z', expires_at: '2026-08-23T00:00:00Z', created_at: '2026-07-24T00:00:00Z', updated_at: '2026-07-24T01:00:00Z' },
    { id: 3, code_suffix: 'JKLM', duration_days: 90, status: 'revoked', revoked_at: '2026-07-24T02:00:00Z', created_at: '2026-07-24T00:00:00Z', updated_at: '2026-07-24T02:00:00Z' },
  ]
}
resetData()

function send(response, status, body, headers = {}) {
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', ...headers })
  response.end(body === null ? '' : JSON.stringify(body))
}

function readJSON(request) {
  return new Promise((resolve) => {
    let value = ''
    request.on('data', (chunk) => { value += chunk })
    request.on('end', () => {
      try { resolve(value ? JSON.parse(value) : {}) } catch { resolve({}) }
    })
  })
}

function authenticated(request) {
  return request.headers.cookie?.includes('mock_session=active') && scenario !== 'session-expired'
}

function dashboardPayload(sourceAccounts = accounts, sourceCards = cards) {
  const eligible = sourceAccounts.filter((item) => item.monitor_status !== 'dead_banned')
  const capacity = eligible.reduce((sum, item) => sum + Number(item.max_concurrent_users || 0), 0)
  const used = eligible.reduce((sum, item) => sum + Number(item.current_allocations || 0), 0)
  const available = Math.max(0, capacity - used)
  const redeemed = sourceCards.filter((item) => item.status === 'redeemed' && item.redeemed_at).length
  const rate = redeemed / 7
  const days = rate > 0 ? available / rate : null
  const level = available === 0 ? 'exhausted' : days === null || days > 15 ? 'safe' : days >= 7 ? 'notice' : 'urgent'
  const label = { exhausted: '耗尽', safe: '安全', notice: '注意', urgent: '紧急' }[level]
  return {
    capacity,
    used,
    available_capacity: available,
    eligible_accounts: eligible.length,
    unused_cards: sourceCards.filter((item) => item.status === 'unused').length,
    redeemed_last_7_days: redeemed,
    daily_redemption_rate: rate,
    days_to_exhaust: days,
    days_to_exhaust_label: days === null ? '∞' : days.toFixed(1),
    recommended_account_add: 0,
    warning_level: level,
    warning_label: label,
    thresholds: { safe_gt_days: 15, notice_min_days: 7, notice_max_days: 15, urgent_lt_days: 7, exhausted_capacity: 0 },
  }
}

function protect(request, response) {
  if (!authenticated(request)) {
    send(response, 401, { code: 'unauthorized' })
    return false
  }
  return true
}

const server = http.createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1:4174')
  if (url.pathname === '/healthz') return send(response, 200, { status: 'ok' })
  if (url.pathname === '/__test/scenario' && request.method === 'POST') {
    const body = await readJSON(request)
    scenario = body.name || 'default'
    if (body.reset) resetData()
    return send(response, 204, null)
  }
  if (url.pathname === '/api/auth/csrf' && request.method === 'GET') return send(response, 200, { csrf_token: csrf }, { 'Set-Cookie': `mock_csrf=${csrf}; Path=/; SameSite=Strict` })
  if (url.pathname === '/api/auth/password' && request.method === 'POST') {
    await readJSON(request)
    if (request.headers['x-csrf-token'] !== csrf) return send(response, 403, { code: 'csrf_rejected' })
    return send(response, 200, { challenge: 'opaque-challenge', expires_in: 120 })
  }
  if (url.pathname === '/api/auth/totp' && request.method === 'POST') {
    const body = await readJSON(request)
    if (body.code !== '123456') return send(response, 401, { code: 'invalid_credentials' })
    return send(response, 204, null, { 'Set-Cookie': 'mock_session=active; Path=/; HttpOnly; SameSite=Strict' })
  }
  if (url.pathname === '/api/auth/logout' && request.method === 'POST') return send(response, 204, null, { 'Set-Cookie': 'mock_session=; Path=/; Max-Age=0' })
  if (url.pathname === '/api/me') return authenticated(request) ? send(response, 200, { username: 'admin' }) : send(response, 401, { code: 'unauthorized' })
  if (url.pathname === '/api/accounts' && request.method === 'GET') { if (!protect(request, response)) return; return send(response, 200, { accounts: scenario === 'empty' ? [] : monitorAccounts }) }
  const monitorAccount = url.pathname.match(/^\/api\/accounts\/(\d+)$/)
  if (monitorAccount && request.method === 'GET') {
    if (!protect(request, response)) return
    const item = monitorAccounts.find((account) => account.id === Number(monitorAccount[1]))
    return item ? send(response, 200, item) : send(response, 404, { code: 'account_not_found' })
  }
  if (url.pathname === '/api/settings' && request.method === 'GET') { if (!protect(request, response)) return; return send(response, 200, monitorSettings) }
  if (url.pathname === '/api/settings' && request.method === 'PUT') { if (!protect(request, response)) return; Object.assign(monitorSettings, await readJSON(request)); return send(response, 200, monitorSettings) }
  if (url.pathname === '/api/admin/config/security-boundaries' && request.method === 'GET') {
    if (!protect(request, response)) return
    return send(response, 200, { key_material_exposed: false, groups: [
      { id: 'unified_admin_auth', configuration: ['ADMIN_PASSWORD_HASH', 'ADMIN_TOTP_SECRET', 'JWT_SIGNING_KEY', 'RATE_LIMIT_KEY'] },
      { id: 'monitor_data_encryption', configuration: ['CREDENTIAL_MASTER_KEYS', 'CREDENTIAL_ACTIVE_KEY_ID'] },
      { id: 'allocation_data_encryption', configuration: ['ALLOCATION_CREDENTIAL_MASTER_KEYS', 'ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID'] },
    ] })
  }
  if (url.pathname === '/api/admin/ping') { if (!protect(request, response)) return; return send(response, 200, { status: 'ok' }) }
  if (url.pathname === '/api/admin/dashboard' && request.method === 'GET') { if (!protect(request, response)) return; if (scenario === 'empty') return send(response, 200, { dashboard: dashboardPayload([], []) }); if (scenario === 'error') return send(response, 503, { code: 'temporary_unavailable' }); return send(response, 200, { dashboard: dashboardPayload() }) }
  if (url.pathname === '/api/admin/allocations' && request.method === 'GET') {
    if (!protect(request, response)) return
    if (scenario === 'empty') return send(response, 200, { allocations: [] })
    if (scenario === 'error') return send(response, 503, { code: 'temporary_unavailable' })
    return send(response, 200, { allocations: [{
      id: 1, card_id: 2, code_suffix: 'EFGH', duration_days: 30,
      account_id: 1, display_username: 'north@example.test', account_expiry: '2026-08-19T00:00:00Z',
      allocation_state: 'primary', active: true, allocated_at: '2026-07-24T01:00:00Z', valid_until: '2026-08-23T00:00:00Z',
    }] })
  }
  if (url.pathname === '/api/admin/accounts' && request.method === 'GET') { if (!protect(request, response)) return; if (scenario === 'empty') return send(response, 200, { accounts: [] }); if (scenario === 'error') return send(response, 503, { code: 'temporary_unavailable' }); return send(response, 200, { accounts }) }
  if (url.pathname === '/api/admin/account-settings' && request.method === 'GET') { if (!protect(request, response)) return; return send(response, 200, { settings: { default_account_capacity: defaultAccountCapacity } }) }
  if (url.pathname === '/api/admin/account-settings' && request.method === 'PUT') { if (!protect(request, response)) return; const body = await readJSON(request); defaultAccountCapacity = Number(body.default_account_capacity); return send(response, 200, { settings: { default_account_capacity: defaultAccountCapacity } }) }
  if (url.pathname === '/api/admin/accounts/pull-monitor' && request.method === 'POST') {
    if (!protect(request, response)) return
    await readJSON(request)
    const existing = accounts.find((item) => item.monitor_account_id === 'mon-pulled')
    if (existing) {
      Object.assign(existing, {
        display_username: 'pulled-sync@example.test',
        account_expiry: '2026-08-20T00:00:00Z',
        monitor_status: 'alive',
      })
      return send(response, 200, { accounts: [existing], created: 0, updated: 1 })
    }
    const account = {
      id: accounts.length + 10,
      display_username: 'pulled-sync@example.test',
      account_expiry: '2026-08-20T00:00:00Z',
      max_concurrent_users: defaultAccountCapacity,
      current_allocations: 0,
      monitor_status: 'alive',
      status: 'pending_credentials',
      monitor_account_id: 'mon-pulled',
    }
    accounts.push(account)
    return send(response, 200, { accounts: [account], created: 1, updated: 0 })
  }
  if (url.pathname === '/api/admin/accounts/apply-default-capacity' && request.method === 'POST') { if (!protect(request, response)) return; accounts = accounts.map((item) => ({ ...item, max_concurrent_users: defaultAccountCapacity, status: item.current_allocations >= defaultAccountCapacity ? 'full' : item.status === 'full' ? 'available' : item.status })); return send(response, 200, { default_account_capacity: defaultAccountCapacity, updated_accounts: accounts.length }) }
  const accountAction = url.pathname.match(/^\/api\/admin\/accounts\/(\d+)\/sync-status$/)
  if (accountAction && request.method === 'POST') { if (!protect(request, response)) return; return send(response, 200, { account: accounts.find((item) => item.id === Number(accountAction[1])) || accounts[0], warnings: [] }) }
  const account = url.pathname.match(/^\/api\/admin\/accounts\/(\d+)$/)
  if (account && request.method === 'PUT') {
    if (!protect(request, response)) return
    const body = await readJSON(request)
    const existing = accounts.find((item) => item.id === Number(account[1]))
    if (!existing) return send(response, 404, { code: 'not_found' })
    Object.assign(existing, {
      display_username: body.display_username,
      account_expiry: body.account_expiry,
      max_concurrent_users: body.max_concurrent_users,
      status: existing.status === 'pending_credentials' && body.display_password && body.display_2fa_secret ? 'available' : body.status,
      monitor_status: body.monitor_status,
      monitor_account_id: body.monitor_account_id,
      source_url: body.source_url,
    })
    return send(response, 200, { account: existing })
  }
  if (account && request.method === 'DELETE') { if (!protect(request, response)) return; accounts = accounts.filter((item) => item.id !== Number(account[1])); return send(response, 200, { archived: true, replaced_allocations: 0, closed_allocations: 0, request_id: 'mock-retire-request' }) }
  if (url.pathname === '/api/admin/accounts/sync-status' && request.method === 'POST') { if (!protect(request, response)) return; return send(response, 200, { accounts, warnings: [], total: accounts.length, ok: accounts.length, failed: 0 }) }

  if (url.pathname === '/api/admin/cards' && request.method === 'GET') {
    if (!protect(request, response)) return
    const status = url.searchParams.get('status')
    if (scenario === 'empty') return send(response, 200, { cards: [] })
    return send(response, 200, { cards: status ? cards.filter((item) => item.status === status) : cards })
  }
  if (url.pathname === '/api/admin/cards/generate' && request.method === 'POST') {
    if (!protect(request, response)) return
    const body = await readJSON(request)
    const generated = Array.from({ length: Number(body.quantity || 1) }, (_, index) => ({ id: 20 + index, code: `2345-6789-ABC${index}`, code_suffix: `ABC${index}`, duration_days: body.duration_days, status: 'unused' }))
    cards.push(...generated.map((item) => ({ id: item.id, code_suffix: item.code_suffix, duration_days: item.duration_days, status: item.status, created_at: '2026-07-24T03:00:00Z', updated_at: '2026-07-24T03:00:00Z' })))
    return send(response, 201, { cards: generated })
  }
  if (url.pathname === '/api/admin/cards/export' && request.method === 'POST') { if (!protect(request, response)) return; await readJSON(request); return send(response, 200, { exported: true }) }
  const revoke = url.pathname.match(/^\/api\/admin\/cards\/(\d+)\/revoke$/)
  if (revoke && request.method === 'POST') { if (!protect(request, response)) return; const card = cards.find((item) => item.id === Number(revoke[1])); if (card) card.status = 'revoked'; return send(response, 200, { card }) }
  const extend = url.pathname.match(/^\/api\/admin\/cards\/(\d+)\/extend$/)
  if (extend && request.method === 'POST') { if (!protect(request, response)) return; const card = cards.find((item) => item.id === Number(extend[1])); if (card) card.expires_at = '2026-09-01T00:00:00Z'; return send(response, 200, { card }) }
  const reveal = url.pathname.match(/^\/api\/admin\/cards\/(\d+)\/reveal$/)
  if (reveal && request.method === 'GET') {
    if (!protect(request, response)) return
    if (request.headers['x-csrf-token'] !== csrf) return send(response, 403, { code: 'csrf_rejected' })
    const card = cards.find((item) => item.id === Number(reveal[1]))
    if (!card) return send(response, 404, { code: 'not_found' })
    if (card.id === 3) return send(response, 200, { card: { id: card.id, code_suffix: card.code_suffix, plaintext_available: false }, message: '明文不可用(旧批次)' })
    return send(response, 200, { card: { id: card.id, code_suffix: card.code_suffix, plaintext_available: true }, code: `2345-6789-${card.code_suffix}` })
  }
  if (url.pathname === '/api/redeem' && request.method === 'POST') {
    await readJSON(request)
    if (scenario === 'expired-card') return send(response, 404, { code: 'not_found' })
    return send(response, 200, { redeemed: true })
  }
  if (url.pathname === '/api/cards/query' && request.method === 'POST') {
    await readJSON(request)
    if (scenario === 'expired-card') return send(response, 404, { code: 'query_not_available' })
    return send(response, 200, { result: { account: { display_username: 'public@example.test', password: 'public-test-password' }, totp: { secret: 'JBSWY3DPEHPK3PXP' }, card: { valid_until: '2026-08-23T00:00:00Z' }, replacement_notice: { state: 'primary' } } })
  }
  return send(response, 404, { code: 'not_found' })
})

server.listen(4174, '127.0.0.1')
for (const signal of ['SIGTERM', 'SIGINT']) process.on(signal, () => server.close(() => process.exit(0)))
