// @vitest-environment jsdom
/**
 * The README's "Config hooks" quick start, compiled and executed by
 * the suite -- the `./react` counterpart to usage-example.test.ts.
 *
 * The README documents one composition: construct `api` once, then
 * build a custom `useAppChrome` hook on top of usePublicConfig and
 * useFeature. This file carries that composition verbatim (the
 * snippet's `from '@speed/api-client'` / `from '@speed/api-client/react'`
 * imports resolve to the package's own entries, './client' and
 * './react', in-suite) and drives both documented paths -- the success
 * transition and the error transition -- through renderHook, the same
 * way react.test.ts exercises the hooks directly. The documented usage
 * therefore cannot drift from the API: the package suite compiles and
 * runs it.
 */

import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { renderHook, waitFor } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { createStandinFetch, jsonResponse } from '../test-utils/fetch-standin'
import { createClient } from './client'
import type { RequestFn } from './client'
import type { ApiError } from './errors'
import { useFeature, usePublicConfig } from './react'

/** Marks the start of the copied composition, in both this file and
 * README.md -- the anchor the verbatim-drift guard below searches for. */
const SNIPPET_START_MARKER = "/** What a host's app shell needs to render its chrome: the effective"

/* ------------------------------------------------------------------ */
/* The README "Config hooks" quick start, verbatim.                    */
/* ------------------------------------------------------------------ */

/** What a host's app shell needs to render its chrome: the effective
 * brand name and whether billing is enabled for the tenant the
 * request host resolves to. */
interface AppChrome {
  readonly brandName: string | undefined
  readonly billingEnabled: boolean
  readonly isLoading: boolean
  readonly error: ApiError | undefined
}

/**
 * Reads the tenant's public config and the `billing` feature flag
 * through one shared cache. `api` must be the same RequestFn reference
 * every caller uses -- that is what lets usePublicConfig and
 * useFeature below compose into a single request instead of two.
 */
function useAppChrome(clientApi: RequestFn): AppChrome {
  const { data, isLoading, error } = usePublicConfig(clientApi)
  const billingEnabled = useFeature(clientApi, 'billing')

  return {
    // Both fields default to "not yet known" rather than a guessed
    // value: brandName stays undefined and billingEnabled stays false
    // until the shared fetch settles.
    brandName:
      typeof data?.config.brand_name === 'string' ? data.config.brand_name : undefined,
    billingEnabled,
    isLoading,
    error,
  }
}

/* ------------------------------------------------------------------ */
/* The suite driving the snippet.                                      */
/* ------------------------------------------------------------------ */

/** A fresh pre-auth-style client over a fresh stand-in -- no
 * accessTokenStore, matching how the two config endpoints are meant to
 * be called (see config-fetcher.ts and the README's main Quick start). */
function standinClient(responder: Parameters<typeof createStandinFetch>[0]) {
  const standin = createStandinFetch(responder)
  const api: RequestFn = createClient({ baseUrl: '/api/v1', fetch: standin.fetch })
  return { standin, api }
}

describe('README "Config hooks" quick start', () => {
  it('resolves the brand name and the billing flag from one shared fetch', async () => {
    const { standin, api } = standinClient(() =>
      jsonResponse(200, {
        config: { brand_name: 'Acme Dental' },
        features: ['billing', 'ai_gateway'],
      }),
    )

    const { result } = renderHook(() => useAppChrome(api))

    expect(result.current.isLoading).toBe(true)
    expect(result.current.brandName).toBeUndefined()
    expect(result.current.billingEnabled).toBe(false)

    await waitFor(() => expect(result.current.isLoading).toBe(false))

    expect(result.current.brandName).toBe('Acme Dental')
    expect(result.current.billingEnabled).toBe(true)
    expect(result.current.error).toBeUndefined()
    // useAppChrome calls usePublicConfig once directly and once more
    // through useFeature -- the shared cache keyed by `api` identity
    // means that is still exactly one request, not two.
    expect(standin.calls).toHaveLength(1)
  })

  it('reports an absent feature as false once resolved', async () => {
    const { api } = standinClient(() =>
      jsonResponse(200, { config: { brand_name: 'Acme Dental' }, features: [] }),
    )

    const { result } = renderHook(() => useAppChrome(api))
    await waitFor(() => expect(result.current.isLoading).toBe(false))

    expect(result.current.billingEnabled).toBe(false)
  })

  it('surfaces a failed fetch as the ApiError, with billing defaulting to false', async () => {
    const { api } = standinClient(() =>
      jsonResponse(500, { code: 'config.internal_error', traceId: 'trace-chrome' }),
    )

    const { result } = renderHook(() => useAppChrome(api))
    await waitFor(() => expect(result.current.isLoading).toBe(false))

    expect(result.current.brandName).toBeUndefined()
    expect(result.current.billingEnabled).toBe(false)
    expect(result.current.error?.code).toBe('config.internal_error')
    expect(result.current.error?.traceId).toBe('trace-chrome')
  })
})

describe('README "Config hooks" quick start snippet stays verbatim', () => {
  it('copies the AppChrome/useAppChrome composition from README.md byte-for-byte', () => {
    // This file's header claims the composition above is carried
    // "verbatim" from README.md so documented usage "cannot drift from
    // the API". That claim is only as good as this check: it re-reads
    // both files at test time and diffs the shared region -- the
    // AppChrome interface through the closing brace of useAppChrome --
    // so a hand-edit to either copy that silently diverges from the
    // other (a renamed parameter, a reworded comment) fails here
    // instead of only being caught by a human diffing the two by eye.
    // `new URL(relative, import.meta.url)` is avoided here: this test
    // runs under the jsdom environment, whose global URL constructor
    // is not Node's, and resolving a relative path against a file:
    // base through it does not reliably yield a file: URL. Deriving
    // the path with node:path instead sidesteps that.
    const selfPath = fileURLToPath(import.meta.url)
    const readmePath = join(dirname(selfPath), '..', 'README.md')
    const readmeText = readFileSync(readmePath, 'utf8')
    const selfText = readFileSync(selfPath, 'utf8')

    const readmeStart = readmeText.indexOf(SNIPPET_START_MARKER)
    const readmeEnd = readmeText.indexOf('\n```', readmeStart)
    if (readmeStart === -1 || readmeEnd === -1) {
      throw new Error(
        'README.md "Config hooks" snippet markers not found -- did the section move or get reworded?',
      )
    }

    // lastIndexOf, not indexOf: SNIPPET_START_MARKER's own declaration
    // above contains this same literal text once already, so indexOf
    // would match that constant instead of the real comment.
    const selfStart = selfText.lastIndexOf(SNIPPET_START_MARKER)
    const selfEnd = selfText.indexOf(
      '/* ------------------------------------------------------------------ */\n' +
        '/* The suite driving the snippet.',
      selfStart,
    )
    if (selfStart === -1 || selfEnd === -1) {
      throw new Error('local useAppChrome composition markers not found -- did this file move?')
    }

    const readmeSnippet = readmeText.slice(readmeStart, readmeEnd).trim()
    const selfSnippet = selfText.slice(selfStart, selfEnd).trim()

    expect(selfSnippet).toBe(readmeSnippet)
  })
})
