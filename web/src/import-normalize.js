const credentialFields = ['access_token', 'refresh_token', 'session_token']
const allowedFields = new Set(['label', ...credentialFields])

export function normalizeLineBatch(raw, credentialType) {
  if (!credentialFields.includes(credentialType)) throw new Error('不支持的凭证类型。')
  return validateBatchSize(String(raw).split(/\r?\n/u)
    .map((value) => value.trim())
    .filter(Boolean)
    .map((value) => ({ [credentialType]: value })))
}

export function normalizeJSONBatch(raw) {
  let parsed
  try {
    parsed = JSON.parse(raw)
  } catch {
    throw new Error('JSON 格式无效，请提供对象数组。')
  }
  if (!Array.isArray(parsed)) throw new Error('JSON 顶层必须是数组。')
  const items = parsed.map((item) => {
    if (!item || typeof item !== 'object' || Array.isArray(item)) throw new Error('数组中的每一项必须是对象。')
    if (Object.keys(item).some((key) => !allowedFields.has(key))) throw new Error('JSON 项包含不支持的字段。')
    const credentials = credentialFields.filter((key) => typeof item[key] === 'string' && item[key].trim())
    if (credentials.length !== 1) throw new Error('每个 JSON 项必须且只能包含一种凭证。')
    if (item.label !== undefined && typeof item.label !== 'string') throw new Error('账号标签必须是字符串。')
    return {
      ...(item.label?.trim() ? { label: item.label.trim() } : {}),
      [credentials[0]]: item[credentials[0]].trim(),
    }
  })
  return validateBatchSize(items)
}

function validateBatchSize(items) {
  if (items.length === 0) throw new Error('请至少输入一个凭证。')
  if (items.length > 50) throw new Error('每批最多导入 50 个账号。')
  return items
}
