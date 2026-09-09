import { beforeEach, describe, expect, it } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, createElement, type ReactNode } from 'react'
import { renderHook, waitFor } from '@testing-library/react'
import type { RequestFn, RequestOptions } from '@speed/api-client'
import { bindRequestFn } from './runtime'
import {
  getAuthnListSessionsQueryKey,
  useAuthnListSessions,
  useAuthnRegister,
} from './index'

/**
 * Exercises the generated surface (orval output, DO-NOT-EDIT) against a
 * bound fake request function: what is under test is that the hooks
 * issue exactly the calls the spec implies, with the query keys the
 * generated code stamps -- never a network, never a real client. The
 * authn operations stand in for any generated group: every platform
 * fragment's operations share the identical generated shape, so this
 * pair pins the surface mechanics the whole package relies on.
 */
interface RecordedCall {
  path: string
  options?: RequestOptions
}
const calls: RecordedCall[] = []
/** The payloads the endpoints would return, keyed by method + path. */
const responses: Record<string, unknown> = {
  'GET /api/v1/authn/sessions': { sessions: [] },
  'POST /api/v1/authn/register': {
    id: 'user-1',
    email: 'a@example.test',
  },
}
const fakeRequestFn: RequestFn = (async <T>(
  path: string,
  options?: RequestOptions,
): Promise<T> => {
  calls.push({ path, options })
  const method = options?.method ?? 'GET'
  return (responses[`${method} ${path}`] ?? undefined) as T
}) as RequestFn

/** The host's QueryClient, shared the way a real consumer shares one
 * QueryClientProvider across the app (the peer-family contract). */
const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false } },
})
const wrapper = ({ children }: { children?: ReactNode }) =>
  createElement(QueryClientProvider, { client: queryClient }, children)

beforeEach(() => {
  calls.length = 0
  bindRequestFn(fakeRequestFn)
})

describe('useAuthnListSessions', () => {
  it('issues a GET for the sessions path through the bound request function', async () => {
    const { result } = renderHook(() => useAuthnListSessions(), { wrapper })
    await waitFor(() => {
      expect(result.current.isSuccess).toBe(true)
    })
    expect(result.current.data).toEqual({ sessions: [] })
    // react-query passes its own AbortSignal into the query function, so
    // the recorded options carry a live signal -- assert the wire
    // slice, not structural equality over the whole options object.
    expect(calls).toMatchObject([
      { path: '/api/v1/authn/sessions', options: { method: 'GET' } },
    ])
  })

  it('stamps the spec path as the query key, with no tenant prefix', () => {
    // Query-key tenant namespacing is a consumer-shell discipline; the
    // generated key is the bare spec path.
    expect(getAuthnListSessionsQueryKey()).toEqual(['/api/v1/authn/sessions'])
  })
})

describe('useAuthnRegister', () => {
  it('issues a POST carrying the request body through the bound request function', async () => {
    const { result } = renderHook(() => useAuthnRegister(), { wrapper })
    act(() => {
      result.current.mutate({
        data: { password: 'secret', email: 'a@example.test' },
      })
    })
    await waitFor(() => {
      expect(result.current.isSuccess).toBe(true)
    })
    expect(calls).toMatchObject([
      {
        path: '/api/v1/authn/register',
        options: {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: { password: 'secret', email: 'a@example.test' },
        },
      },
    ])
  })
})
