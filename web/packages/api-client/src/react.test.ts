// @vitest-environment jsdom
/**
 * Covers usePublicConfig/useFeature (react.ts): the loading -> success
 * and loading -> error transitions, the single-flight cache shared by
 * simultaneous consumers of the same `api`, refresh() republishing to
 * every subscriber, and useFeature's false-while-unresolved default.
 *
 * Every request goes through the package's existing fetch stand-in
 * (test-utils/fetch-standin.ts) via a real createClient -- no network,
 * no mocking of react.ts's own internals.
 */

import { act, renderHook, waitFor } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { createStandinFetch, jsonResponse } from '../test-utils/fetch-standin'
import { createClient } from './client'
import type { RequestFn } from './client'
import { CONFIG_PUBLIC_PATH } from './config-fetcher'
import { useFeature, usePublicConfig } from './react'

/** A fresh pre-auth-style client over a fresh stand-in: no
 * accessTokenStore, matching how the two config endpoints are meant to
 * be called (see config-fetcher.ts). */
function standinClient() {
  const standin = createStandinFetch(() =>
    jsonResponse(200, { config: { brand_name: 'Speed' }, features: ['flag_a'] }),
  )
  const api: RequestFn = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })
  return { standin, api }
}

describe('usePublicConfig', () => {
  it('transitions from loading to success with the fetched data', async () => {
    const { standin, api } = standinClient()
    const { result } = renderHook(() => usePublicConfig(api))

    expect(result.current.isLoading).toBe(true)
    expect(result.current.data).toBeUndefined()
    expect(result.current.error).toBeUndefined()

    await waitFor(() => expect(result.current.isLoading).toBe(false))

    expect(result.current.data).toEqual({
      config: { brand_name: 'Speed' },
      features: ['flag_a'],
    })
    expect(result.current.error).toBeUndefined()
    expect(standin.calls).toHaveLength(1)
    expect(standin.calls[0]?.url).toBe(`/api/v1${CONFIG_PUBLIC_PATH}`)
    expect(standin.calls[0]?.method).toBe('GET')
  })

  it('transitions from loading to error, surfacing the ApiError verbatim', async () => {
    const standin = createStandinFetch(() =>
      jsonResponse(500, {
        code: 'config.internal_error',
        traceId: 'trace-500',
      }),
    )
    const api: RequestFn = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    const { result } = renderHook(() => usePublicConfig(api))
    expect(result.current.isLoading).toBe(true)

    await waitFor(() => expect(result.current.isLoading).toBe(false))

    expect(result.current.data).toBeUndefined()
    expect(result.current.error).toBeDefined()
    expect(result.current.error?.code).toBe('config.internal_error')
    expect(result.current.error?.traceId).toBe('trace-500')
    expect(result.current.error?.status).toBe(500)
    // 500 is not in the client's transient-retry set: one attempt.
    expect(result.current.error?.attempts).toBe(1)
  })

  it('shares one in-flight fetch across simultaneous consumers of the same api', async () => {
    const { standin, api } = standinClient()

    const { result: first } = renderHook(() => usePublicConfig(api))
    const { result: second } = renderHook(() => usePublicConfig(api))

    await waitFor(() => expect(first.current.isLoading).toBe(false))
    await waitFor(() => expect(second.current.isLoading).toBe(false))

    // Two mounted consumers, one request -- the shared store, not two
    // independent fetches.
    expect(standin.calls).toHaveLength(1)
    expect(first.current.data).toEqual(second.current.data)
  })

  it('refresh() forces exactly one new fetch and republishes to every subscriber', async () => {
    let call = 0
    const standin = createStandinFetch(() => {
      call += 1
      return call === 1
        ? jsonResponse(200, { config: {}, features: ['flag_a'] })
        : jsonResponse(200, { config: {}, features: ['flag_a', 'flag_b'] })
    })
    const api: RequestFn = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    const { result: first } = renderHook(() => usePublicConfig(api))
    const { result: second } = renderHook(() => usePublicConfig(api))
    await waitFor(() => expect(first.current.isLoading).toBe(false))
    await waitFor(() => expect(second.current.isLoading).toBe(false))
    expect(standin.calls).toHaveLength(1)
    expect(first.current.data?.features).toEqual(['flag_a'])

    act(() => {
      first.current.refresh()
    })

    // Stale-while-revalidate: the previous data survives the refetch,
    // it is not reset to undefined while the new request is in flight.
    expect(first.current.data?.features).toEqual(['flag_a'])
    expect(first.current.isLoading).toBe(true)

    await waitFor(() => expect(first.current.isLoading).toBe(false))
    await waitFor(() => expect(second.current.isLoading).toBe(false))

    expect(standin.calls).toHaveLength(2)
    expect(first.current.data?.features).toEqual(['flag_a', 'flag_b'])
    // The second, independently mounted subscriber picks up the same
    // refreshed data -- refresh() republishes to all subscribers, not
    // just the caller.
    expect(second.current.data?.features).toEqual(['flag_a', 'flag_b'])
  })
})

describe('useFeature', () => {
  it('reads a present key as true and an absent key as false once resolved', async () => {
    const { api } = standinClient()

    const { result: present } = renderHook(() => useFeature(api, 'flag_a'))
    const { result: absent } = renderHook(() => useFeature(api, 'flag_missing'))

    await waitFor(() => expect(present.current).toBe(true))
    expect(absent.current).toBe(false)
  })

  it('defaults to false while loading, never throwing', () => {
    const { api } = standinClient()
    const { result } = renderHook(() => useFeature(api, 'flag_a'))
    expect(result.current).toBe(false)
  })

  it('defaults to false on error, never throwing', async () => {
    const standin = createStandinFetch(() =>
      jsonResponse(500, { code: 'config.internal_error', traceId: 'trace-501' }),
    )
    const api: RequestFn = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })

    const { result: config } = renderHook(() => usePublicConfig(api))
    const { result: feature } = renderHook(() => useFeature(api, 'flag_a'))

    await waitFor(() => expect(config.current.isLoading).toBe(false))
    expect(config.current.error).toBeDefined()
    expect(feature.current).toBe(false)
  })
})
