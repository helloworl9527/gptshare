import { describe, expect, it } from 'vitest'
import { monitorCheckIssue, summarizeMonitor } from './vitals.js'

describe('monitorCheckIssue', () => {
  it.each(['refresh', 'device'])('explains %s credential refresh rejection with a concrete response', (type) => {
    const issue = monitorCheckIssue({
      last_check_error_code: 'http_401',
      credential: { type },
    })

    expect(issue).toMatchObject({
      badge: '刷新授权 401',
      summary: 'OAuth 刷新被拒绝，需重新授权',
      title: 'OAuth 令牌刷新被拒绝（HTTP 401）',
      code: 'http_401',
    })
    expect(issue.detail).toContain('refresh token')
    expect(issue.detail).toContain('轮换')
    expect(issue.action).toContain('重新授权')
    expect(issue.action).toContain('立即刷新')
  })

  it('distinguishes a rejected access token from a refresh-token failure', () => {
    const issue = monitorCheckIssue({
      last_check_error_code: 'http_401',
      credential: { type: 'access' },
    })

    expect(issue.title).toBe('Access Token 被拒绝（HTTP 401）')
    expect(issue.summary).toContain('重新导入')
    expect(issue.detail).not.toContain('refresh token 被其他程序')
  })

  it('keeps an unknown monitoring code visible for diagnosis', () => {
    const issue = monitorCheckIssue({ last_check_error_code: 'future_error_code' })

    expect(issue.summary).toBe('检查异常 · future_error_code')
    expect(issue.code).toBe('future_error_code')
  })

  it('returns no issue when no monitoring error is present', () => {
    expect(monitorCheckIssue({ last_check_state: 'ok' })).toBeNull()
  })

  it.each([
    ['oauth_invalid_grant', '授权已失效'],
    ['oauth_refresh_token_invalid', '刷新令牌无效'],
    ['oauth_refresh_token_expired', '刷新令牌已过期'],
    ['oauth_session_terminated', '授权会话已终止'],
    ['oauth_refresh_unauthorized', '刷新授权被拒绝'],
    ['oauth_refresh_forbidden', '刷新授权被禁止'],
    ['oauth_refresh_token_missing', '缺少刷新令牌'],
  ])('explains stable OAuth code %s', (code, badge) => {
    const issue = monitorCheckIssue({ last_check_state: 'reauthorization_required', last_check_error_code: code })
    expect(issue.badge).toBe(badge)
    expect(issue.action).toContain('重新授权')
    expect(issue.code).toBe(code)
  })

  it('identifies refresh-token reuse as a detected rotation conflict', () => {
    const issue = monitorCheckIssue({ last_check_error_code: 'oauth_refresh_token_reused' })
    expect(issue.rotationConflict).toBe(true)
    expect(issue.badge).toBe('检测到令牌轮换竞争')
  })

  it('includes reauthorization warnings in abnormal check totals', () => {
    expect(summarizeMonitor([{ status: 'alive', last_check_state: 'reauthorization_required' }]).abnormalChecks).toBe(1)
  })
})
