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

describe('SignOutButton', () => {
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
