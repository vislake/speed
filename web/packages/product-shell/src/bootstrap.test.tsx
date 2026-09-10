/**
 * bootstrap.test.tsx -- the contracts of the app-entry assembly, each
 * proven at the layer it belongs to:
 *
 * bootstrapSpeedApp -- jsdom mounts of the whole composition: the
 * definition's namespaces register alongside the family's automatic
 * four, the provider chain wraps the declared view in declared order
 * with the assembled services, the client the assembly built answers
 * over the environment's own (stubbed) fetch, the session-end strategy
 * is wired (default and override), and unmount tears the page down.
 *
 * watchSessionEnd -- the session-ended default's transition matrix over
 * a real AuthSession (createAuthSession driven through the real-client
 * rig's scripted responder), a real QueryClient and no DOM tree: the
 * wiring is pure session-and-cache plumbing. The matrix pins the four
 * edges that matter: sign-out evicts, a silently refused refresh
 * (session death -- refresh() resolves false, the same authenticated ->
 * anonymous flip) evicts, a session that was never authenticated
 * evicts nothing, and a tenant switch (still authenticated throughout)
 * evicts nothing -- mid-session switch eviction is the host's own
 * concern.
 */

import { QueryClient } from '@tanstack/react-query'
import { act } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import type { ReactElement } from 'react'
import { AUTH_UI_NAMESPACE } from '@speed/auth-ui'
import { useTranslation } from '@speed/i18n'
import { LAYOUT_KIT_NAMESPACE } from '@speed/layout-kit'
import { UI_KIT_NAMESPACE } from '@speed/ui-kit'
import { PRODUCT_SHELL_NAMESPACE } from './resources.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import { bootstrapSpeedApp, watchSessionEnd } from './bootstrap.js'
import type { SpeedAppBootstrap, SpeedAppProvider } from './bootstrap.js'
import type { SpeedAppDefinition } from './bootstrap.js'
import {
  errorResponse,
  jsonResponse,
  makePair,
  makeRealClientRig,
} from '../test-utils/real-client.js'
import type { RealResponder } from '../test-utils/real-client.js'

/** The tenant the scripted happy-path answers name. */
const TENANT_ID = 'tenant-1'

/** A scripted answer table covering the flows both describes drive:
 * password login and tenant switch issue token pairs, logout answers
 * 204, refresh issues a fresh pair. */
const respond: RealResponder = (call) => {
  if (call.method === 'POST' && call.path === '/api/v1/authn/login/password') {
    return jsonResponse(200, makePair())
  }
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
  if (call.method === 'POST' && call.path === '/api/v1/authn/token/refresh') {
    return jsonResponse(200, makePair())
  }
  if (call.method === 'POST' && call.path === '/api/v1/authn/logout') {
    return new Response(null, { status: 204 })
  }
  throw new Error(`unexpected request: ${call.method} ${call.path}`)
}

/** A query row under the app-side tenant-namespaced key convention
 * (['tenant', tenantId, ...]); the suffix is arbitrary -- the eviction
 * removes everything. */
function seedTenantCache(queryClient: QueryClient): void {
  queryClient.setQueryData(
    ['tenant', TENANT_ID, '/api/v1/notes', {}],
    { notes: [{ id: 'note-1', text: 'cached from the departing session' }] },
  )
}

/** The identity-domain shape: bare spec-path keys nothing
 * tenant-namespaced, which only a total eviction reaches. */
function seedIdentityCache(queryClient: QueryClient): void {
  queryClient.setQueryData(['/api/v1/authn/sessions'], {
    sessions: [{ id: 'session-1' }],
  })
}

/** A row no eviction call site names -- the "remove everything" proof
 * that no future domain needs a new eviction site either. */
function seedUnrelatedCache(queryClient: QueryClient): void {
  queryClient.setQueryData(['preferences'], { theme: 'dark' })
}

/** Seeds every domain an authenticated session's reads populate. */
function seedAllDomains(queryClient: QueryClient): void {
  seedTenantCache(queryClient)
  seedIdentityCache(queryClient)
  seedUnrelatedCache(queryClient)
}

/** Mounts a definition into a fresh container and returns the definite
 * handles (the mount runs inside act; the guard keeps the callers'
 * types definite). */
async function mountApp(
  container: Element,
  definition: SpeedAppDefinition,
): Promise<SpeedAppBootstrap> {
  let boot: SpeedAppBootstrap | undefined
  await act(async () => {
    boot = bootstrapSpeedApp(container, definition)
  })
  if (boot === undefined) {
    throw new Error('the bootstrap must return its handles')
  }
  return boot
}

describe('bootstrapSpeedApp', () => {
  let observed: Array<{
    readonly method: string
    readonly path: string
    readonly origin: string
  }>
  let realFetch: typeof globalThis.fetch
  let realLanguages: readonly string[] | undefined
  let realLocalStorage: PropertyDescriptor | undefined

  beforeEach(() => {
    observed = []
    realFetch = globalThis.fetch
    realLanguages = window.navigator.languages
    // Deterministic language: pin the navigator-languages leg of
    // createI18n's negotiation so the mounted view speaks the zh-CN
    // copy the assertions name (jsdom's own languages answer en-US).
    Object.defineProperty(window.navigator, 'languages', {
      value: ['zh-CN'],
      configurable: true,
    })
    // createI18n reads the stored-language choice through a guarded
    // globalThis.localStorage access; under Node 26 that global is the
    // engine's experimental webstorage getter (it warns and offers
    // nothing without --localstorage-file). A memory-backed stub keeps
    // that leg exercised deterministically (an empty store reads back
    // null) without tripping the engine global.
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
    // The environment fetch the assembly's client captures (no fetch
    // option: createClient captures globalThis.fetch at construction).
    // A fresh stub per test answers the scripted flows and fails loudly
    // on anything else -- an unexpected request is a composition
    // regression, not something to answer.
    Object.defineProperty(globalThis, 'fetch', {
      value: async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input))
        observed.push({
          method: init?.method ?? 'GET',
          path: url.pathname,
          origin: url.origin,
        })
        return respond({
          method: init?.method ?? 'GET',
          path: url.pathname,
          authorization: new Headers(init?.headers).get('authorization'),
          body: typeof init?.body === 'string' ? init.body : null,
        })
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
    Object.defineProperty(globalThis, 'fetch', {
      value: realFetch,
      configurable: true,
    })
    if (realLocalStorage !== undefined) {
      Object.defineProperty(globalThis, 'localStorage', realLocalStorage)
    }
    document.body.innerHTML = ''
  })

  it('mounts the declared view under the standard stack, with the family and declared namespaces registered', async () => {
    const container = document.createElement('div')
    document.body.appendChild(container)
    function Probe(): ReactElement {
      const { t } = useTranslation(PRODUCT_SHELL_NAMESPACE)
      return <span data-testid="probe">{t('announcements.sessionEnded')}</span>
    }
    const boot = await mountApp(container, { view: <Probe /> })

    // The assembly's own four namespaces registered automatically --
    // the namespaces the packages it composes render under -- and the
    // view reads them.
    expect(boot.i18n.language).toBe('zh-CN')
    expect(boot.i18n.hasResourceBundle('zh-CN', UI_KIT_NAMESPACE)).toBe(true)
    expect(boot.i18n.hasResourceBundle('zh-CN', LAYOUT_KIT_NAMESPACE)).toBe(true)
    expect(boot.i18n.hasResourceBundle('zh-CN', AUTH_UI_NAMESPACE)).toBe(true)
    expect(
      boot.i18n.hasResourceBundle('zh-CN', PRODUCT_SHELL_NAMESPACE),
    ).toBe(true)
    expect(boot.queryClient).toBeInstanceOf(QueryClient)
    expect(container.textContent).toContain(zhCN.announcements.sessionEnded)

    await act(async () => {
      boot.root.unmount()
    })
    container.remove()
    expect(container.innerHTML).toBe('')
  })

  it('registers the declared namespaces on top of the family four, and threads the assembled services through the providers in declared order', async () => {
    const container = document.createElement('div')
    document.body.appendChild(container)
    const probeNamespace = 'probe-app'
    const probeResources = {
      'zh-CN': { greeting: 'probe' },
      'en-US': { greeting: 'probe' },
    }
    const seen: Array<{ readonly api: unknown }> = []
    const outer: SpeedAppProvider = (services, children) => {
      seen.push({ api: services.api })
      return <div data-testid="outer">{children}</div>
    }
    const inner: SpeedAppProvider = (_services, children) => (
      <div data-testid="inner">{children}</div>
    )
    const boot = await mountApp(container, {
      namespaces: [{ namespace: probeNamespace, resources: probeResources }],
      providers: [outer, inner],
      view: <span data-testid="view">app</span>,
    })

    expect(boot.i18n.hasResourceBundle('zh-CN', probeNamespace)).toBe(true)
    // The provider fold, outermost first: outer wraps inner wraps the
    // view -- the declared order is the DOM nesting order.
    const outerNode = container.querySelector('[data-testid="outer"]')
    const innerNode = container.querySelector('[data-testid="inner"]')
    const viewNode = container.querySelector('[data-testid="view"]')
    expect(outerNode).not.toBeNull()
    expect(innerNode).not.toBeNull()
    expect(viewNode).not.toBeNull()
    expect(outerNode?.contains(innerNode)).toBe(true)
    expect(innerNode?.contains(viewNode)).toBe(true)
    // The providers receive the assembled services, and the api is the
    // client bound into the runtime seam: the session operations
    // travel over the environment fetch stub.
    expect(seen).toHaveLength(1)
    expect(typeof seen[0]?.api).toBe('function')
    await act(async () => {
      await boot.session.loginWithPassword({
        identifier: 'owner@example.test',
        password: 'correct-horse-battery-staple',
      })
    })
    expect(observed.some((call) => call.path === '/api/v1/authn/login/password'))
      .toBe(true)

    await act(async () => {
      boot.root.unmount()
    })
    container.remove()
  })

  it("honours a declared baseUrl for the assembled client", async () => {
    const container = document.createElement('div')
    document.body.appendChild(container)
    const boot = await mountApp(container, {
      baseUrl: 'https://api.example.test',
      view: <span>app</span>,
    })
    await act(async () => {
      await boot.session.loginWithPassword({
        identifier: 'owner@example.test',
        password: 'correct-horse-battery-staple',
      })
    })
    expect(observed[0]?.origin).toBe('https://api.example.test')

    await act(async () => {
      boot.root.unmount()
    })
    container.remove()
  })

  it('a declared sessionEnded replaces the shipped default, and evictAllQueries still empties the cache from the override', async () => {
    const container = document.createElement('div')
    document.body.appendChild(container)
    const contexts: Array<{
      readonly queryClient: QueryClient
      readonly evictAllQueries: () => void
    }> = []
    const boot = await mountApp(container, {
      view: <span>app</span>,
      sessionEnded: (context) => {
        contexts.push(context)
      },
    })
    await act(async () => {
      await boot.session.loginWithPassword({
        identifier: 'owner@example.test',
        password: 'correct-horse-battery-staple',
      })
    })
    seedAllDomains(boot.queryClient)
    await act(async () => {
      await boot.session.logout()
    })

    // The override ran -- and only the override: the declared handler
    // replaces the shipped default action rather than chaining onto it,
    // so the cache the override did not evict is still there, and the
    // context hands the default action back for an override that wants
    // it (asserted next).
    expect(contexts).toHaveLength(1)
    expect(contexts[0]?.queryClient).toBe(boot.queryClient)
    expect(boot.queryClient.getQueryCache().findAll().length).toBeGreaterThan(0)

    contexts[0]?.evictAllQueries()
    expect(boot.queryClient.getQueryCache().findAll()).toHaveLength(0)

    await act(async () => {
      boot.root.unmount()
    })
    container.remove()
  })

  it('with no override, the shipped default evicts the cache on the session-end transition', async () => {
    const container = document.createElement('div')
    document.body.appendChild(container)
    const boot = await mountApp(container, { view: <span>app</span> })
    await act(async () => {
      await boot.session.loginWithPassword({
        identifier: 'owner@example.test',
        password: 'correct-horse-battery-staple',
      })
    })
    seedAllDomains(boot.queryClient)
    expect(boot.queryClient.getQueryCache().findAll()).toHaveLength(3)
    await act(async () => {
      await boot.session.logout()
    })
    expect(boot.queryClient.getQueryCache().findAll()).toHaveLength(0)

    await act(async () => {
      boot.root.unmount()
    })
    container.remove()
  })
})

describe('watchSessionEnd', () => {
  it('empties every domain a sign-out leaves behind: the tenant rows, the identity-domain rows and any unrelated key', async () => {
    // The identity-domain rows (bare spec-path keys -- nothing in
    // @speed/api-sdk is tenant-namespaced) have no tenant segment for a
    // tenant-prefixed removal to reach, so without total eviction a
    // different account signing in afterward would inherit them. The
    // eviction is total: every domain is asserted gone, including a key
    // no eviction call site knows about -- a domain no eviction path
    // names cannot leak either.
    const rig = makeRealClientRig(respond)
    const queryClient = new QueryClient()
    watchSessionEnd(rig.session, queryClient)

    await rig.session.loginWithPassword({
      identifier: 'owner@example.test',
      password: 'correct-horse-battery-staple',
    })
    seedAllDomains(queryClient)
    expect(queryClient.getQueryCache().findAll()).toHaveLength(3)

    await rig.session.logout()

    expect(queryClient.getQueryCache().findAll()).toHaveLength(0)
    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeUndefined()
    expect(queryClient.getQueryData(['/api/v1/authn/sessions'])).toBeUndefined()
    expect(queryClient.getQueryData(['preferences'])).toBeUndefined()
  })

  it('empties the same domains a session death (a refused silent refresh) leaves behind', async () => {
    // refresh() never rejects for a refused token -- it resolves false
    // and signs the session out locally (auth-core's own contract), the
    // same authenticated -> anonymous transition a manual sign-out
    // produces, so the default must fire on this path too.
    const rig = makeRealClientRig((call) => {
      if (call.method === 'POST' && call.path === '/api/v1/authn/token/refresh') {
        return errorResponse(401, 'authn.token_invalid')
      }
      return respond(call)
    })
    const queryClient = new QueryClient()
    watchSessionEnd(rig.session, queryClient)

    await rig.session.loginWithPassword({
      identifier: 'owner@example.test',
      password: 'correct-horse-battery-staple',
    })
    seedAllDomains(queryClient)

    expect(await rig.session.refresh()).toBe(false)

    expect(queryClient.getQueryCache().findAll()).toHaveLength(0)
    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeUndefined()
    expect(queryClient.getQueryData(['/api/v1/authn/sessions'])).toBeUndefined()
  })

  it('does nothing while the session was never authenticated', async () => {
    const rig = makeRealClientRig(respond)
    const queryClient = new QueryClient()
    watchSessionEnd(rig.session, queryClient)
    seedAllDomains(queryClient)

    // No authenticated -> anonymous edge can occur without a login, so
    // every seeded row survives.
    expect(queryClient.getQueryCache().findAll()).toHaveLength(3)
    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeDefined()
  })

  it('leaves the cache alone across a still-authenticated transition (a tenant switch)', async () => {
    // A tenant switch keeps state authenticated throughout (only the
    // principal's tenant_id changes), so the strategy -- which fires
    // strictly on the authenticated -> anonymous edge -- must not also
    // fire here; the host's own switch-time eviction owns that
    // transition.
    const rig = makeRealClientRig(respond)
    const queryClient = new QueryClient()
    watchSessionEnd(rig.session, queryClient)

    await rig.session.loginWithPassword({
      identifier: 'owner@example.test',
      password: 'correct-horse-battery-staple',
    })
    seedAllDomains(queryClient)

    await rig.session.switchTenant('tenant-globex')

    expect(queryClient.getQueryCache().findAll()).toHaveLength(3)
    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeDefined()
    expect(queryClient.getQueryData(['/api/v1/authn/sessions'])).toBeDefined()
    expect(queryClient.getQueryData(['preferences'])).toBeDefined()
  })
})
