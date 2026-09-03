/**
 * SignInScreen behaviour: the password channel is selected by default
 * and a successful password login fires onSignedIn once; the tab strip
 * switches to the SMS channel whose two-step flow then drives its own
 * requests, and switching channels unmounts the previous form -- its
 * half-typed state is gone on return. The social block renders only when
 * options are given, defaultChannel selects the first channel shown, the
 * en-US bundle renders on an English-starting instance, and the tree
 * passes axe. Text expectations read the bundle values, never inline
 * language.
 */

import { describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SignInScreen } from './SignInScreen.js'
import type { SocialProviderConfig } from './SocialSignInSection.js'
import { renderWithProviders } from '../test-utils/render.js'
import {
  LOGIN_PASSWORD,
  LOGIN_SMS,
  REQUEST_SMS_CODE,
  SOCIAL_AUTHORIZE,
  makeHarness,
  makePair,
} from '../test-utils/session-harness.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

const PHONE = '+8613800138000'

const SOCIAL_PROVIDERS: readonly SocialProviderConfig[] = [
  { provider: 'google', redirectUri: 'https://app.example.com/auth/callback' },
]

describe('SignInScreen', () => {
  it('show the password channel first and fire onSignedIn from its login', async () => {
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    const onSignedIn = vi.fn()
    renderWithProviders(
      <SignInScreen session={harness.session} onSignedIn={onSignedIn} />,
    )
    expect(
      screen.getByRole('tab', { name: zhCN.passwordSignIn.title }),
    ).toHaveAttribute('aria-selected', 'true')
    expect(
      screen.getByLabelText(zhCN.passwordSignIn.identifierLabel),
    ).toBeInTheDocument()
    // Without social options no social divider appears.
    expect(screen.queryByText(zhCN.social.title)).not.toBeInTheDocument()
    const user = userEvent.setup()
    await user.type(
      screen.getByLabelText(zhCN.passwordSignIn.identifierLabel),
      'alice@example.com',
    )
    await user.type(
      screen.getByLabelText(zhCN.passwordSignIn.passwordLabel),
      's3cret-pass',
    )
    await user.click(
      screen.getByRole('button', { name: zhCN.passwordSignIn.submit }),
    )
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(harness.store.get()).toBe('access-1')
  })

  it('switch to the SMS channel, whose flow drives its own requests', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [REQUEST_SMS_CODE]: () => undefined,
      [LOGIN_SMS]: () => makePair(),
    })
    const onSignedIn = vi.fn()
    renderWithProviders(
      <SignInScreen session={harness.session} onSignedIn={onSignedIn} />,
    )
    const user = userEvent.setup()
    await user.click(
      screen.getByRole('tab', { name: zhCN.smsSignIn.title }),
    )
    await user.type(screen.getByLabelText(zhCN.smsSignIn.phoneLabel), PHONE)
    await user.click(
      screen.getByRole('button', { name: zhCN.smsSignIn.sendCode }),
    )
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    await user.type(screen.getByLabelText(zhCN.smsSignIn.codeLabel), '123456')
    await user.click(
      screen.getByRole('button', { name: zhCN.smsSignIn.submit }),
    )
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(harness.calls).toHaveLength(2)
    expect(harness.calls[0]?.path).toBe('/api/v1/authn/login/sms/request')
    expect(harness.calls[1]?.path).toBe('/api/v1/authn/login/sms')
    // The password channel never fired.
    expect(
      harness.calls.every((call) => call.path !== '/api/v1/authn/login/password'),
    ).toBe(true)
  })

  it('reset a half-typed channel when switching away and back', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
    })
    renderWithProviders(<SignInScreen session={harness.session} />)
    const user = userEvent.setup()
    const identifier = screen.getByLabelText(zhCN.passwordSignIn.identifierLabel)
    await user.type(identifier, 'alice@example.com')
    expect(identifier).toHaveValue('alice@example.com')
    await user.click(
      screen.getByRole('tab', { name: zhCN.smsSignIn.title }),
    )
    await user.click(
      screen.getByRole('tab', { name: zhCN.passwordSignIn.title }),
    )
    expect(
      screen.getByLabelText(zhCN.passwordSignIn.identifierLabel),
    ).toHaveValue('')
    expect(harness.calls).toHaveLength(0)
  })

  it('render the social block under the form when options are given', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => makePair(),
      [SOCIAL_AUTHORIZE]: () => ({
        authorize_url: 'https://accounts.google.com/o/oauth2/v2/auth',
      }),
    })
    const onAuthorizeUrl = vi.fn()
    renderWithProviders(
      <SignInScreen
        session={harness.session}
        social={{ providers: SOCIAL_PROVIDERS, onAuthorizeUrl }}
      />,
    )
    expect(screen.getByText(zhCN.social.title)).toBeInTheDocument()
    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: zhCN.social.provider.google }),
    )
    await waitFor(() => expect(onAuthorizeUrl).toHaveBeenCalledTimes(1))
  })

  it('open on the SMS channel when defaultChannel says so', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
    })
    renderWithProviders(
      <SignInScreen session={harness.session} defaultChannel="sms" />,
    )
    expect(
      screen.getByRole('tab', { name: zhCN.smsSignIn.title }),
    ).toHaveAttribute('aria-selected', 'true')
    expect(
      screen.getByLabelText(zhCN.smsSignIn.phoneLabel),
    ).toBeInTheDocument()
    expect(
      screen.queryByLabelText(zhCN.passwordSignIn.identifierLabel),
    ).not.toBeInTheDocument()
  })

  it('render the en-US bundle on an English-starting instance', async () => {
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    renderWithProviders(
      <SignInScreen
        session={harness.session}
        social={{ providers: SOCIAL_PROVIDERS }}
      />,
      { language: 'en-US' },
    )
    expect(
      screen.getByRole('tab', { name: enUS.passwordSignIn.title }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole('tab', { name: enUS.smsSignIn.title }),
    ).toBeInTheDocument()
    expect(
      screen.getByLabelText(enUS.passwordSignIn.identifierLabel),
    ).toBeInTheDocument()
    expect(screen.getByText(enUS.social.title)).toBeInTheDocument()
  })

  it('pass axe with no violations', async () => {
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    renderWithProviders(
      <SignInScreen session={harness.session} social={{ providers: SOCIAL_PROVIDERS }} />,
    )
    await expectNoAxeViolations()
  })
})
