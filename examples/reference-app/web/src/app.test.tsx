/**
 * AppView contract: the product-shell composition of this app -- the
 * hash-routed fragment machine over the three-branch view machine.
 *
 * The pure half of this suite pins parseHashFragment's degradation
 * rules: the three routes parse to their kinds, the binding subroute
 * parses to a binding target only for a demo provider carrying both
 * halves of the (code, state) pair (anything else on that prefix is
 * account content, never an exchange driven with garbage), and anything
 * the app does not know degrades to unknown -- home content with no nav
 * item selected. The journeys render AppView over a real client bound
 * into the runtime seam and drive it through what a user actually does:
 * a fresh visitor sees the sign-in surface over one config fetch (the
 * page's whole first paint is one GET /api/config/public), a completed
 * sign-in flips the machine into the frame (header brand, nav carrying
 * host-computed aria-current, home over the served brand), navigation
 * travels home/notes/account through the location hash -- notes
 * answering with its served list (the empty demo list renders the
 * list's empty state), the account fragment rendering the account
 * surface over its served state, and the binding subroute completing
 * its exchange inside that surface: the host's answer to the handler's
 * onBound cue is navigation back to the account fragment, unmounting
 * the completion UI (the journey gates the exchange's answer so it can
 * rest on the pending notice before the cue). Unknown fragments
 * degrade to home with nothing selected, and a sign-out after the
 * frame converges to the session-ended screen and back to the sign-in
 * surface -- the session still anonymous, the config cache still one
 * fetch. A bilingual leg proves the frame and the auth surface speak
 * the active language while the served brand stays verbatim.
 *
 * Built-in strings are asserted through the bundles they render from --
 * the app's own zh-CN/en-US fixtures imported relatively, the auth-ui
 * and account-ui copy through the packages' locale fixtures (relative
 * imports, the product-shell precedent) -- never inline: the CJK scan
 * treats test files as English text like everything else, and inline
 * copy would both violate that rule and drift from the resources.
 */

import { act, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { UserEvent } from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import accountUiZhCN from '../../../../web/packages/account-ui/src/locales/zh-CN.json' with { type: 'json' }
import authUiEnUS from '../../../../web/packages/auth-ui/src/locales/en-US.json' with { type: 'json' }
import authUiZhCN from '../../../../web/packages/auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }
import { AppView, parseHashFragment } from './app.js'
import { demoServer } from './test-utils/demo-server.js'
import { makeRealClientRig, type RealClientRig } from './test-utils/real-client.js'
import { renderWithAppServices } from './test-utils/render.js'

/** The demo server's served brand, scripted as Public config data. */
const BRAND = 'Demo Smile Lab'

/** A rig answering the demo's endpoints with one served brand and no
 * enabled features (feature-matrix coverage lives in home-view's own
 * suite; here the frame's chrome is the subject). */
function makeAppRig(): RealClientRig {
  return makeRealClientRig(
    demoServer({
      publicConfig: { config: { 'brand.site_name': BRAND }, features: [] },
    }),
  )
}

function configGets(rig: RealClientRig): number {
  return rig.calls.filter((call) => call.path === '/api/config/public').length
}

function rendered(rig: RealClientRig) {
  return renderWithAppServices(<AppView />, {
    session: rig.session,
    api: rig.api,
  })
}

/** Drives the sign-in surface of a rendered AppView through a password
 * sign-in, returning once the frame is up (the nav is frame chrome). */
async function signInWithPasswordUi(
  view: ReturnType<typeof rendered>,
  user: UserEvent,
): Promise<void> {
  await user.type(
    view.getByLabelText(authUiZhCN.passwordSignIn.identifierLabel),
    'owner@example.test',
  )
  await user.type(
    view.getByLabelText(authUiZhCN.passwordSignIn.passwordLabel),
    'correct-horse-battery-staple',
  )
  await user.click(
    view.getByRole('button', { name: authUiZhCN.passwordSignIn.submit }),
  )
}

/** Fragment navigation the way a user's click on a nav anchor drives it
 * (the anchors are plain #-links; the app's one routed state is the
 * fragment). Assigning the hash and dispatching the event inside act
 * keeps the resulting re-render inside the act window. */
function navigateTo(hash: string): void {
  act(() => {
    window.location.hash = hash
    window.dispatchEvent(new HashChangeEvent('hashchange'))
  })
}

describe('parseHashFragment', () => {
  it('parses the three routes, with or without a leading slash', () => {
    expect(parseHashFragment('')).toEqual({ kind: 'home' })
    expect(parseHashFragment('/')).toEqual({ kind: 'home' })
    expect(parseHashFragment('/notes')).toEqual({ kind: 'notes' })
    expect(parseHashFragment('/account')).toEqual({ kind: 'account' })
  })

  it('drops the query string when parsing a route', () => {
    expect(parseHashFragment('/notes?x=1')).toEqual({ kind: 'notes' })
  })

  it('parses the binding subroute into a target only for a demo provider carrying the full pair', () => {
    expect(
      parseHashFragment('/auth/binding/github?code=c&state=s'),
    ).toEqual({ kind: 'binding', target: { provider: 'github', code: 'c', state: 's' } })
  })

  it('degrades binding-shaped fragments without a valid target to account content', () => {
    // Unknown provider: never an exchange.
    expect(
      parseHashFragment('/auth/binding/twitter?code=c&state=s'),
    ).toEqual({ kind: 'account' })
    // A provider with only half (or none) of the pair: nothing to complete.
    expect(
      parseHashFragment('/auth/binding/github?code=c'),
    ).toEqual({ kind: 'account' })
    expect(parseHashFragment('/auth/binding/github')).toEqual({
      kind: 'account',
    })
  })

  it('degrades anything else to unknown', () => {
    expect(parseHashFragment('/nope')).toEqual({ kind: 'unknown' })
    expect(parseHashFragment('nope')).toEqual({ kind: 'unknown' })
  })
})

describe('AppView', () => {
  // Each journey starts as a fresh visitor on the bare page. jsdom
  // keeps location.hash between tests, so a journey that navigated
  // would leak its fragment into the next one -- reset per test and
  // every journey is order-independent.
  beforeEach(() => {
    window.location.hash = ''
  })

  it('shows an anonymous visitor the sign-in surface over one config fetch', async () => {
    const rig = makeAppRig()
    const view = rendered(rig)
    // The sign-in surface's own heading is the served brand.
    expect(await view.findByText(BRAND)).toBeInTheDocument()
    expect(view.getByText(zhCN.signIn.registerPrompt)).toBeInTheDocument()
    expect(
      view.getByRole('button', { name: zhCN.signIn.registerAction }),
    ).toBeInTheDocument()
    expect(configGets(rig)).toBe(1)
    expect(rig.calls).toHaveLength(1)
  })

  it('signs in into the frame: header brand, nav with host-computed selection, home content', async () => {
    const rig = makeAppRig()
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)

    // The frame is up: the nav is frame chrome.
    await view.findByRole('link', { name: zhCN.nav.notes })
    // Brand in the AppBar and on the home heading: two rendered slots,
    // one served value, one fetch.
    expect(view.getAllByText(BRAND)).toHaveLength(2)
    expect(view.getByText(zhCN.home.intro)).toBeInTheDocument()

    const homeLink = view.getByRole('link', { name: zhCN.nav.home })
    expect(homeLink).toHaveAttribute('href', '#/')
    expect(homeLink).toHaveAttribute('aria-current', 'page')
    const notesLink = view.getByRole('link', { name: zhCN.nav.notes })
    expect(notesLink).toHaveAttribute('href', '#/notes')
    expect(notesLink).not.toHaveAttribute('aria-current')
    const accountLink = view.getByRole('link', { name: zhCN.nav.account })
    expect(accountLink).toHaveAttribute('href', '#/account')
    expect(accountLink).not.toHaveAttribute('aria-current')

    // One config fetch served the whole first paint and the frame.
    expect(configGets(rig)).toBe(1)
    expect(
      rig.calls.some(
        (call) =>
          call.method === 'POST' && call.path === '/api/v1/authn/login/password',
      ),
    ).toBe(true)
  })

  it('travels home/notes/account through the hash, lands the binding exchange back on the account fragment, and degrades unknown fragments', async () => {
    const server = demoServer({
      publicConfig: { config: { 'brand.site_name': BRAND }, features: [] },
    })
    // The binding callback's answer is held open so the journey can rest
    // on the completion handler's pending notice before the exchange
    // lands and the host's own navigation answers the onBound cue.
    let releaseBinding: (() => void) | undefined
    const rig = makeRealClientRig((call) => {
      if (
        call.method === 'POST' &&
        call.path === '/api/v1/authn/social/github/callback'
      ) {
        return new Promise<Response>((resolve) => {
          releaseBinding = () => {
            resolve(server(call))
          }
        })
      }
      return server(call)
    })
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.notes })

    const notesLink = () => view.getByRole('link', { name: zhCN.nav.notes })
    const accountLink = () => view.getByRole('link', { name: zhCN.nav.account })
    const homeLink = () => view.getByRole('link', { name: zhCN.nav.home })

    navigateTo('#/notes')
    // The notes surface's read answered the demo's empty list, so the
    // list's empty state stands in for the notes page.
    expect(
      await view.findByText(zhCN.notes.list.emptyTitle),
    ).toBeInTheDocument()
    expect(notesLink()).toHaveAttribute('aria-current', 'page')
    expect(homeLink()).not.toHaveAttribute('aria-current')

    navigateTo('#/account')
    // The account fragment renders the account surface: the host's own
    // heading and intro above the account-ui sections. The heading is
    // role-scoped -- its text is also the account nav item's label.
    expect(
      await view.findByRole('heading', { name: zhCN.account.heading }),
    ).toBeInTheDocument()
    expect(view.getByText(zhCN.account.intro)).toBeInTheDocument()
    expect(accountLink()).toHaveAttribute('aria-current', 'page')
    expect(notesLink()).not.toHaveAttribute('aria-current')

    // The binding subroute renders the same surface with the completion
    // handler exchanging the fragment's (code, state) pair over the
    // app's client; the pending notice rests while the answer is held,
    // and the account nav stays selected.
    navigateTo('#/auth/binding/github?code=c&state=s')
    expect(
      await view.findByText(accountUiZhCN.bindingCallback.pending),
    ).toBeInTheDocument()
    expect(accountLink()).toHaveAttribute('aria-current', 'page')

    // The exchange lands a binding-shaped answer. The host answers the
    // handler's onBound cue by navigating back to the account fragment
    // -- the hash moves, the machine re-renders the plain account
    // surface, and the completion UI unmounts with it.
    act(() => {
      releaseBinding?.()
    })
    await waitFor(() => expect(window.location.hash).toBe('#/account'))
    await waitFor(() =>
      expect(
        view.queryByText(accountUiZhCN.bindingCallback.pending),
      ).not.toBeInTheDocument(),
    )
    expect(accountLink()).toHaveAttribute('aria-current', 'page')

    // Unknown fragments: home content, nothing selected.
    navigateTo('#/definitely-not-a-route')
    await view.findByText(zhCN.home.intro)
    expect(homeLink()).not.toHaveAttribute('aria-current')
    expect(notesLink()).not.toHaveAttribute('aria-current')
    expect(accountLink()).not.toHaveAttribute('aria-current')
  })

  it('signs out of the frame into the session-ended screen, then back to sign-in', async () => {
    const rig = makeAppRig()
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.notes })

    await user.click(
      view.getByRole('button', { name: authUiZhCN.signOut.label }),
    )
    expect(
      await view.findByText(authUiZhCN.sessionEnded.title),
    ).toBeInTheDocument()
    await user.click(
      view.getByRole('button', { name: authUiZhCN.sessionEnded.signInAction }),
    )
    expect(await view.findByText(zhCN.signIn.registerPrompt)).toBeInTheDocument()

    // The whole journey stayed on one config fetch: the cache survived
    // the frame mount, the logout and the sign-in surface remount.
    expect(configGets(rig)).toBe(1)
    expect(
      rig.calls.some(
        (call) => call.method === 'POST' && call.path === '/api/v1/authn/logout',
      ),
    ).toBe(true)
  })

  it('speaks the active language in the frame while the served brand stays verbatim', async () => {
    const rig = makeAppRig()
    const view = rendered(rig)
    const user = userEvent.setup()
    await signInWithPasswordUi(view, user)
    await view.findByRole('link', { name: zhCN.nav.notes })

    await act(async () => {
      await switchLanguage(view.i18n, 'en-US')
    })

    expect(view.getByRole('link', { name: enUS.nav.home })).toHaveAttribute(
      'aria-current',
      'page',
    )
    expect(view.getByRole('link', { name: enUS.nav.notes })).toBeInTheDocument()
    expect(view.getByRole('link', { name: enUS.nav.account })).toBeInTheDocument()
    expect(view.getByText(enUS.home.intro)).toBeInTheDocument()
    expect(view.getAllByText(BRAND)).toHaveLength(2)
    expect(view.getByText(authUiEnUS.signOut.label)).toBeInTheDocument()
    // The demo roster's display names are app copy and switch language.
    expect(
      view.getByRole('button', { name: enUS.tenants.acme }),
    ).toBeInTheDocument()
  })
})
