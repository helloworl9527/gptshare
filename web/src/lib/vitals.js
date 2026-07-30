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
  const abnormalChecks = accounts.filter((item) => ['error', 'verification_required', 'contract_changed'].includes(item.last_check_state))
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
