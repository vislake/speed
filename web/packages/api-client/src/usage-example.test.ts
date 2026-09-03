/**
 * The README Quick start, compiled and executed by the suite.
 *
 * The README documents one host composition -- createClient wiring,
 * Note, loadNotes -- and claims it is the way to use the package. This
 * file carries that composition verbatim (the snippet's
 * `from '@speed/api-client'` import resolves to the package entry,
 * './index', in-suite), stubs the global fetch before loadNotes runs
 * (the client captures fetch at construction, inside the function), and
 * drives both documented paths: the success branch and the
 * isApiError branch of the catch. The documented usage therefore cannot
 * drift from the API -- the package suite compiles and runs it.
 *
 * The console.error call is part of the documented snippet (the
 * constant-message/snake_case-attribute shape hosts are told to use);
 * tests spy the console so suite output stays clean and pin the exact
 * reported message and attributes.
 */

import { afterEach, describe, expect, it, vi } from 'vitest'
import { createStandinFetch, jsonResponse } from '../test-utils/fetch-standin'
import type { StandinCall, StandinFetch } from '../test-utils/fetch-standin'
import {
  createClient,
  createMemoryAccessTokenStore,
  isApiError,
} from './index'

/* ------------------------------------------------------------------ */
/* The README Quick start, verbatim.                                   */
/* ------------------------------------------------------------------ */

/** A note as the API returns it. */
interface Note {
  id: string
  title: string
}

/** Loads the current session's notes through the API client. */
export async function loadNotes(): Promise<Note[]> {
  const api = createClient({
    // Scheme + host + optional prefix, or a same-origin '/api/...'
    // path in development. The fetch implementation is injectable
    // (tests pass a deterministic stand-in); when omitted, the
    // environment's global fetch is captured at construction time.
    baseUrl: '/api/v1',
    // The bearer token store starts empty: auth fills it in memory,
    // never in storage (an access token in localStorage is a
    // credential an XSS walks away with). With no token, requests go
    // out without Authorization.
    accessTokenStore: createMemoryAccessTokenStore(),
    // Silent 401 refresh: M1 authn supplies the real hook against the
    // session-refresh endpoint (the refresh token is an httpOnly
    // cookie JavaScript never sees). Until then every 401 rejects an
    // ApiError with auth: true, and hosts route it to sign-in.
    refreshAccessToken: async () => false,
    // Abort requests slower than 10s. Transient retries follow
    // DEFAULT_RETRY_POLICY: idempotent methods only (GET/HEAD/OPTIONS),
    // up to 3 attempts, 200ms doubling backoff capped at 4s.
    timeoutMs: 10_000,
  })

  try {
    return await api<Note[]>('/notes', { query: { page: 1 } })
  } catch (error) {
    if (isApiError(error)) {
      // error.code is the envelope's module code ('notes.…') or a
      // reserved client.* code when the API layer itself failed; map
      // codes to user-facing text through the i18n catalog, never here.
      console.error('loading notes failed', {
        code: error.code,
        trace_id: error.traceId,
      })
    }
    throw error
  }
}

/* ------------------------------------------------------------------ */
/* The suite driving the snippet.                                      */
/* ------------------------------------------------------------------ */

/** The two notes the scripted stand-in serves. */
const NOTES: readonly Note[] = [
  { id: 'n-1', title: 'First note' },
  { id: 'n-2', title: 'Second note' },
]

/** Returns the stand-in's one recorded call, failing on any deviation. */
function onlyCall(standin: StandinFetch): StandinCall {
  if (standin.calls.length !== 1) {
    throw new Error(
      `expected exactly one request, the stand-in saw ${standin.calls.length}`,
    )
  }
  const call = standin.calls[0]
  if (call === undefined) {
    throw new Error('unreachable: a one-element array has no first element')
  }
  return call
}

/** Resolves the rejection and fails the test unless it is an ApiError.
 * The return type is the narrowing isApiError performs. */
async function expectApiErrorRejection(promise: Promise<unknown>) {
  let settled: unknown
  try {
    await promise
  } catch (error) {
    settled = error
  }
  if (settled === undefined) {
    throw new Error('expected loadNotes to reject on a 401')
  }
  if (!isApiError(settled)) {
    throw new Error(`expected an ApiError rejection, saw ${String(settled)}`)
  }
  return settled
}

describe('README usage example', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('loads the notes through the scripted stand-in, with no noise', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})
    const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const standin = createStandinFetch(() => jsonResponse(200, NOTES))
    vi.stubGlobal('fetch', standin.fetch)

    await expect(loadNotes()).resolves.toEqual([...NOTES])

    const call = onlyCall(standin)
    expect(call.url).toBe('/api/v1/notes?page=1')
    expect(call.method).toBe('GET')
    expect(call.headers.get('accept')).toBe('application/json')
    // The memory store starts empty: no Authorization header, and no
    // tenant header exists anywhere in this package.
    expect(call.headers.has('authorization')).toBe(false)
    expect(call.headers.has('x-tenant-id')).toBe(false)
    // The happy path reports nothing.
    expect(consoleError).not.toHaveBeenCalled()
    expect(consoleWarn).not.toHaveBeenCalled()
  })

  it('routes a session-expired 401 through the isApiError branch', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})
    const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const standin = createStandinFetch(() =>
      jsonResponse(401, {
        code: 'authn.session_expired',
        traceId: 'trace-1',
        message: 'Your session expired.',
      }),
    )
    vi.stubGlobal('fetch', standin.fetch)

    const error = await expectApiErrorRejection(loadNotes())
    expect(error.status).toBe(401)
    expect(error.auth).toBe(true)
    expect(error.code).toBe('authn.session_expired')
    expect(error.traceId).toBe('trace-1')
    expect(error.attempts).toBe(1)

    // The refresh hook is the M1 stub (returns false): one refresh
    // attempt, reported, and no retry of the refused request.
    const call = onlyCall(standin)
    expect(call.url).toBe('/api/v1/notes?page=1')
    expect(consoleWarn).toHaveBeenCalledWith('access token refresh failed', {
      status: 401,
      code: 'authn.session_expired',
      traceId: 'trace-1',
    })
    // The snippet's catch reported the envelope code and trace id.
    expect(consoleError).toHaveBeenCalledWith('loading notes failed', {
      code: 'authn.session_expired',
      trace_id: 'trace-1',
    })
  })
})
