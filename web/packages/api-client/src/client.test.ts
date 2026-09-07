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
  isTransportFailure,
  type ApiError,
  type RequestFn,
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

/** Waits for `isSettled()` to turn true -- flushing the microtask
 * queue first, then polling on short real timers -- and fails the test
 * when the budget runs out with the thing still unsettled. The
 * never-settling shapes this guards (a refresh hook that never
 * resolves) are deterministic: the pre-fix code stays pending forever,
 * so the bounded wait turns what used to be a hang into an assertion
 * failure, in both directions of the regression. */
async function expectSettled(
  isSettled: () => boolean,
  budgetMs = 2000,
): Promise<void> {
  for (let i = 0; i < 64 && !isSettled(); i += 1) {
    await Promise.resolve()
  }
  const deadline = Date.now() + budgetMs
  while (!isSettled() && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 2))
  }
  expect(isSettled()).toBe(true)
}

/** A body stream the test gates by hand: the response's text() stays
 * pending until release (success) or fail (error) is called. A
 * cancellation of the stream -- the client's connection-release path
 * when it discards an unread response body -- is recorded in
 * `cancelled`, and `pulling` records that a read has genuinely started
 * (the stream's pull has been requested), giving tests a deterministic
 * mid-read point. Both are getters, not value fields: the hooks mutate
 * the closure variables, and the caller must observe the live state
 * after the request ran. */
function gatedBody(): {
  stream: ReadableStream<Uint8Array>
  release: (text: string) => void
  fail: (cause: unknown) => void
  readonly cancelled: boolean
  readonly pulling: boolean
} {
  let release: (text: string) => void = () => {}
  let fail: (cause: unknown) => void = () => {}
  let cancelled = false
  let pulling = false
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
    pull() {
      pulling = true
    },
    cancel() {
      cancelled = true
    },
  })
  return {
    stream,
    release,
    fail,
    get cancelled() {
      return cancelled
    },
    get pulling() {
      return pulling
    },
  }
}

/** Flushes microtasks until the body read has genuinely started (its
 * pull has been requested) or the hop budget runs out. */
async function waitForReadStart(body: {
  readonly pulling: boolean
}): Promise<void> {
  for (let i = 0; i < 200 && !body.pulling; i += 1) {
    await Promise.resolve()
  }
  expect(body.pulling).toBe(true)
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

  it('honours omitAccessToken: the request goes out credential-less even when the store holds a token', async () => {
    // A session's own refresh request is the canonical user: it must
    // never present an access token, and saying so per request must
    // not require clearing the store to make it true.
    const store = createMemoryAccessTokenStore()
    store.set('store-token')
    const standin = scriptedStandin(jsonResponse(200, { ok: true }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
    })
    await api<{ ok: boolean }>('/api/v1/authn/token/refresh', {
      method: 'POST',
      omitAccessToken: true,
    })
    const call = recorded(standin)
    expect(call.headers.get('authorization')).toBeNull()
    // The store is untouched: credential-less-ness was declared on the
    // request, never manufactured by clearing the session's token --
    // concurrent requests keep presenting the token they hold.
    expect(store.get()).toBe('store-token')
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

  it('honours a caller-supplied accept: application/json is only the default when absent', async () => {
    // accept and content-type are defaults, not overrides: each is set
    // only when the caller did not already supply the header (mirroring
    // the content-type guard), so a caller that knows the endpoint
    // answers something other than JSON keeps its own Accept.
    const standin = scriptedStandin(
      jsonResponse(200, { ok: true }),
      jsonResponse(200, { ok: true }),
    )
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    await api<{ ok: boolean }>('/notes', {
      headers: { 'x-request-id': 'req-1', accept: 'text/plain' },
    })
    const call = recorded(standin)
    expect(call.headers.get('x-request-id')).toBe('req-1')
    expect(call.headers.get('accept')).toBe('text/plain')
    // The default still applies to a request that sends no accept.
    await api<{ ok: boolean }>('/other')
    expect(recorded(standin, 1).headers.get('accept')).toBe('application/json')
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

  it('rejects a body that cannot be JSON-serialized as a coded client.protocol ApiError', async () => {
    // A circular body can never be sent: it must reject through the
    // package's one error type with the reserved client.* vocabulary,
    // not as a bare TypeError out of JSON.stringify. Nothing was ever
    // put on the wire, so attempts is 0.
    const circular: Record<string, unknown> = {}
    circular.self = circular
    const standin = scriptedStandin(jsonResponse(201, { id: 'n-1' }))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(
      api<{ id: string }>('/notes', { method: 'POST', body: circular }),
    )
    expect(error.code).toBe(ERROR_CODE_PROTOCOL)
    expect(error.status).toBe(0)
    expect(error.attempts).toBe(0)
    expect(error.cause).toBeInstanceOf(TypeError)
    expect(standin.calls).toHaveLength(0)
  })

  it('refuses a body JSON.stringify cannot serialize, instead of sending the request silently bodyless', async () => {
    // Symmetric with the circular guard: a body that serializes to
    // nothing -- JSON.stringify yields undefined (the value) for a
    // top-level function or symbol -- is a request the client can
    // never send as JSON, and must refuse as client.protocol before
    // anything goes on the wire. (Before the fix it went out silently
    // bodyless -- no body, no content-type -- and the "created"
    // answer came back for a request the caller believed carried
    // data.)
    for (const body of [
      (): Record<string, string> => ({ title: 'T' }),
      Symbol('no-json'),
    ]) {
      const standin = scriptedStandin(jsonResponse(201, { id: 'n-1' }))
      const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
      const error = await expectApiError(
        api<{ id: string }>('/notes', { method: 'POST', body }),
      )
      expect(error.code, `body ${String(body)}`).toBe(ERROR_CODE_PROTOCOL)
      expect(error.status).toBe(0)
      expect(error.attempts).toBe(0)
      expect(error.cause).toBeInstanceOf(TypeError)
      expect(standin.calls).toHaveLength(0)
    }
  })

  it('encodes an array query value as repeated parameters, never comma-joined', async () => {
    // An array is the form/explode convention: tag=['a','b c',3] must
    // reach the backend as three repeated parameters -- the shape a Go
    // r.URL.Query() handler parses as a multi-valued key. (Before the
    // fix the array was String()-ed whole, folding into one
    // comma-joined ?tag=a,b c,3 the backend would read as a single
    // literal value.)
    const standin = scriptedStandin(jsonResponse(200, []))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    await api('/notes', {
      query: { tag: ['a', 'b c', 3], keep: 'x' },
    })
    expect(recorded(standin).url).toBe(
      `${BASE_URL}/notes?tag=a&tag=b+c&tag=3&keep=x`,
    )
  })

  it('omitAccessToken skips only the store token: a caller-supplied authorization header survives untouched', async () => {
    // omitAccessToken's contract is "never read the token store", not
    // "strip every Authorization header": the request's headers are the
    // caller's, so a caller-supplied authorization survives while the
    // store token (which would have overwritten it) is never attached.
    // (Probe: store null + caller header; and the store-token case
    // below, where the store holds one and the caller header still
    // wins, because the store is not consulted at all.)
    for (const probe of [
      { storeToken: null, callerHeader: 'Bearer CALLER' },
      { storeToken: 'store-token', callerHeader: 'Bearer CALLER' },
    ]) {
      const store = createMemoryAccessTokenStore()
      store.set(probe.storeToken)
      const standin = scriptedStandin(jsonResponse(200, { ok: true }))
      const api = createClient({
        baseUrl: BASE_URL,
        fetch: standin.fetch,
        accessTokenStore: store,
      })
      await api('/notes', {
        omitAccessToken: true,
        headers: { authorization: probe.callerHeader },
      })
      expect(recorded(standin).headers.get('authorization')).toBe(
        'Bearer CALLER',
      )
    }
  })

  it('refuses a query option on a path that already carries its own query string', async () => {
    // buildUrl appends query parameters with '?': a path that already
    // carries a query string would become a malformed double-? URL
    // (?draft=1?tag=a) the server would misparse. Request paths are
    // spec paths -- never query-bearing -- so the combination is a
    // programmer error, refused before anything is put on the wire,
    // with the fix named in the message.
    const standin = scriptedStandin(jsonResponse(200, { ok: true }))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const promise = api<{ ok: boolean }>('/notes?draft=1', {
      query: { tag: 'a' },
    })
    await expect(promise).rejects.toThrow(
      'request path must not carry a query string',
    )
    expect(standin.calls).toHaveLength(0)
  })

  it('sends a query-bearing path verbatim when no query option is given', async () => {
    // The refusal above applies only to the ?-appending combination: a
    // path that carries its own query string and no query option is a
    // complete URL the caller wrote, sent exactly as given.
    const standin = scriptedStandin(jsonResponse(200, { ok: true }))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    await api<{ ok: boolean }>('/notes?draft=1')
    expect(recorded(standin).url).toBe(`${BASE_URL}/notes?draft=1`)
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
    // The refused request presents a token (the bearer-only rule: a
    // credential-less 401 never triggers a refresh), so the sequence
    // under test is stale-token 401 -> refresh -> retry refused again.
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      jsonResponse(401, { ...SESSION_EXPIRED, traceId: 'trace-2' }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
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
    // A token must be presented for the refresh path to engage.
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    const standin = scriptedStandin(jsonResponse(401, { ...SESSION_EXPIRED }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
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
    // the envelope's code and trace id so it can be correlated to
    // server logs -- under the reporter's snake_case key (the
    // envelope's wire field stays camelCase traceId on the ApiError;
    // only the report attribute is trace_id).
    expect(memory.warns).toEqual([
      {
        message: 'access token refresh failed',
        attrs: {
          status: 401,
          code: 'authn.session_expired',
          trace_id: 'trace-1',
        },
      },
    ])
  })

  it('reports a bare-401 refresh failure without envelope attrs', async () => {
    const memory = createMemoryReporter()
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    const standin = scriptedStandin(jsonResponse(401, { error: 'no envelope' }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
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

  it('reports a traceId-less refresh-failure envelope with its real code', async () => {
    // The 401 path of the same backend-shaped envelope regression: a
    // code-only body must keep its real module code through the
    // refresh-failure report, and the report's attrs must not carry an
    // undefined-valued traceId key when the envelope had none.
    const memory = createMemoryReporter()
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    const standin = scriptedStandin(
      jsonResponse(401, { code: 'authn.session_expired' }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
      refreshAccessToken: async () => false,
      reporter: memory.reporter,
    })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.auth).toBe(true)
    expect(error.code).toBe('authn.session_expired')
    expect(error.traceId).toBeUndefined()
    expect(memory.warns).toEqual([
      {
        message: 'access token refresh failed',
        attrs: { status: 401, code: 'authn.session_expired' },
      },
    ])
  })

  it('treats a throwing refresh hook as a failed refresh', async () => {
    const memory = createMemoryReporter()
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    const standin = scriptedStandin(jsonResponse(401, { ...SESSION_EXPIRED }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
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

  it('leaves a credential-less 401 untouched: refresh cannot supply missing authentication', async () => {
    const memory = createMemoryReporter()
    let refreshCalls = 0
    const standin = scriptedStandin(jsonResponse(401, { ...SESSION_EXPIRED }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: createMemoryAccessTokenStore(),
      refreshAccessToken: async () => {
        refreshCalls += 1
        return true
      },
      reporter: memory.reporter,
    })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.auth).toBe(true)
    expect(error.code).toBe('authn.session_expired')
    expect(error.attempts).toBe(1)
    // No token was presented, so no refresh was attempted: a 401 on a
    // credential-less request means the endpoint demands
    // authentication, which refreshing cannot provide -- and no false
    // 'refresh failed' warning fires either.
    expect(refreshCalls).toBe(0)
    expect(memory.warns).toHaveLength(0)
    expect(standin.calls).toHaveLength(1)
    expect(recorded(standin).headers.get('authorization')).toBeNull()
  })

  it('does not recurse when the refresh request itself is refused', async () => {
    // The session wiring a host builds -- the refresh operation
    // declared credential-less via omitAccessToken, travelling through
    // this same client -- must not deadlock when the endpoint refuses
    // the stale token. Before the bearer-only rule the refresh
    // request's own 401 re-entered the refresh path and awaited the
    // in-flight refresh it was part of -- a request awaiting itself,
    // forever. The declaration makes that impossibility structural: the
    // refresh request is credential-less no matter what the store
    // holds, so its 401 never engages the refresh path.
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    let refreshCalls = 0
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      jsonResponse(401, { ...SESSION_EXPIRED, traceId: 'trace-2' }),
    )
    // The hook only ever runs after createClient has returned (it fires
    // on a request's 401), so the const binding is initialized by the
    // time the closure body executes.
    const api: RequestFn = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
      refreshAccessToken: async () => {
        refreshCalls += 1
        // A session's refresh travels credential-less by declaration --
        // the refresh token rides in the body, never in an
        // Authorization header, and the store is never cleared for it.
        try {
          await api('/api/v1/authn/token/refresh', {
            method: 'POST',
            body: { refresh_token: 'stale' },
            omitAccessToken: true,
          })
          return true
        } catch {
          return false
        }
      },
    })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.auth).toBe(true)
    expect(error.code).toBe('authn.session_expired')
    // The original 401's own envelope is delivered (trace-1), not the
    // refresh request's (trace-2).
    expect(error.traceId).toBe('trace-1')
    // Exactly one refresh, two HTTP calls in total, no recursion.
    expect(refreshCalls).toBe(1)
    expect(standin.calls).toHaveLength(2)
    // The refresh request itself carried no bearer token -- and the
    // store was never cleared to make it so.
    expect(recorded(standin, 1).headers.get('authorization')).toBeNull()
    expect(store.get()).toBe('stale-token')
  })

  it('does not self-deadlock when the refresh operation forgets omitAccessToken on its own request', async () => {
    // The host bug this guard backstops: the refresh operation travels
    // through this same client WITHOUT the per-request credential-less
    // declaration, so its request presents the stale token and the
    // refresh endpoint refuses it with a token-bearing 401. Before the
    // guard that 401 re-entered the refresh path and joined -- and
    // awaited -- the very flight it was part of: a request awaiting
    // itself forever (the flight could only settle when its hook
    // settled, and the hook awaited the request). refreshCalls froze
    // at 1 and nothing ever settled. The round's own request is now
    // refused the refresh path, exactly like the bearer-only rule
    // refuses a credential-less 401: the inner 401 surfaces as the
    // terminal auth error, the hook answers false, and the original
    // request takes the ordinary failed-refresh path -- a finite,
    // honest failure, never a self-deadlock and never a second
    // concurrent refresh.
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    let refreshCalls = 0
    const memory = createMemoryReporter()
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      jsonResponse(401, { ...SESSION_EXPIRED, traceId: 'trace-refresh' }),
    )
    // The hook only ever runs after createClient has returned (it fires
    // on a request's 401), so the const binding is initialized by the
    // time the closure body executes.
    const api: RequestFn = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
      refreshAccessToken: async () => {
        refreshCalls += 1
        try {
          await api('/api/v1/authn/token/refresh', {
            method: 'POST',
            body: { refresh_token: 'stale' },
            // omitAccessToken deliberately omitted: the host bug.
          })
          return true
        } catch {
          return false
        }
      },
      reporter: memory.reporter,
    })
    let settled = false
    let caught: unknown
    const observed = api<{ ok: boolean }>('/notes').then(
      () => {},
      (error: unknown) => {
        settled = true
        caught = error
      },
    )
    await expectSettled(() => settled)
    expect(isApiError(caught)).toBe(true)
    if (isApiError(caught)) {
      expect(caught.auth).toBe(true)
      expect(caught.code).toBe('authn.session_expired')
      // The original 401's own envelope is delivered (trace-1), not the
      // refresh request's own refusal (trace-refresh).
      expect(caught.traceId).toBe('trace-1')
    }
    // Exactly one refresh, two HTTP calls in total, no recursion -- and
    // the failure was reported like any other failed refresh.
    expect(refreshCalls).toBe(1)
    expect(standin.calls).toHaveLength(2)
    expect(memory.warns).toEqual([
      {
        message: 'access token refresh failed',
        attrs: {
          status: 401,
          code: 'authn.session_expired',
          trace_id: 'trace-1',
        },
      },
    ])
    await observed
  })

  it('keeps a declared credential-less 401 terminal even while the store holds a token', async () => {
    // The refresh operation's own request is the canonical declared
    // credential-less one: when the endpoint refuses a stale refresh
    // token, that 401 must surface to the session -- it must not
    // re-enter the refresh path just because the store happens to hold
    // an access token (the stale one, never cleared). Before
    // omitAccessToken existed this scenario was only reachable with an
    // emptied store; the declaration pins it without the clearing.
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    let refreshCalls = 0
    const standin = scriptedStandin(jsonResponse(401, { ...SESSION_EXPIRED }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
      refreshAccessToken: async () => {
        refreshCalls += 1
        return true
      },
    })
    const error = await expectApiError(
      api<{ ok: boolean }>('/api/v1/authn/token/refresh', {
        method: 'POST',
        body: { refresh_token: 'stale' },
        omitAccessToken: true,
      }),
    )
    expect(error.auth).toBe(true)
    expect(error.attempts).toBe(1)
    expect(refreshCalls).toBe(0)
    expect(standin.calls).toHaveLength(1)
    expect(recorded(standin).headers.get('authorization')).toBeNull()
    expect(store.get()).toBe('stale-token')
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

  it('does not let the refresh round consume the transient-retry budget', async () => {
    // maxAttempts 2 leaves a fresh request exactly one transient
    // retry. A 401-refresh round in the middle performs no transient
    // retry, so it must not eat that budget: the 503 that follows the
    // refresh still gets its retry. (Before the fix the refresh round
    // consumed the second attempt, so the retryable 503 was delivered
    // without ever being retried.)
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    let refreshCalls = 0
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      textResponse(503, 'Service Unavailable'),
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
      retryPolicy: zeroDelay(2),
    })
    await expect(api<{ ok: boolean }>('/notes')).resolves.toEqual({ ok: true })
    expect(refreshCalls).toBe(1)
    expect(standin.calls).toHaveLength(3)
  })

  it('releases the refused 401 body when the refresh succeeds and the request retries', async () => {
    // The refresh-success path discards the 401 response: its body is
    // cancelled so the connection is released deterministically
    // instead of staying held by an unread response -- and a stalled
    // error body must not delay or hang the refresh round, which never
    // needs to read it.
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    const body = gatedBody()
    const standin = scriptedStandin(
      new Response(body.stream, { status: 401 }),
      jsonResponse(200, { ok: true }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
      refreshAccessToken: async () => {
        store.set('fresh-token')
        return true
      },
    })
    await expect(api<{ ok: boolean }>('/notes')).resolves.toEqual({ ok: true })
    expect(standin.calls).toHaveLength(2)
    expect(body.cancelled).toBe(true)
  })

  it('keeps the real 401 envelope when a failed refresh outlives timeoutMs', { timeout: 1000 }, async () => {
    // The timeout/refresh cross case: a refresh hook configured WITH a
    // timeoutMs, where the refresh round trip outlives the timeout and
    // then fails. The timeout bounds one HTTP exchange -- never the
    // refresh hook -- so the envelope read after the failed refresh
    // must surface the real 401 envelope (authn.session_expired and
    // its trace id), not a synthetic client.http.401. (Before the fix
    // the attempt timer fired at timeoutMs into the refresh; the
    // already-settled abort trigger then resolved the envelope read as
    // a timeout, and the envelope -- code and trace id -- was lost.)
    vi.useFakeTimers()
    try {
      const store = createMemoryAccessTokenStore()
      store.set('stale-token')
      let refreshCalls = 0
      let releaseRefresh: (ok: boolean) => void = () => {}
      const refreshGate = new Promise<boolean>((resolve) => {
        releaseRefresh = resolve
      })
      const standin = scriptedStandin(jsonResponse(401, { ...SESSION_EXPIRED }))
      const api = createClient({
        baseUrl: BASE_URL,
        fetch: standin.fetch,
        accessTokenStore: store,
        refreshAccessToken: () => {
          refreshCalls += 1
          return refreshGate
        },
        timeoutMs: 50,
      })
      const pending = api<{ ok: boolean }>('/notes')
      // Flush until the 401 has started the refresh round.
      for (let i = 0; i < 32 && refreshCalls === 0; i += 1) {
        await Promise.resolve()
      }
      expect(refreshCalls).toBe(1)
      // The refresh outlives the 50ms timeout by two orders of
      // magnitude...
      await vi.advanceTimersByTimeAsync(5000)
      // ...and then fails: the session is still gone.
      releaseRefresh(false)
      const error = await expectApiError(pending)
      expect(error.auth).toBe(true)
      expect(error.code).toBe('authn.session_expired')
      expect(error.traceId).toBe('trace-1')
      expect(error.attempts).toBe(1)
      expect(standin.calls).toHaveLength(1)
    } finally {
      vi.useRealTimers()
    }
  })

  it('keeps the attempt timeout live for a stalled 401 body after a failed refresh', { timeout: 1000 }, async () => {
    // The failure path's envelope read stays inside the attempt's
    // abort scope: a 401 whose body stalls (headers arrived, body
    // never comes) must still degrade to an envelope-less
    // client.http.401 instead of hanging on the read. The timer is
    // re-armed for the budget that was left at the pause once the
    // failed refresh is out of the way -- if the pause were never
    // resumed, this read would hang on the stalled stream.
    vi.useFakeTimers()
    try {
      const store = createMemoryAccessTokenStore()
      store.set('stale-token')
      let refreshCalls = 0
      let releaseRefresh: (ok: boolean) => void = () => {}
      const refreshGate = new Promise<boolean>((resolve) => {
        releaseRefresh = resolve
      })
      const body = gatedBody()
      const standin = scriptedStandin(
        new Response(body.stream, { status: 401 }),
      )
      const api = createClient({
        baseUrl: BASE_URL,
        fetch: standin.fetch,
        accessTokenStore: store,
        refreshAccessToken: () => {
          refreshCalls += 1
          return refreshGate
        },
        timeoutMs: 50,
      })
      const pending = api<{ ok: boolean }>('/notes')
      for (let i = 0; i < 32 && refreshCalls === 0; i += 1) {
        await Promise.resolve()
      }
      expect(refreshCalls).toBe(1)
      // The refresh fails fast -- no fake time passes -- and the
      // envelope read then genuinely starts on the stalled body.
      releaseRefresh(false)
      await waitForReadStart(body)
      // Attach the rejection handler before the re-armed timer fires,
      // so the rejection inside advanceTimersByTimeAsync is never
      // unhandled.
      const rejection = expectApiError(pending)
      // The re-armed timer (the ~50ms that were left) fires into the
      // stalled read: envelope-less degradation, never a hang.
      await vi.advanceTimersByTimeAsync(50)
      const error = await rejection
      expect(error.auth).toBe(true)
      expect(error.code).toBe('client.http.401')
      expect(error.traceId).toBeUndefined()
      expect(standin.calls).toHaveLength(1)
    } finally {
      vi.useRealTimers()
    }
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

  it('releases the discarded response body before retrying a retryable status', async () => {
    // A 429 (or any retryable status) whose body is not wanted is
    // retried from the headers alone -- but the unread response must
    // not keep holding its connection: the body is cancelled before
    // the retry instead of being left for garbage collection.
    const body = gatedBody()
    const standin = scriptedStandin(
      new Response(body.stream, { status: 429 }),
      jsonResponse(200, { ok: true }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      retryPolicy: zeroDelay(2),
    })
    await expect(api<{ ok: boolean }>('/limited')).resolves.toEqual({ ok: true })
    expect(standin.calls).toHaveLength(2)
    expect(body.cancelled).toBe(true)
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

  it('times out a 2xx whose body stalls after the headers arrived', { timeout: 1000 }, async () => {
    // Real un-released-stream shape: the server answered headers and
    // then never finishes the body. The per-attempt timeout keeps
    // running through the body read, so the stall rejects as
    // client.timeout instead of hanging forever on a half-open
    // response. (Before the fix the timer stopped at header arrival
    // and the read waited on the stalled stream indefinitely.) The
    // release is the controller abort (real fetch ties the response
    // body to the fetch signal): the recorded request signal shows the
    // attempt aborted mid-read.
    const body = gatedBody()
    const standin = scriptedStandin(new Response(body.stream, { status: 200 }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      timeoutMs: 50,
      retryPolicy: zeroDelay(1),
    })
    const pending = api<{ ok: boolean }>('/slow-body')
    // Attach the rejection handler before the fake timers fire, so the
    // rejection inside advanceTimersByTimeAsync is never unhandled.
    const rejection = expectApiError(pending)
    // Let the headers arrive and the body read genuinely start.
    await vi.advanceTimersByTimeAsync(0)
    await waitForReadStart(body)
    await vi.advanceTimersByTimeAsync(50)
    const error = await rejection
    expect(error.code).toBe(ERROR_CODE_TIMEOUT)
    expect(error.status).toBe(0)
    expect(error.attempts).toBe(1)
    expect(error.cause).toBeInstanceOf(DOMException)
    expect(standin.calls).toHaveLength(1)
    expect(recorded(standin).signal?.aborted).toBe(true)
  })

  it('retries a 2xx whose body stalls mid-read, exactly like any other timeout', { timeout: 1000 }, async () => {
    // The stalled-body timeout is a transport-class failure of the
    // attempt: an idempotent request with budget left retries it, and
    // the retried attempt answers in full.
    const body = gatedBody()
    let first = true
    const standin = createStandinFetch(() => {
      if (first) {
        first = false
        return new Response(body.stream, { status: 200 })
      }
      return jsonResponse(200, { ok: true })
    })
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      timeoutMs: 50,
      retryPolicy: zeroDelay(2),
    })
    const pending = api<{ ok: boolean }>('/slow-body')
    await vi.advanceTimersByTimeAsync(0)
    await waitForReadStart(body)
    await vi.advanceTimersByTimeAsync(50)
    // The retry's 0ms backoff timer is armed while that advancement is
    // in progress (due at its target time); a further 1ms advancement
    // carries the clock past it and lets attempt 2 proceed.
    await vi.advanceTimersByTimeAsync(1)
    await expect(pending).resolves.toEqual({ ok: true })
    expect(standin.calls).toHaveLength(2)
    expect(recorded(standin, 0).signal?.aborted).toBe(true)
    expect(recorded(standin, 1).signal?.aborted).toBe(false)
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

  it('rejects raw when an abort lands while a never-settling refresh is in flight', async () => {
    // The reviewer's real shape: a client WITH a timeoutMs, whose
    // 401-refresh round suspends the attempt timer (the pause), and a
    // refresh hook that never settles. An abort landing after the
    // hook has entered used to race nothing: the attempt's fetch had
    // already settled and its timer was suspended, so the caller-abort
    // forwarding had no pending promise to reject -- the request
    // promise stayed pending forever even though the caller had
    // cancelled (and at 50ms the suspended attempt timer could not
    // save it either). The refresh wait now races the caller's signal
    // exactly like the backoff sleeps do, so the abort rejects the
    // request raw on the next microtask.
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    let refreshCalls = 0
    const standin = scriptedStandin(jsonResponse(401, { ...SESSION_EXPIRED }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
      refreshAccessToken: () => {
        refreshCalls += 1
        // A refresh that never settles: no host wiring will end it.
        return new Promise<boolean>(() => {})
      },
      timeoutMs: 50,
    })
    const controller = new AbortController()
    let reason: unknown
    const observed = api<{ ok: boolean }>('/notes', {
      signal: controller.signal,
    }).then(
      () => {},
      (caught: unknown) => {
        reason = caught
      },
    )
    // Each plain await advances the request chain by roughly one
    // microtask hop, so flush until the 401 has started the refresh.
    for (let i = 0; i < 32 && refreshCalls === 0; i += 1) {
      await Promise.resolve()
    }
    expect(refreshCalls).toBe(1)
    controller.abort()
    await expectSettled(() => reason !== undefined)
    expect(reason).toBeInstanceOf(DOMException)
    if (reason instanceof DOMException) {
      expect(reason.name).toBe('AbortError')
    }
    expect(isApiError(reason)).toBe(false)
    // The abort ended the request: exactly the one refused attempt,
    // no post-refresh retry.
    expect(standin.calls).toHaveLength(1)
    await observed
  })

  it('keeps the round alive when the starting request aborts: a later 401 joins it, never a second hook invocation', async () => {
    // The round is shared, not owned by the request that started it:
    // that request's abort detaches only itself (its own call-site
    // race rejects it raw), while the flight settles only when the
    // hook settles. A later, independent 401 arriving while the round
    // is still in flight therefore joins it -- the hook is not invoked
    // again (a fresh invocation would present the same refresh token a
    // second time while the first round still holds it, which the
    // authn server reads as theft) -- and the later request's own
    // abort rejects it raw in turn: each waiter is bounded by its own
    // signal. (Before the fix the starting request's abort settled the
    // whole round as a failed refresh, so the later 401 started a
    // fresh round and invoked the hook a second time.)
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    let refreshCalls = 0
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      jsonResponse(401, { ...SESSION_EXPIRED, traceId: 'trace-2' }),
    )
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      accessTokenStore: store,
      refreshAccessToken: () => {
        refreshCalls += 1
        // A refresh that never settles: no host wiring will end it.
        return new Promise<boolean>(() => {})
      },
    })
    const firstController = new AbortController()
    let firstReason: unknown
    const first = api<{ ok: boolean }>('/notes', {
      signal: firstController.signal,
    }).then(
      () => {},
      (caught: unknown) => {
        firstReason = caught
      },
    )
    for (let i = 0; i < 32 && refreshCalls === 0; i += 1) {
      await Promise.resolve()
    }
    expect(refreshCalls).toBe(1)
    // The first caller gives up mid-refresh: the round is orphaned,
    // not dead -- nothing settles it but the hook itself.
    firstController.abort()
    await expectSettled(() => firstReason !== undefined)
    expect(firstReason).toBeInstanceOf(DOMException)
    if (firstReason instanceof DOMException) {
      expect(firstReason.name).toBe('AbortError')
    }
    expect(isApiError(firstReason)).toBe(false)
    // The second, independent request with its own controller: its 401
    // joins the round still in flight -- the hook is never invoked a
    // second time -- and its own abort rejects it raw.
    const secondController = new AbortController()
    let secondReason: unknown
    const second = api<{ ok: boolean }>('/notes', {
      signal: secondController.signal,
    }).then(
      () => {},
      (caught: unknown) => {
        secondReason = caught
      },
    )
    for (let i = 0; i < 64 && standin.calls.length < 2; i += 1) {
      await Promise.resolve()
    }
    // Flush well past the second request's own 401 branch: by the time
    // it decides the round's fate -- join the in-flight flight (the
    // honest shape) or start a sibling round (the pre-fix shape, which
    // invokes the hook again) -- it must still be the one round.
    for (let i = 0; i < 200; i += 1) {
      await Promise.resolve()
    }
    expect(refreshCalls).toBe(1)
    secondController.abort()
    await expectSettled(() => secondReason !== undefined)
    expect(secondReason).toBeInstanceOf(DOMException)
    if (secondReason instanceof DOMException) {
      expect(secondReason.name).toBe('AbortError')
    }
    expect(isApiError(secondReason)).toBe(false)
    // Two refused attempts only: no retry fired for either cancelled
    // request.
    expect(standin.calls).toHaveLength(2)
    await first
    await second
  })

  it('lets requests already joined onto the flight succeed when the initiator aborts and the refresh then succeeds', async () => {
    // The conflation shape: the single-flight round used to be bound
    // to the initiating request's own signal -- that request's abort
    // settled the WHOLE round as a failed refresh (the race rejection
    // was caught and turned into false), so a request that had already
    // joined was told "refresh failed" -- an auth:true ApiError, the
    // host's basis for judging the session ended -- even while the
    // hook went on to succeed and write a fresh token into the store.
    // The initiator's abort must detach only the initiator: the joined
    // request is bounded by its own (live) signal, so it must receive
    // the hook's real outcome -- a retry with the fresh token, and
    // success.
    const store = createMemoryAccessTokenStore()
    store.set('stale-token')
    let refreshCalls = 0
    let releaseRefresh: (ok: boolean) => void = () => {}
    const refreshGate = new Promise<boolean>((resolve) => {
      releaseRefresh = resolve
    })
    const standin = scriptedStandin(
      jsonResponse(401, { ...SESSION_EXPIRED }),
      jsonResponse(401, { ...SESSION_EXPIRED, traceId: 'trace-2' }),
      jsonResponse(200, { notes: ['two'] }),
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
    const initiatorController = new AbortController()
    const initiator = expectRawAbort(
      api<{ ok: boolean }>('/notes', {
        signal: initiatorController.signal,
      }),
    )
    // Flush until the initiator's 401 has started the refresh round.
    for (let i = 0; i < 32 && refreshCalls === 0; i += 1) {
      await Promise.resolve()
    }
    expect(refreshCalls).toBe(1)
    // A second request 401s while the round is in flight and joins it.
    const joiner = api<{ notes: string[] }>('/notes')
    for (let i = 0; i < 64 && standin.calls.length < 2; i += 1) {
      await Promise.resolve()
    }
    // Let the joiner's refused attempt reach the joined wait before
    // the round's fate is decided.
    for (let i = 0; i < 64; i += 1) {
      await Promise.resolve()
    }
    // The initiator gives up mid round -- and the hook then succeeds,
    // storing the fresh token.
    initiatorController.abort()
    store.set('fresh-token')
    releaseRefresh(true)
    await initiator
    // The already-joined request heard nothing of the initiator's
    // abort: its own signal never fired, so it gets the hook's real
    // outcome and its retry with the fresh token succeeds. (Before the
    // fix the initiator's abort settled the shared round as a failed
    // refresh, so the joiner was told the refresh failed and this
    // rejected with an auth:true ApiError instead.)
    await expect(joiner).resolves.toEqual({ notes: ['two'] })
    expect(recorded(standin, 2).headers.get('authorization')).toBe(
      'Bearer fresh-token',
    )
    expect(refreshCalls).toBe(1)
    expect(standin.calls).toHaveLength(3)
  })

  it('never delivers a 2xx for a caller that aborts mid-read -- even when the body never settles', { timeout: 1000 }, async () => {
    // Real un-released-stream shape: the body stream is never released
    // and never fails. The rejection must arrive anyway -- the client
    // refuses to wait on a body whose caller is gone -- so the
    // cancelled caller gets the raw AbortError, never the 2xx result.
    // The release is the controller abort itself (real fetch ties the
    // response body to the fetch signal): the recorded request signal
    // must show the attempt aborted even though the fetch itself had
    // already resolved with its headers.
    const body = gatedBody()
    const standin = scriptedStandin(new Response(body.stream, { status: 200 }))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const controller = new AbortController()
    const rejection = expectRawAbort(
      api<{ ok: boolean }>('/notes', { signal: controller.signal }),
    )
    // Let the request reach the point where it is genuinely reading
    // the body (the stream's pull has been requested).
    await waitForReadStart(body)
    // The first pull -- waitForReadStart's signal -- can fire while the
    // request is still settling its fetch, and an abort at that moment
    // is answered by the fetch path that both the fixed and the
    // pre-fix code share. Flush the whole microtask chain so the
    // request genuinely settles and suspends on the stalled body read:
    // an abort then lands mid-read, where the fix's trigger is the
    // only thing that answers it. (The pre-fix code waited on the
    // never-settling stream with no abort wiring and hung.)
    for (let i = 0; i < 2000; i += 1) {
      await Promise.resolve()
    }
    controller.abort()
    await rejection
    expect(standin.calls).toHaveLength(1)
    expect(recorded(standin).signal?.aborted).toBe(true)
  })

  it('surfaces the raw abort, never a transient retry, when a cancelled caller abandons the body read', { timeout: 1000 }, async () => {
    // The same real un-released-stream shape under a retry policy with
    // budget to spare: an abandoned body read is the cancellation, not
    // a retryable network-class failure, so no retry may follow it.
    const body = gatedBody()
    const standin = scriptedStandin(new Response(body.stream, { status: 200 }))
    const api = createClient({
      baseUrl: BASE_URL,
      fetch: standin.fetch,
      retryPolicy: zeroDelay(3),
    })
    const controller = new AbortController()
    const rejection = expectRawAbort(
      api<{ ok: boolean }>('/notes', { signal: controller.signal }),
    )
    // Let the request reach the point where it is genuinely reading
    // the body (the stream's pull has been requested).
    await waitForReadStart(body)
    // The first pull -- waitForReadStart's signal -- can fire while the
    // request is still settling its fetch, and an abort at that moment
    // is answered by the fetch path that both the fixed and the
    // pre-fix code share. Flush the whole microtask chain so the
    // request genuinely settles and suspends on the stalled body read:
    // an abort then lands mid-read, where the fix's trigger is the
    // only thing that answers it. (The pre-fix code waited on the
    // never-settling stream with no abort wiring and hung.)
    for (let i = 0; i < 2000; i += 1) {
      await Promise.resolve()
    }
    controller.abort()
    await rejection
    expect(standin.calls).toHaveLength(1)
    expect(recorded(standin).signal?.aborted).toBe(true)
  })

  it('rejects a request aborted during the backoff promptly, without waiting out the delay', async () => {
    // The backoff sleep races the caller's signal: an abort landing in
    // the middle of a 2s Retry-After backoff rejects the request raw
    // on the next microtask -- it never sits out the remaining delay,
    // and no retry fires for the cancelled caller. (Before the fix the
    // sleep ignored the signal: with the fake clock never advanced the
    // promise was still pending at the assertion point below.)
    vi.useFakeTimers()
    try {
      const standin = scriptedStandin(
        textResponse(503, 'Service Unavailable', { 'retry-after': '2' }),
      )
      const api = createClient({
        baseUrl: BASE_URL,
        fetch: standin.fetch,
        retryPolicy: DEFAULT_RETRY_POLICY,
      })
      const controller = new AbortController()
      let settled = false
      let reason: unknown
      const pending = api<{ ok: boolean }>('/notes', {
        signal: controller.signal,
      }).then(
        () => {
          settled = true
        },
        (caught: unknown) => {
          settled = true
          reason = caught
        },
      )
      // Let the 503 arrive and the 2000ms backoff arm.
      await vi.advanceTimersByTimeAsync(0)
      controller.abort()
      // Flush microtasks only -- no timer advancement, so the 2000ms
      // backoff is still pending. Only a sleep that aborts on the
      // signal can have settled by now.
      for (let i = 0; i < 32; i += 1) {
        await Promise.resolve()
      }
      expect(settled).toBe(true)
      expect(reason).toBeInstanceOf(DOMException)
      if (reason instanceof DOMException) {
        expect(reason.name).toBe('AbortError')
      }
      expect(isApiError(reason)).toBe(false)
      expect(standin.calls).toHaveLength(1)
      await pending
    } finally {
      vi.useRealTimers()
    }
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

  it('parses a real backend-shaped envelope: code and params, no traceId', async () => {
    // The wire shape every module's handler actually writes: a
    // {code, params} body with no traceId (go/authn/middleware.go's
    // errorBody, and every module's own writeError, encode only those
    // two fields -- the OpenAPI fragments document {code, params} as
    // the error contract). traceId is optional, kept only for
    // correlation when a backend sends one; the code of a traceId-less
    // body must not be discarded into client.http.<status>, or every
    // real backend error would degrade to the generic fallback.
    const standin = scriptedStandin(
      jsonResponse(400, {
        code: 'authn.password_too_short',
        params: { min_length: 12 },
      }),
    )
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.status).toBe(400)
    expect(error.code).toBe('authn.password_too_short')
    expect(error.params).toEqual({ min_length: 12 })
    expect(error.traceId).toBeUndefined()
    expect(error.details).toBeUndefined()
  })

  it('keeps a traceId-bearing envelope correlated when params are absent', async () => {
    // The optional-field counterpart: a body that does carry traceId
    // still surfaces it on the ApiError, so user reports can be
    // correlated to server logs even though no backend sends one yet.
    const standin = scriptedStandin(
      jsonResponse(500, {
        code: 'config.internal_error',
        traceId: 'trace-50',
      }),
    )
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.status).toBe(500)
    expect(error.code).toBe('config.internal_error')
    expect(error.traceId).toBe('trace-50')
    expect(error.params).toBeUndefined()
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

  it('names what the 2xx-primitive refusal actually refuses', async () => {
    // The branch refuses JSON that is not an object or an array -- a
    // JSON string or number *is* a JSON value, so the refusal must say
    // what it means and not misname the branch as rejecting non-JSON.
    const standin = scriptedStandin(jsonResponse(200, 'just a string'))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.code).toBe(ERROR_CODE_PROTOCOL)
    expect(error.status).toBe(200)
    expect(error.cause).toBeInstanceOf(SyntaxError)
    expect((error.cause as SyntaxError).message).toBe(
      '2xx body is not a JSON object or array',
    )
  })

  it('refuses an envelope whose code borrows the reserved client.* namespace', async () => {
    // An envelope can only arrive through an HTTP response, and server
    // codes are module-scoped (authn.*, notes.* -- per the error-code
    // contract);
    // `client` is no module's domain. A backend or intermediary that
    // answers with an envelope carrying a client.*-shaped code is
    // borrowing this client's reserved vocabulary -- accepted, it
    // would let a session error read as a transport failure in
    // consumer surfaces. The envelope is refused at parse time and the
    // failure lands on the honest client.http.<status> code with the
    // real status, never on a code that claims a diagnosis the client
    // did not make.
    const standin = scriptedStandin(
      jsonResponse(400, { code: 'client.timeout', traceId: 'forged-1' }),
    )
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.code).toBe('client.http.400')
    expect(error.status).toBe(400)
    expect(error.traceId).toBeUndefined()
    expect(error.message).toBe(
      'The server answered HTTP 400 without a valid ApiError envelope.',
    )
  })

  it('never presents a forged client.* envelope as a transport failure', async () => {
    // The concrete harm behind the reserved-namespace rule: a 401
    // whose envelope says client.network is still an auth answer --
    // isTransportFailure must answer false for it, so a login surface
    // cannot render "the request never completed" for a session error.
    const standin = scriptedStandin(jsonResponse(401, { code: 'client.network' }))
    const api = createClient({ baseUrl: BASE_URL, fetch: standin.fetch })
    const error = await expectApiError(api<{ ok: boolean }>('/notes'))
    expect(error.status).toBe(401)
    expect(error.auth).toBe(true)
    expect(error.code).toBe('client.http.401')
    expect(isTransportFailure(error)).toBe(false)
  })
})

