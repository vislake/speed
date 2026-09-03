/**
 * React hooks over go/config's public endpoints -- the `./react`
 * subpath export, isolated from the package's main entry so the
 * dependency-free core (createClient, ApiError, retry, reporter,
 * token store, and the config-fetcher.ts functions this file wraps)
 * never grows a React dependency for consumers who don't render.
 *
 * Both hooks share one cache per `RequestFn` identity: the first
 * mounted consumer of a given `api` starts exactly one
 * fetchPublicConfig call, and every other usePublicConfig/useFeature
 * instance backed by the same `api` reads and re-renders off that same
 * in-flight/settled state, via useSyncExternalStore. A host that wants
 * the two hooks to compose therefore constructs its RequestFn once
 * (typically in a module-scope singleton or a value memoized for the
 * app's lifetime) and passes the same reference everywhere -- passing
 * a fresh RequestFn on every render defeats the sharing and starts a
 * fresh fetch per instance, exactly as it would for any reference-keyed
 * cache.
 *
 * Design notes, recorded here so a future reader doesn't go looking
 * for logic that deliberately doesn't exist:
 *
 * - No fallback-to-platform-defaults detection. go/config's handlers
 *   already resolve an unmatched host to platform defaults as a normal
 *   200 (never an error) -- by the time a response reaches this file
 *   there is nothing left to distinguish, so neither hook special-cases
 *   it.
 * - No tenant-switch revalidation. Both endpoints resolve tenant from
 *   the request's *host*, never from the access token, so a token
 *   refresh or a tenant switch (which only changes token claims) has
 *   no bearing on this cache. This is a deliberate non-feature.
 * - No auto-polling or window-focus revalidation in this round -- the
 *   design commits only to "fetched at startup"; `refresh()` is the
 *   sole revalidation lever. A future shell round that wants "admin
 *   edited config, show a live update" can add a revalidation policy
 *   without changing either hook's public shape.
 * - No unmount cancellation. The cache is keyed by `api` identity, not
 *   by component lifetime -- an in-flight fetch outliving the component
 *   that triggered it is the point: a second mount of the same `api`
 *   should observe (or await) the one shared request, not start a new
 *   one.
 */

import { useCallback, useSyncExternalStore } from 'react'
import { fetchPublicConfig } from './config-fetcher.js'
import type { PublicConfigResponse } from './config-fetcher.js'
import type { RequestFn } from './client.js'
import { isApiError, type ApiError } from './errors.js'

/** The state one config store publishes to its subscribers. */
interface ConfigSnapshot {
  readonly data: PublicConfigResponse | undefined
  readonly error: ApiError | undefined
  readonly isLoading: boolean
}

/** Return shape of {@link usePublicConfig}. */
export interface UsePublicConfigResult extends ConfigSnapshot {
  /**
   * Forces a refetch and republishes the result to every
   * usePublicConfig/useFeature instance sharing this hook's `api`.
   * The previous `data` (if any) is kept while the refetch is in
   * flight -- stale-while-revalidate, not a reset to `undefined`.
   */
  readonly refresh: () => void
}

/** One shared cache entry: current snapshot, subscribers, and the
 * fetch lifecycle that keeps them in sync. */
interface ConfigStore {
  getSnapshot: () => ConfigSnapshot
  subscribe: (listener: () => void) => () => void
  refresh: () => void
}

const LOADING_SNAPSHOT: ConfigSnapshot = {
  data: undefined,
  error: undefined,
  isLoading: true,
}

/**
 * Per-`api`-identity stores. A WeakMap so a host that ever replaces its
 * RequestFn (tests, most often) does not leak a store forever; the
 * common case is one long-lived `api` for the app's whole lifetime.
 */
const stores = new WeakMap<RequestFn, ConfigStore>()

function createConfigStore(api: RequestFn): ConfigStore {
  let snapshot: ConfigSnapshot = LOADING_SNAPSHOT
  const listeners = new Set<() => void>()
  // Incremented on every load(); a settling promise checks it against
  // the current value before publishing, so an overlapping refresh()
  // (fired while an earlier fetch is still in flight) always wins --
  // the stale response's resolution is a no-op instead of clobbering
  // newer state.
  let fetchToken = 0
  // Fetching starts lazily, on the first subscribe (useSyncExternalStore
  // calls subscribe from a passive effect, never during render) --
  // not at store construction, so getStore/createConfigStore stay free
  // of side effects a render could trigger more than once.
  let started = false

  function publish(next: ConfigSnapshot): void {
    snapshot = next
    for (const listener of listeners) {
      listener()
    }
  }

  function load(): void {
    const token = (fetchToken += 1)
    publish({ data: snapshot.data, error: undefined, isLoading: true })
    fetchPublicConfig(api).then(
      (data) => {
        if (token !== fetchToken) {
          return
        }
        publish({ data, error: undefined, isLoading: false })
      },
      (error: unknown) => {
        if (token !== fetchToken) {
          return
        }
        // The RequestFn contract (client.ts) rejects only ApiError, or
        // the caller's raw AbortError when a signal was passed -- this
        // file never passes one (see "No unmount cancellation" above),
        // so isApiError is expected to always narrow here. The guard
        // is defensive rather than load-bearing: keeps `error` typed
        // as ApiError | undefined even if a non-conforming RequestFn
        // is ever injected, rather than crashing the subscriber loop.
        publish({
          data: snapshot.data,
          error: isApiError(error) ? error : undefined,
          isLoading: false,
        })
      },
    )
  }

  return {
    getSnapshot: () => snapshot,
    subscribe(listener) {
      listeners.add(listener)
      if (!started) {
        started = true
        load()
      }
      return () => {
        listeners.delete(listener)
      }
    },
    refresh: load,
  }
}

function getStore(api: RequestFn): ConfigStore {
  const existing = stores.get(api)
  if (existing !== undefined) {
    return existing
  }
  const created = createConfigStore(api)
  stores.set(api, created)
  return created
}

/**
 * Fetches the effective Public config values and enabled feature flags
 * once per `api` identity, sharing the result (and the in-flight
 * request) with every other usePublicConfig/useFeature instance backed
 * by the same `api`. `isLoading` is true until the first response (or
 * error) for this `api` settles; `error` surfaces the rejected
 * `ApiError` verbatim, never re-wrapped.
 */
export function usePublicConfig(api: RequestFn): UsePublicConfigResult {
  const store = getStore(api)
  const snapshot = useSyncExternalStore(store.subscribe, store.getSnapshot)
  const refresh = useCallback(() => {
    store.refresh()
  }, [store])
  return {
    data: snapshot.data,
    error: snapshot.error,
    isLoading: snapshot.isLoading,
    refresh,
  }
}

/**
 * Whether feature `key` is enabled for the tenant `api` resolves to.
 * Composes on {@link usePublicConfig}'s shared cache rather than
 * calling `/api/system/features` separately, so using both hooks
 * together costs one fetch, not two -- a caller that genuinely wants
 * only the lighter endpoint can call `fetchSystemFeatures` directly.
 *
 * Returns `false` while loading and on error -- never throws -- so a
 * consumer such as a NavItem's `requiredFeature` stays hidden until
 * the flag is known to be on, rather than flashing or crashing.
 */
export function useFeature(api: RequestFn, key: string): boolean {
  const { data } = usePublicConfig(api)
  return data?.features.includes(key) ?? false
}
