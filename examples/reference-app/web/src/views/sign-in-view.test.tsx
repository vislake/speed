/**
 * SignInView contract: the anonymous branch's host composition around
 * auth-ui's sign-in family -- the served brand heading over the
 * SignInScreen, with the register surface toggled from the footer and
 * a completed registration landing in the success state with a way
 * back to the sign-in surface.
 *
 * The surface's own register flow is driven for real (a register call
 * lands on the wire, its answer walks the form into the host's success
 * state), and the register-never-signs-in boundary of the composition
 * is pinned at the app layer: a completed registration issues no login
 * request and the view state turns over without the auth snapshot
 * moving. Strings are asserted through the bundles they render from --
 * the app namespace's own fixture for host copy, auth-ui's fixture
 * (relative import, the product-shell precedent) for the surface copy.
 *
 * The sign-in surface offers the password channel alone. This
 * deployment has no way to deliver an SMS code -- the server's SMS
 * seam resolves to the console sender, so a code the surface said went
 * to a phone reaches no phone -- and the page is what a prospective
 * customer sees in a demo, not a developer's own machine (the outcome
 * e2e/offered-channels-work.spec.ts holds this deployment to). One
 * consequence: the channel tab strip is gone, because one channel has
 * nothing to switch between, and the assertions below read the channel
 * through the form itself rather than a tab's title -- the tab title
 * renders nowhere, while the identifier field is the channel, visible
 * without a click.
 */

import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import authUiZhCN from '../../../../../web/packages/auth-ui/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { demoServer } from '../test-utils/demo-server.js'
import { makeRealClientRig } from '../test-utils/real-client.js'
import type { RealCall } from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { SignInView } from './sign-in-view.js'

/** The demo server's served brand, scripted as Public config data. */
const BRAND = 'Demo Smile Lab'

function rigWithBrand(): ReturnType<typeof makeRealClientRig> {
  return makeRealClientRig(
    demoServer({
      publicConfig: { config: { 'brand.site_name': BRAND }, features: [] },
    }),
  )
}

function rendered(rig: ReturnType<typeof makeRealClientRig>) {
  return renderWithAppServices(<SignInView />, {
    session: rig.session,
    api: rig.api,
  })
}

function configGets(
  rig: ReturnType<typeof makeRealClientRig>,
): number {
  return rig.calls.filter((call) => call.path === '/api/config/public').length
}

function registerCalls(
  rig: ReturnType<typeof makeRealClientRig>,
): readonly RealCall[] {
  return rig.calls.filter(
    (call) => call.method === 'POST' && call.path === '/api/v1/authn/register',
  )
}

describe('SignInView', () => {
  it('offers the register turn off the sign-in surface', async () => {
    const rig = rigWithBrand()
    const view = rendered(rig)
    const user = userEvent.setup()
    expect(await view.findByText(BRAND)).toBeInTheDocument()

    await view.findByText(zhCN.signIn.registerPrompt)
    await user.click(
      view.getByRole('button', { name: zhCN.signIn.registerAction }),
    )

    // The register surface replaces the sign-in form: the sign-in
    // surface's own action -- its submit button -- is gone with it.
    // (The former proxy for the sign-in form, the password tab's title
    // text, renders nowhere now that this deployment's one channel
    // carries no tab strip, and the two forms' identifier fields share
    // one label, so the submit action is the property to ask about.)
    expect(await view.findByText(zhCN.register.heading)).toBeInTheDocument()
    expect(
      view.queryByRole('button', { name: authUiZhCN.passwordSignIn.submit }),
    ).not.toBeInTheDocument()
    expect(
      view.getByRole('button', { name: authUiZhCN.register.submit }),
    ).toBeInTheDocument()
    expect(
      view.getByRole('button', { name: zhCN.register.backToSignIn }),
    ).toBeInTheDocument()

    // Toggling the view is local: nothing hit the network but the
    // brand's one config fetch.
    expect(configGets(rig)).toBe(1)
    expect(rig.calls).toHaveLength(1)
  })

  it('offers no SMS channel: nothing in this deployment can deliver a code to a phone', async () => {
    // Regression for e2e/offered-channels-work.spec.ts's state 1: a
    // channel is not offered when nothing can deliver its code. In this
    // deployment the SMS seam resolves to the console sender, so a code
    // the surface claims went to a phone reaches no phone -- and this
    // page is what a prospect sees in a demo. If the surface offered the
    // channel here (the SMS tab and its phone field rendering), this
    // test would fail.
    const rig = rigWithBrand()
    const view = rendered(rig)

    await view.findByText(BRAND)
    expect(
      view.queryByRole('tab', { name: authUiZhCN.smsSignIn.title }),
    ).not.toBeInTheDocument()
    expect(
      view.queryByLabelText(authUiZhCN.smsSignIn.phoneLabel),
    ).not.toBeInTheDocument()

    // The channels that CAN finish a sign-in are unaffected: the
    // password form is the surface, visible without a click.
    expect(
      view.getByLabelText(authUiZhCN.passwordSignIn.identifierLabel),
    ).toBeInTheDocument()
    expect(
      view.getByLabelText(authUiZhCN.passwordSignIn.passwordLabel),
    ).toBeInTheDocument()
  })

  it('registers a new account into the success state, issuing no sign-in', async () => {
    const rig = rigWithBrand()
    const view = rendered(rig)
    const user = userEvent.setup()

    await view.findByText(zhCN.signIn.registerPrompt)
    await user.click(
      view.getByRole('button', { name: zhCN.signIn.registerAction }),
    )
    await view.findByText(zhCN.register.heading)

    await user.type(
      view.getByLabelText(authUiZhCN.register.identifierLabel),
      'new@example.test',
    )
    await user.type(
      view.getByLabelText(authUiZhCN.register.passwordLabel),
      'correct-horse-battery-staple',
    )
    await user.type(
      view.getByLabelText(authUiZhCN.register.displayNameLabel),
      'New User',
    )
    await user.click(
      view.getByRole('button', { name: authUiZhCN.register.submit }),
    )

    // The register answer walks the view into the success state.
    expect(await view.findByText(zhCN.register.success)).toBeInTheDocument()
    expect(
      view.getByRole('button', { name: zhCN.register.backToSignIn }),
    ).toBeInTheDocument()

    // The register body carried the split identifier shape and the
    // optional display name; no login request was ever issued -- the
    // register surface is a destination, never a session operation.
    expect(registerCalls(rig)).toHaveLength(1)
    const registerBody = JSON.parse(registerCalls(rig)[0]?.body ?? '{}') as Record<
      string,
      unknown
    >
    expect(registerBody.email).toBe('new@example.test')
    expect(registerBody.display_name).toBe('New User')
    expect(typeof registerBody.password).toBe('string')
    expect(
      rig.calls.some(
        (call) =>
          call.method === 'POST' && call.path === '/api/v1/authn/login/password',
      ),
    ).toBe(false)
    expect(configGets(rig)).toBe(1)

    // Back to the sign-in surface: the footer prompt is host copy of
    // the sign-in mode again, and the password channel is up -- its
    // form visible directly. (The former proxy for that, the channel
    // tab's title text, renders nowhere now that one channel carries
    // no tab strip; the identifier field is the channel.)
    await user.click(
      view.getByRole('button', { name: zhCN.register.backToSignIn }),
    )
    expect(await view.findByText(zhCN.signIn.registerPrompt)).toBeInTheDocument()
    expect(
      await view.findByLabelText(authUiZhCN.passwordSignIn.identifierLabel),
    ).toBeInTheDocument()
  })

  it('speaks the active language on both turns of the surface', async () => {
    const rig = rigWithBrand()
    const view = renderWithAppServices(
      <SignInView />,
      { session: rig.session, api: rig.api },
      { language: 'en-US' },
    )
    const user = userEvent.setup()

    expect(await view.findByText(enUS.signIn.registerPrompt)).toBeInTheDocument()
    // The served brand stays verbatim under either language.
    expect(view.getAllByText(BRAND)).toHaveLength(1)
    await user.click(
      view.getByRole('button', { name: enUS.signIn.registerAction }),
    )
    expect(await view.findByText(enUS.register.heading)).toBeInTheDocument()
    expect(
      view.getByRole('button', { name: enUS.register.backToSignIn }),
    ).toBeInTheDocument()
  })
})
