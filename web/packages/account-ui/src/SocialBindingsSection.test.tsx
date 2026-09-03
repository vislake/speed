/**
 * SocialBindingsSection behaviour, driven over the real-client rig: the
 * section never sees a mock fetch -- the responder answers genuine
 * Response objects to a real @speed/api-client (bound through the same
 * bindRequestFn seam the generated hooks use) and every answer is the
 * shape the server answers, assertions on what the user sees.
 *
 * The suite pins the surface contract: every bound identity renders its
 * provider label (mapped through bindings.provider for the known
 * providers, the generic "other" label for a value outside the set --
 * never a raw provider string) with the provider account's email when
 * the answer carries one, and only an identity that carries an id has a
 * row-end unlink action; unlinking sits behind the ui-kit danger
 * dialog, a refused unbind renders its code text (last_login_method
 * keeps the row, identity_not_found -- an unbound-elsewhere race --
 * refetches the list so the stale row disappears and the banner clears
 * once the list converged); the add area lists exactly the configured
 * providers not already bound and clicking one asks the session for
 * that channel's authorization URL, reporting it upward through
 * onAuthorizeUrl (never navigating, and single-flight across the
 * providers: while any channel's URL is being built every provider
 * button is disabled and a raw second click sends nothing); when every
 * configured provider is already bound the add area does not render.
 * Loading, error and empty states render their
 * own placeholder with the section header hidden (except the first-run
 * shape: an empty list with an unbound configured provider is exactly
 * the add area's cue and keeps the header). Every scenario ends with an
 * axe pass.
 */

import { describe, expect, it } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AuthnIdentity } from '@speed/api-sdk'
import { uiKitResources } from '@speed/ui-kit'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import {
  errorResponse,
  jsonResponse,
  makePair,
  makeRealClientRig,
  signInWithPassword,
} from '../test-utils/real-client.js'
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import {
  SocialBindingsSection,
  type SocialProvider,
  type SocialProviderConfig,
} from './SocialBindingsSection.js'

const LOGIN_PATH = '/api/v1/authn/login/password'
const IDENTITIES_PATH = '/api/v1/authn/identities'

/** The ui-kit dialog strings the cancel leg depends on, typed straight
 * from the package's own exported resources. */
const UI_KIT_ZH = uiKitResources['zh-CN'] as unknown as {
  readonly confirmDialog: {
    readonly cancelLabel: string
  }
}

function identity(overrides: Partial<AuthnIdentity> = {}): AuthnIdentity {
  return {
    id: 'identity-1',
    provider: 'github',
    created_at: '2026-07-01T00:00:00.000Z',
    ...overrides,
  }
}

function config(provider: SocialProvider): SocialProviderConfig {
  return { provider, redirectUri: 'https://app.test/callback' }
}

/** The fresh-204 answer the unbind endpoint gives a successful delete. */
const NO_CONTENT = () => new Response(null, { status: 204 })

const UNLINK = zhCN.bindings.confirmLabel
const DIALOG_TITLE = zhCN.bindings.confirmTitle
const DIALOG_MESSAGE = zhCN.bindings.confirmMessage
const ADD_TITLE = zhCN.bindings.addSectionTitle

/** The unlink action of the first row. The two-row scenarios list the
 * github identity first, and both legs act on that row; a fixture where
 * no row carries an action would be a test bug, so the guard throws
 * rather than click a phantom. */
function firstUnlink(): HTMLElement {
  const first = screen.getAllByRole('button', { name: UNLINK })[0]
  if (first === undefined) {
    throw new Error('expected at least one unlink action')
  }
  return first
}

describe('SocialBindingsSection', () => {
  it('render every identity with its provider label, email and one unlink action per id-carrying row', async () => {
    const list = [
      identity({
        id: 'github-1',
        provider: 'github',
        email: 'dev@example.test',
      }),
      identity({ id: 'google-1', provider: 'google' }),
      identity({
        id: 'zeta-1',
        // A provider value outside the spec's five: the generic
        // "other" label renders, never the raw value.
        provider: 'zeta',
        email: 'zeta@example.test',
      }),
    ]
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        return jsonResponse(200, { identities: list })
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    renderWithProviders(
      <SocialBindingsSection
        session={rig.session}
        providers={[config('google'), config('github'), config('wechat')]}
      />,
    )

    // The heading renders with the pending skeleton too, so the rows
    // themselves are what settle the loaded state.
    expect(
      await screen.findByRole('heading', { level: 2, name: zhCN.bindings.title }),
    ).toBeTruthy()
    expect(await screen.findByText('GitHub')).toBeTruthy()
    expect(screen.getByText('Google')).toBeTruthy()
    expect(
      screen.getByText(zhCN.bindings.provider.other),
    ).toBeTruthy()
    expect(screen.getByText('dev@example.test')).toBeTruthy()
    expect(screen.getByText('zeta@example.test')).toBeTruthy()
    // The three id-carrying rows have exactly one unlink action each.
    expect(screen.getAllByRole('button', { name: UNLINK })).toHaveLength(3)
    // The unbound channel (wechat) is the only add-area option.
    expect(
      screen.getByRole('button', { name: zhCN.bindings.provider.wechat }),
    ).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'GitHub' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Google' })).toBeNull()

    await expectNoAxeViolations()
  })

  it('unlink a row behind the danger dialog: cancel changes nothing, confirm deletes and the list refetches', async () => {
    const list = [
      identity({ id: 'github-1', provider: 'github', email: 'dev@example.test' }),
      identity({ id: 'google-1', provider: 'google' }),
    ]
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        return jsonResponse(200, { identities: list })
      }
      if (call.method === 'DELETE' && call.path === `${IDENTITIES_PATH}/github-1`) {
        list.splice(0, 1)
        return NO_CONTENT()
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    renderWithProviders(
      <SocialBindingsSection
        session={rig.session}
        providers={[config('google'), config('github')]}
      />,
    )
    await screen.findByText('dev@example.test')

    // Cancel: the dialog closes and nothing was deleted.
    await userEvent.click(firstUnlink())
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(DIALOG_TITLE)).toBeTruthy()
    expect(within(dialog).getByText(DIALOG_MESSAGE)).toBeTruthy()
    await userEvent.click(
      within(dialog).getByRole('button', { name: UI_KIT_ZH.confirmDialog.cancelLabel }),
    )
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(rig.calls.filter((call) => call.method === 'DELETE')).toHaveLength(0)

    // Confirm: the delete goes out with the caller's bearer token and
    // the list refetches without the row.
    await userEvent.click(firstUnlink())
    const openDialog = await screen.findByRole('dialog')
    await userEvent.click(
      within(openDialog).getByRole('button', { name: UNLINK }),
    )
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    const deleteCall = rig.calls.find(
      (call) => call.method === 'DELETE' && call.path === `${IDENTITIES_PATH}/github-1`,
    )
    expect(deleteCall?.authorization).toBe('Bearer access-1')
    await waitFor(() => expect(screen.queryByText('dev@example.test')).toBeNull())
    expect(screen.getByText('Google')).toBeTruthy()
    expect(screen.getAllByRole('button', { name: UNLINK })).toHaveLength(1)
    // GitHub is unbound again, so the add area offers the channel.
    expect(screen.getByText(ADD_TITLE)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'GitHub' })).toBeTruthy()

    await expectNoAxeViolations()
  })

  it('render the last_login_method text when the unbind would shed the final sign-in method', async () => {
    const list = [identity({ id: 'github-1', provider: 'github' })]
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        return jsonResponse(200, { identities: list })
      }
      if (call.method === 'DELETE') {
        return errorResponse(409, 'authn.last_login_method')
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    renderWithProviders(
      <SocialBindingsSection
        session={rig.session}
        providers={[config('google')]}
      />,
    )
    await userEvent.click(await screen.findByRole('button', { name: UNLINK }))
    await userEvent.click(
      within(await screen.findByRole('dialog')).getByRole('button', {
        name: UNLINK,
      }),
    )

    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(zhCN.errors.authn.last_login_method)
    // The refused row stays; the add area keeps offering the unbound
    // channel and no refetch happened (one GET total).
    expect(screen.getByText('GitHub')).toBeTruthy()
    expect(rig.calls.filter((call) => call.method === 'GET')).toHaveLength(1)

    await expectNoAxeViolations()
  })

  it('handle an unbound-elsewhere race: identity_not_found text, then the list refetches and the banner clears', async () => {
    const list = [identity({ id: 'github-1', provider: 'github' })]
    // The post-race refetch is gated so the banner stays visible while
    // it is in flight -- otherwise the clear lands in the same tick as
    // the notice and the intermediate state is unobservable.
    let deleted = false
    let releaseRefetch: () => void = () => undefined
    const refetchGate = new Promise<void>((resolve) => {
      releaseRefetch = resolve
    })
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        if (deleted) {
          await refetchGate
          return jsonResponse(200, { identities: [] })
        }
        return jsonResponse(200, { identities: list })
      }
      if (call.method === 'DELETE') {
        deleted = true
        list.splice(0, 1)
        return errorResponse(404, 'authn.identity_not_found')
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    renderWithProviders(
      <SocialBindingsSection
        session={rig.session}
        providers={[config('github')]}
      />,
    )
    await userEvent.click(await screen.findByRole('button', { name: UNLINK }))
    await userEvent.click(
      within(await screen.findByRole('dialog')).getByRole('button', {
        name: UNLINK,
      }),
    )

    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(zhCN.errors.authn.identity_not_found)
    // The invalidate's refetch is gated in flight: the banner stays
    // until it converges.
    expect(
      rig.calls.filter(
        (call) => call.method === 'GET' && call.path === IDENTITIES_PATH,
      ),
    ).toHaveLength(2)
    releaseRefetch()
    // Once the stale row is gone the banner clears -- the race it
    // announced is over. The channel is unbound again, so the add area
    // offers it (the row is gone either way: zero unlink actions).
    await waitFor(() =>
      expect(screen.queryAllByRole('button', { name: UNLINK })).toHaveLength(0),
    )
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull())
    expect(screen.getByRole('button', { name: 'GitHub' })).toBeTruthy()

    await expectNoAxeViolations()
  })

  it('ask the session for the authorization URL of a clicked add-area channel and report it through onAuthorizeUrl', async () => {
    const list = [identity({ id: 'github-1', provider: 'github' })]
    let releaseAuthorize: () => void = () => undefined
    const authorizeGate = new Promise<void>((resolve) => {
      releaseAuthorize = resolve
    })
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        return jsonResponse(200, { identities: list })
      }
      if (call.method === 'GET' && call.path === '/api/v1/authn/social/wechat/authorize') {
        await authorizeGate
        return jsonResponse(200, {
          authorize_url: 'https://open.weixin.qq.com/connect/oauth2/authorize?state=st-1',
        })
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    let reported: string | undefined
    renderWithProviders(
      <SocialBindingsSection
        session={rig.session}
        providers={[config('wechat')]}
        onAuthorizeUrl={(url) => {
          reported = url
        }}
      />,
    )

    const wechatButton = await screen.findByRole('button', {
      name: zhCN.bindings.provider.wechat,
    })
    await userEvent.click(wechatButton)
    // Single-flight: while the URL is being built the channel's button
    // is disabled (userEvent would refuse the click -- pointer-events
    // none -- so the second attempt is dispatched raw) and a second
    // click sends nothing.
    expect(wechatButton).toBeDisabled()
    fireEvent.click(wechatButton)
    expect(
      rig.calls.filter((call) => call.path === '/api/v1/authn/social/wechat/authorize'),
    ).toHaveLength(1)

    releaseAuthorize()
    await waitFor(() => expect(reported).toBeDefined())
    expect(reported).toBe(
      'https://open.weixin.qq.com/connect/oauth2/authorize?state=st-1',
    )
    const authorizeCall = rig.calls.find(
      (call) => call.path === '/api/v1/authn/social/wechat/authorize',
    )
    expect(authorizeCall?.method).toBe('GET')
    expect(authorizeCall?.authorization).toBe('Bearer access-1')
    const query = new URLSearchParams(authorizeCall?.query)
    expect(query.get('redirect_uri')).toBe('https://app.test/callback')
    await waitFor(() => expect(wechatButton).toBeEnabled())

    await expectNoAxeViolations()
  })

  it('single-flight the authorize requests across providers: one URL built at a time, every button disabled together', async () => {
    const list = [identity({ id: 'github-1', provider: 'github' })]
    let releaseWechat: () => void = () => undefined
    const wechatGate = new Promise<void>((resolve) => {
      releaseWechat = resolve
    })
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        return jsonResponse(200, { identities: list })
      }
      if (
        call.method === 'GET' &&
        call.path === '/api/v1/authn/social/wechat/authorize'
      ) {
        await wechatGate
        return jsonResponse(200, {
          authorize_url:
            'https://open.weixin.qq.com/connect/oauth2/authorize?state=st-1',
        })
      }
      if (
        call.method === 'GET' &&
        call.path === '/api/v1/authn/social/google/authorize'
      ) {
        // An answer the pre-fix code would have collected: a second
        // flow started while the WeChat one was still building. The
        // fix keeps this path untouched -- the assertion below pins it.
        return jsonResponse(200, {
          authorize_url:
            'https://accounts.google.com/o/oauth2/v2/auth?state=st-2',
        })
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    const reported: string[] = []
    renderWithProviders(
      <SocialBindingsSection
        session={rig.session}
        providers={[config('wechat'), config('google')]}
        onAuthorizeUrl={(url) => {
          reported.push(url)
        }}
      />,
    )

    const wechatButton = await screen.findByRole('button', {
      name: zhCN.bindings.provider.wechat,
    })
    const googleButton = screen.getByRole('button', { name: 'Google' })
    await userEvent.click(wechatButton)
    // The busy slot is provider-wide, never one channel's alone: while
    // the WeChat URL is being built the Google button is disabled with
    // it, so no second flow can even be started from the surface.
    expect(wechatButton).toBeDisabled()
    expect(googleButton).toBeDisabled()
    // A raw second click -- any channel's -- sends nothing either: the
    // request path is single-flight, not just the disabled styling
    // (userEvent would refuse the click on a disabled button --
    // pointer-events none -- so this attempt is dispatched raw).
    fireEvent.click(googleButton)
    expect(
      rig.calls.filter(
        (call) => call.path === '/api/v1/authn/social/google/authorize',
      ),
    ).toHaveLength(0)

    releaseWechat()
    // Exactly one URL is reported -- the WeChat one, the flow that
    // actually started -- and the Google flow never happened.
    await waitFor(() => expect(reported).toHaveLength(1))
    expect(reported[0]).toBe(
      'https://open.weixin.qq.com/connect/oauth2/authorize?state=st-1',
    )
    expect(
      rig.calls.filter(
        (call) => call.path === '/api/v1/authn/social/wechat/authorize',
      ),
    ).toHaveLength(1)
    // The flow answered: both buttons are enabled again.
    await waitFor(() => expect(wechatButton).toBeEnabled())
    expect(googleButton).toBeEnabled()

    await expectNoAxeViolations()
  })

  it('render a refused authorize-URL request as the code text and report no URL', async () => {
    const list = [identity({ id: 'github-1', provider: 'github' })]
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        return jsonResponse(200, { identities: list })
      }
      if (call.method === 'GET') {
        return errorResponse(400, 'authn.provider_unknown')
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    let reported = false
    renderWithProviders(
      <SocialBindingsSection
        session={rig.session}
        providers={[config('google')]}
        onAuthorizeUrl={() => {
          reported = true
        }}
      />,
    )
    await userEvent.click(await screen.findByRole('button', { name: 'Google' }))
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(zhCN.errors.authn.provider_unknown)
    expect(reported).toBe(false)

    await expectNoAxeViolations()
  })

  it('hide the add area when every configured provider is already bound', async () => {
    const list = [
      identity({ id: 'github-1', provider: 'github' }),
      identity({ id: 'google-1', provider: 'google' }),
    ]
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        return jsonResponse(200, { identities: list })
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    renderWithProviders(
      <SocialBindingsSection
        session={rig.session}
        providers={[config('google'), config('github')]}
      />,
    )
    await screen.findByText('GitHub')
    expect(screen.queryByText(ADD_TITLE)).toBeNull()
    expect(screen.getAllByRole('button', { name: UNLINK })).toHaveLength(2)

    await expectNoAxeViolations()
  })

  it('render the first-run shape: an empty list with an unbound channel is the add area, header kept', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        return jsonResponse(200, { identities: [] })
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    renderWithProviders(
      <SocialBindingsSection
        session={rig.session}
        providers={[config('google'), config('feishu')]}
      />,
    )
    expect(
      await screen.findByRole('heading', { level: 2, name: zhCN.bindings.title }),
    ).toBeTruthy()
    // The empty list answers quickly but not synchronously, so the
    // add-area buttons are what settle the loaded state.
    expect(await screen.findByRole('button', { name: 'Google' })).toBeTruthy()
    expect(
      screen.getByRole('button', { name: zhCN.bindings.provider.feishu }),
    ).toBeTruthy()
    expect(screen.queryByText(zhCN.bindings.empty.title)).toBeNull()

    await expectNoAxeViolations()
  })

  it('render the empty state, header hidden, when the list is empty and no channel is configured', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        return jsonResponse(200, { identities: [] })
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    renderWithProviders(
      <SocialBindingsSection session={rig.session} providers={[]} />,
    )
    expect(
      await screen.findByText(zhCN.bindings.empty.title),
    ).toBeTruthy()
    expect(screen.queryByText(zhCN.bindings.title)).toBeNull()
    expect(screen.queryByText(ADD_TITLE)).toBeNull()

    await expectNoAxeViolations()
  })

  it('render the error state with a retry that refetches, header hidden', async () => {
    let failFirst = true
    const list = [identity({ id: 'github-1', provider: 'github' })]
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        if (failFirst) {
          failFirst = false
          return errorResponse(500, 'internal')
        }
        return jsonResponse(200, { identities: list })
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    renderWithProviders(
      <SocialBindingsSection
        session={rig.session}
        providers={[config('github')]}
      />,
    )
    expect(await screen.findByText(zhCN.bindings.error.title)).toBeTruthy()
    expect(screen.queryByText(zhCN.bindings.title)).toBeNull()

    await userEvent.click(screen.getByRole('button', { name: zhCN.bindings.retry }))
    expect(await screen.findByText('GitHub')).toBeTruthy()

    await expectNoAxeViolations()
  })

  it('announce the loading list as one status region until the rows land', async () => {
    let releaseList: () => void = () => undefined
    const listGate = new Promise<void>((resolve) => {
      releaseList = resolve
    })
    const list = [identity({ id: 'github-1', provider: 'github' })]
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        await listGate
        return jsonResponse(200, { identities: list })
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    renderWithProviders(
      <SocialBindingsSection
        session={rig.session}
        providers={[config('github')]}
      />,
    )
    expect(
      await screen.findByRole('status', { name: zhCN.bindings.loading }),
    ).toBeTruthy()
    expect(
      screen.getByRole('heading', { level: 2, name: zhCN.bindings.title }),
    ).toBeTruthy()

    releaseList()
    expect(await screen.findByText('GitHub')).toBeTruthy()
    await waitFor(() =>
      expect(screen.queryByRole('status', { name: zhCN.bindings.loading })).toBeNull(),
    )

    await expectNoAxeViolations()
  })
})
