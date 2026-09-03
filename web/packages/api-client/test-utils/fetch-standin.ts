/**
 * Deterministic fetch stand-ins for api-client tests.
 *
 * Every test injects one through ClientOptions.fetch -- the package
 * never touches a real network and never relies on an implicit global
 * fetch. A stand-in answers a scripted sequence of steps (Response,
 * Error for a network failure, or `hang` for a response that only ends
 * when the client's timeout or the caller's signal aborts it -- like a
 * real fetch that never answers). It rejects on abort the way real
 * fetch does, records every call, and fails loudly when the client
 * makes an unscripted request, so a retry-count mistake surfaces as a
 * test failure instead of a silent pass.
 */

/** One recorded request, as the client sent it. */
export interface StandinCall {
  /** The exact URL the client sent. */
  readonly url: string
  /** The HTTP method as sent (uppercased by the client). */
  readonly method: string
  /** The headers as sent. */
  readonly headers: Headers
  /** Raw body text, when the client sent a string body. */
  readonly bodyText: string | undefined
  /** The body parsed as JSON, when bodyText carried JSON. */
  readonly bodyJson: unknown
  /** The abort signal the client passed (its internal timeout
   * controller or the caller's own signal). */
  readonly signal: AbortSignal | null
}

/** What one scripted step may be: an answered response, a network
 * failure, or a promise (hang) that only ends by abort. */
export type StandinStep = Response | Error | Promise<Response | Error>

/** The per-call decision function behind a stand-in. */
export type StandinResponder = (call: StandinCall) => StandinStep

export interface StandinFetch {
  /** Inject as ClientOptions.fetch. */
  readonly fetch: typeof fetch
  /** Every call, in order, for assertions. */
  readonly calls: readonly StandinCall[]
}

/** A JSON response with the content type the client expects. An
 * omitted body produces a body-less response (204-style: the Response
 * constructor forbids a body on null-body statuses, and real fetch
 * delivers those as body: null). */
export function jsonResponse(
  status: number,
  body?: unknown,
  extraHeaders?: Readonly<Record<string, string>>,
): Response {
  const text = body === undefined ? null : JSON.stringify(body)
  return new Response(text, {
    status,
    headers: { 'content-type': 'application/json', ...extraHeaders },
  })
}

/** A non-JSON response (an HTML error page, a plain-text body...). */
export function textResponse(
  status: number,
  text: string,
  extraHeaders?: Readonly<Record<string, string>>,
): Response {
  return new Response(text, { status, headers: { ...extraHeaders } })
}

/** A step that never answers on its own: only the client's timeout or
 * a caller abort ends it, exactly like a real fetch to a dead peer. */
export const hang: StandinStep = new Promise<Response>(() => {})

/** Responds with each step in order; a further call is unscripted and
 * throws, surfacing an unexpected request (a wrong retry count, an
 * extra refresh attempt...) as a loud failure. */
export function sequence(...steps: StandinStep[]): StandinResponder {
  let index = 0
  return () => {
    const step = steps[index]
    if (step === undefined) {
      throw new Error(
        'fetch stand-in exhausted: the client made more requests than the test scripted',
      )
    }
    index += 1
    return step
  }
}

function abortError(): DOMException {
  return new DOMException('The operation was aborted.', 'AbortError')
}

/** A promise that rejects with AbortError when the signal aborts. */
function waitForAbort(signal: AbortSignal): Promise<never> {
  return new Promise((_resolve, reject) => {
    if (signal.aborted) {
      reject(abortError())
      return
    }
    signal.addEventListener('abort', () => reject(abortError()), { once: true })
  })
}

/**
 * Builds a stand-in around a responder. Call shape mirrors real fetch:
 * sync responder throws and Error steps reject (network failures), the
 * returned promise rejects on signal abort, everything is recorded in
 * `calls`.
 */
export function createStandinFetch(responder: StandinResponder): StandinFetch {
  const calls: StandinCall[] = []

  const deliver = async (call: StandinCall): Promise<Response> => {
    const step = await responder(call)
    if (step instanceof Response) {
      return step
    }
    if (step instanceof Error) {
      throw step
    }
    throw new TypeError(
      'fetch stand-in responder produced neither Response nor Error',
    )
  }

  const fetchFn: typeof fetch = async (input, init) => {
    const url =
      typeof input === 'string'
        ? input
        : input instanceof URL
          ? input.href
          : input.url
    const headers = new Headers(init?.headers)
    const bodyText =
      typeof init?.body === 'string' ? init.body : undefined
    let bodyJson: unknown = undefined
    if (
      bodyText !== undefined &&
      (headers.get('content-type') ?? '').includes('application/json')
    ) {
      try {
        bodyJson = JSON.parse(bodyText)
      } catch {
        bodyJson = undefined
      }
    }
    const call: StandinCall = {
      url,
      method: init?.method ?? 'GET',
      headers,
      bodyText,
      bodyJson,
      signal: init?.signal ?? null,
    }
    calls.push(call)

    const signal = init?.signal ?? null
    if (signal === null) {
      return deliver(call)
    }
    return Promise.race([deliver(call), waitForAbort(signal)])
  }

  return { fetch: fetchFn, calls }
}

/** Convenience: a stand-in answering a scripted sequence. */
export function scriptedStandin(
  ...steps: StandinStep[]
): StandinFetch {
  return createStandinFetch(sequence(...steps))
}
