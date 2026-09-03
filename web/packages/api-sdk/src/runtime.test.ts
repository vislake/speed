import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { RequestFn, RequestOptions } from '@speed/api-client'
import {
  bindRequestFn,
  speedRequest,
  speedRequestCredentialless,
} from './runtime'

/** The host side of the seam: a request function that records every
 * call instead of touching a network. */
interface RecordedCall {
  path: string
  options?: RequestOptions
}
const calls: RecordedCall[] = []
const fakeRequestFn: RequestFn = (async <T>(
  path: string,
  options?: RequestOptions,
): Promise<T> => {
  calls.push({ path, options })
  return undefined as T
}) as RequestFn

beforeEach(() => {
  calls.length = 0
  bindRequestFn(fakeRequestFn)
})

describe('bindRequestFn / speedRequest', () => {
  it('forwards an orval-shaped GET call to the bound request function', async () => {
    const result = await speedRequest<{ notes: unknown[] }>({
      url: '/api/v1/notes',
      method: 'GET',
    })
    expect(result).toBeUndefined()
    expect(calls).toEqual([
      { path: '/api/v1/notes', options: { method: 'GET' } },
    ])
  })

  it('maps the axios-shaped POST call onto body and headers', async () => {
    const body = { text: 'hello' }
    const signal = new AbortController().signal
    await speedRequest<unknown>({
      url: '/api/v1/notes',
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      data: body,
      signal,
    })
    expect(calls).toEqual([
      {
        path: '/api/v1/notes',
        options: {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body,
          signal,
        },
      },
    ])
  })

  it('maps orval query params onto the query option', async () => {
    await speedRequest<unknown>({
      url: '/api/v1/notes',
      method: 'GET',
      params: { page: 1, tag: null },
    })
    expect(calls[0]?.options?.query).toEqual({ page: 1, tag: null })
  })

  it('never sets the omitAccessToken key on an ordinary speedRequest call', async () => {
    // Exact-shape assertion: the flag is only ever present when a
    // credential-less operation declared it, so hosts can rely on its
    // absence meaning "attach the bearer token from the store".
    await speedRequest<unknown>({
      url: '/api/v1/notes',
      method: 'GET',
    })
    expect(calls).toEqual([
      { path: '/api/v1/notes', options: { method: 'GET' } },
    ])
    expect(calls[0]?.options?.omitAccessToken).toBeUndefined()
  })

  it('declares the credential-less flag on speedRequestCredentialless calls', async () => {
    const body = { refresh_token: 'stale' }
    await speedRequestCredentialless<unknown>({
      url: '/api/v1/authn/token/refresh',
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      data: body,
    })
    expect(calls).toEqual([
      {
        path: '/api/v1/authn/token/refresh',
        options: {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body,
          omitAccessToken: true,
        },
      },
    ])
  })

  it('rebinding replaces the previous request function (last bind wins)', async () => {
    // The second function records without a method option, so the
    // recorded shape tells which function served the call.
    const second: RequestFn = (async <T>(path: string): Promise<T> => {
      calls.push({ path })
      return undefined as T
    }) as RequestFn
    bindRequestFn(second)
    await speedRequest<unknown>({ url: '/api/v1/notes', method: 'GET' })
    expect(calls).toEqual([{ path: '/api/v1/notes', options: undefined }])
  })

  it('throws a programmer error while no request function is bound', async () => {
    // A fresh module instance (re-imported after resetModules) starts
    // with the binding unset, so the unbound path is observable
    // without exposing a reset on the package surface. speedRequest
    // throws synchronously -- the unbound state is a programmer error,
    // not a request outcome.
    vi.resetModules()
    const fresh = await import('./runtime')
    expect(() =>
      fresh.speedRequest({ url: '/api/v1/notes', method: 'GET' }),
    ).toThrow(
      '[speed-api-sdk] no request function bound: call bindRequestFn(createClient(...)) once at bootstrap before any generated hook runs.',
    )
    // Both mutators share the unbound guard -- a credential-less
    // operation is no less a programmer error when nothing is bound.
    expect(() =>
      fresh.speedRequestCredentialless({
        url: '/api/v1/authn/token/refresh',
        method: 'POST',
      }),
    ).toThrow(
      '[speed-api-sdk] no request function bound: call bindRequestFn(createClient(...)) once at bootstrap before any generated hook runs.',
    )
  })
})
