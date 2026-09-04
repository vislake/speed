/**
 * app-harness.tsx -- the shared AppView journey harness of the
 * composition-level suites: the served brand and the rig over the demo
 * server, the config-fetch counter, the AppView render over app
 * services, the password sign-in drive and the hash navigation. The
 * suites that render the composed AppView (app.test.tsx's AppView
 * journeys and the app-journey suite) share these helpers by importing
 * them from test-utils, the standing shared-helper discipline -- never
 * duplicated per suite -- so the whole app's journey surface drives
 * the one rig and the one sign-in leg.
 *
 * The helper file carries no JSX of its own beyond the AppView render,
 * but it composes that render, so it is a .tsx like the render harness.
 *
 * signInWithPasswordUi drives the sign-in surface of a rendered AppView
 * through a password sign-in as the given identifier -- the owner (the
 * demo-server's default principal) unless a suite names another, the
 * reader being the member day's shape -- returning once the frame is
 * up (the nav is frame chrome). navigateTo moves the location hash the
 * way a user's click on a nav anchor moves it (the anchors are plain
 * #-links; the app's one routed state is the fragment): assigning the
 * hash and dispatching the event inside act keeps the resulting
 * re-render inside the act window.
 *
 * Built-in strings are asserted through the bundles they render from,
 * so the sign-in drive reads the auth-ui fixture rather than an inline
 * label; fixtures are relative imports here exactly as they are in the
 * suites that use this harness.
 */

import { act } from '@testing-library/react'
import type { UserEvent } from '@testing-library/user-event'
import authUiZhCN from '../../../../../web/packages/auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import { AppView } from '../app.js'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { demoServer } from './demo-server.js'
import type { DemoServerOptions } from './demo-server.js'
import { DEMO_OWNER_IDENTIFIER } from './demo-server.js'
import { makeRealClientRig } from './real-client.js'
import type { RealClientRig } from './real-client.js'
import { renderWithAppServices } from './render.js'
import type { RenderWithAppServicesResult } from './render.js'

/** The demo server's served brand, scripted as Public config data. */
export const BRAND = 'Demo Smile Lab'

/** The scripted password of the demo accounts -- the demo server
 * validates no password, but the journeys sign in with one, the shape
 * a real sign-in has, and register with the same one. */
export const APP_PASSWORD = 'correct-horse-battery-staple'

/** A rig answering the demo's endpoints with one served brand and no
 * enabled features (feature-matrix coverage lives in home-view's own
 * suite; here the frame's chrome is the subject). Options override the
 * demo facts a journey needs, the reader and the deny switches among
 * them. */
export function makeAppRig(options: DemoServerOptions = {}): RealClientRig {
  return makeRealClientRig(
    demoServer({
      publicConfig: { config: { 'brand.site_name': BRAND }, features: [] },
      ...options,
    }),
  )
}

/** The number of Public-config fetches a journey made -- one fetch
 * serves the page's whole first paint; the cache serves every later
 * reader, so the count staying at one is a journey property the
 * suites pin. */
export function configGets(rig: RealClientRig): number {
  return rig.calls.filter((call) => call.path === '/api/config/public').length
}

/** A rendered AppView over app services -- the value the journey
 * suites drive. */
export type AppViewJourneyView = RenderWithAppServicesResult

/** Renders the AppView under app services over the rig, attaching the
 * rig's session the way the app's own bootstrap does. */
export function rendered(rig: RealClientRig): AppViewJourneyView {
  return renderWithAppServices(<AppView />, {
    session: rig.session,
    api: rig.api,
  })
}

/** Drives the sign-in surface of a rendered AppView through a password
 * sign-in as the given identifier (the owner by default), returning
 * once the frame is up (the nav is frame chrome). */
export async function signInWithPasswordUi(
  view: AppViewJourneyView,
  user: UserEvent,
  identifier: string = DEMO_OWNER_IDENTIFIER,
): Promise<void> {
  await user.type(
    view.getByLabelText(authUiZhCN.passwordSignIn.identifierLabel),
    identifier,
  )
  await user.type(
    view.getByLabelText(authUiZhCN.passwordSignIn.passwordLabel),
    APP_PASSWORD,
  )
  await user.click(
    view.getByRole('button', { name: authUiZhCN.passwordSignIn.submit }),
  )
  await view.findByRole('link', { name: zhCN.nav.home })
}

/** Fragment navigation the way a user's click on a nav anchor drives
 * it. */
export function navigateTo(hash: string): void {
  act(() => {
    window.location.hash = hash
    window.dispatchEvent(new HashChangeEvent('hashchange'))
  })
}
