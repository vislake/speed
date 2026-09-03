/**
 * AccountView contract: the account surface composes the account-ui
 * family over the app's session and the generated authn API -- every
 * list read and mutation travels through the generated hooks over the
 * host's QueryClient, driven by the same real client bound into the
 * runtime seam the bootstrap binds. The journeys sign in through the
 * real session operation (the surfaces need a signed-in principal, and
 * the access token in the shared store is what the requests carry) and
 * pin the observed requests: bearer header, page-size query, and the
 * invalidating refetches after a revoke or an exchange.
 *
 * The demo server's served state is scripted once in demo-server.ts and
 * travels verbatim: the three session rows' device strings, the two
 * login-history rows' method and reason tokens, and the identities a
 * suite seeds. The default account starts with nothing bound, so the
 * add area offers every demo channel; state is per-responder, so a
 * revoke or an exchange is visible to the next list answer the way a
 * real server's would be.
 *
 * Built-in strings are asserted through the bundles they render from --
 * the app's own zh-CN/en-US fixtures plus the account-ui and ui-kit
 * package fixtures (relative imports, the notes-view precedent) --
 * never inline: the CJK scan treats test files as English text like
 * everything else. Served account text (device strings) is server
 * data, not copy, and travels in the journey verbatim.
 *
 * Six journeys: the zh default render over the served lists (one read
 * per list, zero MFA calls -- the MFA surface is idle-only by
 * contract), the en-US leg of that render, a single session revoked
 * in place, the double-confirmed revoke-others flow with its counted
 * notice, an unbind the server refuses with authn.last_login_method
 * (the row survives, no refetch), and the binding subroute: an
 * exchange held open by a gated answer, its pending notice resting
 * until the release, then the identities refetch landing the bound
 * row and the host's onBound cue firing exactly once. The authorize
 * clicks are never exercised -- the add-area buttons navigate the
 * window (the view's one navigation duty), and jsdom answers
 * navigation with a Not-implemented error; the binding side of the
 * journey is the callback route, which needs no navigation.
 */

import { act, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AuthnIdentity } from '@speed/api-sdk'
import { describe, expect, it, vi } from 'vitest'
import accountUiEnUS from '../../../../../web/packages/account-ui/src/locales/en-US.json' with { type: 'json' }
import accountUiZhCN from '../../../../../web/packages/account-ui/src/locales/zh-CN.json' with { type: 'json' }
import uiKitZhCN from '../../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { demoServer } from '../test-utils/demo-server.js'
import type { RealCall, RealClientRig } from '../test-utils/real-client.js'
import { makeRealClientRig, signInWithPassword } from '../test-utils/real-client.js'
import type { RenderWithProvidersOptions } from '../test-utils/render.js'
import { renderWithAppServices } from '../test-utils/render.js'
import type { AccountBindingTarget } from './account-view.js'
import { AccountView } from './account-view.js'

/** The device strings the demo server's default session rows serve:
 * verbatim travel into the assertions, like served note text. */
const CURRENT_SESSION_DEVICE = 'Demo laptop'
const OTHER_SESSION_DEVICE_ONE = 'Windows desktop'
const OTHER_SESSION_DEVICE_TWO = 'iPad Safari'

/** The other session row's served id -- the row the single-revoke
 * journey deletes by. */
const OTHER_SESSION_ID_ONE = 'session-2'

/** The sign-in and demo-server identity address, served data. */
const OWNER_EMAIL = 'owner@example.test'

/** The bound GitHub identity the unbind journeys seed. */
const GITHUB_IDENTITY: AuthnIdentity = {
  id: 'identity-github',
  provider: 'github',
  email: OWNER_EMAIL,
  created_at: '2026-08-30T10:00:00.000Z',
}

/** The (code, state) pair the binding journey's subroute fragment
 * carries into the exchange. */
const BINDING_TARGET: AccountBindingTarget = {
  provider: 'github',
  code: 'code-from-provider',
  state: 'state-from-url',
}

/** The callback path the binding journey's exchange posts to. */
const SOCIAL_CALLBACK_PATH = '/api/v1/authn/social/github/callback'

/** The requests a journey observed, filtered by method and path. */
function callsFor(
  rig: RealClientRig,
  method: string,
  path: string,
): RealCall[] {
  return rig.calls.filter(
    (call) => call.method === method && call.path === path,
  )
}

/** The account surface over a signed-in rig, with the optional
 * binding-subroute props the app's AppView switch passes down. */
interface AccountRenderOptions extends RenderWithProvidersOptions {
  readonly bindingTarget?: AccountBindingTarget
  readonly onBound?: () => void
}

function renderAccount(
  rig: RealClientRig,
  options: AccountRenderOptions = {},
): ReturnType<typeof renderWithAppServices> {
  const { bindingTarget, onBound, ...providerOptions } = options
  return renderWithAppServices(
    <AccountView bindingTarget={bindingTarget} onBound={onBound} />,
    { session: rig.session, api: rig.api },
    providerOptions,
  )
}

describe('AccountView', () => {
  it('renders the served sessions, history, bindings and MFA surfaces over one read per list', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderAccount(rig)

    expect(
      view.getByRole('heading', { name: zhCN.account.heading }),
    ).toBeInTheDocument()
    expect(view.getByText(zhCN.account.intro)).toBeInTheDocument()

    // Sessions: the three served rows, the current one badged from the
    // server's own is_current, the two others each revocable in place
    // (the current session is never revocable from the list), and the
    // revoke-others action present while another row can be revoked.
    await view.findByText(CURRENT_SESSION_DEVICE)
    expect(view.getByText(OTHER_SESSION_DEVICE_ONE)).toBeInTheDocument()
    expect(view.getByText(OTHER_SESSION_DEVICE_TWO)).toBeInTheDocument()
    expect(view.getByText(accountUiZhCN.sessions.current)).toBeInTheDocument()
    expect(
      view.getByRole('button', {
        name: accountUiZhCN.sessions.revokeAriaWithDevice.replaceAll(
          '{{device}}',
          OTHER_SESSION_DEVICE_ONE,
        ),
      }),
    ).toBeInTheDocument()
    expect(
      view.getByRole('button', {
        name: accountUiZhCN.sessions.revokeAriaWithDevice.replaceAll(
          '{{device}}',
          OTHER_SESSION_DEVICE_TWO,
        ),
      }),
    ).toBeInTheDocument()
    expect(
      view.queryByRole('button', {
        name: accountUiZhCN.sessions.revokeAriaWithDevice.replaceAll(
          '{{device}}',
          CURRENT_SESSION_DEVICE,
        ),
      }),
    ).not.toBeInTheDocument()
    expect(
      view.getByRole('button', {
        name: accountUiZhCN.sessions.revokeOthers.label,
      }),
    ).toBeInTheDocument()

    // History: both served rows are password attempts -- the method
    // token renders twice -- with the success row's result chip and the
    // failure row's bad-password reason beside them.
    await waitFor(() =>
      expect(
        view.getAllByText(accountUiZhCN.history.method.password),
      ).toHaveLength(2),
    )
    expect(
      view.getByText(accountUiZhCN.history.result.success),
    ).toBeInTheDocument()
    expect(
      view.getByText(accountUiZhCN.history.reason.bad_password),
    ).toBeInTheDocument()

    // Bindings: nothing bound, so the add area offers all five demo
    // channels.
    await view.findByText(accountUiZhCN.bindings.addSectionTitle)
    for (const label of [
      accountUiZhCN.bindings.provider.google,
      accountUiZhCN.bindings.provider.github,
      accountUiZhCN.bindings.provider.wechat,
      accountUiZhCN.bindings.provider.dingtalk,
      accountUiZhCN.bindings.provider.feishu,
    ]) {
      expect(view.getByRole('button', { name: label })).toBeInTheDocument()
    }

    // MFA: the idle surface -- both entry actions rendered, and no
    // network of its own (the section discovers factors only through a
    // step-up refusal, never at idle).
    await view.findByRole('button', {
      name: accountUiZhCN.mfa.authenticator.enrollButton,
    })
    expect(
      view.getByRole('button', {
        name: accountUiZhCN.mfa.recoveryCodes.regenerateButton,
      }),
    ).toBeInTheDocument()

    // One authenticated read per list -- the history one carrying its
    // page-size query -- plus the sign-in itself, and nothing else.
    expect(callsFor(rig, 'GET', '/api/v1/authn/sessions')).toHaveLength(1)
    const historyCalls = callsFor(rig, 'GET', '/api/v1/authn/login-history')
    expect(historyCalls).toHaveLength(1)
    expect(historyCalls[0]?.query).toBe('?limit=20')
    expect(historyCalls[0]?.authorization).toBe('Bearer access-1')
    expect(callsFor(rig, 'GET', '/api/v1/authn/identities')).toHaveLength(1)
    expect(rig.calls).toHaveLength(4)
  })

  it('speaks the active language over the served state', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderAccount(rig, { language: 'en-US' })

    expect(
      view.getByRole('heading', { name: enUS.account.heading }),
    ).toBeInTheDocument()

    // The section chrome and the served rows all speak en-US; the
    // served device strings travel language-independently. The session
    // rows are the data-level anchor -- the section title is static
    // chrome and renders before the list answer lands.
    await view.findByText(CURRENT_SESSION_DEVICE)
    expect(view.getByText(accountUiEnUS.sessions.title)).toBeInTheDocument()
    expect(
      view.getByText(accountUiEnUS.sessions.current),
    ).toBeInTheDocument()
    expect(
      view.getByRole('button', {
        name: accountUiEnUS.sessions.revokeAriaWithDevice.replaceAll(
          '{{device}}',
          OTHER_SESSION_DEVICE_ONE,
        ),
      }),
    ).toBeInTheDocument()
    await waitFor(() =>
      expect(
        view.getAllByText(accountUiEnUS.history.method.password),
      ).toHaveLength(2),
    )
    expect(
      view.getByText(accountUiEnUS.history.result.success),
    ).toBeInTheDocument()
    await view.findByText(accountUiEnUS.bindings.addSectionTitle)
    await view.findByRole('button', {
      name: accountUiEnUS.mfa.authenticator.enrollButton,
    })
  })

  it('revokes one session in place and leaves the others revocable', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderAccount(rig)

    await view.findByText(CURRENT_SESSION_DEVICE)
    const user = userEvent.setup()
    await user.click(
      view.getByRole('button', {
        name: accountUiZhCN.sessions.revokeAriaWithDevice.replaceAll(
          '{{device}}',
          OTHER_SESSION_DEVICE_ONE,
        ),
      }),
    )

    // The in-place revoke is a single DELETE of the row the button
    // names, authenticated like every list read.
    await waitFor(() =>
      expect(
        callsFor(
          rig,
          'DELETE',
          `/api/v1/authn/sessions/${OTHER_SESSION_ID_ONE}`,
        ),
      ).toHaveLength(1),
    )
    const deleteCall = callsFor(
      rig,
      'DELETE',
      `/api/v1/authn/sessions/${OTHER_SESSION_ID_ONE}`,
    )[0]
    expect(deleteCall?.authorization).toBe('Bearer access-1')
    expect(deleteCall?.body).toBe('')

    // The refetched list marks the row revoked: the chip replaces the
    // row's revoke button, the other active row keeps its own, and the
    // current session still has none.
    await view.findByText(accountUiZhCN.sessions.status.revoked)
    expect(
      view.queryByRole('button', {
        name: accountUiZhCN.sessions.revokeAriaWithDevice.replaceAll(
          '{{device}}',
          OTHER_SESSION_DEVICE_ONE,
        ),
      }),
    ).not.toBeInTheDocument()
    expect(
      view.getByRole('button', {
        name: accountUiZhCN.sessions.revokeAriaWithDevice.replaceAll(
          '{{device}}',
          OTHER_SESSION_DEVICE_TWO,
        ),
      }),
    ).toBeInTheDocument()
    expect(
      view.queryByRole('button', {
        name: accountUiZhCN.sessions.revokeAriaWithDevice.replaceAll(
          '{{device}}',
          CURRENT_SESSION_DEVICE,
        ),
      }),
    ).not.toBeInTheDocument()

    // The refetch after the revoke is the list's second and last read.
    expect(callsFor(rig, 'GET', '/api/v1/authn/sessions')).toHaveLength(2)
  })

  it('revokes the other sessions behind the double-confirmed dialog', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderAccount(rig)
    const user = userEvent.setup()

    await view.findByText(CURRENT_SESSION_DEVICE)
    await user.click(
      view.getByRole('button', {
        name: accountUiZhCN.sessions.revokeOthers.label,
      }),
    )

    // The danger dialog names what is about to happen; its first
    // confirm only arms the action (the button re-labels with the
    // ui-kit confirm-again text), the second confirm fires it.
    const dialog = await view.findByRole('dialog')
    expect(
      within(dialog).getByText(
        accountUiZhCN.sessions.revokeOthers.confirmTitle,
      ),
    ).toBeInTheDocument()
    await user.click(
      within(dialog).getByRole('button', {
        name: accountUiZhCN.sessions.revokeOthers.confirmLabel,
      }),
    )
    await user.click(
      await within(dialog).findByRole('button', {
        name: uiKitZhCN.confirmDialog.confirmAgainLabel,
      }),
    )

    // Both non-current rows were active, so the answered count is two
    // and the notice renders the bundle's plural form for it.
    await waitFor(() =>
      expect(
        callsFor(rig, 'POST', '/api/v1/authn/sessions/revoke-others'),
      ).toHaveLength(1),
    )
    expect(
      callsFor(rig, 'POST', '/api/v1/authn/sessions/revoke-others')[0]
        ?.authorization,
    ).toBe('Bearer access-1')
    await view.findByText(
      accountUiZhCN.sessions.revokeOthers.done_other.replaceAll(
        '{{count}}',
        '2',
      ),
    )

    // The refetched list marks both rows revoked: their revoke buttons
    // and the revoke-others action are gone, and the dialog closed.
    await waitFor(() =>
      expect(
        view.getAllByText(accountUiZhCN.sessions.status.revoked),
      ).toHaveLength(2),
    )
    expect(
      view.queryByRole('button', {
        name: accountUiZhCN.sessions.revokeAriaWithDevice.replaceAll(
          '{{device}}',
          OTHER_SESSION_DEVICE_ONE,
        ),
      }),
    ).not.toBeInTheDocument()
    expect(
      view.queryByRole('button', {
        name: accountUiZhCN.sessions.revokeAriaWithDevice.replaceAll(
          '{{device}}',
          OTHER_SESSION_DEVICE_TWO,
        ),
      }),
    ).not.toBeInTheDocument()
    expect(
      view.queryByRole('button', {
        name: accountUiZhCN.sessions.revokeOthers.label,
      }),
    ).not.toBeInTheDocument()
    await waitFor(() =>
      expect(view.queryByRole('dialog')).not.toBeInTheDocument(),
    )
    expect(callsFor(rig, 'GET', '/api/v1/authn/sessions')).toHaveLength(2)
  })

  it('keeps a bound identity whose unbind the server refuses on the page', async () => {
    const rig = makeRealClientRig(
      demoServer({
        initialIdentities: [GITHUB_IDENTITY],
        refuseUnbindIdentityId: GITHUB_IDENTITY.id,
      }),
    )
    await signInWithPassword(rig)
    const view = renderAccount(rig)
    const user = userEvent.setup()

    // The bound row renders its provider and address, and the add area
    // offers only the four channels not yet bound.
    await view.findByText(accountUiZhCN.bindings.provider.github)
    expect(view.getByText(OWNER_EMAIL)).toBeInTheDocument()
    expect(
      view.queryByRole('button', {
        name: accountUiZhCN.bindings.provider.github,
      }),
    ).not.toBeInTheDocument()
    for (const label of [
      accountUiZhCN.bindings.provider.google,
      accountUiZhCN.bindings.provider.wechat,
      accountUiZhCN.bindings.provider.dingtalk,
      accountUiZhCN.bindings.provider.feishu,
    ]) {
      expect(view.getByRole('button', { name: label })).toBeInTheDocument()
    }

    // Unbind is single-confirmed: the danger dialog's own confirm is
    // the whole gate.
    await user.click(
      view.getByRole('button', {
        name: accountUiZhCN.bindings.unbind,
      }),
    )
    const dialog = await view.findByRole('dialog')
    expect(
      within(dialog).getByText(accountUiZhCN.bindings.confirmTitle),
    ).toBeInTheDocument()
    await user.click(
      within(dialog).getByRole('button', {
        name: accountUiZhCN.bindings.confirmLabel,
      }),
    )

    // The refusal -- an account whose last sign-in method is the
    // binding cannot shed it -- renders its code text above the rows,
    // the row and its unbind button survive, and no refetch follows
    // (only an identity_not_found answer refetches).
    await view.findByText(accountUiZhCN.errors.authn.last_login_method)
    await waitFor(() =>
      expect(view.queryByRole('dialog')).not.toBeInTheDocument(),
    )
    expect(
      view.getByRole('button', {
        name: accountUiZhCN.bindings.unbind,
      }),
    ).toBeInTheDocument()
    expect(
      view.getByText(accountUiZhCN.bindings.provider.github),
    ).toBeInTheDocument()
    expect(callsFor(rig, 'GET', '/api/v1/authn/identities')).toHaveLength(1)
  })

  it('completes a binding exchange, refetches the identities and cues the host once', async () => {
    const server = demoServer()
    // The callback answer is held open: the pending notice rests until
    // the exchange lands, and the served identity joins the list only
    // when the answer actually resolves.
    let releaseExchange: (() => void) | undefined
    const rig = makeRealClientRig((call) => {
      if (call.method === 'POST' && call.path === SOCIAL_CALLBACK_PATH) {
        return new Promise<Response>((resolve) => {
          releaseExchange = () => {
            resolve(server(call))
          }
        })
      }
      return server(call)
    })
    await signInWithPassword(rig)
    const onBound = vi.fn()
    const view = renderAccount(rig, {
      bindingTarget: BINDING_TARGET,
      onBound,
    })

    // The exchange opens on mount with the fragment's own (code, state)
    // pair, authenticated like every surface request; the pending
    // notice rests while the answer is held.
    await waitFor(() =>
      expect(callsFor(rig, 'POST', SOCIAL_CALLBACK_PATH)).toHaveLength(1),
    )
    const exchangeCall = callsFor(rig, 'POST', SOCIAL_CALLBACK_PATH)[0]
    expect(JSON.parse(exchangeCall?.body ?? '{}')).toEqual({
      code: BINDING_TARGET.code,
      state: BINDING_TARGET.state,
    })
    expect(exchangeCall?.authorization).toBe('Bearer access-1')
    expect(
      await view.findByText(accountUiZhCN.bindingCallback.pending),
    ).toBeInTheDocument()
    // Nothing is bound yet, so the add area still offers the channel
    // whose exchange is in flight.
    expect(
      view.getByRole('button', {
        name: accountUiZhCN.bindings.provider.github,
      }),
    ).toBeInTheDocument()

    // The release lands a binding-shaped answer: the identities list
    // refetches (its invalidation precedes the cue) and onBound fires
    // exactly once.
    await act(async () => {
      releaseExchange?.()
    })
    await waitFor(() => expect(onBound).toHaveBeenCalledTimes(1))

    // The refetched list shows the bound row -- its provider and the
    // address the server bound -- and the add area no longer offers the
    // channel. The pending notice still rests: navigation off the
    // subroute is the host's answer to the cue, and this render is the
    // surface without that host.
    await view.findByText(OWNER_EMAIL)
    await waitFor(() =>
      expect(
        view.queryByRole('button', {
          name: accountUiZhCN.bindings.provider.github,
        }),
      ).not.toBeInTheDocument(),
    )
    for (const label of [
      accountUiZhCN.bindings.provider.google,
      accountUiZhCN.bindings.provider.wechat,
      accountUiZhCN.bindings.provider.dingtalk,
      accountUiZhCN.bindings.provider.feishu,
    ]) {
      expect(view.getByRole('button', { name: label })).toBeInTheDocument()
    }
    expect(
      view.getByText(accountUiZhCN.bindingCallback.pending),
    ).toBeInTheDocument()

    expect(callsFor(rig, 'POST', SOCIAL_CALLBACK_PATH)).toHaveLength(1)
    expect(callsFor(rig, 'GET', '/api/v1/authn/identities')).toHaveLength(2)
  })
})
