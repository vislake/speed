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
 * regression test pins against) -- and one role="status" notice names
 * the destination (switchingTo) and then, once the switch commits, the
 * landed tenant (switchedTo): the same live region announces the flight
 * and confirms the commit, because a context change that silently
 * alters which rows a host shows must say so out loud. A synchronous
 * entry guard in the switch handler refuses any second attempt in that
 * window; jsdom cannot stage such an attempt -- the closing list
 * unmounts synchronously here instead of animating out, so no
 * activation can reach the handler mid-flight, and the guard's
 * browser-tier window (rows of the still-closing list, mounted and
 * clickable for the exit transition) is the reference-app Playwright
 * journeys' to pin. A successful switch is announced (no alert --
 * nothing failed -- but a role=status confirmation) and fires
 * onSwitched exactly once per committed switch, after the commit --
 * never for a switch that loses a commit race, which stays lost
 * (the racing describe below); a throwing onSwitched is
 * contained: the commit stands and still announces itself, no error
 * renders, and no rejection escapes the fire-and-forget row handler. A
 * rejected switch clears the notice, renders the answer's code
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
import { createAppTheme } from '@speed/ui-kit'
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

const NO_CURRENT_ZH = zhCN.tenantSwitcher.noCurrentTenant
const NO_CURRENT_EN = enUS.tenantSwitcher.noCurrentTenant

/** The zh switchingTo text with one tenant name interpolated. */
function SWITCHING_TO_ZH(tenant: string): string {
  return zhCN.tenantSwitcher.switchingTo.replace('{{tenant}}', tenant)
}

/** The zh switchedTo text with one tenant name interpolated. */
function SWITCHED_TO_ZH(tenant: string): string {
  return zhCN.tenantSwitcher.switchedTo.replace('{{tenant}}', tenant)
}

/** The en switchedTo text with one tenant name interpolated. */
function SWITCHED_TO_EN(tenant: string): string {
  return enUS.tenantSwitcher.switchedTo.replace('{{tenant}}', tenant)
}

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

/** The WCAG 2.1 contrast ratio between two hex colours. */
function contrastRatio(a: string, b: string): number {
  const channel = (value: number): number => {
    const c = value / 255
    return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4)
  }
  const luminance = (hex: string): number => {
    const value = hex.replace('#', '')
    const r = Number.parseInt(value.slice(0, 2), 16)
    const g = Number.parseInt(value.slice(2, 4), 16)
    const b = Number.parseInt(value.slice(4, 6), 16)
    return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b)
  }
  const lighter = Math.max(luminance(a), luminance(b))
  const darker = Math.min(luminance(a), luminance(b))
  return (lighter + 0.05) / (darker + 0.05)
}

describe('TenantSwitcher', () => {
  it('default the trigger to an inheriting color, never the primary palette color', async () => {
    // Regression for the acceptance measurement that named the trigger
    // at 1:1: a trigger defaulting to the primary palette color
    // vanishes on the very surface it usually sits on -- an AppBar
    // whose background IS the primary color (the reference-app header
    // measured rgb(37,99,235) text on an rgb(37,99,235) background).
    // color="inherit" makes the text follow the ambient color: the
    // AppBar's own contrastText there, the surrounding text color on a
    // plain surface.
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    await signIn(harness)
    renderSwitcher(harness)
    const trigger = screen.getByRole('button', { name: 'Sunshine Dental' })
    expect(trigger).toHaveClass('MuiButton-colorInherit')
    expect(trigger).not.toHaveClass('MuiButton-colorPrimary')
    // The token relationship the inheritance relies on, guarded here
    // because jsdom resolves no cascaded styles: while an AppBar keeps
    // painting primary.main, its own contrastText must stay at WCAG AA
    // distance from it, or an inherited control is legible nowhere.
    // The composed real-browser proof is the reference-app
    // visible-controls e2e gate.
    const { theme } = createAppTheme()
    const ratio = contrastRatio(
      theme.palette.primary.main,
      theme.palette.primary.contrastText,
    )
    expect(ratio).toBeGreaterThanOrEqual(4.5)
    expect(theme.palette.primary.main).not.toBe(theme.palette.primary.contrastText)
  })

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
    // The switch announces itself: no alert (nothing failed), but the
    // role=status confirmation names the tenant the session now runs
    // under -- a context change that silently alters which rows a host
    // shows must say so out loud (the reference-app acceptance gate for
    // "switching clinic says so"). The list closed, the trigger is back
    // to its idle label (host data, unchanged by the switch) and
    // enabled.
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    const status = screen.getByRole('status')
    expect(status).toHaveTextContent(SWITCHED_TO_ZH('Bright Smile Clinic'))
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
    // The notice names the destination, so the in-flight announcement
    // is not a promise without an object.
    const status = screen.getByRole('status')
    expect(status).toHaveTextContent(SWITCHING_TO_ZH('Bright Smile Clinic'))
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
    // The same live region now confirms the commit: the switching
    // announcement must not be the last thing a screen reader user
    // hears about a context change that already happened.
    expect(status).toHaveTextContent(SWITCHED_TO_ZH('Bright Smile Clinic'))
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
      // not a switch failure -- the commit still announces itself), and
      // the throw was contained instead of becoming an unhandled
      // rejection of the fire-and-forget promise.
      expect(onSwitched).toHaveBeenCalledTimes(1)
      expect(onSwitched).toHaveBeenCalledWith('tenant-2')
      expect(screen.queryByRole('alert')).not.toBeInTheDocument()
      expect(screen.getByRole('status')).toHaveTextContent(
        SWITCHED_TO_ZH('Bright Smile Clinic'),
      )
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
    expect(screen.getByRole('status')).toHaveTextContent(
      enUS.tenantSwitcher.switchingTo.replace(
        '{{tenant}}',
        'Bright Smile Clinic',
      ),
    )
    await act(async () => {
      resolveSwitch(
        makePair({
          access_token: 'access-2',
          principal: { user_id: 'user-1', tenant_id: 'tenant-2', session_id: 'session-1' },
        }),
      )
    })
    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        SWITCHED_TO_EN('Bright Smile Clinic'),
      ),
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
    // Regression for web-auth-api.md P2-1: this component's own
    // switching-ref guard only refuses a second concurrent call
    // THROUGH ITSELF -- it says nothing about a second TenantSwitcher
    // instance mounted elsewhere (host chrome plus a mobile drawer
    // copy, a transient double-mount during a route transition) calling
    // switchTenant on the same shared session. auth-core's settleIssued
    // rejects whichever request answers after a sibling committed with
    // OperationSupersededError -- and supersession is decided by
    // RESPONSE settlement order, never send order, so either racing
    // request can be the loser. The loser's own request DID go through
    // server-side, but it cannot know whose write the session row keeps
    // (see the file header), so a superseded switch stays LOST: no
    // error, no re-issue, no onSwitched -- the winning operation's own
    // commit fired its own callback exactly once, for the tenant the
    // session genuinely runs under, and the controlled component
    // converges to that same tenant through the host's own
    // currentTenantId. The three tests below pin both settlement
    // orders and the drift-probe residual the lost-race rule records.
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
        // refresh after the race is the drift probe: it converges the
        // session onto tenant-3, the convergence the lost-race rule's
        // recorded residual leaves to the session's own refresh path.
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

    it('keep a superseded switch lost: the winner reports its own commit, the loser contributes nothing', async () => {
      // Settlement order mirrors send order: instance A's tenant-2
      // request settles first and commits; instance B's tenant-3
      // request settles second and is superseded. B's own server write
      // may be the one the session row now holds -- but a superseded
      // request cannot know that (see the file header), so it stays
      // lost: no corrective re-issue, no onSwitched, no error. The
      // winner's commit is the only report, and the session runs the
      // tenant it committed. Fails before the lost-race correction:
      // B's re-issue fires onSwitchedB('tenant-3') on a fourth call.
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
      // tenant-2 already committed: B lost the generation race and
      // stays lost -- the losing request is never re-issued, fires no
      // onSwitched and renders no error.
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
      // Flush the superseded rejection and the lost-race exit.
      await act(async () => {})
      expect(onSwitchedB).not.toHaveBeenCalled()
      expect(harness.calls).toHaveLength(3) // login + the two racing requests
      expect(harness.store.get()).toBe('access-tenant-2')
      expect(harness.session.getSnapshot().principal?.tenant_id).toBe(
        'tenant-2',
      )
      expect(screen.queryAllByRole('alert')).toHaveLength(0)
      // Only the winner's commit announced itself; the loser's
      // in-flight notice cleared with the lost race.
      const statusTexts = screen
        .queryAllByRole('status')
        .map((status) => status.textContent ?? '')
      expect(statusTexts).toEqual([SWITCHED_TO_ZH('Bright Smile Clinic')])
      await waitFor(() => {
        const rows = screen.getAllByRole('button', {
          name: 'Sunshine Dental',
        }) as [HTMLElement, HTMLElement]
        expect(rows[0]).toBeEnabled()
        expect(rows[1]).toBeEnabled()
      })
    })

    it('never re-commit a superseded request: the earlier-sent switch settling last stays lost', async () => {
      // The D2 drift shape, settlement order inverted: instance A
      // issues its tenant-2 request first and instance B its tenant-3
      // request second, but B's response settles FIRST and commits. A's
      // response settles second, so A -- the EARLIER-sent request, the
      // tenant the user already left behind -- is the superseded one.
      // The pre-correction reconciliation re-issued A here, actively
      // switching the session back onto the abandoned tenant; a
      // superseded request must stay lost and the session must settle
      // on B, the tenant the user actually settled on. Fails before:
      // the corrective re-issue commits access-2 on a fourth call and
      // fires onSwitchedA('tenant-2').
      const { harness, releaseTenant2, releaseTenant3 } = makeRacingHarness()
      await signIn(harness)
      const onSwitchedA = vi.fn()
      const onSwitchedB = vi.fn()
      renderRacingSwitchers(harness, onSwitchedA, onSwitchedB)
      const user = userEvent.setup()
      await startRace(harness, user)

      // tenant-3's response settles first and commits: the session runs
      // tenant-3 and B's host is told, exactly once.
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
      expect(harness.store.get()).toBe('access-tenant-3')

      // tenant-2's response settles afterwards: A is superseded -- even
      // though it was sent first -- and stays lost.
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
      await act(async () => {})
      expect(onSwitchedA).not.toHaveBeenCalled()
      expect(harness.calls).toHaveLength(3) // login + the two racing requests
      expect(harness.store.get()).toBe('access-tenant-3')
      expect(harness.session.getSnapshot().principal?.tenant_id).toBe(
        'tenant-3',
      )
      expect(screen.queryAllByRole('alert')).toHaveLength(0)
      // Only B's commit announced itself; A's in-flight notice cleared
      // with the lost race, so nothing on A's instance is left pending.
      const statusTexts = screen
        .queryAllByRole('status')
        .map((status) => status.textContent ?? '')
      expect(statusTexts).toEqual([SWITCHED_TO_ZH('Third Practice')])
      await waitFor(() => {
        const rows = screen.getAllByRole('button', {
          name: 'Sunshine Dental',
        }) as [HTMLElement, HTMLElement]
        expect(rows[0]).toBeEnabled()
        expect(rows[1]).toBeEnabled()
      })
    })

    it('leave the superseded switch lost across the refresh probe: only the session own refresh moves the principal', async () => {
      // The drift-probe descendant of the pre-correction
      // no-silent-drift regression, recording the lost-race rule's
      // residual. When responses settle in send order (the race below:
      // A first, B second), the superseded request is the later-sent
      // one, so its server write is the last one the session row keeps
      // -- the refresh mints for it. A superseded request can never
      // know that (settlement order is not send order), so it stays
      // lost even here; the session converges to the server row only
      // through its own refresh path, announced by no onSwitched. The
      // recorded alternative -- re-issuing the lost request -- is the
      // active wrong switch the regression above pins when settlement
      // order inverts. Fails before: the corrective re-issue fires
      // onSwitchedB('tenant-3') on a fourth call before the refresh.
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
      expect(onSwitchedA).toHaveBeenCalledWith('tenant-2')

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
      // Flush the superseded rejection and the lost-race exit before
      // probing the refresh path.
      await act(async () => {})
      expect(onSwitchedB).not.toHaveBeenCalled()

      // A silent refresh mints for the server-stored current tenant --
      // tenant-3, the write that landed last. The session converges
      // there through the refresh's own commit; the lost instance
      // contributes nothing along the way.
      let refreshed = false
      await act(async () => {
        refreshed = await harness.session.refresh()
      })
      expect(refreshed).toBe(true)
      expect(harness.session.getSnapshot().principal?.tenant_id).toBe(
        'tenant-3',
      )
      expect(harness.store.get()).toBe('access-refreshed')
      expect(onSwitchedA).toHaveBeenCalledTimes(1)
      expect(onSwitchedB).not.toHaveBeenCalled()
      expect(harness.calls).toHaveLength(4) // login + two switches + the refresh
      expect(screen.queryAllByRole('alert')).toHaveLength(0)
    })
  })
})
