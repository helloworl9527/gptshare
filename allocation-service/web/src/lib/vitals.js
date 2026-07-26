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

export function summarize(accounts = [], cards = []) {
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

export function derivedAllocations(accounts = [], cards = []) {
  const activeAccounts = accounts.filter((item) => Number(item.current_allocations || 0) > 0)
  return cards
    .filter((card) => card.status === 'redeemed')
    .map((card, index) => {
      const account = activeAccounts[index % Math.max(1, activeAccounts.length)]
      return {
        id: card.id,
        card_id: card.id,
        account: account?.display_username || '容量占用账号',
        state: 'primary',
        allocated_at: card.redeemed_at,
        valid_until: cardValidUntil(card),
        code_suffix: card.code_suffix,
      }
    })
}
