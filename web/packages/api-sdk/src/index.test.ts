import { beforeEach, describe, expect, it } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, createElement, type ReactNode } from 'react'
import { renderHook, waitFor } from '@testing-library/react'
import type { RequestFn, RequestOptions } from '@speed/api-client'
import { bindRequestFn } from './runtime'
import {
  getNotesListNotesQueryKey,
  useNotesCreateNote,
  useNotesListNotes,
} from './index'

/**
 * Exercises the generated surface (orval output, DO-NOT-EDIT) against a
 * bound fake request function: what is under test is that the hooks
 * issue exactly the calls the spec implies, with the query keys the
 * generated code stamps -- never a network, never a real client.
 */
interface RecordedCall {
  path: string
  options?: RequestOptions
}
const calls: RecordedCall[] = []
/** The payloads the endpoints would return, keyed by method + path. */
const responses: Record<string, unknown> = {
  'GET /api/v1/notes': { notes: [] },
  'POST /api/v1/notes': { id: 'note-1', text: 'hello' },
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

describe('useNotesListNotes', () => {
  it('issues a GET for the notes path through the bound request function', async () => {
    const { result } = renderHook(() => useNotesListNotes(), { wrapper })
    await waitFor(() => {
      expect(result.current.isSuccess).toBe(true)
    })
    expect(result.current.data).toEqual({ notes: [] })
    // react-query passes its own AbortSignal into the query function, so
    // the recorded options carry a live signal -- assert the wire
    // slice, not structural equality over the whole options object.
    expect(calls).toMatchObject([
      { path: '/api/v1/notes', options: { method: 'GET' } },
    ])
  })

  it('stamps the spec path as the query key, with no tenant prefix', () => {
    // Query-key tenant namespacing is an M1 consumer-shell discipline;
    // the generated key is the bare spec path (package AGENTS.md).
    expect(getNotesListNotesQueryKey()).toEqual(['/api/v1/notes'])
  })
})

describe('useNotesCreateNote', () => {
  it('issues a POST carrying the request body through the bound request function', async () => {
    const { result } = renderHook(() => useNotesCreateNote(), { wrapper })
    act(() => {
      result.current.mutate({ data: { text: 'hello' } })
    })
    await waitFor(() => {
      expect(result.current.isSuccess).toBe(true)
    })
    expect(calls).toMatchObject([
      {
        path: '/api/v1/notes',
        options: {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: { text: 'hello' },
        },
      },
    ])
  })
})
