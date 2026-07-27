import { describe, expect, it } from 'vitest'
import { normalizeJSONBatch, normalizeLineBatch } from './import-normalize.js'

describe('batch token normalization', () => {
  it('trims lines and ignores blank input lines', () => {
    expect(normalizeLineBatch(' first \n\nsecond\r\n', 'refresh_token')).toEqual([
      { refresh_token: 'first' },
      { refresh_token: 'second' },
    ])
  })

  it('accepts mixed credential types in a JSON array', () => {
    expect(normalizeJSONBatch(JSON.stringify([
      { label: ' One ', access_token: ' a ' },
      { refresh_token: 'b' },
      { session_token: 'c' },
    ]))).toEqual([
      { label: 'One', access_token: 'a' },
      { refresh_token: 'b' },
      { session_token: 'c' },
    ])
  })

  it('rejects multiple credential fields, unknown fields, and more than 50 items', () => {
    expect(() => normalizeJSONBatch('[{"access_token":"a","refresh_token":"b"}]')).toThrow()
    expect(() => normalizeJSONBatch('[{"access_token":"a","unknown":"b"}]')).toThrow()
    expect(() => normalizeLineBatch(Array.from({ length: 51 }, (_, index) => `token-${index}`).join('\n'), 'access_token')).toThrow()
  })
})
