/**
 * SocialSignInSection behaviour: each configured provider renders its
 * bundle name and, on click, asks the session for that channel's
 * authorization URL (asserted on method, path and query -- the redirect
 * URI travels as a query parameter, never in the path) and reports it
 * upward through onAuthorizeUrl; the section itself never navigates. A
 * provider the server does not know renders its code text in one alert
 * and reports nothing; while one request is in flight its button
 * disables and the others stay live. The en-US bundle renders on an
 * English-starting instance and the tree passes axe. Text expectations
 * read the bundle values, never inline language.
 */

import { describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SocialSignInSection } from './SocialSignInSection.js'
import type { SocialProviderConfig } from './SocialSignInSection.js'
import { renderWithProviders } from '../test-utils/render.js'
import {
  SOCIAL_AUTHORIZE,
  apiError,
  makeHarness,
} from '../test-utils/session-harness.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

const CALLBACK_URI = 'https://app.example.com/auth/social/google/callback'

const PROVIDERS: readonly SocialProviderConfig[] = [
  { provider: 'google', redirectUri: CALLBACK_URI },
  { provider: 'github', redirectUri: CALLBACK_URI },
]

const GOOGLE_AUTH_URL = 'https://accounts.google.com/o/oauth2/v2/auth?client_id=c1'

describe('SocialSignInSection', () => {
  it('build one authorization URL per clicked provider and report it upward', async () => {
    const harness = makeHarness({
      [SOCIAL_AUTHORIZE]: () => ({ authorize_url: GOOGLE_AUTH_URL }),
      'GET /api/v1/authn/social/github/authorize': () => ({
        authorize_url: 'https://github.com/login/oauth/authorize',
      }),
    })
    const onAuthorizeUrl = vi.fn()
    renderWithProviders(
      <SocialSignInSection
        session={harness.session}
        providers={PROVIDERS}
        onAuthorizeUrl={onAuthorizeUrl}
      />,
    )
    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: zhCN.social.provider.google }),
    )
    await waitFor(() => expect(onAuthorizeUrl).toHaveBeenCalledTimes(1))
    expect(onAuthorizeUrl).toHaveBeenCalledWith('google', GOOGLE_AUTH_URL)
    await user.click(
      screen.getByRole('button', { name: zhCN.social.provider.github }),
    )
    await waitFor(() => expect(onAuthorizeUrl).toHaveBeenCalledTimes(2))
    expect(onAuthorizeUrl).toHaveBeenLastCalledWith(
      'github',
      'https://github.com/login/oauth/authorize',
    )
    expect(harness.calls).toHaveLength(2)
    expect(harness.calls[0]?.method).toBe('GET')
    expect(harness.calls[0]?.path).toBe('/api/v1/authn/social/google/authorize')
    // The redirect URI travels as the query parameter, never in the path.
    expect(harness.calls[0]?.options?.query).toEqual({
      redirect_uri: CALLBACK_URI,
    })
    expect(harness.calls[1]?.path).toBe('/api/v1/authn/social/github/authorize')
    expect(harness.calls[1]?.options?.query).toEqual({
      redirect_uri: CALLBACK_URI,
    })
  })

  it('render an unknown-provider answer in one alert and report nothing', async () => {
    const harness = makeHarness({
      [SOCIAL_AUTHORIZE]: () => {
        throw apiError(404, 'authn.provider_unknown')
      },
    })
    const onAuthorizeUrl = vi.fn()
    renderWithProviders(
      <SocialSignInSection
        session={harness.session}
        providers={PROVIDERS}
        onAuthorizeUrl={onAuthorizeUrl}
      />,
    )
    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: zhCN.social.provider.google }),
    )
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.provider_unknown,
      ),
    )
    expect(onAuthorizeUrl).not.toHaveBeenCalled()
    expect(harness.calls).toHaveLength(1)
  })

  it('disable only the clicked provider button while its URL is in flight', async () => {
    let resolveAuthorize: (value: unknown) => void = () => {}
    const harness = makeHarness({
      [SOCIAL_AUTHORIZE]: () =>
        new Promise((resolve) => {
          resolveAuthorize = resolve
        }),
    })
    renderWithProviders(
      <SocialSignInSection session={harness.session} providers={PROVIDERS} />,
    )
    const user = userEvent.setup()
    const google = screen.getByRole('button', {
      name: zhCN.social.provider.google,
    })
    const github = screen.getByRole('button', {
      name: zhCN.social.provider.github,
    })
    await user.click(google)
    await waitFor(() => expect(google).toBeDisabled())
    expect(github).toBeEnabled()
    resolveAuthorize({ authorize_url: GOOGLE_AUTH_URL })
    await waitFor(() => expect(google).toBeEnabled())
  })

  it('render the section title and the en-US provider names on an English-starting instance', async () => {
    const harness = makeHarness({
      [SOCIAL_AUTHORIZE]: () => ({ authorize_url: GOOGLE_AUTH_URL }),
    })
    renderWithProviders(
      <SocialSignInSection session={harness.session} providers={PROVIDERS} />,
      { language: 'en-US' },
    )
    expect(screen.getByText(enUS.social.title)).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: enUS.social.provider.google }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: enUS.social.provider.github }),
    ).toBeInTheDocument()
  })

  it('pass axe with no violations', async () => {
    const harness = makeHarness({
      [SOCIAL_AUTHORIZE]: () => ({ authorize_url: GOOGLE_AUTH_URL }),
    })
    renderWithProviders(
      <SocialSignInSection session={harness.session} providers={PROVIDERS} />,
    )
    await expectNoAxeViolations()
  })
})
