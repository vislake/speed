/**
 * Contract tests for the ApiError envelope and the reserved client.*
 * vocabulary: constructor fields, auth flag, synthesized message
 * defaults, the client.http.* code helper, and the isApiError guard
 * (instanceof plus the structural shape hosts actually branch on).
 */

import { describe, expect, it } from 'vitest'
import {
  ApiError,
  ERROR_CODE_NETWORK,
  ERROR_CODE_PROTOCOL,
  ERROR_CODE_TIMEOUT,
  httpErrorCode,
  isApiError,
  type FieldError,
} from './index'

const FIELD_ERRORS: readonly FieldError[] = [
  { field: 'credits', code: 'billing.insufficient_credits' },
  { field: 'planId', code: 'billing.plan_not_found', params: { id: 'p-1' } },
]

describe('ApiError', () => {
  it('carries the envelope fields through', () => {
    const error = new ApiError({
      status: 400,
      code: 'billing.quota_exceeded',
      traceId: 'trace-1',
      message: 'Quota exceeded',
      params: { planId: 'p-1' },
      details: FIELD_ERRORS,
      attempts: 1,
    })
    expect(error).toBeInstanceOf(ApiError)
    expect(error).toBeInstanceOf(Error)
    expect(error.name).toBe('ApiError')
    expect(error.status).toBe(400)
    expect(error.code).toBe('billing.quota_exceeded')
    expect(error.traceId).toBe('trace-1')
    expect(error.message).toBe('Quota exceeded')
    expect(error.params).toEqual({ planId: 'p-1' })
    expect(error.details).toEqual(FIELD_ERRORS)
    expect(error.attempts).toBe(1)
  })

  it('marks exactly HTTP 401 as auth', () => {
    const refused = new ApiError({
      status: 401,
      code: 'authn.session_expired',
      traceId: 'trace-1',
      attempts: 2,
    })
    expect(refused.auth).toBe(true)

    const other = new ApiError({
      status: 403,
      code: 'rbac.permission_denied',
      traceId: 'trace-2',
      attempts: 1,
    })
    expect(other.auth).toBe(false)
  })

  it('defaults the message for every code without an envelope message', () => {
    const cases: Array<[number, string, string]> = [
      [0, ERROR_CODE_NETWORK, 'The request failed before a response arrived.'],
      [0, ERROR_CODE_TIMEOUT, 'The request timed out before a response arrived.'],
      [200, ERROR_CODE_PROTOCOL, 'The response did not satisfy the API JSON contract.'],
      [502, 'client.http.502', 'The server answered HTTP 502 without a valid ApiError envelope.'],
      [400, 'notes.text_required', 'The API rejected the request (notes.text_required).'],
    ]
    for (const [status, code, expected] of cases) {
      const error = new ApiError({ status, code, attempts: 1 })
      expect(error.message, `default message for ${code}`).toBe(expected)
      expect(error.cause).toBeUndefined()
    }
  })

  it('links the underlying failure as cause', () => {
    const cause = new TypeError('fetch failed')
    const error = new ApiError({
      status: 0,
      code: ERROR_CODE_NETWORK,
      attempts: 2,
      cause,
    })
    expect(error.cause).toBe(cause)
  })

  it('keeps an envelope-provided message even for client.* codes', () => {
    const error = new ApiError({
      status: 401,
      code: 'client.http.401',
      message: 'hand-rolled message wins over the synthesized default',
      attempts: 1,
    })
    expect(error.message).toBe(
      'hand-rolled message wins over the synthesized default',
    )
  })
})

describe('httpErrorCode', () => {
  it('shapes any status into the client.http.<status> vocabulary', () => {
    expect(httpErrorCode(401)).toBe('client.http.401')
    expect(httpErrorCode(503)).toBe('client.http.503')
  })
})

describe('isApiError', () => {
  it('accepts ApiError instances', () => {
    const error = new ApiError({
      status: 503,
      code: 'client.http.503',
      attempts: 3,
    })
    expect(isApiError(error)).toBe(true)
  })

  it('accepts structurally shaped errors (a second copy of the library, a proxied error)', () => {
    const foreign = {
      name: 'ApiError',
      status: 429,
      code: 'client.http.429',
      message: 'The server answered HTTP 429 without a valid ApiError envelope.',
      auth: false,
    }
    expect(isApiError(foreign)).toBe(true)
  })

  it('rejects everything else', () => {
    expect(isApiError(null)).toBe(false)
    expect(isApiError(undefined)).toBe(false)
    expect(isApiError('billing.quota_exceeded')).toBe(false)
    expect(isApiError(new Error('plain'))).toBe(false)
    expect(isApiError({ status: 503 })).toBe(false)
    expect(isApiError({ code: 'x', message: 'm', auth: false })).toBe(false)
    expect(isApiError({ status: 503, code: 'x', auth: false })).toBe(false)
    expect(isApiError({ status: 503, code: 'x', message: 'm' })).toBe(false)
  })

  it('narrows for the canonical catch-site pattern', () => {
    const caught: unknown = new ApiError({
      status: 400,
      code: 'notes.text_required',
      traceId: 'trace-9',
      attempts: 1,
    })
    if (isApiError(caught)) {
      // Narrowed: the fields hosts branch on are readable and typed.
      expect(caught.code).toBe('notes.text_required')
      expect(caught.traceId).toBe('trace-9')
      expect(caught.auth).toBe(false)
    } else {
      expect.unreachable('the fixture above is an ApiError')
    }
  })
})
