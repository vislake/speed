/**
 * Contract tests for the whole request lifecycle createClient wires:
 * bearer attachment without any tenant header, the single-flight silent
 * 401 refresh (at most one refresh per request, one retry on any
 * method), transient retry only for idempotent methods with the pure
 * backoff maths pinned in retry.test.ts, caller cancellation passing
 * the raw AbortError through, and the full error-normalization table
 * (envelope codes win, everything else falls into the reserved
 * client.* vocabulary). Every request goes through a scripted fetch
 * stand-in -- the package never touches a real network.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  createClient,
  createMemoryAccessTokenStore,
  DEFAULT_RETRY_POLICY,
  ERROR_CODE_NETWORK,
  ERROR_CODE_PROTOCOL,
  ERROR_CODE_TIMEOUT,
  isApiError,
  type ApiError,
  type RetryPolicy,
} from './index'
import { createMemoryReporter } from '../test-utils/memory-reporter'
import {
  createStandinFetch,
  hang,
  jsonResponse,
  scriptedStandin,
  textResponse,
  type StandinCall,
  type StandinFetch,
} from '../test-utils/fetch-standin'

const BASE_URL = 'https://api.example.test'

/** A policy whose retries fire as fast as the event loop allows. */
const zeroDelay = (maxAttempts: number): RetryPolicy => ({
  maxAttempts,
  initialDelayMs: 0,
  maxDelayMs: 0,
})

/** The stand-in call at `index`, or a loud test failure. */
function recorded(standin: StandinFetch, index = 0): StandinCall {
  const call = standin.calls[index]
  if (call === undefined) {
    throw new Error(
      `expected the stand-in to have recorded call #${index + 1}; ` +
        `it recorded ${standin.calls.length}`,
    )
  }
  return call
}

/** Awaits a rejection and returns it only when it is an ApiError. */
async function expectApiError(promise: Promise<unknown>): Promise<ApiError> {
  let error: unknown
  try {
    await promise
  } catch (caught) {
    error = caught
  }
  if (error === undefined) {
    throw new Error('expected the request to reject, but it resolved')
  }
  if (!isApiError(error)) {
    throw new Error(`expected an ApiError, got ${String(error)}`)
  }
  return error
}

/** Asserts the promise rejects with the raw AbortError -- caller
 * cancellation is never wrapped in an ApiError. */
async function expectRawAbort(promise: Promise<unknown>): Promise<void> {
  let error: unknown
  try {
    await promise
  } catch (caught) {
    error = caught
  }
  expect(error).toBeInstanceOf(DOMException)
  if (error instanceof DOMException) {
    expect(error.name).toBe('AbortError')
  }
  expect(isApiError(error)).toBe(false)
}

/** A body stream the test gates by hand: the response's text() stays
 * pending until release (success) or fail (error) is called. */
function gatedBody(): {
  stream: ReadableStream<Uint8Array>
  release: (text: string) => void
  fail: (cause: unknown) => void
} {
  let release: (text: string) => void = () => {}
  let fail: (cause: unknown) => void = () => {}
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      release = (text: string) => {
        controller.enqueue(new TextEncoder().encode(text))
        controller.close()
      }
      fail = (cause: unknown) => {
        controller.error(cause)
      }
    },
  })
  return { stream, release, fail }
}

const SESSION_EXPIRED = {
  code: 'authn.session_expired',
  traceId: 'trace-1',
}

describe('construction validation', () => {
  const dummyFetch = async (): Promise<Response> => new Response()

  it('rejects an empty baseUrl', () => {
    expect(() => createClient({ baseUrl: '' })).toThrow('[speed-api-client]')
  })

  it('throws at construction when no fetch exists anywhere', () => {
    vi.stubGlobal('fetch', undefined)
    expect(() => createClient({ baseUrl: BASE_URL })).toThrow(
      /no fetch implementation/,
    )
    vi.unstubAllGlobals()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('rejects malformed retry policies', () => {
    // All shapes are structurally valid RetryPolicy values (the ranges
    // are runtime constraints): each must fail construction validation.
    const badPolicies: RetryPolicy[] = [
      { maxAttempts: 0, initialDelayMs: 200, maxDelayMs: 4000 },
      { maxAttempts: 1.5, initialDelayMs: 200, maxDelayMs: 4000 },
      { maxAttempts: 3, initialDelayMs: -1, maxDelayMs: 4000 },
      { maxAttempts: 3, initialDelayMs: 200, maxDelayMs: Number.NaN },
    ]
    for (const retryPolicy of badPolicies) {
      expect(
        () =>
          createClient({
            baseUrl: BASE_URL,
            fetch: dummyFetch,
            retryPolicy,
          }),
        `policy ${JSON.stringify(retryPolicy)}`,
      ).toThrow('[speed-api-client]')
    }
  })

  it('rejects a non-positive timeout', () => {
    for (const timeoutMs of [0, -10, Number.NaN]) {
      expect(
        () =>
          createClient({ baseUrl: BASE_URL, fetch: dummyFetch, timeoutMs }),
        `timeoutMs ${timeoutMs}`,
      ).toThrow('[speed-api-client]')
    }
  })

  it('rejects request paths that do not start with "/"', async () => {
    const api = createClient({ baseUrl: BASE_URL, fetch: dummyFetch })
    await expect(api('notes')).rejects.toThrow(/absolute path/)
  })
})

describe('request shape', () => {
  it('attaches the bearer token and nothing tenant-shaped', async () => {
    const store = createMemoryAccessTokenStore()
    store.set('token-1')
    const standin = scriptedStandin(jsonResponse(200, { ok: true }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
    })
    await api<{ ok: boolean }>('/notes')
    const call = recorded(standin)
    expect(call.url).toBe(`${BASE_URL}/notes`)
    expect(call.method).toBe('GET')
    expect(call.headers.get('authorization')).toBe('Bearer token-1')
    expect(call.headers.get('accept')).toBe('application/json')
    expect(call.headers.has('content-type')).toBe(false)
    // Exactly the two documented headers go out -- and the tenant never
    // appears as a header (docs/internal/12-frontend.md): it travels in
    // the access-token claims, so nothing tenant-shaped may exist here.
    expect([...call.headers.keys()].sort()).toEqual([
      'accept',
      'authorization',
    ])
    for (const name of [
      'x-tenant-id',
      'x-tenant',
      'tenant-id',
      'x-tenant-context',
    ]) {
      expect(call.headers.has(name)).toBe(false)
    }
  })

  it('sends no Authorization header without a token', async () => {
    const standin = scriptedStandin(jsonResponse(200, { ok: true }))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    await api<{ ok: boolean }>('/public/config')
    expect(recorded(standin).headers.has('authorization')).toBe(false)
  })

  it('lets the store win over a caller-supplied authorization header', async () => {
    const store = createMemoryAccessTokenStore()
    store.set('store-token')
    const standin = scriptedStandin(jsonResponse(200, { ok: true }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
    })
    await api<{ ok: boolean }>('/notes', {
      headers: { authorization: 'Bearer caller-token' },
    })
    expect(recorded(standin).headers.get('authorization')).toBe(
      'Bearer store-token',
    )
  })

  it('keeps caller headers but overrides accept with application/json', async () => {
    const standin = scriptedStandin(jsonResponse(200, { ok: true }))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    await api<{ ok: boolean }>('/notes', {
      headers: { 'x-request-id': 'req-1', accept: 'text/plain' },
    })
    const call = recorded(standin)
    expect(call.headers.get('x-request-id')).toBe('req-1')
    expect(call.headers.get('accept')).toBe('application/json')
  })

  it('strips trailing slashes from baseUrl and appends the path verbatim', async () => {
    const standin = scriptedStandin(jsonResponse(200, { ok: true }))
    const api = createClient({
      baseUrl: 'https://api.example.test///',
      fetch: standin.fetch,
    })
    await api<{ ok: boolean }>('/v1/notes/')
    expect(recorded(standin).url).toBe('https://api.example.test/v1/notes/')
  })

  it('encodes query parameters and skips null and undefined entries', async () => {
    const standin = scriptedStandin(jsonResponse(200, []))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    await api('/notes', {
      query: {
        page: 2,
        q: 'a b',
        flag: true,
        title: 'x&y',
        skip: null,
        nothing: undefined,
      },
    })
    expect(recorded(standin).url).toBe(
      `${BASE_URL}/notes?page=2&q=a+b&flag=true&title=x%26y`,
    )
  })

  it('serializes JSON bodies with the JSON content type', async () => {
    const standin = scriptedStandin(jsonResponse(201, { id: 'n-1' }))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const created = await api<{ id: string }>('/notes', {
      method: 'POST',
      body: { title: 'T', n: 1 },
    })
    expect(created).toEqual({ id: 'n-1' })
    const call = recorded(standin)
    expect(call.method).toBe('POST')
    expect(call.headers.get('content-type')).toBe('application/json')
    expect(call.bodyJson).toEqual({ title: 'T', n: 1 })
  })
})

describe('successful responses', () => {
  it('resolves a parsed JSON object', async () => {
    const standin = scriptedStandin(jsonResponse(200, { notes: ['one'] }))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    await expect(api<{ notes: string[] }>('/notes')).resolves.toEqual({
      notes: ['one'],
    })
  })

  it('resolves a JSON array body', async () => {
    const standin = scriptedStandin(jsonResponse(200, [1, 2]))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    await expect(api<number[]>('/ids')).resolves.toEqual([1, 2])
  })

  it('resolves undefined for an empty 2xx body', async () => {
    const standin = scriptedStandin(jsonResponse(204))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    await expect(api<undefined>('/notes/n-1')).resolves.toBeUndefined()
  })

  it('treats a whitespace-only 2xx body as an empty success', async () => {
    const standin = scriptedStandin(textResponse(200, ' \n '))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    await expect(api<undefined>('/notes/n-1')).resolves.toBeUndefined()
  })

  it('retries a 2xx whose body fails to arrive, then succeeds', async () => {
    let first = true
    const standin = createStandinFetch(() => {
      if (first) {
        first = false
        // A response whose body stream is locked: text() rejects.
        const locked = new Response('{"ok":true}', {
          status: 200,
          headers: { 'content-type': 'application/json' },
        })
        void locked.body?.getReader()
        return locked
      }
      return jsonResponse(200, { ok: true })
    })
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      retryPolicy: zeroDelay(3),
    })
    await expect(api<{ ok: boolean }>('/notes')).resolves.toEqual({ ok: true })
    expect(standin.calls).toHaveLength(2)
  })

  it('surfaces a body-read failure as a network ApiError when out of budget', async () => {
    const locked = new Response('{"ok":true}', {
      status: 200,
      headers: { 'content-type': 'application/json' },
    })
    void locked.body?.getReader()
    const standin = scriptedStandin(locked)
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      retryPolicy: zeroDelay(1),
    })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.code).toBe(ERROR_CODE_NETWORK)
    expect(error.status).toBe(0)
    expect(error.attempts).toBe(1)
    expect(error.cause).toBeInstanceOf(TypeError)
  })
})

describe('401 and the refresh hook', () => {
  it('surfaces an envelope 401 as an auth ApiError when no hook is set', async () => {
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      jsonResponse(200, { ok: true }),
    )
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.status).toBe(401)
    expect(error.auth).toBe(true)
    expect(error.code).toBe('authn.session_expired')
    expect(error.traceId).toBe('trace-1')
    expect(error.attempts).toBe(1)
    // 401 is never transient-retried: exactly one HTTP attempt.
    expect(standin.calls).toHaveLength(1)
  })

  it('refreshes once, retries the original request, and picks up the new token', async () => {
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    let refreshCalls = 0
    const memory = createMemoryReporter()
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      jsonResponse(200, { ok: true }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
      refreshAccessToken: async () => {
        refreshCalls += 1
        store.set('fresh-token')
        return true
      },
      reporter: memory.reporter,
    })
    await expect(api<{ ok: boolean }>('/notes')).resolves.toEqual({ ok: true })
    expect(refreshCalls).toBe(1)
    expect(standin.calls).toHaveLength(2)
    expect(recorded(standin, 0).headers.get('authorization')).toBe(
      'Bearer stale-token',
    )
    // The retry re-read the token: the refreshed one is on the wire.
    expect(recorded(standin, 1).headers.get('authorization')).toBe(
      'Bearer fresh-token',
    )
    expect(memory.warns).toHaveLength(0)
  })

  it('retries a non-idempotent method once after a successful refresh', async () => {
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    let refreshCalls = 0
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      jsonResponse(201, { id: 'n-1' }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
      refreshAccessToken: async () => {
        refreshCalls += 1
        store.set('fresh-token')
        return true
      },
    })
    await expect(
      api<{ id: string }>('/notes', { method: 'POST', body: { title: 'T' } }),
    ).resolves.toEqual({ id: 'n-1' })
    expect(refreshCalls).toBe(1)
    expect(standin.calls).toHaveLength(2)
    expect(recorded(standin, 1).method).toBe('POST')
  })

  it('does not refresh twice when the retried request is refused again', async () => {
    let refreshCalls = 0
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      jsonResponse(401, { ...SESSION_EXPIRED, traceId: 'trace-2' }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: createMemoryAccessTokenStore(),
      refreshAccessToken: async () => {
        refreshCalls += 1
        return true
      },
    })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.auth).toBe(true)
    expect(error.attempts).toBe(2)
    expect(error.traceId).toBe('trace-2')
    expect(refreshCalls).toBe(1)
    expect(standin.calls).toHaveLength(2)
  })

  it('surfaces the 401 and reports the refresh failure with the envelope attrs', async () => {
    const memory = createMemoryReporter()
    const standin = scriptedStandin(jsonResponse(401, { ...SESSION_EXPIRED }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: createMemoryAccessTokenStore(),
      refreshAccessToken: async () => false,
      reporter: memory.reporter,
    })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.auth).toBe(true)
    expect(error.code).toBe('authn.session_expired')
    expect(error.traceId).toBe('trace-1')
    expect(error.attempts).toBe(1)
    expect(standin.calls).toHaveLength(1)
    // The warning reuses the same 401 body as the ApiError: it carries
    // the envelope's code and traceId so it can be correlated to
    // server logs.
    expect(memory.warns).toEqual([
      {
        message: 'access token refresh failed',
        attrs: {
          status: 401,
          code: 'authn.session_expired',
          traceId: 'trace-1',
        },
      },
    ])
  })

  it('reports a bare-401 refresh failure without envelope attrs', async () => {
    const memory = createMemoryReporter()
    const standin = scriptedStandin(jsonResponse(401, { error: 'no envelope' }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: createMemoryAccessTokenStore(),
      refreshAccessToken: async () => false,
      reporter: memory.reporter,
    })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.auth).toBe(true)
    expect(error.code).toBe('client.http.401')
    expect(error.traceId).toBeUndefined()
    expect(memory.warns).toEqual([
      { message: 'access token refresh failed', attrs: { status: 401 } },
    ])
  })

  it('treats a throwing refresh hook as a failed refresh', async () => {
    const memory = createMemoryReporter()
    const standin = scriptedStandin(jsonResponse(401, { ...SESSION_EXPIRED }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: createMemoryAccessTokenStore(),
      refreshAccessToken: async () => {
        throw new Error('refresh endpoint unreachable')
      },
      reporter: memory.reporter,
    })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.auth).toBe(true)
    expect(memory.warns).toHaveLength(1)
    expect(standin.calls).toHaveLength(1)
  })

  it('coalesces concurrent 401s onto one in-flight refresh', async () => {
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    let refreshCalls = 0
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      jsonResponse(401, { ...SESSION_EXPIRED, traceId: 'trace-2' }),
      jsonResponse(200, { notes: ['one'] }),
      jsonResponse(200, { notes: ['two'] }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
      refreshAccessToken: async () => {
        refreshCalls += 1
        store.set('fresh-token')
        return true
      },
    })
    const [first, second] = await Promise.all([
      api<{ notes: string[] }>('/notes'),
      api<{ notes: string[] }>('/notes'),
    ])
    expect(first).toEqual({ notes: ['one'] })
    expect(second).toEqual({ notes: ['two'] })
    expect(refreshCalls).toBe(1)
    expect(standin.calls).toHaveLength(4)
    // Both retried requests carry the token the single refresh stored.
    expect(recorded(standin, 2).headers.get('authorization')).toBe(
      'Bearer fresh-token',
    )
    expect(recorded(standin, 3).headers.get('authorization')).toBe(
      'Bearer fresh-token',
    )
  })
})

describe('transient retries (idempotent methods only)', () => {
  it('retries a 503 and succeeds', async () => {
    const standin = scriptedStandin(
      textResponse(503, 'Service Unavailable'),
      jsonResponse(200, { ok: true }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      retryPolicy: zeroDelay(3),
    })
    await expect(api<{ ok: boolean }>('/notes')).resolves.toEqual({ ok: true })
    expect(standin.calls).toHaveLength(2)
  })

  it('retries a network failure and succeeds', async () => {
    const standin = scriptedStandin(
      new TypeError('fetch failed'),
      jsonResponse(200, { ok: true }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      retryPolicy: zeroDelay(3),
    })
    await expect(api<{ ok: boolean }>('/notes')).resolves.toEqual({ ok: true })
    expect(standin.calls).toHaveLength(2)
  })

  it('exhausts the budget on repeated bare 503s with the attempt count', async () => {
    const standin = scriptedStandin(
      textResponse(503, 'down'),
      textResponse(503, 'down'),
      textResponse(503, 'down'),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      retryPolicy: zeroDelay(3),
    })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.code).toBe('client.http.503')
    expect(error.status).toBe(503)
    expect(error.attempts).toBe(3)
    expect(standin.calls).toHaveLength(3)
  })

  it('surfaces network exhaustion with the original failure as cause', async () => {
    const standin = scriptedStandin(
      new TypeError('fetch failed: socket hang up'),
      new TypeError('fetch failed: socket hang up'),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      retryPolicy: zeroDelay(2),
    })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.code).toBe(ERROR_CODE_NETWORK)
    expect(error.status).toBe(0)
    expect(error.attempts).toBe(2)
    expect(error.cause).toBeInstanceOf(TypeError)
  })

  for (const method of ['POST', 'PUT', 'PATCH', 'DELETE'] as const) {
    it(`never transient-retries ${method}`, async () => {
      const standin = scriptedStandin(
        textResponse(503, 'Service Unavailable'),
        jsonResponse(200, { ok: true }),
      )
      const api = createClient({
        baseUrl: BASE_URL,
        fetch: standin.fetch,
        retryPolicy: zeroDelay(3),
      })
      const error = await expectApiError(
        api<{ ok: boolean }>('/things', { method, body: {} }),
      )
      expect(error.code).toBe('client.http.503')
      expect(error.attempts).toBe(1)
      expect(standin.calls).toHaveLength(1)
    })
  }

  it('honours Retry-After on 429 before retrying', async () => {
    vi.useFakeTimers()
    try {
      const standin = scriptedStandin(
        textResponse(429, 'rate limited', { 'retry-after': '1' }),
        jsonResponse(200, { ok: true }),
      )
      const api = createClient({
        baseUrl: BASE_URL,
        fetch: standin.fetch,
        retryPolicy: DEFAULT_RETRY_POLICY,
      })
      const pending = api<{ ok: boolean }>('/limited')
      // The header says one second: 999ms in, nothing has been retried.
      await vi.advanceTimersByTimeAsync(999)
      expect(standin.calls).toHaveLength(1)
      await vi.advanceTimersByTimeAsync(1)
      await expect(pending).resolves.toEqual({ ok: true })
      expect(standin.calls).toHaveLength(2)
    } finally {
      vi.useRealTimers()
    }
  })

  it('caps Retry-After at the policy maximum delay', async () => {
    vi.useFakeTimers()
    try {
      const standin = scriptedStandin(
        textResponse(429, 'rate limited', { 'retry-after': '120' }),
        jsonResponse(200, { ok: true }),
      )
      const api = createClient({
        baseUrl: BASE_URL,
        fetch: standin.fetch,
        retryPolicy: DEFAULT_RETRY_POLICY,
      })
      const pending = api<{ ok: boolean }>('/limited')
      // 120s would exceed the 4000ms cap: 3999ms in, still waiting.
      await vi.advanceTimersByTimeAsync(3999)
      expect(standin.calls).toHaveLength(1)
      await vi.advanceTimersByTimeAsync(1)
      await expect(pending).resolves.toEqual({ ok: true })
      expect(standin.calls).toHaveLength(2)
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('timeouts', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('retries a timed-out request when budget remains', async () => {
    const standin = scriptedStandin(hang, jsonResponse(200, { ok: true }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      timeoutMs: 50,
      retryPolicy: zeroDelay(2),
    })
    const pending = api<{ ok: boolean }>('/slow')
    await vi.advanceTimersByTimeAsync(50)
    await vi.advanceTimersByTimeAsync(1)
    await expect(pending).resolves.toEqual({ ok: true })
    expect(standin.calls).toHaveLength(2)
  })

  it('rejects client.timeout with the attempt count once the budget is out', async () => {
    const standin = scriptedStandin(hang)
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      timeoutMs: 50,
      retryPolicy: zeroDelay(1),
    })
    const pending = api<{ ok: boolean }>('/slow')
    // Attach the rejection handler before the fake timers fire, so the
    // rejection inside advanceTimersByTimeAsync is never unhandled.
    const rejection = expectApiError(pending)
    await vi.advanceTimersByTimeAsync(50)
    const error = await rejection
    expect(error.code).toBe(ERROR_CODE_TIMEOUT)
    expect(error.status).toBe(0)
    expect(error.attempts).toBe(1)
    expect(error.cause).toBeInstanceOf(DOMException)
  })
})

describe('caller cancellation', () => {
  it('rejects an already-aborted signal raw, before any request', async () => {
    const standin = scriptedStandin()
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const controller = new AbortController()
    controller.abort()
    let caught: unknown
    try {
      await api<{ ok: boolean }>('/notes', { signal: controller.signal })
    } catch (error) {
      caught = error
    }
    expect(caught).toBeInstanceOf(DOMException)
    if (caught instanceof DOMException) {
      expect(caught.name).toBe('AbortError')
    }
    expect(isApiError(caught)).toBe(false)
    expect(standin.calls).toHaveLength(0)
  })

  it('passes a mid-flight abort through raw, without retrying', async () => {
    const standin = scriptedStandin(hang)
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      retryPolicy: DEFAULT_RETRY_POLICY,
    })
    const controller = new AbortController()
    const pending = api<{ ok: boolean }>('/slow', { signal: controller.signal })
    controller.abort()
    let caught: unknown
    try {
      await pending
    } catch (error) {
      caught = error
    }
    expect(caught).toBeInstanceOf(DOMException)
    if (caught instanceof DOMException) {
      expect(caught.name).toBe('AbortError')
    }
    expect(isApiError(caught)).toBe(false)
    // The caller's signal reached the fetch call and was aborted...
    const call = recorded(standin)
    expect(call.signal?.aborted).toBe(true)
    // ...and no retry followed the cancellation.
    expect(standin.calls).toHaveLength(1)
  })

  it('cancels a request aborted during the backoff, before any retry fires', async () => {
    vi.useFakeTimers()
    try {
      // A deterministic 2s backoff (Retry-After honoured on 503), long
      // enough to cancel in the middle of it.
      const standin = scriptedStandin(
        textResponse(503, 'Service Unavailable', { 'retry-after': '2' }),
      )
      const api = createClient({
        baseUrl: BASE_URL,
        fetch: standin.fetch,
        retryPolicy: DEFAULT_RETRY_POLICY,
      })
      const controller = new AbortController()
      const rejection = expectRawAbort(
        api<{ ok: boolean }>('/notes', { signal: controller.signal }),
      )
      // Let the 503 arrive and the backoff timer arm.
      await vi.advanceTimersByTimeAsync(0)
      controller.abort()
      // The backoff elapses; the retry must not fire after cancellation.
      await vi.advanceTimersByTimeAsync(2000)
      await rejection
      expect(standin.calls).toHaveLength(1)
    } finally {
      vi.useRealTimers()
    }
  })

  it('cancels a request aborted while the refresh is in flight, before the retry fires', async () => {
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    let refreshCalls = 0
    let releaseRefresh: () => void = () => {}
    const refreshGate = new Promise<boolean>((resolve) => {
      releaseRefresh = () => resolve(true)
    })
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      jsonResponse(200, { ok: true }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
      refreshAccessToken: () => {
        refreshCalls += 1
        return refreshGate
      },
    })
    const controller = new AbortController()
    const rejection = expectRawAbort(
      api<{ ok: boolean }>('/notes', { signal: controller.signal }),
    )
    // Each plain await advances the request chain by roughly one
    // microtask hop, so flush until the 401 has started the refresh.
    for (let i = 0; i < 32 && refreshCalls === 0; i += 1) {
      await Promise.resolve()
    }
    expect(refreshCalls).toBe(1)
    controller.abort()
    // The refresh completes successfully -- but the post-refresh retry
    // must not fire for a caller that cancelled.
    releaseRefresh()
    await rejection
    expect(standin.calls).toHaveLength(1)
  })

  it('never delivers a 2xx whose body finished reading after an abort', async () => {
    const body = gatedBody()
    const standin = scriptedStandin(
      new Response(body.stream, { status: 200 }),
    )
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const controller = new AbortController()
    const rejection = expectRawAbort(
      api<{ ok: boolean }>('/notes', { signal: controller.signal }),
    )
    // Let the request reach the point where it is reading the body.
    await Promise.resolve()
    await Promise.resolve()
    controller.abort()
    // The body arrives in full -- the cancelled caller still gets the
    // raw AbortError, never the 2xx result.
    body.release('{"ok":true}')
    await rejection
    expect(standin.calls).toHaveLength(1)
  })

  it('surfaces the raw abort, not a network retry, when the body read fails after cancellation', async () => {
    const body = gatedBody()
    const standin = scriptedStandin(
      new Response(body.stream, { status: 200 }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      retryPolicy: zeroDelay(3),
    })
    const controller = new AbortController()
    const rejection = expectRawAbort(
      api<{ ok: boolean }>('/notes', { signal: controller.signal }),
    )
    // Let the request reach the point where it is reading the body.
    await Promise.resolve()
    await Promise.resolve()
    controller.abort()
    // The body dies mid-read: cancellation wins over the retryable
    // network-class failure the client would otherwise see.
    body.fail(new TypeError('connection lost mid-body'))
    await rejection
    expect(standin.calls).toHaveLength(1)
  })
})

describe('error normalization', () => {
  it('parses a full envelope: code, traceId, message, params, details', async () => {
    const standin = scriptedStandin(
      jsonResponse(400, {
        code: 'notes.validation_failed',
        traceId: 'trace-9',
        message: 'One field failed validation.',
        params: { max: 100 },
        details: [
          { field: 'title', code: 'notes.text_required' },
          {
            field: 'credits',
            code: 'billing.insufficient_credits',
            params: { need: 5 },
          },
        ],
      }),
    )
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.status).toBe(400)
    expect(error.code).toBe('notes.validation_failed')
    expect(error.traceId).toBe('trace-9')
    expect(error.message).toBe('One field failed validation.')
    expect(error.params).toEqual({ max: 100 })
    expect(error.details).toEqual([
      { field: 'title', code: 'notes.text_required' },
      {
        field: 'credits',
        code: 'billing.insufficient_credits',
        params: { need: 5 },
      },
    ])
    expect(error.auth).toBe(false)
    expect(error.attempts).toBe(1)
  })

  it('drops structurally invalid entries from envelope details', async () => {
    const standin = scriptedStandin(
      jsonResponse(400, {
        code: 'notes.validation_failed',
        traceId: 'trace-9',
        details: [
          { field: 'title', code: 'notes.text_required' },
          {
            field: 'price',
            code: 'billing.invalid_amount',
            params: { min: 0 },
          },
          { field: 'orphan' },
          { code: 'notes.unknown' },
          { field: 'broken', code: 42 },
          'not an object',
          { field: 'p', code: 'notes.ok', params: 'junk' },
        ],
      }),
    )
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.details).toEqual([
      { field: 'title', code: 'notes.text_required' },
      { field: 'price', code: 'billing.invalid_amount', params: { min: 0 } },
      // A valid entry keeps its place even when junk params are dropped.
      { field: 'p', code: 'notes.ok' },
    ])
  })

  it('synthesizes an English diagnostic when the envelope has no message', async () => {
    const standin = scriptedStandin(
      jsonResponse(400, {
        code: 'notes.text_required',
        traceId: 'trace-1',
      }),
    )
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.code).toBe('notes.text_required')
    expect(error.message).toBe('The API rejected the request (notes.text_required).')
  })

  it('maps a bare non-JSON error body to client.http.<status>', async () => {
    const standin = scriptedStandin(
      textResponse(500, '<html><body>Internal Server Error</body></html>'),
    )
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.code).toBe('client.http.500')
    expect(error.status).toBe(500)
    expect(error.traceId).toBeUndefined()
    expect(error.message).toBe(
      'The server answered HTTP 500 without a valid ApiError envelope.',
    )
  })

  it('maps JSON without the required envelope fields to client.http.<status>', async () => {
    const standin = scriptedStandin(
      jsonResponse(400, { error: 'no envelope here' }),
    )
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.code).toBe('client.http.400')
    expect(error.message).toBe(
      'The server answered HTTP 400 without a valid ApiError envelope.',
    )
  })

  it('rejects a non-JSON 2xx body as client.protocol', async () => {
    const standin = scriptedStandin(textResponse(200, 'definitely not json'))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.code).toBe(ERROR_CODE_PROTOCOL)
    expect(error.status).toBe(200)
    expect(error.attempts).toBe(1)
    expect(error.cause).toBeInstanceOf(SyntaxError)
  })

  it('rejects a 2xx JSON primitive as client.protocol', async () => {
    for (const body of [42, 'just a string', true]) {
      const standin = scriptedStandin(jsonResponse(200, body))
      const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
      const error = await expectApiError(api<{ ok: boolean }>('/notes'))
      expect(error.code, `body ${JSON.stringify(body)}`).toBe(
        ERROR_CODE_PROTOCOL,
      )
    }
  })
})
