/**
 * ProductShell view-machine suite.
 *
 * ProductShell is one decision: which of its three branches renders for
 * the current snapshot (authenticated -> frame, anonymous after the
 * frame -> session-ended view, anonymous before it -> sign-in view).
 * These tests drive real sessions over the real-client rig (see
 * test-utils/real-client.ts) attached to the auth-core hooks exactly the
 * way a host attaches one, and assert what renders and what the machine
 * remembers -- never internal state of the component under test. Every
 * built-in string asserted here comes from the shipped sibling bundles
 * (auth-ui's for the default ended screen, layout-kit's for the frame),
 * imported relatively -- never inline translations. Slot and content
 * strings are English fixtures on purpose: they stand in for a host's
 * own content and are data in a test file, not rendered product text.
 *
 * The three branches are mutually exclusive by construction (the same
 * order auth-ui's SessionGate pattern established): the ended view is
 * checked before the sign-in view, so a signed-out user never falls back
 * to a fresh-visitor sign-in.
 */

import { act, fireEvent } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { attachSession } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'
import authUiZhCN from '../../../auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import layoutKitZhCN from '../../../layout-kit/src/locales/zh-CN.json' with { type: 'json' }
import { renderWithProviders } from '../../test-utils/render.js'
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import {
  jsonResponse,
  makePair,
  makeRealClientRig,
  type RealClientRig,
} from '../../test-utils/real-client.js'
import { ProductShell } from './ProductShell.js'

const LOGIN_PASSWORD = 'POST /api/v1/authn/login/password'
const LOGOUT = 'POST /api/v1/authn/logout'

/** The navItems fixture: host data in host order, with the selected
 * item pre-computed (AppShell never path-matches). */
const NAV_ITEMS = [
  { id: 'home', label: 'Home', href: '/home', selected: true },
] as const

/** A rig whose script covers the two happy paths the machine journeys
 * drive -- a token-issuing login and a 204 logout -- and fails loudly
 * on anything else, so an unpinned request fails the test. */
function makeJourneyRig(): RealClientRig {
  return makeRealClientRig((call) => {
    const key = `${call.method} ${call.path}`
    if (key === LOGIN_PASSWORD) {
      return jsonResponse(200, makePair())
    }
    if (key === LOGOUT) {
      return new Response(null, { status: 204 })
    }
    throw new Error(`unexpected request: ${key}`)
  })
}

/** Drive a password login to completion on a rig session. The session
 * operations are called from test code, never from a component, so the
 * state flips they notify are wrapped in act explicitly. */
async function signInTo(session: AuthSession): Promise<void> {
  await act(async () => {
    await session.loginWithPassword({
      identifier: 'alice@example.com',
      password: 'password-1',
    })
  })
}

async function signOutOf(session: AuthSession): Promise<void> {
  await act(async () => {
    await session.logout()
  })
}

describe('ProductShell view machine', () => {
  it('renders the host sign-in view while anonymous, and never the frame', async () => {
    const rig = makeJourneyRig()
    attachSession(rig.session)
    const utils = renderWithProviders(
      <ProductShell navItems={NAV_ITEMS} header="My App" signIn={<p>sign-in view</p>}>
        <p>app content</p>
      </ProductShell>,
    )
    expect(utils.getByText('sign-in view')).toBeInTheDocument()
    expect(utils.queryByText('app content')).not.toBeInTheDocument()
    expect(utils.queryByRole('banner')).not.toBeInTheDocument()
    expect(utils.queryByRole('main')).not.toBeInTheDocument()
    expect(
      utils.queryByRole('navigation', { name: layoutKitZhCN.appShell.navLabel }),
    ).not.toBeInTheDocument()
    // The default ended screen is unreachable: the frame was never
    // reached, so the anonymous branch is the sign-in one.
    expect(utils.queryByText(authUiZhCN.sessionEnded.title)).not.toBeInTheDocument()
  })

  it('mounts the AppShell frame when the attached session turns authenticated', async () => {
    const rig = makeJourneyRig()
    attachSession(rig.session)
    const utils = renderWithProviders(
      <ProductShell navItems={NAV_ITEMS} header="My App" signIn={<p>sign-in view</p>}>
        <p>app content</p>
      </ProductShell>,
    )
    expect(utils.getByText('sign-in view')).toBeInTheDocument()
    await signInTo(rig.session)
    // The frame: chrome landmarks, the host nav item, header content
    // and the app children inside the main landmark -- and the sign-in
    // view unmounted with the anonymous branch.
    expect(utils.getByRole('banner')).toBeInTheDocument()
    expect(utils.getByRole('main')).toBeInTheDocument()
    expect(
      utils.getByRole('navigation', { name: layoutKitZhCN.appShell.navLabel }),
    ).toBeInTheDocument()
    expect(utils.getByRole('link', { name: 'Home' })).toHaveAttribute('href', '/home')
    expect(utils.getByRole('link', { name: 'Home' })).toHaveAttribute(
      'aria-current',
      'page',
    )
    expect(utils.getByText('My App')).toBeInTheDocument()
    expect(utils.getByText('app content')).toBeInTheDocument()
    expect(utils.queryByText('sign-in view')).not.toBeInTheDocument()
  })

  it('renders nothing for a fresh visitor when no sign-in view is given', async () => {
    const rig = makeJourneyRig()
    attachSession(rig.session)
    const utils = renderWithProviders(
      <ProductShell navItems={NAV_ITEMS} header="My App">
        <p>app content</p>
      </ProductShell>,
    )
    // Anonymous and never-in-the-app with no signIn slot: the branch
    // renders nothing -- a blank page the host pairs its own sign-in
    // surface with, deliberately (see the package README).
    expect(utils.container).toBeEmptyDOMElement()
    expect(utils.queryByRole('banner')).not.toBeInTheDocument()
  })

  it('shows the session-ended screen after a logout, whose action returns to the sign-in view', async () => {
    const rig = makeJourneyRig()
    attachSession(rig.session)
    const utils = renderWithProviders(
      <ProductShell navItems={NAV_ITEMS} header="My App" signIn={<p>sign-in view</p>}>
        <p>app content</p>
      </ProductShell>,
    )
    await signInTo(rig.session)
    expect(utils.getByText('app content')).toBeInTheDocument()
    await signOutOf(rig.session)
    // The frame is gone and the ended screen stands in the sign-in
    // view's place: the machine remembers the app was reached.
    expect(utils.getByText(authUiZhCN.sessionEnded.title)).toBeInTheDocument()
    expect(utils.getByText(authUiZhCN.sessionEnded.description)).toBeInTheDocument()
    expect(utils.queryByRole('banner')).not.toBeInTheDocument()
    expect(utils.queryByText('sign-in view')).not.toBeInTheDocument()
    fireEvent.click(
      utils.getByRole('button', { name: authUiZhCN.sessionEnded.signInAction }),
    )
    expect(utils.getByText('sign-in view')).toBeInTheDocument()
    expect(utils.queryByText(authUiZhCN.sessionEnded.title)).not.toBeInTheDocument()
  })

  it('renders a host sessionEnded override as-is, and still returns to the frame on the next sign-in', async () => {
    const rig = makeJourneyRig()
    attachSession(rig.session)
    const utils = renderWithProviders(
      <ProductShell
        navItems={NAV_ITEMS}
        header="My App"
        signIn={<p>sign-in view</p>}
        sessionEnded={<p>custom ended view</p>}
      >
        <p>app content</p>
      </ProductShell>,
    )
    await signInTo(rig.session)
    await signOutOf(rig.session)
    // The override renders where the default ended screen would have,
    // and the default is nowhere to be seen.
    expect(utils.getByText('custom ended view')).toBeInTheDocument()
    expect(utils.queryByText(authUiZhCN.sessionEnded.title)).not.toBeInTheDocument()
    expect(utils.queryByText('sign-in view')).not.toBeInTheDocument()
    // The override owns its own way back: signing in again flips the
    // snapshot and the frame returns, machine state untouched.
    await signInTo(rig.session)
    expect(utils.getByText('app content')).toBeInTheDocument()
    expect(utils.queryByText('custom ended view')).not.toBeInTheDocument()
  })

  it('fails closed to the sign-in view for an anonymous session and follows an authenticated attach', async () => {
    // An anonymous binding -- the same snapshot the hooks report before
    // any attach, by auth-core's own fail-closed contract (its suite
    // pins that half; attachSession is module-level and last-bind-wins,
    // so this test starts from a fresh anonymous session rather than
    // whatever an earlier test left attached). Whichever way the
    // session is anonymous, the machine can never reach the frame or
    // the ended screen, whatever the host slots say.
    const anonymousRig = makeJourneyRig()
    act(() => {
      attachSession(anonymousRig.session)
    })
    const utils = renderWithProviders(
      <ProductShell navItems={NAV_ITEMS} header="My App" signIn={<p>sign-in view</p>}>
        <p>app content</p>
      </ProductShell>,
    )
    expect(utils.getByText('sign-in view')).toBeInTheDocument()
    expect(utils.queryByRole('banner')).not.toBeInTheDocument()
    expect(utils.queryByText(authUiZhCN.sessionEnded.title)).not.toBeInTheDocument()
    // A session that authenticates while unattached, then attaches:
    // the machine follows the bind immediately (last bind wins), and
    // the ended screen is reachable afterwards like any other session.
    const rig = makeJourneyRig()
    await signInTo(rig.session)
    act(() => {
      attachSession(rig.session)
    })
    expect(utils.getByText('app content')).toBeInTheDocument()
    expect(utils.queryByText('sign-in view')).not.toBeInTheDocument()
    await signOutOf(rig.session)
    expect(utils.getByText(authUiZhCN.sessionEnded.title)).toBeInTheDocument()
  })

  it('has no axe violations on the authenticated frame', async () => {
    const rig = makeJourneyRig()
    attachSession(rig.session)
    const utils = renderWithProviders(
      <ProductShell
        navItems={NAV_ITEMS}
        header="My App"
        signIn={<p>sign-in view</p>}
        userMenu={<p>account menu</p>}
        headerActions={<p>actions</p>}
      >
        <p>app content</p>
      </ProductShell>,
    )
    await signInTo(rig.session)
    expect(utils.getByRole('main')).toBeInTheDocument()
    // Page-chrome scan: this helper keeps the axe `region` rule enabled
    // (all content inside a landmark) because the authenticated branch
    // is a full app page, unlike the widget-shaped pre-auth branches.
    await expectNoAxeViolations()
  })
})
