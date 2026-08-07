export function daysUntil(value, now = Date.now()) {
  if (!value) return null
  const time = new Date(value).getTime()
  if (!Number.isFinite(time)) return null
  return Math.ceil((time - now) / 86400000)
}

export function formatDateTime(value) {
  if (!value) return '—'
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value))
}

export function formatDate(value) {
  if (!value) return '—'
  return new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date(value))
}

export function accountTone(account) {
  if (account.monitor_status === 'dead_banned') return 'red'
  if (account.status === 'pending_credentials') return 'amber'
  if (account.status === 'full' || account.current_allocations >= account.max_concurrent_users) return 'amber'
  if (account.status === 'disabled' || account.monitor_status === 'dead_normal') return 'retired'
  return 'green'
}

export function cardTone(card) {
  if (card.status === 'revoked') return 'red'
  if (card.status === 'expired') return 'retired'
  if (card.status === 'redeemed') return 'green'
  return 'cyan'
}

export function cardValidUntil(card) {
  return card.expires_at || card.valid_until || null
}

export function summarizeAllocation(accounts = [], cards = []) {
  const capacity = accounts.reduce((sum, item) => sum + Number(item.max_concurrent_users || 0), 0)
  const used = accounts.reduce((sum, item) => sum + Number(item.current_allocations || 0), 0)
  const redeemed = cards.filter((item) => item.status === 'redeemed')
  const expiring = redeemed.filter((item) => {
    const days = daysUntil(cardValidUntil(item))
    return days !== null && days <= 3
  })
  return {
    accounts: accounts.length,
    cards: cards.length,
    capacity,
    used,
    availableCapacity: Math.max(0, capacity - used),
    redeemed: redeemed.length,
    unused: cards.filter((item) => item.status === 'unused').length,
    revoked: cards.filter((item) => item.status === 'revoked').length,
    expiring: expiring.length,
    health: capacity === 0 ? 0 : Math.round((Math.max(0, capacity - used) / capacity) * 100),
  }
}

export function summarizeMonitor(accounts = []) {
  const alive = accounts.filter((item) => item.status === 'alive')
  const near = accounts.filter((item) => item.status === 'alive' && item.near_expiry)
  const banned = accounts.filter((item) => item.status === 'dead_banned')
  const retired = accounts.filter((item) => item.status === 'dead_normal')
  const abnormalChecks = accounts.filter((item) => ['error', 'verification_required', 'contract_changed', 'reauthorization_required'].includes(item.last_check_state))
  const survivalDays = banned.map((item) => Number(item.banned_survival_days)).filter(Number.isFinite)
  return {
    total: accounts.length,
    alive: alive.length,
    near: near.length,
    banned: banned.length,
    retired: retired.length,
    abnormalChecks: abnormalChecks.length,
    averageSurvival: survivalDays.length ? Math.round(survivalDays.reduce((sum, value) => sum + value, 0) / survivalDays.length) : '—',
  }
}

export function monitorCheckIssue(account = {}) {
  const code = String(account.last_check_error_code || '').trim()
  if (!code) return null

  const credentialType = String(account.credential?.type || account.token_type || '').toLowerCase()
  const oauthReasons = {
    oauth_invalid_grant: ['授权已失效', 'OAuth 授权已失效（invalid_grant）', '上游已拒绝当前授权。可能是授权被撤销、授权会话失效，或 refresh token 已不再有效。'],
    oauth_refresh_token_reused: ['检测到令牌轮换竞争', 'Refresh Token 已被重复使用', '系统检测到 refresh token 已被使用过。这通常表示另一个刷新请求或外部程序已完成令牌轮换，当前保存的旧令牌不能再次使用。'],
    oauth_refresh_token_invalid: ['刷新令牌无效', 'Refresh Token 已失效', '上游确认当前 refresh token 无效或已撤销，系统已停止继续使用该令牌。'],
    oauth_refresh_token_expired: ['刷新令牌已过期', 'Refresh Token 已过期', '当前 refresh token 已超过有效期，系统已停止无效重试。'],
    oauth_session_terminated: ['授权会话已终止', 'OAuth 会话已终止', '与当前 refresh token 关联的 OAuth 会话已被终止或撤销。'],
    oauth_refresh_unauthorized: ['刷新授权被拒绝', 'OAuth 刷新未经授权（HTTP 401）', 'OAuth Token 端点拒绝了当前刷新凭据，但未返回更具体的稳定原因。'],
    oauth_refresh_forbidden: ['刷新授权被禁止', 'OAuth 刷新被禁止（HTTP 403）', 'OAuth Token 端点禁止当前刷新请求，当前授权需要重新建立。'],
    oauth_refresh_token_missing: ['缺少刷新令牌', '缺少可用的 Refresh Token', 'Access Token 已需要刷新，但保存的凭据中没有可用于续期的 refresh token。'],
  }
  if (oauthReasons[code]) {
    const [badge, title, detail] = oauthReasons[code]
    return {
      badge,
      summary: `${badge}，需重新授权`,
      title,
      detail,
      action: '请选择 OAuth、令牌或设备码重新授权入口；授权成功后系统会恢复自动轮询。',
      rotationConflict: code === 'oauth_refresh_token_reused',
      code,
    }
  }
  if (code === 'http_401') {
    if (credentialType === 'refresh' || credentialType === 'device') {
      return {
        badge: '刷新授权 401',
        summary: 'OAuth 刷新被拒绝，需重新授权',
        title: 'OAuth 令牌刷新被拒绝（HTTP 401）',
        detail: '系统在使用 refresh token 换取新 access token 时被上游拒绝。常见原因是同一 refresh token 被其他程序刷新后发生轮换、继续复用旧令牌，或上游撤销了当前授权会话。',
        action: '请按原授权方式重新授权，完成后执行“立即刷新”。不要在其他程序中继续使用原 refresh token。',
        rotationConflict: false,
        code,
      }
    }
    if (credentialType === 'access') {
      return {
        badge: 'Access 401',
        summary: 'Access Token 被拒绝，需重新导入',
        title: 'Access Token 被拒绝（HTTP 401）',
        detail: '上游账号检查接口不再接受当前 access token。该令牌可能已过期、被撤销，或不具备当前接口所需权限。',
        action: '请重新导入有效令牌，完成后执行“立即刷新”。',
        code,
      }
    }
    if (credentialType === 'session') {
      return {
        badge: '会话授权 401',
        summary: '网页登录会话被拒绝，需重新授权',
        title: '网页登录会话被拒绝（HTTP 401）',
        detail: '上游不再接受当前网页登录会话，或无法用该会话换取有效 access token。常见于退出登录、修改安全设置或会话被撤销。',
        action: '请重新导入当前有效的会话令牌，完成后执行“立即刷新”。',
        code,
      }
    }
    return {
      badge: '授权 401',
      summary: '上游拒绝当前授权，需重新授权',
      title: '上游拒绝当前授权（HTTP 401）',
      detail: '当前凭据未通过上游身份校验，但上游没有返回可进一步区分的稳定错误码。',
      action: '请按原授权方式重新授权，完成后执行“立即刷新”。',
      code,
    }
  }

  return {
    badge: '检查异常',
    summary: `检查异常 · ${code}`,
    title: '上次账号检查异常',
    detail: '系统已保留最近一次可信业务状态。请根据下方原始错误码排查，避免将检查故障误判为账号失效。',
    action: '可先执行“立即刷新”；若错误持续出现，再检查授权状态或上游服务。',
    code,
  }
}

export function statusVisual(account) {
  if (account.status === 'dead_banned') return 'banned'
  if (account.status === 'dead_normal') return 'retired'
  return account.near_expiry ? 'near' : 'alive'
}

export function sortAccounts(accounts = [], mode = 'status') {
  const rank = { dead_banned: 0, alive: 1, dead_normal: 2 }
  return [...accounts].sort((left, right) => {
    if (mode === 'expiry') return timestamp(left.current_expiry || left.auth_expiry) - timestamp(right.current_expiry || right.auth_expiry)
    if (mode === 'email') return String(left.email || '').localeCompare(String(right.email || ''))
    return (rank[left.status] ?? 9) - (rank[right.status] ?? 9) || String(left.label || left.provider_account_id).localeCompare(String(right.label || right.provider_account_id))
  })
}

function timestamp(value) {
  if (!value) return Number.MAX_SAFE_INTEGER
  const time = new Date(value).getTime()
  return Number.isFinite(time) ? time : Number.MAX_SAFE_INTEGER
}
