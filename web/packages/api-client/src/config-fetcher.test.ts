/**
 * Tests for config-fetcher.ts. Every call goes through a real
 * createClient wired to the deterministic fetch stand-in -- never a
 * real network -- exercising fetchPublicConfig / fetchSystemFeatures
 * exactly as a consumer would use them: pass the RequestFn, get back
 * a typed response or a rejected ApiError/AbortError.
 *
 * A note on the two "surfaces a non-2xx envelope ... with its code"
 * tests below: their mocked body includes `traceId`, matching the API
 * contract's envelope schema (docs/internal/21-api-contract.md requires
 * both `code` and `traceId`), which is what client.ts's parseEnvelope
 * demands before it will trust a body's `code` at all. go/config's real
 * handlers do not send `traceId` (go/config/http.go's `errorEnvelope`
 * only ever encodes `{code, params}`), so those two tests exercise
 * spec-compliant envelope handling in general, not the literal
 * go/config wire shape. The "actual go/config error shape" tests
 * further down pin what a real go/config failure looks like today --
 * see their comments and AGENTS.md's Known limitations entry for the
 * gap and its deferred follow-up.
 */

import { afterEach, describe, expect, it, vi } from 'vitest'
import { createStandinFetch, hang, jsonResponse } from '../test-utils/fetch-standin'
import { createClient } from './client.js'
import {
  CONFIG_PUBLIC_PATH,
  SYSTEM_FEATURES_PATH,
  fetchPublicConfig,
  fetchSystemFeatures,
} from './config-fetcher.js'
import { isApiError } from './errors.js'

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

  it('degrades the real go/config error shape (no traceId) to a synthetic client.http code', async () => {
    // go/config/http.go's writeError encodes only {"code", "params"} --
    // never a traceId. client.ts's parseEnvelope requires traceId to
    // trust a body as an envelope at all, so this is what a genuine
    // fetchPublicConfig failure against go/config looks like today: the
    // real module code (config.internal_error) is discarded and the
    // caller sees the synthetic client.http.500 code instead. This is a
    // known, pre-existing gap between go/config and the API contract's
    // required-traceId envelope schema (docs/internal/21-api-contract.md)
    // -- see AGENTS.md's Known limitations for the deferred follow-up.
    // This test exists to keep that reality pinned and visible rather
    // than only ever exercised against a fabricated, spec-compliant body.
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
      expect(caught.code).toBe('client.http.500')
      expect(caught.code).not.toBe('config.internal_error')
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

  it('degrades the real go/config error shape (no traceId) to a synthetic client.http code', async () => {
    // Same gap as fetchPublicConfig's equivalent test above: go/config
    // never sends traceId, so its real code is discarded here too.
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
      expect(caught.code).toBe('client.http.404')
      expect(caught.code).not.toBe('config.not_found')
      expect(caught.traceId).toBeUndefined()
    }
  })
})
