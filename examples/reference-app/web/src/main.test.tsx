/**
 * main.test.tsx -- the contract of the app's bootstrap module: a jsdom
 * mount of the whole composition, the real bootstrap (main.tsx ->
 * @speed/product-shell/bootstrap's bootstrapSpeedApp) executed at least
 * once in this repo over the app's own definition, plus the one
 * app-specific session-end duty the definition carries.
 *
 * Every other suite composes the tree by hand through the shared
 * test-utils rigs; these run the function a real page runs. The
 * browser-level half of that proof -- the real page in a real browser
 * over the real server -- lives in this directory's e2e suite, not
 * here. The session-end default's own transition matrix (the query
 * cache eviction on sign-out and session death, and its no-op edges)
 * lives in the package that ships it
 * (web/packages/product-shell/src/bootstrap.test.tsx); what is pinned
 * here is the app's half of the wiring: the override composing that
 * default with the notes create form's half-typed draft
 * (views/notes-draft.ts -- text typed by the departing account must
 * not greet the next account signing into this page), fired through
 * the real mounted composition on the same authenticated -> anonymous
 * transition a sign-out or a silently refused refresh produces.
 */

import { QueryClient } from '@tanstack/react-query'
import { act, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { PRODUCT_SHELL_NAMESPACE } from '@speed/product-shell'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import { bootstrapReferenceApp } from './main.js'
import type { ReferenceAppBootstrap } from './main.js'
import { readNotesDraft, writeNotesDraft } from './views/notes-draft.js'
import { jsonResponse } from './test-utils/real-client.js'

const TENANT_ID = 'tenant-acme'

describe('bootstrapReferenceApp', () => {
  // The bootstrap's runtime exercises in this repo at jsdom level. The
  // whole composition -- i18n registration, the session, the client
  // over the environment's own fetch (createClient captures
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
    // surface drives, the password login and the logout the session-end
    // test drives, and failing loudly on anything else -- an unexpected
    // request is a composition regression, not something to answer.
    Object.defineProperty(window, 'fetch', {
      value: async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input))
        const method = init?.method ?? 'GET'
        const authorization = new Headers(init?.headers).get('authorization')
        observedCalls.push({ method, path: url.pathname, authorization })
        if (url.pathname === '/api/v1/config/public') {
          return jsonResponse(200, { config: {}, features: [] })
        }
        if (
          method === 'POST' &&
          url.pathname === '/api/v1/authn/login/password'
        ) {
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
        if (method === 'POST' && url.pathname === '/api/v1/authn/logout') {
          return new Response(null, { status: 204 })
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
    let boot: ReferenceAppBootstrap | undefined
    await act(async () => {
      boot = bootstrapReferenceApp(container)
    })

    // The composed app booted anonymous: the sign-in surface stands in
    // the frame, speaking the pinned language.
    expect(boot?.i18n.language).toBe('zh-CN')
    // The assembly registers every namespace a rendered unit reads --
    // its own family four automatically, the app's declared three on
    // top -- so the shell's session-ended announcement (the namespace
    // product-shell's own resources.ts declares as a host obligation)
    // is registered independent of any journey reaching the ended
    // view.
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
      path: '/api/v1/config/public',
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
    let boot: ReferenceAppBootstrap | undefined
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

  it("the app's session-end override composes the default eviction with the notes draft's clearing", async () => {
    const container = document.createElement('div')
    document.body.appendChild(container)
    let boot: ReferenceAppBootstrap | undefined
    await act(async () => {
      boot = bootstrapReferenceApp(container)
    })
    if (boot === undefined) {
      throw new Error('the bootstrap must return its handles')
    }

    await act(async () => {
      await boot?.session.loginWithPassword({
        identifier: 'owner@example.test',
        password: 'correct-horse-battery-staple',
      })
    })
    // The departing principal left a cached row and a half-typed note
    // in the notes create form's draft store -- the same class of
    // leftover, owned by the app's half of the strategy.
    boot.queryClient.setQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}], {
      notes: [{ id: 'note-1', text: 'cached from the departing session' }],
    })
    writeNotesDraft('Half-typed by the departing account')

    await act(async () => {
      await boot?.session.logout()
    })

    // Both halves fired on the one transition: the default action's
    // eviction (the whole cache is gone) and the draft's clearing,
    // which must not greet the next account signing into this page.
    expect(boot.queryClient.getQueryCache().findAll()).toHaveLength(0)
    expect(readNotesDraft()).toBe('')

    await act(async () => {
      boot?.root.unmount()
    })
    container.remove()
  })
})
