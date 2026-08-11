const CALLBACK_INVALID_CODES = new Set(['oauth_callback_invalid', 'validation_failed'])
const SESSION_INVALID_CODES = new Set([
  'oauth_session_expired',
  'oauth_session_used',
  'oauth_session_not_found',
  'oauth_session_invalid',
])
const CREDENTIAL_INCOMPLETE_CODES = new Set([
  'oauth_token_incomplete',
  'credential_status_incomplete',
  'http_401',
  'http_403',
])

const CREDENTIAL_STATUS_ERRORS = {
  credential_account_id_missing: {
    title: '账号身份信息缺失',
    detail: '授权已完成，但状态响应中没有目标账号 ID。请重新授权并确认使用了正确账号。',
  },
  credential_plan_unknown: {
    title: '订阅套餐无法识别',
    detail: '状态响应中的订阅套餐无法验证。请确认账号套餐后重新授权。',
  },
  credential_subscription_expiry_missing: {
    title: '订阅到期时间缺失',
    detail: '状态响应中没有可验证的订阅到期时间。请确认订阅状态后重新授权。',
  },
  credential_subscription_expired: {
    title: '账号订阅已过期',
    detail: '状态响应显示订阅已经到期。请续订后重新授权。',
  },
  credential_account_inactive: {
    title: '账号当前未激活',
    detail: '状态响应显示账号当前不可用。请先恢复账号状态，再重新授权。',
  },
  credential_evidence_unverified: {
    title: '账号状态证据未验证',
    detail: '状态响应未达到实时验证要求。请稍后重新授权；若持续出现，请联系维护人员。',
  },
}

const KNOWN_CODES = new Set([
  ...CALLBACK_INVALID_CODES,
  ...SESSION_INVALID_CODES,
  ...CREDENTIAL_INCOMPLETE_CODES,
  ...Object.keys(CREDENTIAL_STATUS_ERRORS),
  'oauth_state_mismatch',
  'oauth_authorization_denied',
  'provider_account_mismatch',
])

function safeRequestId(value) {
  return typeof value === 'string' && /^[A-Za-z0-9._:-]{1,128}$/.test(value) ? value : ''
}

export function describeOAuthError(reason = {}) {
  const code = typeof reason.code === 'string' ? reason.code : ''
  const requestId = safeRequestId(reason.requestId)

  if (CALLBACK_INVALID_CODES.has(code)) {
    return {
      title: '回调 URL 格式不正确',
      detail: '请粘贴以 http://localhost:1455/auth/callback? 开头，并同时包含 code 和 state 参数的完整 URL。',
      recovery: 'retry_callback',
      code,
      requestId,
    }
  }
  if (code === 'oauth_state_mismatch') {
    return {
      title: '此回调不属于当前授权会话',
      detail: '它可能来自旧标签页或另一次授权。请关闭旧标签页并生成新的授权链接。',
      recovery: 'restart_authorization',
      code,
      requestId,
    }
  }
  if (code === 'oauth_authorization_denied') {
    return {
      title: 'OpenAI 授权已取消或未完成',
      detail: '请重新打开当前授权页并完成授权后，再粘贴新的回调 URL。',
      recovery: 'reopen_authorization',
      code,
      requestId,
    }
  }
  if (SESSION_INVALID_CODES.has(code)) {
    return {
      title: '当前授权会话已失效',
      detail: '此会话已过期、使用过或无法识别。请生成新的授权链接。',
      recovery: 'restart_authorization',
      code,
      requestId,
    }
  }
  if (code === 'provider_account_mismatch') {
    return {
      title: '授权账号与目标账号不一致',
      detail: '当前授权的是另一个 OpenAI 账号，不能替换目标账号。请使用原账号重新开始授权。',
      recovery: 'restart_original_account',
      code,
      requestId,
    }
  }
  if (Object.hasOwn(CREDENTIAL_STATUS_ERRORS, code)) {
    return {
      ...CREDENTIAL_STATUS_ERRORS[code],
      recovery: 'restart_authorization',
      code,
      requestId,
    }
  }
  if (CREDENTIAL_INCOMPLETE_CODES.has(code) || reason.status === 401 || reason.status === 403) {
    return {
      title: '未取得可验证的账号凭证',
      detail: '授权已完成，但账号凭证不完整或无法验证。请确认 OpenAI 账号状态后重新授权。',
      recovery: 'restart_authorization',
      code: KNOWN_CODES.has(code) ? code : 'oauth_credential_unavailable',
      requestId,
    }
  }
  if (code === 'network_error' || reason.status === 503 || reason.status >= 500) {
    return {
      title: '授权服务暂时不可用',
      detail: 'OpenAI 或本地服务暂时不可用，请稍后生成新的授权链接。',
      recovery: 'restart_authorization',
      code: code === 'network_error' ? code : 'oauth_service_unavailable',
      requestId,
    }
  }
  return {
    title: '授权未完成',
    detail: '请根据下方错误码和请求编号联系维护人员。',
    recovery: 'contact_support',
    code: 'oauth_unknown_error',
    requestId,
  }
}
