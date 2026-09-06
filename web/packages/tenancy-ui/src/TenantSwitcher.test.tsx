/**
 * TenantSwitcher behaviour: the trigger shows the host-supplied current
 * tenant and opens the host-supplied list; picking a row that is not the
 * current tenant drives exactly one switch round-trip through the
 * bindRequestFn harness (asserted on method, path and body) and commits
 * the fresh token; the current-tenant row is disabled and can never
 * re-trigger a switch. While a switch is in flight the trigger is inert
 * -- aria-disabled and focusable, never the native disabled attribute,
 * so the menu-close focus restore lands on a live control (a
 * native-disabled trigger cannot take focus in a real browser and the
 * round trip would strand focus on document.body; jsdom reports the
 * same code path as focus on the disabled trigger, which the focus
 * regression test pins against) -- and a role="status" notice renders
 * the switching text, and a synchronous entry guard in the switch
 * handler refuses any second attempt in that window; jsdom cannot stage
 * such an attempt -- the closing list unmounts synchronously here
 * instead of animating out, so no activation can reach the handler
 * mid-flight, and the guard's browser-tier window (rows of the
 * still-closing list, mounted and clickable for the exit transition) is
 * the reference-app Playwright journeys' to pin. A successful switch is
 * quiet (no alert) and fires
 * onSwitched exactly once per committed switch, after the commit --
 * including the reconciling re-issue a superseded switch triggers when
 * it loses a race (the racing describe below); a throwing onSwitched is
 * contained: the commit stands, no error renders, and no rejection
 * escapes the fire-and-forget row handler. A rejected switch renders the
 * answer's code
 * text in one alert, changes nothing locally -- the store keeps its
 * token and the trigger stays ready to retry, and a retry clears the
 * alert. The session-lifecycle codes resolve to their own texts in the
 * current language (asserted zh, then en after a language switch); an
 * unlisted code renders the unknown fallback; with no current tenant
 * the trigger is disabled and shows the noCurrentTenant text. Text
 * expectations read the bundle values, never inline language.
 */

import { describe, expect, it, vi } from 'vitest'
import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { switchLanguage } from '@speed/i18n'
import { TenantSwitcher } from './TenantSwitcher.js'
import type { TenantSwitcherProps } from './TenantSwitcher.js'
import { renderWithProviders } from '../test-utils/render.js'
import {
  LOGIN_PASSWORD,
  REFRESH,
  SWITCH_TENANT,
  apiError,
  makeHarness,
  makePair,
  type Harness,
} from '../test-utils/session-harness.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

const TENANTS = [
  { id: 'tenant-1', name: 'Sunshine Dental' },
  { id: 'tenant-2', name: 'Bright Smile Clinic' },
] as const

const SWITCHING_ZH = zhCN.tenantSwitcher.switching
const NO_CURRENT_ZH = zhCN.tenantSwitcher.noCurrentTenant
const NO_CURRENT_EN = enUS.tenantSwitcher.noCurrentTenant

/** Signs the harness session in through the real login operation. */
async function signIn(harness: Harness): Promise<void> {
  await harness.session.loginWithPassword({
    identifier: 'alice@example.com',
    password: 's3cret-pass',
  })
}

/** Renders the switcher over a signed-in harness session, current tenant-1. */
function renderSwitcher(
  harness: Harness,
  props: Partial<TenantSwitcherProps> = {},
) {
  const onSwitched = vi.fn()
  const result = renderWithProviders(
    <TenantSwitcher
      session={harness.session}
      tenants={TENANTS}
      currentTenantId="tenant-1"
      onSwitched={onSwitched}
      {...props}
    />,
  )
  return { ...result, onSwitched }
}

/** Opens the list from the current-tenant trigger and returns the rows. */
async function openMenu(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: 'Sunshine Dental' }))
  return {
    current: await screen.findByRole('menuitem', { name: 'Sunshine Dental' }),
    other: await screen.findByRole('menuitem', { name: 'Bright Smile Clinic' }),
  }
}

describe('TenantSwitcher', () => {
  it('show the current tenant and open the full list, current row disabled', async () => {
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    await signIn(harness)
    renderSwitcher(harness)
    const trigger = screen.getByRole('button', { name: 'Sunshine Dental' })
    expect(trigger).toBeEnabled()
    const user = userEvent.setup()
    const { current, other } = await openMenu(user)
    // MUI v9 renders a disabled MenuItem as aria-disabled="true" (no
    // native disabled attribute on the li), which is what assistive
    // tech reads; jest-dom's toBeDisabled only knows the native
    // attribute, so the a11y semantics are asserted directly.
    expect(current).toHaveAttribute('aria-disabled', 'true')
    expect(other).not.toHaveAttribute('aria-disabled')
    expect(screen.getByRole('menu')).toBeInTheDocument()
  })

  it('never re-trigger a switch from the current-tenant row', async () => {
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    await signIn(harness)
    renderSwitcher(harness)
    const user = userEvent.setup()
    const { current } = await openMenu(user)
    // fireEvent bypasses the pointer-events check user-event enforces on
    // disabled controls -- the disabled row is the inertness guard under
    // test, so the click must reach it synthetically.
    fireEvent.click(current)
    await waitFor(() => {
      // Nothing happened: no switch call, token untouched, menu still
      // open, row still disabled.
      expect(harness.calls).toHaveLength(1)
      expect(harness.store.get()).toBe('access-1')
      expect(
        screen.getByRole('menuitem', { name: 'Sunshine Dental' }),
      ).toHaveAttribute('aria-disabled', 'true')
    })
  })

  it('switch on picking another tenant, firing onSwitched exactly once after the commit', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        makePair({
          access_token: 'access-2',
          principal: { user_id: 'user-1', tenant_id: 'tenant-2', session_id: 'session-1' },
        }),
    })
    await signIn(harness)
    const { onSwitched } = renderSwitcher(harness)
    const user = userEvent.setup()
    const { other } = await openMenu(user)
    await user.click(other)
    await waitFor(() => expect(harness.store.get()).toBe('access-2'))
    expect(harness.calls).toHaveLength(2)
    expect(harness.calls[1]?.method).toBe('POST')
    expect(harness.calls[1]?.path).toBe('/api/v1/authn/tenant/switch')
    expect(harness.calls[1]?.options?.body).toEqual({ tenant_id: 'tenant-2' })
    expect(onSwitched).toHaveBeenCalledTimes(1)
    expect(onSwitched).toHaveBeenCalledWith('tenant-2')
    // Success is quiet: no alert, the list closed, the trigger back to
    // its idle label (host data, unchanged by the switch) and enabled.
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    await waitFor(() =>
      expect(screen.queryByRole('menu')).not.toBeInTheDocument(),
    )
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Sunshine Dental' }),
      ).toBeEnabled(),
    )
  })

  it('make the trigger inert with a status notice while the switch is in flight', async () => {
    let resolveSwitch: (value: unknown) => void = () => {}
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        new Promise((resolve) => {
          resolveSwitch = resolve
        }),
    })
    await signIn(harness)
    const { onSwitched } = renderSwitcher(harness)
    const user = userEvent.setup()
    const { other } = await openMenu(user)
    await user.click(other)
    // Inert while the flight is pending -- aria-disabled, never the
    // native attribute (a native-disabled trigger cannot take the
    // menu-close focus restore; the focus regression test pins that).
    const trigger = screen.getByRole('button', { name: 'Sunshine Dental' })
    expect(trigger).toHaveAttribute('aria-disabled', 'true')
    expect(trigger).not.toHaveAttribute('disabled')
    const status = screen.getByRole('status')
    expect(status).toHaveTextContent(SWITCHING_ZH)
    expect(harness.calls).toHaveLength(2)
    await act(async () => {
      resolveSwitch(
        makePair({
          access_token: 'access-2',
          principal: { user_id: 'user-1', tenant_id: 'tenant-2', session_id: 'session-1' },
        }),
      )
    })
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Sunshine Dental' }),
      ).toBeEnabled(),
    )
    expect(onSwitched).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('keep the focus on a live control when the menu closes onto the inert trigger', async () => {
    // Regression for the P2-2 focus finding: picking a row closes the
    // menu while the switch is in flight, and MUI's focus trap restores
    // focus to the trigger on close. A NATIVE-disabled trigger cannot
    // take focus in a real browser (focus() on a disabled control is a
    // no-op), so the restore silently fails and the round trip strands
    // focus on document.body; jsdom lacks that rule and reports the
    // same code path as focus resting ON the native-disabled trigger --
    // both are one defect: the focus target is not a live element. The
    // trigger therefore stays focusable while the switch is in flight,
    // inert in the accessible way (aria-disabled, clicks refused by the
    // open guard), never the native attribute. The round trip then
    // leaves the focus on the trigger -- a failed switch is retryable
    // from where the user is, with no tab-out-and-back to find the
    // control again.
    let rejectSwitch!: (reason: unknown) => void
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        new Promise((_resolve, reject) => {
          rejectSwitch = reject
        }),
    })
    await signIn(harness)
    const { onSwitched } = renderSwitcher(harness)
    const user = userEvent.setup()
    const { other } = await openMenu(user)
    await user.click(other)
    const trigger = screen.getByRole('button', { name: 'Sunshine Dental' })
    await waitFor(() =>
      expect(screen.queryByRole('menu')).not.toBeInTheDocument(),
    )
    // The menu is gone and the switch is in flight: the element holding
    // focus is the trigger, and the trigger is not native-disabled -- a
    // real browser could not focus it otherwise (jsdom's stand-in for
    // the body-stranding defect is focus on a native-disabled control).
    expect(document.activeElement).toBe(trigger)
    expect(trigger).not.toHaveAttribute('disabled')
    expect(trigger).toHaveAttribute('aria-disabled', 'true')
    // Fail the switch: the alert renders under the control, the trigger
    // re-enables and the focus is still on it -- retry from where the
    // user is.
    await act(async () => {
      rejectSwitch(apiError(403, 'authn.tenant_membership_required'))
    })
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.tenant_membership_required,
      ),
    )
    expect(document.activeElement).toBe(trigger)
    expect(trigger).toBeEnabled()
    expect(onSwitched).not.toHaveBeenCalled()
    // The retry, keyboard-first: Enter on the focused trigger reopens
    // the list and the same row is pickable again.
    await user.keyboard('{Enter}')
    expect(
      await screen.findByRole('menuitem', { name: 'Bright Smile Clinic' }),
    ).toBeInTheDocument()
    expect(harness.calls).toHaveLength(2)
  })

  it('contain a throwing onSwitched: the commit stands and no rejection escapes', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        makePair({
          access_token: 'access-2',
          principal: { user_id: 'user-1', tenant_id: 'tenant-2', session_id: 'session-1' },
        }),
    })
    await signIn(harness)
    const onUnhandled = vi.fn<(reason: unknown) => void>()
    const listener = (event: PromiseRejectionEvent) => {
      onUnhandled(event.reason)
    }
    window.addEventListener('unhandledrejection', listener)
    const onSwitched = vi.fn(() => {
      throw new Error('host post-switch work failed')
    })
    try {
      renderSwitcher(harness, { onSwitched })
      const user = userEvent.setup()
      const { other } = await openMenu(user)
      await user.click(other)
      await waitFor(() => expect(harness.store.get()).toBe('access-2'))
      // The host's own work failed, but the switch committed and the
      // contract holds: the callback fired exactly once with the new
      // tenant, nothing rendered an error (a throwing host callback is
      // not a switch failure), and the throw was contained instead of
      // becoming an unhandled rejection of the fire-and-forget promise.
      expect(onSwitched).toHaveBeenCalledTimes(1)
      expect(onSwitched).toHaveBeenCalledWith('tenant-2')
      expect(screen.queryByRole('alert')).not.toBeInTheDocument()
      expect(screen.queryByRole('status')).not.toBeInTheDocument()
      await waitFor(() =>
        expect(
          screen.getByRole('button', { name: 'Sunshine Dental' }),
        ).toBeEnabled(),
      )
      // unhandledrejection dispatches after the microtask queue drains;
      // yield once, then assert none ever fired.
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 0))
      })
      expect(onUnhandled).not.toHaveBeenCalled()
    } finally {
      window.removeEventListener('unhandledrejection', listener)
    }
  })

  it('render a membership-refusal answer in one alert and change nothing locally', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () => {
        throw apiError(403, 'authn.tenant_membership_required')
      },
    })
    await signIn(harness)
    const { onSwitched } = renderSwitcher(harness)
    const user = userEvent.setup()
    const { other } = await openMenu(user)
    await user.click(other)
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.tenant_membership_required,
      ),
    )
    // The auth-core failure contract: the rejection changed nothing, so
    // the local session still holds its token and the trigger retries.
    expect(harness.store.get()).toBe('access-1')
    expect(harness.calls).toHaveLength(2)
    expect(onSwitched).not.toHaveBeenCalled()
    expect(
      screen.getByRole('button', { name: 'Sunshine Dental' }),
    ).toBeEnabled()
  })

  it('retry a refused switch: the second attempt clears the alert and commits', async () => {
    let attempts = 0
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () => {
        attempts += 1
        if (attempts === 1) {
          throw apiError(403, 'authn.tenant_membership_required')
        }
        return makePair({
          access_token: 'access-2',
          principal: { user_id: 'user-1', tenant_id: 'tenant-2', session_id: 'session-1' },
        })
      },
    })
    await signIn(harness)
    const { onSwitched } = renderSwitcher(harness)
    const user = userEvent.setup()
    const { other } = await openMenu(user)
    await user.click(other)
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.tenant_membership_required,
      ),
    )
    expect(harness.store.get()).toBe('access-1')
    // Retry: reopen the list and pick the same row again.
    const rows = await openMenu(user)
    await user.click(rows.other)
    await waitFor(() => expect(harness.store.get()).toBe('access-2'))
    expect(harness.calls).toHaveLength(3)
    expect(onSwitched).toHaveBeenCalledTimes(1)
    expect(onSwitched).toHaveBeenCalledWith('tenant-2')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('render a session-lifecycle answer with its own code text', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () => {
        throw apiError(401, 'authn.token_expired')
      },
    })
    await signIn(harness)
    renderSwitcher(harness)
    const user = userEvent.setup()
    const { other } = await openMenu(user)
    await user.click(other)
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.token_expired,
      ),
    )
  })

  it('render the unknown fallback for a code outside the whitelist', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () => {
        throw apiError(500, 'authn.internal_error')
      },
    })
    await signIn(harness)
    renderSwitcher(harness)
    const user = userEvent.setup()
    const { other } = await openMenu(user)
    await user.click(other)
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(zhCN.errors.unknown),
    )
  })

  it('re-render a failure text in the switched language', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () => {
        throw apiError(403, 'authn.tenant_membership_required')
      },
    })
    await signIn(harness)
    const { i18n } = renderSwitcher(harness)
    const user = userEvent.setup()
    const { other } = await openMenu(user)
    await user.click(other)
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.tenant_membership_required,
      ),
    )
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    expect(screen.getByRole('alert')).toHaveTextContent(
      enUS.errors.authn.tenant_membership_required,
    )
    expect(
      screen.getByRole('button', { name: 'Sunshine Dental' }),
    ).toBeInTheDocument()
  })

  it('render the en-US bundle on an English-starting instance', async () => {
    let resolveSwitch: (value: unknown) => void = () => {}
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SWITCH_TENANT]: () =>
        new Promise((resolve) => {
          resolveSwitch = resolve
        }),
    })
    await signIn(harness)
    renderWithProviders(
      <TenantSwitcher
        session={harness.session}
        tenants={TENANTS}
        currentTenantId="tenant-1"
      />,
      { language: 'en-US' },
    )
    const user = userEvent.setup()
    const { other } = await openMenu(user)
    await user.click(other)
    expect(screen.getByRole('status')).toHaveTextContent(enUS.tenantSwitcher.switching)
    await act(async () => {
      resolveSwitch(
        makePair({
          access_token: 'access-2',
          principal: { user_id: 'user-1', tenant_id: 'tenant-2', session_id: 'session-1' },
        }),
      )
    })
    await waitFor(() =>
      expect(screen.queryByRole('status')).not.toBeInTheDocument(),
    )
  })

  it('fail closed with a disabled trigger and no list when there is no current tenant', async () => {
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    await signIn(harness)
    renderSwitcher(harness, { currentTenantId: null })
    const trigger = screen.getByRole('button', { name: NO_CURRENT_ZH })
    expect(trigger).toBeDisabled()
    expect(trigger).toHaveAttribute('aria-haspopup', 'menu')
    // fireEvent bypasses the pointer-events check user-event enforces on
    // disabled controls -- a disabled trigger is the inertness guard
    // under test, so the click must reach it synthetically.
    fireEvent.click(trigger)
    // Nothing opened, nothing switched.
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    expect(harness.calls).toHaveLength(1)
    expect(harness.store.get()).toBe('access-1')
  })

  it('render the noCurrentTenant text on an English-starting instance', async () => {
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    await signIn(harness)
    renderWithProviders(
      <TenantSwitcher
        session={harness.session}
        tenants={TENANTS}
        currentTenantId={null}
      />,
      { language: 'en-US' },
    )
    expect(
      screen.getByRole('button', { name: NO_CURRENT_EN }),
    ).toBeDisabled()
  })

  it('pass axe with no violations over the open list', async () => {
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    await signIn(harness)
    renderSwitcher(harness)
    const user = userEvent.setup()
    await openMenu(user)
    await expectNoAxeViolations()
  })

  describe('two instances racing a switch on one shared session', () => {
    // Regression for web-auth-api.md P2-1 plus the P1-1 supersession
    // reconciliation: this component's own switching-ref guard only
    // refuses a second concurrent call THROUGH ITSELF -- it says nothing
    // about a second TenantSwitcher instance mounted elsewhere (host
    // chrome plus a mobile drawer copy, a transient double-mount during
    // a route transition) calling switchTenant on the same shared
    // session. auth-core's settleIssued makes the losing call reject
    // with OperationSupersededError -- the loser must not fire its own
    // onSwitched for a tenant the session is not running under. But the
    // loser's request DID go through server-side, and the server
    // session's stored current tenant is whichever request wrote it
    // last -- normally the loser's, since the browser's requests arrive
    // in order -- so a quiet swallow leaves the session able to drift
    // into the loser's tenant on the next silent refresh (the refresh
    // mints for the server-stored current tenant) with no onSwitched to
    // move the host's caches. The superseded switch therefore
    // reconciles by re-issuing its own request (bounded; see the file
    // header): the tenant the server actually committed lands as a
    // real, announced commit and no silent drift can follow.
    const TENANTS_3 = [
      { id: 'tenant-1', name: 'Sunshine Dental' },
      { id: 'tenant-2', name: 'Bright Smile Clinic' },
      { id: 'tenant-3', name: 'Third Practice' },
    ] as const

    /** Mounts two instances over one session, both on tenant-1. */
    function renderRacingSwitchers(
      harness: Harness,
      onSwitchedA: (tenantId: string) => void,
      onSwitchedB: (tenantId: string) => void,
    ) {
      renderWithProviders(
        <>
          <TenantSwitcher
            session={harness.session}
            tenants={TENANTS_3}
            currentTenantId="tenant-1"
            onSwitched={onSwitchedA}
          />
          <TenantSwitcher
            session={harness.session}
            tenants={TENANTS_3}
            currentTenantId="tenant-1"
            onSwitched={onSwitchedB}
          />
        </>,
      )
    }

    /** Instance A picks tenant-2 and instance B picks tenant-3, both
     * in flight before either commits (the gates are unreleased). */
    async function startRace(
      harness: Harness,
      user: ReturnType<typeof userEvent.setup>,
    ): Promise<void> {
      const triggers = screen.getAllByRole('button', {
        name: 'Sunshine Dental',
      })
      expect(triggers).toHaveLength(2)
      const [triggerA, triggerB] = triggers as [HTMLElement, HTMLElement]
      await user.click(triggerA)
      await user.click(
        await screen.findByRole('menuitem', { name: 'Bright Smile Clinic' }),
      )
      await user.click(triggerB)
      await user.click(
        await screen.findByRole('menuitem', { name: 'Third Practice' }),
      )
      expect(harness.calls).toHaveLength(3) // login + two switch calls
    }

    /** A racing harness: tenant-2 and tenant-3 switches answer from
     * per-tenant gates, and a refresh mints for the tenant the server
     * session last committed to. */
    function makeRacingHarness() {
      let releaseTenant2!: (value: unknown) => void
      let releaseTenant3!: (value: unknown) => void
      const tenant2Gate = new Promise((resolve) => {
        releaseTenant2 = resolve
      })
      const tenant3Gate = new Promise((resolve) => {
        releaseTenant3 = resolve
      })
      const harness = makeHarness({
        [LOGIN_PASSWORD]: () => makePair(),
        [SWITCH_TENANT]: (call) => {
          const body = call.options?.body as { tenant_id?: string }
          return body?.tenant_id === 'tenant-2' ? tenant2Gate : tenant3Gate
        },
        // The refresh endpoint mints for the server-stored current
        // tenant -- the last switch request the server processed. In the
        // race below that is tenant-3 (instance B's request), so a
        // refresh after the race is the drift probe: a session that did
        // not reconcile would silently flip onto tenant-3 here.
        [REFRESH]: () =>
          makePair({
            access_token: 'access-refreshed',
            principal: {
              user_id: 'user-1',
              tenant_id: 'tenant-3',
              session_id: 'session-1',
            },
          }),
      })
      return { harness, releaseTenant2, releaseTenant3 }
    }

    it('reconcile the superseded switch: every commit fires onSwitched, the loser included', async () => {
      const { harness, releaseTenant2, releaseTenant3 } = makeRacingHarness()
      await signIn(harness)
      const onSwitchedA = vi.fn()
      const onSwitchedB = vi.fn()
      renderRacingSwitchers(harness, onSwitchedA, onSwitchedB)
      const user = userEvent.setup()
      await startRace(harness, user)

      // tenant-2's response arrives first and commits: the session runs
      // tenant-2 and A's host is told, exactly once.
      releaseTenant2(
        makePair({
          access_token: 'access-tenant-2',
          principal: {
            user_id: 'user-1',
            tenant_id: 'tenant-2',
            session_id: 'session-1',
          },
        }),
      )
      await waitFor(() => expect(onSwitchedA).toHaveBeenCalledTimes(1))
      expect(onSwitchedA).toHaveBeenCalledWith('tenant-2')
      expect(harness.store.get()).toBe('access-tenant-2')

      // tenant-3's response answers successfully too, but only after
      // tenant-2 already committed: B lost the generation race. Its own
      // server write is the one the session row now holds, so the
      // superseded call reconciles by re-issuing the switch -- B's
      // onSwitched fires exactly once, for the corrective's commit, and
      // nothing renders an error.
      releaseTenant3(
        makePair({
          access_token: 'access-tenant-3',
          principal: {
            user_id: 'user-1',
            tenant_id: 'tenant-3',
            session_id: 'session-1',
          },
        }),
      )
      await waitFor(() => expect(onSwitchedB).toHaveBeenCalledTimes(1))
      expect(onSwitchedB).toHaveBeenCalledWith('tenant-3')
      expect(harness.calls).toHaveLength(4) // the corrective re-issue
      expect(onSwitchedA).toHaveBeenCalledTimes(1)
      expect(harness.store.get()).toBe('access-tenant-3')
      expect(screen.queryAllByRole('alert')).toHaveLength(0)
      expect(screen.queryAllByRole('status')).toHaveLength(0)
      await waitFor(() => {
        const rows = screen.getAllByRole('button', {
          name: 'Sunshine Dental',
        }) as [HTMLElement, HTMLElement]
        expect(rows[0]).toBeEnabled()
        expect(rows[1]).toBeEnabled()
      })
    })

    it('leave no silent drift: the host observes the outcome actually committed', async () => {
      // The regression for P1-1, in the mechanism's own terms. A
      // superseded switch whose server write landed (the session row's
      // current tenant is the loser's tenant) must not resolve into a
      // later silent refresh flipping the session there behind the
      // host's back: the host's session state must match the server's
      // current tenant THROUGH AN ANNOUNCED COMMIT, with onSwitched
      // invalidating the caches for the outcome actually committed.
      // Fails before the reconciliation fix: the superseded call
      // swallows quietly, the refresh drifts the session onto tenant-3
      // and onSwitchedB never fires.
      const { harness, releaseTenant2, releaseTenant3 } = makeRacingHarness()
      await signIn(harness)
      const onSwitchedA = vi.fn()
      const onSwitchedB = vi.fn()
      renderRacingSwitchers(harness, onSwitchedA, onSwitchedB)
      const user = userEvent.setup()
      await startRace(harness, user)

      releaseTenant2(
        makePair({
          access_token: 'access-tenant-2',
          principal: {
            user_id: 'user-1',
            tenant_id: 'tenant-2',
            session_id: 'session-1',
          },
        }),
      )
      await waitFor(() => expect(onSwitchedA).toHaveBeenCalledTimes(1))

      releaseTenant3(
        makePair({
          access_token: 'access-tenant-3',
          principal: {
            user_id: 'user-1',
            tenant_id: 'tenant-3',
            session_id: 'session-1',
          },
        }),
      )
      // Flush the superseded rejection and whatever reconciliation it
      // triggers (the corrective re-issue commits here when the fix is
      // in place) before probing the refresh path.
      await act(async () => {})
      // A silent refresh mints for the server-stored current tenant --
      // tenant-3, the write that landed last. The session the host
      // observes must be tenant-3 either way...
      let refreshed = false
      await act(async () => {
        refreshed = await harness.session.refresh()
      })
      expect(refreshed).toBe(true)
      expect(harness.session.getSnapshot().principal?.tenant_id).toBe(
        'tenant-3',
      )
      // ...but it must have got there through an announced commit: the
      // host caches invalidate on the outcome actually committed, never
      // through a silent drift that no onSwitched ever reported.
      expect(onSwitchedB).toHaveBeenCalledTimes(1)
      expect(onSwitchedB).toHaveBeenCalledWith('tenant-3')
      expect(onSwitchedA).toHaveBeenCalledTimes(1)
      expect(harness.store.get()).toBe('access-refreshed')
      expect(screen.queryAllByRole('alert')).toHaveLength(0)
    })
  })
})
