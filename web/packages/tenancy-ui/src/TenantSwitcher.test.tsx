/**
 * TenantSwitcher behaviour: the trigger shows the host-supplied current
 * tenant and opens the host-supplied list; picking a row that is not the
 * current tenant drives exactly one switch round-trip through the
 * bindRequestFn harness (asserted on method, path and body) and commits
 * the fresh token; the current-tenant row is disabled and can never
 * re-trigger a switch. While a switch is in flight the trigger is
 * disabled and a role="status" notice renders the switching text; a
 * successful switch is quiet (no alert) and fires onSwitched exactly
 * once, after the commit. A rejected switch renders the answer's code
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

  it('disable the trigger with a status notice while the switch is in flight', async () => {
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
    expect(screen.getByRole('button', { name: 'Sunshine Dental' })).toBeDisabled()
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
})
