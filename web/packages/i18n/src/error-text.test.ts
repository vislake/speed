/**
 * The error-code text convention: a listed code resolves to its own
 * leaf under the errors section, anything unlisted to the shared
 * unknown fallback, and the resolver is bound to the passed translation
 * function and whitelist alone.
 */

import { describe, expect, it, vi } from 'vitest'
import {
  CLIENT_TRANSPORT_ERROR_CODES,
  SESSION_LIFECYCLE_ERROR_CODES,
  createErrorTextResolver,
} from './error-text.js'

describe('createErrorTextResolver', () => {
  it('resolves a listed code to its own errors-section leaf', () => {
    const t = vi.fn((key: string) => `translated:${key}`)
    const resolve = createErrorTextResolver(
      t,
      new Set(['authn.session_revoked']),
    )
    expect(resolve('authn.session_revoked')).toBe(
      'translated:errors.authn.session_revoked',
    )
    expect(t).toHaveBeenCalledWith('errors.authn.session_revoked')
  })

  it('resolves an unlisted code to the shared unknown fallback', () => {
    const t = vi.fn((key: string) => `translated:${key}`)
    const resolve = createErrorTextResolver(t, new Set(['authn.session_revoked']))
    expect(resolve('authn.future_code')).toBe('translated:errors.unknown')
  })

  it('treats an empty whitelist as listing nothing', () => {
    const t = vi.fn((key: string) => key)
    const resolve = createErrorTextResolver(t, new Set())
    expect(resolve('client.network')).toBe('errors.unknown')
  })
})

describe('the shared code vocabulary', () => {
  it('names the session-lifecycle family every surface shares', () => {
    expect([...SESSION_LIFECYCLE_ERROR_CODES]).toEqual([
      'authn.session_not_found',
      'authn.session_revoked',
      'authn.token_expired',
      'authn.refresh_token_invalid',
      'authn.refresh_token_reused',
    ])
  })

  it('names the transport codes of the api-client contract', () => {
    expect([...CLIENT_TRANSPORT_ERROR_CODES]).toEqual([
      'client.network',
      'client.timeout',
      'client.protocol',
    ])
  })
})
