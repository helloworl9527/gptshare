import { describe, expect, it } from 'vitest'
import { describeOAuthError } from './oauth-error.js'

describe('OAuth callback error descriptions', () => {
  it.each([
    ['oauth_callback_invalid', '回调 URL 格式不正确', 'retry_callback', 'code 和 state'],
    ['validation_failed', '回调 URL 格式不正确', 'retry_callback', 'code 和 state'],
    ['oauth_state_mismatch', '此回调不属于当前授权会话', 'restart_authorization', '旧标签页'],
    ['oauth_authorization_denied', 'OpenAI 授权已取消或未完成', 'reopen_authorization', '重新打开当前授权页'],
    ['oauth_session_expired', '当前授权会话已失效', 'restart_authorization', '已过期'],
    ['oauth_session_used', '当前授权会话已失效', 'restart_authorization', '使用过'],
    ['oauth_session_not_found', '当前授权会话已失效', 'restart_authorization', '无法识别'],
    ['oauth_session_invalid', '当前授权会话已失效', 'restart_authorization', '无法识别'],
    ['provider_account_mismatch', '授权账号与目标账号不一致', 'restart_original_account', '不能替换目标账号'],
    ['oauth_token_incomplete', '未取得可验证的账号凭证', 'restart_authorization', '凭证不完整'],
    ['credential_status_incomplete', '未取得可验证的账号凭证', 'restart_authorization', '无法验证'],
    ['http_401', '未取得可验证的账号凭证', 'restart_authorization', '账号状态'],
    ['http_403', '未取得可验证的账号凭证', 'restart_authorization', '账号状态'],
    ['credential_account_id_missing', '账号身份信息缺失', 'restart_authorization', '目标账号 ID'],
    ['credential_plan_unknown', '订阅套餐无法识别', 'restart_authorization', '订阅套餐'],
    ['credential_subscription_expiry_missing', '订阅到期时间缺失', 'restart_authorization', '订阅到期时间'],
    ['credential_subscription_expired', '账号订阅已过期', 'restart_authorization', '续订'],
    ['credential_account_inactive', '账号当前未激活', 'restart_authorization', '恢复账号状态'],
    ['credential_evidence_unverified', '账号状态证据未验证', 'restart_authorization', '实时验证'],
    ['network_error', '授权服务暂时不可用', 'restart_authorization', '本地服务'],
  ])('maps %s to a safe recovery instruction', (code, title, recovery, detail) => {
    expect(describeOAuthError({ code })).toMatchObject({ code, title, recovery })
    expect(describeOAuthError({ code }).detail).toContain(detail)
  })

  it.each([401, 403])('maps upstream HTTP %s to incomplete credentials', (status) => {
    expect(describeOAuthError({ status, code: 'request_failed' })).toMatchObject({
      title: '未取得可验证的账号凭证',
      recovery: 'restart_authorization',
      code: 'oauth_credential_unavailable',
    })
  })

  it.each([503, 500, 502])('maps HTTP %s to temporary unavailability', (status) => {
    expect(describeOAuthError({ status, code: 'upstream_failure' })).toMatchObject({
      title: '授权服务暂时不可用',
      recovery: 'restart_authorization',
      code: 'oauth_service_unavailable',
    })
  })

  it('uses safe identifiers for an unknown error', () => {
    expect(describeOAuthError({
      code: 'secret-from-upstream',
      requestId: 'request-safe_123',
      message: 'upstream secret',
    })).toEqual({
      title: '授权未完成',
      detail: '请根据下方错误码和请求编号联系维护人员。',
      recovery: 'contact_support',
      code: 'oauth_unknown_error',
      requestId: 'request-safe_123',
    })
  })

  it('drops an unsafe request identifier', () => {
    expect(describeOAuthError({ code: 'unknown', requestId: 'code=secret&state=secret' }).requestId).toBe('')
  })
})
