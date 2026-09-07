/**
 * SignOutButton behaviour: a click drives one real logout round-trip
 * through the bindRequestFn harness (asserted on method, path and
 * bodyless request) and clears the session token; while the logout is in
 * flight the button is disabled and shows the busy label. A rejected
 * logout renders the answer's code text in one alert and changes nothing
 * locally -- the store keeps its token and the button stays ready to
 * retry, and a retry after a transport failure clears the alert and
 * succeeds. The session-lifecycle codes resolve to their own texts in
 * the current language (asserted zh, then en after a language switch);
 * an unlisted code renders the unknown fallback. Text expectations read
 * the bundle values, never inline language.
 */

import { describe, expect, it } from 'vitest'
import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { switchLanguage } from '@speed/i18n'
import { createAppTheme } from '@speed/ui-kit'
import { SignOutButton } from './SignOutButton.js'
import { renderWithProviders } from '../test-utils/render.js'
import {
  LOGIN_PASSWORD,
  LOGOUT,
  apiError,
  makeHarness,
  makePair,
  type Harness,
} from '../test-utils/session-harness.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

const LABEL_ZH = zhCN.signOut.label
const BUSY_ZH = zhCN.signOut.busy
const LABEL_EN = enUS.signOut.label

/** Signs the harness session in through the real login operation. */
async function signIn(harness: Harness): Promise<void> {
  await harness.session.loginWithPassword({
    identifier: 'alice@example.com',
    password: 's3cret-pass',
  })
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

describe('SignOutButton', () => {
  it('default the button to an inheriting color, never the primary palette color', async () => {
    // Regression for the acceptance measurement that named the sign-out
    // control at 1:1: a button defaulting to the primary palette color
    // vanishes on the very surface it usually sits on -- an AppBar
    // whose background IS the primary color (the reference-app header
    // measured rgb(37,99,235) text on an rgb(37,99,235) background).
    // color="inherit" makes the text follow the ambient color: the
    // AppBar's own contrastText there, the surrounding text color on a
    // plain surface.
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    await signIn(harness)
    renderWithProviders(<SignOutButton session={harness.session} />)
    const button = screen.getByRole('button', { name: LABEL_ZH })
    expect(button).toHaveClass('MuiButton-colorInherit')
    expect(button).not.toHaveClass('MuiButton-colorPrimary')
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

  it('sign out on click, clearing the session token', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => undefined,
    })
    await signIn(harness)
    expect(harness.store.get()).toBe('access-1')
    renderWithProviders(<SignOutButton session={harness.session} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: LABEL_ZH }))
    await waitFor(() => expect(harness.store.get()).toBeNull())
    expect(harness.calls).toHaveLength(2)
    expect(harness.calls[1]?.method).toBe('POST')
    expect(harness.calls[1]?.path).toBe('/api/v1/authn/logout')
    expect(harness.calls[1]?.options?.body).toBeUndefined()
    // Success is the host's to observe; the button itself returns to
    // its idle label, ready for the next (idempotent) click.
    await waitFor(() =>
      expect(screen.getByRole('button', { name: LABEL_ZH })).toBeEnabled(),
    )
  })

  it('disable the button with the busy label while the logout is in flight', async () => {
    let resolveLogout: (value: unknown) => void = () => {}
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () =>
        new Promise((resolve) => {
          resolveLogout = resolve
        }),
    })
    await signIn(harness)
    renderWithProviders(<SignOutButton session={harness.session} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: LABEL_ZH }))
    const busy = await screen.findByRole('button', { name: BUSY_ZH })
    expect(busy).toBeDisabled()
    expect(harness.calls).toHaveLength(2)
    await act(async () => {
      resolveLogout(undefined)
    })
    await waitFor(() =>
      expect(screen.getByRole('button', { name: LABEL_ZH })).toBeEnabled(),
    )
  })

  it('render a revoked-session answer in one alert and change nothing locally', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => {
        throw apiError(401, 'authn.session_revoked')
      },
    })
    await signIn(harness)
    renderWithProviders(<SignOutButton session={harness.session} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: LABEL_ZH }))
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.session_revoked,
      ),
    )
    // The auth-core failure contract: the rejection changed nothing, so
    // the local session still holds its token and the button retries.
    expect(harness.store.get()).toBe('access-1')
    expect(harness.calls).toHaveLength(2)
    expect(screen.getByRole('button', { name: LABEL_ZH })).toBeEnabled()
  })

  it('retry a failed logout: the second attempt clears the alert and succeeds', async () => {
    let attempts = 0
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => {
        attempts += 1
        if (attempts === 1) {
          throw apiError(0, 'client.network')
        }
        return undefined
      },
    })
    await signIn(harness)
    renderWithProviders(<SignOutButton session={harness.session} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: LABEL_ZH }))
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.client.network,
      ),
    )
    expect(harness.store.get()).toBe('access-1')
    await user.click(screen.getByRole('button', { name: LABEL_ZH }))
    await waitFor(() => expect(harness.store.get()).toBeNull())
    expect(harness.calls).toHaveLength(3)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('re-render a session-code failure text in the switched language', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => {
        throw apiError(401, 'authn.token_expired')
      },
    })
    await signIn(harness)
    const { i18n } = renderWithProviders(
      <SignOutButton session={harness.session} />,
    )
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: LABEL_ZH }))
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.token_expired,
      ),
    )
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    expect(screen.getByRole('alert')).toHaveTextContent(
      enUS.errors.authn.token_expired,
    )
    expect(
      screen.getByRole('button', { name: LABEL_EN }),
    ).toBeInTheDocument()
  })

  it('render the en-US bundle on an English-starting instance', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => undefined,
    })
    await signIn(harness)
    renderWithProviders(<SignOutButton session={harness.session} />, {
      language: 'en-US',
    })
    expect(
      screen.getByRole('button', { name: LABEL_EN }),
    ).toBeInTheDocument()
  })

  it('render the unknown fallback for a code outside the whitelist', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => {
        throw apiError(500, 'authn.internal_error')
      },
    })
    await signIn(harness)
    renderWithProviders(<SignOutButton session={harness.session} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: LABEL_ZH }))
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(zhCN.errors.unknown),
    )
  })

  it('pass axe with no violations after a failed logout', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [LOGOUT]: () => {
        throw apiError(404, 'authn.session_not_found')
      },
    })
    await signIn(harness)
    renderWithProviders(<SignOutButton session={harness.session} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: LABEL_ZH }))
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.session_not_found,
      ),
    )
    await expectNoAxeViolations()
  })
})
