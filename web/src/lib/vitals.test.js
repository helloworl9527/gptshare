import { describe, expect, it } from 'vitest'
import { monitorCheckIssue } from './vitals.js'

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
})
