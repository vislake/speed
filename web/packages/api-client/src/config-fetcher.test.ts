/**
 * Tests for config-fetcher.ts. Every call goes through a real
 * createClient wired to the deterministic fetch stand-in -- never a
 * real network -- exercising fetchPublicConfig / fetchSystemFeatures
 * exactly as a consumer would use them: pass the RequestFn, get back
 * a typed response or a rejected ApiError/AbortError.
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
})
