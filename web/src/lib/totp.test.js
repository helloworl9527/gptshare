import { describe, expect, it } from 'vitest'
import { decodeBase32, generateTOTP, secondsUntilNextPeriod } from './totp.js'

describe('TOTP', () => {
  it('generates the RFC 6238 SHA1 value with six digits', async () => {
    await expect(generateTOTP('GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ', 59_000)).resolves.toBe('287082')
  })

  it('rejects malformed base32 without producing a code', () => {
    expect(() => decodeBase32('NOT-A-SECRET!')).toThrow('invalid base32')
  })

  it('computes the countdown from a corrected server timestamp', () => {
    expect(secondsUntilNextPeriod(25_000)).toBe(5)
    expect(secondsUntilNextPeriod(30_000)).toBe(30)
  })
})
