/**
 * main.test.tsx -- the two contracts of the app's bootstrap module,
 * each proven at the layer it belongs to:
 *
 * bootstrapReferenceApp -- a jsdom mount of the whole composition: the
 * real bootstrap executed at least once in this repo, its client built
 * over the (stubbed) environment fetch it captures, the anonymous
 * sign-in surface rendering under the provider stack, and a clean
 * unmount. Every other suite composes the tree by hand through the
 * shared test-utils rigs; this is the one that runs the function a
 * real page runs. The browser-level half of that proof -- the real
 * page in a real browser over the real server -- lives in this
 * directory's e2e suite, not here.
 *
 * evictQueriesOnSessionEnd -- the session-end eviction of everything
 * the departing principal left on the page -- the query cache and the
 * notes create form's half-typed draft (views/notes-draft.ts, a
 * leftover of the same class as a cached row) -- in isolation, over a
 * real AuthSession (createAuthSession, driven through the real-client
 * rig's own scripted responder), a real QueryClient and the draft
 * store, without mounting any DOM tree: the wiring is pure
 * session-and-cache plumbing, so nothing here needs React or the
 * app's own views. The cross-account leaks this pins: nothing evicted
 * a tenant's cached queries on sign-out or session death (only a
 * tenant switch did), and an eviction scoped to the departing
 * tenant's ['tenant', tenantId] prefix cannot reach the identity-
 * domain rows the account surface reads through bare spec-path keys
 * (sessions, login history, bound identities) -- a different account
 * signing in afterward would inherit them. The eviction is therefore
 * total: whatever domain a surface reads in, no row outlives the
 * session that fetched it. The notes-view suite covers its own
 * gate-ordering half of the same session hygiene; the full
 * end-to-end regressions (a signed-out account's cached notes and
 * account rows never reaching a different account signing in after
 * it) live in app-journey.test.tsx.
 */

import { QueryClient } from '@tanstack/react-query'
import { act, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import {
  getAuthnListIdentitiesQueryKey,
  getAuthnListLoginHistoryQueryKey,
  getAuthnListSessionsQueryKey,
} from '@speed/api-sdk'
import { PRODUCT_SHELL_NAMESPACE } from '@speed/product-shell'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import { bootstrapReferenceApp } from './main.js'
import { evictQueriesOnSessionEnd } from './main.js'
import {
  readNotesDraft,
  writeNotesDraft,
} from './views/notes-draft.js'
import { jsonResponse, makeRealClientRig } from './test-utils/real-client.js'
import type { RealResponder } from './test-utils/real-client.js'

const TENANT_ID = 'tenant-acme'

/** A minimal responder: password login answers a token pair in
 * TENANT_ID, logout answers 204. Nothing else is reachable from this
 * suite's calls. */
const respond: RealResponder = (call) => {
  if (call.method === 'POST' && call.path === '/api/v1/authn/login/password') {
    return jsonResponse(200, {
      access_token: 'access-1',
      refresh_token: 'refresh-1',
      principal: {
        user_id: 'user-1',
        tenant_id: TENANT_ID,
        session_id: 'session-1',
      },
    })
  }
  if (call.method === 'POST' && call.path === '/api/v1/authn/logout') {
    return new Response(null, { status: 204 })
  }
  throw new Error(`unexpected request: ${call.method} ${call.path}`)
}

/** A note-shaped cache entry under the app's own tenant-namespaced
 * query-key convention (['tenant', tenantId, ...]); the exact suffix
 * does not matter to the eviction, which removes the whole prefix. */
function seedNotesCache(queryClient: QueryClient): void {
  queryClient.setQueryData(
    ['tenant', TENANT_ID, '/api/v1/notes', {}],
    { notes: [{ id: 'note-1', text: 'cached from the departing session' }] },
  )
}

/** The identity-domain cache entries of the account surface, under the
 * exact bare generated keys its reads use (the login-history key
 * carries its {limit} query params as a further element, the shape the
 * real hook's key has). Nothing here is tenant-namespaced -- the shape
 * no tenant-prefixed removal can reach. */
function seedIdentityCache(queryClient: QueryClient): void {
  queryClient.setQueryData(
    [...getAuthnListSessionsQueryKey()],
    { sessions: [{ id: 'session-1' }] },
  )
  queryClient.setQueryData(
    [...getAuthnListLoginHistoryQueryKey(), { limit: 20 }],
    { attempts: [{ method: 'password' }] },
  )
  queryClient.setQueryData(
    [...getAuthnListIdentitiesQueryKey()],
    { identities: [{ id: 'social-github' }] },
  )
}

/** A query row that belongs to no domain the surface evictions ever
 * named -- the "remove everything" proof that no future domain needs a
 * new eviction call site either. */
function seedUnrelatedCache(queryClient: QueryClient): void {
  queryClient.setQueryData(['preferences'], { theme: 'dark' })
}

/** Seeds every domain an authenticated session's reads can populate. */
function seedAllDomains(queryClient: QueryClient): void {
  seedNotesCache(queryClient)
  seedIdentityCache(queryClient)
  seedUnrelatedCache(queryClient)
}

describe('bootstrapReferenceApp', () => {
  // The bootstrap's one runtime exercise in this repo at jsdom level.
  // The whole composition -- i18n registration, the session, the
  // client over the environment's own fetch (createClient captures
  // globalThis.fetch at construction), the seam binding, the provider
  // stack and the view machine -- runs exactly once per call, so a
  // jsdom mount proves the real bootstrap executes at all. The
  // browser-level leg of that proof -- the real page over the real
  // server -- lives in the e2e suite, which is where a real network
  // and a real browser belong.
  let observedCalls: Array<{
    readonly method: string
    readonly path: string
    readonly authorization: string | null
  }>
  let realFetch: typeof globalThis.fetch
  let realLanguages: readonly string[] | undefined
  let realLocalStorage: PropertyDescriptor | undefined

  beforeEach(() => {
    window.location.hash = ''
    observedCalls = []
    realFetch = globalThis.fetch
    realLanguages = window.navigator.languages
    // Deterministic language: pin the navigator-languages leg of
    // createI18n's negotiation so the mounted surface speaks the
    // zh-CN copy the assertions name. jsdom's own languages answer
    // en-US; forcing the list makes the boot language a fact, not an
    // environment accident.
    Object.defineProperty(window.navigator, 'languages', {
      value: ['zh-CN'],
      configurable: true,
    })
    // The bootstrap's createI18n reads the stored-language choice from
    // globalThis.localStorage. Under Node 26 that global is the
    // engine's own experimental webstorage getter (it warns and offers
    // nothing without --localstorage-file -- a Node 26 artifact: the
    // pinned .nvmrc toolchain is Node 24, where the global does not
    // exist and the read yields null the same way); stubbing a
    // memory-backed storage keeps the composition's own storage leg
    // exercised deterministically (an empty store reads back null)
    // without tripping the engine global.
    realLocalStorage = Object.getOwnPropertyDescriptor(
      globalThis,
      'localStorage',
    )
    Object.defineProperty(globalThis, 'localStorage', {
      value: {
        getItem: () => null,
        setItem: () => undefined,
      },
      configurable: true,
      writable: true,
    })
    // The environment fetch the bootstrap's client captures. A fresh
    // stub per test, answering the pre-auth config GET the anonymous
    // surface drives and failing loudly on anything else -- an
    // unexpected request is a composition regression, not something to
    // answer.
    Object.defineProperty(window, 'fetch', {
      value: async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input))
        const authorization = new Headers(init?.headers).get('authorization')
        observedCalls.push({
          method: init?.method ?? 'GET',
          path: url.pathname,
          authorization,
        })
        if (url.pathname === '/api/config/public') {
          return jsonResponse(200, { config: {}, features: [] })
        }
        throw new Error(`bootstrap mount: unexpected request ${url.pathname}`)
      },
      configurable: true,
    })
  })

  afterEach(() => {
    if (realLanguages !== undefined) {
      Object.defineProperty(window.navigator, 'languages', {
        value: realLanguages,
        configurable: true,
      })
    }
    Object.defineProperty(window, 'fetch', {
      value: realFetch,
      configurable: true,
    })
    if (realLocalStorage !== undefined) {
      Object.defineProperty(globalThis, 'localStorage', realLocalStorage)
    }
    document.body.innerHTML = ''
  })

  it('mounts the whole composition into a container and renders the anonymous sign-in surface', async () => {
    const container = document.createElement('div')
    document.body.appendChild(container)
    let boot: ReturnType<typeof bootstrapReferenceApp> | undefined
    await act(async () => {
      boot = bootstrapReferenceApp(container)
    })

    // The composed app booted anonymous: the sign-in surface stands in
    // the frame, speaking the pinned language.
    expect(boot?.i18n.language).toBe('zh-CN')
    // The bootstrap registers every namespace a rendered unit reads,
    // the product-shell namespace among them: product-shell's own
    // resources.ts declares the host obligation, and the shell's
    // session-ended announcement renders only where the registration
    // happened -- the bundle check is the registration itself,
    // independent of any journey reaching the ended view.
    expect(
      boot?.i18n.hasResourceBundle('zh-CN', PRODUCT_SHELL_NAMESPACE),
    ).toBe(true)
    expect(boot?.queryClient).toBeInstanceOf(QueryClient)
    expect(
      await screen.findByRole('button', { name: zhCN.signIn.registerAction }),
    ).toBeInTheDocument()
    // The frame is not up: no signed-in navigation exists.
    expect(screen.queryByRole('link', { name: zhCN.nav.home })).not.toBeInTheDocument()

    // The surface's one read reached the environment fetch the client
    // captured, credential-less, through the real api-client machinery.
    expect(observedCalls).toHaveLength(1)
    expect(observedCalls[0]).toEqual({
      method: 'GET',
      path: '/api/config/public',
      authorization: null,
    })

    // Tearing the page down unmounts the tree.
    await act(async () => {
      boot?.root.unmount()
    })
    container.remove()
    expect(container.innerHTML).toBe('')
  })

  it('a second bootstrap into a fresh container mounts again (the composition is not single-use)', async () => {
    const container = document.createElement('div')
    document.body.appendChild(container)
    let boot: ReturnType<typeof bootstrapReferenceApp> | undefined
    await act(async () => {
      boot = bootstrapReferenceApp(container)
    })
    expect(
      await screen.findByRole('button', { name: zhCN.signIn.registerAction }),
    ).toBeInTheDocument()
    // Each bootstrap builds its own client over the (stubbed) environment
    // fetch, so each mount drives its own config read.
    expect(observedCalls.length).toBeGreaterThanOrEqual(1)
    await act(async () => {
      boot?.root.unmount()
    })
    container.remove()
  })
})

describe('evictQueriesOnSessionEnd', () => {
  it('empties every domain a sign-out leaves behind: the tenant rows, the identity-domain rows and any unrelated key', async () => {
    // The identity-domain rows (bare spec-path keys -- nothing in
    // @speed/api-sdk is tenant-namespaced) have no tenant segment for
    // a tenant-prefixed removal to reach, so without total eviction a
    // different account signing in afterward would inherit them. The
    // eviction is total: every domain is asserted gone, including a
    // key no eviction call site knows about, so a future surface's
    // domain cannot leak either.
    const rig = makeRealClientRig(respond)
    const queryClient = new QueryClient()
    evictQueriesOnSessionEnd(rig.session, queryClient)

    await rig.session.loginWithPassword({
      identifier: 'owner@example.test',
      password: 'correct-horse-battery-staple',
    })
    seedAllDomains(queryClient)
    // The departing principal also left a half-typed note in the
    // notes create form's draft store -- the same class of leftover
    // as a cached row.
    writeNotesDraft('Half-typed by the departing account')
    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeDefined()
    expect(queryClient.getQueryData([...getAuthnListSessionsQueryKey()]))
      .toBeDefined()
    expect(
      queryClient.getQueryData([
        ...getAuthnListLoginHistoryQueryKey(),
        { limit: 20 },
      ]),
    ).toBeDefined()
    expect(queryClient.getQueryData([...getAuthnListIdentitiesQueryKey()]))
      .toBeDefined()
    expect(queryClient.getQueryData(['preferences'])).toBeDefined()

    await rig.session.logout()

    // Every leftover the departing session left behind is gone --
    // every query its reads cached (the notes rows, the account
    // surface's three identity-domain lists, and the key no eviction
    // knows by name) and the half-typed draft, which must not greet
    // the next account signing into this page.
    expect(queryClient.getQueryCache().findAll()).toHaveLength(0)
    expect(readNotesDraft()).toBe('')
    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeUndefined()
    expect(queryClient.getQueryData([...getAuthnListSessionsQueryKey()]))
      .toBeUndefined()
    expect(
      queryClient.getQueryData([
        ...getAuthnListLoginHistoryQueryKey(),
        { limit: 20 },
      ]),
    ).toBeUndefined()
    expect(queryClient.getQueryData([...getAuthnListIdentitiesQueryKey()]))
      .toBeUndefined()
    expect(queryClient.getQueryData(['preferences'])).toBeUndefined()
  })

  it('empties the same domains a session death (a refused silent refresh) leaves behind', async () => {
    // refresh() never rejects for a refused token -- it resolves false
    // and signs the session out locally (session.ts's own contract),
    // the same authenticated -> anonymous transition a manual sign-out
    // produces, so the eviction must fire on this path too.
    const rig = makeRealClientRig((call) => {
      if (call.method === 'POST' && call.path === '/api/v1/authn/token/refresh') {
        return new Response(
          JSON.stringify({ code: 'authn.token_invalid', traceId: 'trace-1' }),
          { status: 401, headers: { 'content-type': 'application/json' } },
        )
      }
      return respond(call)
    })
    const queryClient = new QueryClient()
    evictQueriesOnSessionEnd(rig.session, queryClient)

    await rig.session.loginWithPassword({
      identifier: 'owner@example.test',
      password: 'correct-horse-battery-staple',
    })
    seedAllDomains(queryClient)
    writeNotesDraft('Half-typed when the session died')

    expect(await rig.session.refresh()).toBe(false)

    // The death path clears the draft too: the same authenticated ->
    // anonymous edge, the same rule.
    expect(queryClient.getQueryCache().findAll()).toHaveLength(0)
    expect(readNotesDraft()).toBe('')
    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeUndefined()
    expect(queryClient.getQueryData([...getAuthnListSessionsQueryKey()]))
      .toBeUndefined()
    expect(
      queryClient.getQueryData([
        ...getAuthnListLoginHistoryQueryKey(),
        { limit: 20 },
      ]),
    ).toBeUndefined()
    expect(queryClient.getQueryData([...getAuthnListIdentitiesQueryKey()]))
      .toBeUndefined()
  })

  it('does nothing while the session was never authenticated', async () => {
    const rig = makeRealClientRig(respond)
    const queryClient = new QueryClient()
    evictQueriesOnSessionEnd(rig.session, queryClient)
    seedAllDomains(queryClient)
    writeNotesDraft('A draft from before any session')

    // A snapshot notification with no prior authenticated state (none
    // fires here since nothing logs in) must not evict anything --
    // asserted by the cache and the draft surviving to this point
    // unexamined by any transition at all.
    expect(queryClient.getQueryCache().findAll()).toHaveLength(5)
    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeDefined()
    expect(readNotesDraft()).toBe('A draft from before any session')
  })

  it('leaves the cache alone across a still-authenticated transition (a tenant switch)', async () => {
    // A tenant switch keeps state === 'authenticated' throughout (only
    // the principal's tenant_id changes), so this eviction -- which
    // fires strictly on the authenticated -> anonymous edge -- must
    // not also fire here; user-menu.tsx's own tenant-switch eviction
    // owns that transition, and double-eviction is harmless but would
    // mask this function targeting the wrong edge if it fired.
    const rig = makeRealClientRig((call) => {
      if (call.method === 'POST' && call.path === '/api/v1/authn/tenant/switch') {
        return jsonResponse(200, {
          access_token: 'access-2',
          principal: {
            user_id: 'user-1',
            tenant_id: 'tenant-globex',
            session_id: 'session-1',
          },
        })
      }
      return respond(call)
    })
    const queryClient = new QueryClient()
    evictQueriesOnSessionEnd(rig.session, queryClient)

    await rig.session.loginWithPassword({
      identifier: 'owner@example.test',
      password: 'correct-horse-battery-staple',
    })
    seedAllDomains(queryClient)
    writeNotesDraft('Half-typed across the switch')

    await rig.session.switchTenant('tenant-globex')

    // The same person stays signed in across a switch (notes-draft.ts:
    // a switch keeps the form mounted, so the form's own state carries
    // the text), so the draft survives it like the cache does.
    expect(queryClient.getQueryCache().findAll()).toHaveLength(5)
    expect(readNotesDraft()).toBe('Half-typed across the switch')
    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeDefined()
    expect(queryClient.getQueryData([...getAuthnListSessionsQueryKey()]))
      .toBeDefined()
    expect(
      queryClient.getQueryData([
        ...getAuthnListLoginHistoryQueryKey(),
        { limit: 20 },
      ]),
    ).toBeDefined()
    expect(queryClient.getQueryData([...getAuthnListIdentitiesQueryKey()]))
      .toBeDefined()
  })
})
