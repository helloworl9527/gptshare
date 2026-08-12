export function decodeBase32(value) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  const normalized = String(value || '').toUpperCase().replace(/\s+/g, '')
  if (!normalized || !/^[A-Z2-7]+=*$/.test(normalized)) throw new Error('invalid base32')
  const clean = normalized.replace(/=+$/, '')
  let accumulator = 0
  let bitCount = 0
  const bytes = []
  for (const character of clean) {
    accumulator = (accumulator << 5) | alphabet.indexOf(character)
    bitCount += 5
    while (bitCount >= 8) {
      bitCount -= 8
      bytes.push((accumulator >>> bitCount) & 0xff)
    }
  }
  if (bytes.length === 0) throw new Error('invalid base32')
  return new Uint8Array(bytes)
}

export async function generateTOTP(secret, timestampMs, options = {}) {
  const period = options.period || 30
  const digits = options.digits || 6
  const algorithm = String(options.algorithm || 'SHA1').toUpperCase()
  if (period !== 30 || digits !== 6 || algorithm !== 'SHA1') throw new Error('unsupported totp parameters')

  const counter = Math.floor(timestampMs / 1000 / period)
  const buffer = new ArrayBuffer(8)
  const view = new DataView(buffer)
  view.setUint32(0, Math.floor(counter / 0x100000000))
  view.setUint32(4, counter >>> 0)
  const key = await crypto.subtle.importKey('raw', decodeBase32(secret), { name: 'HMAC', hash: 'SHA-1' }, false, ['sign'])
  const signature = new Uint8Array(await crypto.subtle.sign('HMAC', key, buffer))
  const offset = signature[signature.length - 1] & 0x0f
  const binary = ((signature[offset] & 0x7f) << 24)
    | ((signature[offset + 1] & 0xff) << 16)
    | ((signature[offset + 2] & 0xff) << 8)
    | (signature[offset + 3] & 0xff)
  return String(binary % (10 ** digits)).padStart(digits, '0')
}

export function secondsUntilNextPeriod(timestampMs, period = 30) {
  const elapsed = Math.floor(timestampMs / 1000) % period
  return period - elapsed
}
