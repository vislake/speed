/**
 * Tests for config-fetcher.ts. Every call goes through a real
 * createClient wired to the deterministic fetch stand-in -- never a
 * real network -- exercising fetchPublicConfig / fetchSystemFeatures
 * exactly as a consumer would use them: pass the RequestFn, get back
 * a typed response or a rejected ApiError/AbortError.
 *
 * A note on the envelope tests below: client.ts's parseEnvelope treats
 * `code` as the envelope's only required field and `traceId` as
 * optional, so the two wire shapes a consumer can actually meet are
 * both exercised -- the traceId-bearing bodies of the "surfaces a
 * non-2xx envelope ... with its code" tests (a backend that sends a
 * trace id, for correlation) and the bare `{code, params}` bodies of
 * the "carries the real go/config error shape" tests, which pin the
 * literal go/config wire shape: go/config/http.go's `errorEnvelope`
 * encodes only `{code, params}`, never a `traceId`. A real failure
 * against either shape must surface the module's real code.
 */

import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  createStandinFetch,
  hang,
  jsonResponse,
  scriptedStandin,
  textResponse,
} from '../test-utils/fetch-standin'
import { createClient } from './client.js'
import {
  CONFIG_PUBLIC_PATH,
  SYSTEM_FEATURES_PATH,
  fetchPublicConfig,
  fetchSystemFeatures,
} from './config-fetcher.js'
import { ERROR_CODE_PROTOCOL, isApiError } from './errors.js'

describe('fetchPublicConfig', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('GETs CONFIG_PUBLIC_PATH and round-trips the typed body', async () => {
    const standin = createStandinFetch(() =>
      jsonResponse(200, {
        config: { 'brand.name': 'Speed', 'brand.primaryColor': '#123456' },
        features: ['billing', 'sharing'],
      }),
    )
    const api = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    const result = await fetchPublicConfig(api)

    expect(result).toEqual({
      config: { 'brand.name': 'Speed', 'brand.primaryColor': '#123456' },
      features: ['billing', 'sharing'],
    })
    expect(standin.calls).toHaveLength(1)
    const call = standin.calls[0]
    expect(call?.url).toBe(`/api/v1${CONFIG_PUBLIC_PATH}`)
    // The RequestOptions.method default is GET -- no override passed.
    expect(call?.method).toBe('GET')
    // No tenant header, no Authorization: the endpoint is pre-auth and
    // tenant-resolves server-side from the request host.
    expect(call?.headers.has('x-tenant-id')).toBe(false)
    expect(call?.headers.has('authorization')).toBe(false)
  })

  it('refuses an empty 2xx body as a coded client.protocol error, not a silent undefined', async () => {
    // The same empty-document refusal as fetchPublicConfig's: an empty
    // 2xx where a features document was required is distinguishable
    // from real data by its coded error.
    const standin = createStandinFetch(() => new Response(null, { status: 200 }))
    const api = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    let caught: unknown
    try {
      await fetchSystemFeatures(api)
    } catch (error) {
      caught = error
    }

    expect(isApiError(caught)).toBe(true)
    if (isApiError(caught)) {
      expect(caught.code).toBe(ERROR_CODE_PROTOCOL)
    }
    expect(standin.calls).toHaveLength(1)
  })

  it('reports the empty-document refusal with the real attempts and status of the exchange', async () => {
    // The request actually was three HTTP attempts (two transient
    // 503s, then an empty 200): the client.protocol refusal is raised
    // inside the exchange, so it must carry that truth (attempts 3,
    // status 200) instead of the hardcoded 1/0 a wrapper-level error
    // used to synthesize -- which misreported a retried exchange as a
    // single attempt that never reached a response.
    const standin = scriptedStandin(
      textResponse(503, 'Service Unavailable'),
      textResponse(503, 'Service Unavailable'),
      new Response(null, { status: 200 }),
    )
    const api = createClient({
      baseUrl: '/api/v1',
      fetch: standin.fetch,
      retryPolicy: { maxAttempts: 3, initialDelayMs: 0, maxDelayMs: 0 },
    })

    let caught: unknown
    try {
      await fetchPublicConfig(api)
    } catch (error) {
      caught = error
    }

    expect(isApiError(caught)).toBe(true)
    if (isApiError(caught)) {
      expect(caught.code).toBe(ERROR_CODE_PROTOCOL)
      expect(caught.status).toBe(200)
      expect(caught.attempts).toBe(3)
    }
    expect(standin.calls).toHaveLength(3)
  })

  it('round-trips an empty features array as an array, not null or undefined', async () => {
    const standin = createStandinFetch(() =>
      jsonResponse(200, { config: {}, features: [] }),
    )
    const api = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    const result = await fetchPublicConfig(api)

    expect(Array.isArray(result.features)).toBe(true)
    expect(result.features).toEqual([])
  })

  it('surfaces a non-2xx envelope as an ApiError with its code', async () => {
    const standin = createStandinFetch(() =>
      jsonResponse(500, {
        code: 'config.internal',
        traceId: 'trace-config-1',
      }),
    )
    const api = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    let caught: unknown
    try {
      await fetchPublicConfig(api)
    } catch (error) {
      caught = error
    }

    expect(isApiError(caught)).toBe(true)
    if (isApiError(caught)) {
      expect(caught.code).toBe('config.internal')
      expect(caught.traceId).toBe('trace-config-1')
      expect(caught.status).toBe(500)
    }
  })

  it('carries the real go/config error shape (code only, no traceId) with its code', async () => {
    // go/config/http.go's writeError encodes only {"code", "params"} --
    // never a traceId. parseEnvelope trusts a body as an envelope on its
    // `code` string alone (traceId is optional), so a genuine
    // fetchPublicConfig failure against go/config surfaces its real
    // module code: config.internal_error, not a synthetic
    // client.http.500. This test pins the literal wire shape a real
    // failure produces, not a fabricated, traceId-padded body.
    const standin = createStandinFetch(() =>
      jsonResponse(500, { code: 'config.internal_error' }),
    )
    const api = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    let caught: unknown
    try {
      await fetchPublicConfig(api)
    } catch (error) {
      caught = error
    }

    expect(isApiError(caught)).toBe(true)
    if (isApiError(caught)) {
      expect(caught.code).toBe('config.internal_error')
      expect(caught.code).not.toBe('client.http.500')
      expect(caught.traceId).toBeUndefined()
    }
  })

  it('passes an AbortSignal through and rejects the raw AbortError on cancel', async () => {
    const standin = createStandinFetch(() => hang)
    const api = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })
    const controller = new AbortController()

    const pending = fetchPublicConfig(api, { signal: controller.signal })
    controller.abort()

    let caught: unknown
    try {
      await pending
    } catch (error) {
      caught = error
    }

    expect(caught).toBeInstanceOf(DOMException)
    expect((caught as DOMException).name).toBe('AbortError')
    // Cancellation is not an ApiError -- raw AbortError, never wrapped.
    expect(isApiError(caught)).toBe(false)
  })
})

describe('fetchSystemFeatures', () => {
  it('GETs SYSTEM_FEATURES_PATH and returns only the features array', async () => {
    const standin = createStandinFetch(() =>
      jsonResponse(200, { features: ['ai-gateway'] }),
    )
    const api = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    const result = await fetchSystemFeatures(api)

    expect(result).toEqual({ features: ['ai-gateway'] })
    expect(standin.calls).toHaveLength(1)
    const call = standin.calls[0]
    expect(call?.url).toBe(`/api/v1${SYSTEM_FEATURES_PATH}`)
    expect(call?.method).toBe('GET')
  })

  it('refuses an empty 2xx body as a coded client.protocol error, not a silent undefined', async () => {
    // The same empty-document refusal as fetchPublicConfig's: go/config
    // always writes a features document here, so an empty 2xx (the
    // RequestFn's own 204-style empty-success shape) is refused with a
    // distinguishable coded error instead of resolving undefined.
    const standin = createStandinFetch(() => new Response(null, { status: 200 }))
    const api = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    let caught: unknown
    try {
      await fetchSystemFeatures(api)
    } catch (error) {
      caught = error
    }

    expect(isApiError(caught)).toBe(true)
    if (isApiError(caught)) {
      expect(caught.code).toBe(ERROR_CODE_PROTOCOL)
    }
    expect(standin.calls).toHaveLength(1)
  })

  it('surfaces a non-2xx envelope as an ApiError with its code', async () => {
    const standin = createStandinFetch(() =>
      jsonResponse(404, { code: 'config.not_found', traceId: 'trace-config-2' }),
    )
    const api = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    let caught: unknown
    try {
      await fetchSystemFeatures(api)
    } catch (error) {
      caught = error
    }

    expect(isApiError(caught)).toBe(true)
    if (isApiError(caught)) {
      expect(caught.code).toBe('config.not_found')
      expect(caught.traceId).toBe('trace-config-2')
    }
  })

  it('carries the real go/config error shape (code only, no traceId) with its code', async () => {
    // The same literal go/config wire shape as fetchPublicConfig's
    // equivalent test above: {code} with no traceId, which must keep
    // its real module code rather than degrade to client.http.404.
    const standin = createStandinFetch(() =>
      jsonResponse(404, { code: 'config.not_found' }),
    )
    const api = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    let caught: unknown
    try {
      await fetchSystemFeatures(api)
    } catch (error) {
      caught = error
    }

    expect(isApiError(caught)).toBe(true)
    if (isApiError(caught)) {
      expect(caught.code).toBe('config.not_found')
      expect(caught.code).not.toBe('client.http.404')
      expect(caught.traceId).toBeUndefined()
    }
  })
})
