/**
 * SocialCallbackHandler behaviour: the handler starts one exchange per
 * (code, state) pair -- asserted on method, path and body -- shows the
 * pending notice while it is in flight, and fires onSignedIn once on
 * success; StrictMode's double effect invocation starts exactly one
 * exchange. A failed exchange renders its code text in one alert under a
 * retry button that re-runs the same pair, and a changed pair starts a
 * fresh exchange. The en-US bundle renders on an English-starting
 * instance and the pending tree passes axe. Text expectations read the
 * bundle values, never inline language.
 */

import { StrictMode } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SocialCallbackHandler } from './SocialCallbackHandler.js'
import { renderWithProviders } from '../test-utils/render.js'
import {
  SOCIAL_CALLBACK,
  apiError,
  makeHarness,
  makePair,
} from '../test-utils/session-harness.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

const CODE = 'oauth-code-1'
const STATE = 'csrf-state-1'

describe('SocialCallbackHandler', () => {
  it('complete one exchange per pair, then fire onSignedIn once', async () => {
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () => ({ tokens: makePair() }),
    })
    const onSignedIn = vi.fn()
    renderWithProviders(
      <SocialCallbackHandler
        session={harness.session}
        provider="google"
        code={CODE}
        state={STATE}
        onSignedIn={onSignedIn}
      />,
    )
    expect(
      screen.getByText(zhCN.socialCallback.pending),
    ).toBeInTheDocument()
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(harness.calls).toHaveLength(1)
    expect(harness.calls[0]?.method).toBe('POST')
    expect(harness.calls[0]?.path).toBe('/api/v1/authn/social/google/callback')
    expect(harness.calls[0]?.options?.body).toEqual({ code: CODE, state: STATE })
    expect(harness.store.get()).toBe('access-1')
  })

  it('start exactly one exchange under StrictMode double effect invocation', async () => {
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () => ({ tokens: makePair() }),
    })
    const onSignedIn = vi.fn()
    renderWithProviders(
      <StrictMode>
        <SocialCallbackHandler
          session={harness.session}
          provider="google"
          code={CODE}
          state={STATE}
          onSignedIn={onSignedIn}
        />
      </StrictMode>,
    )
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(harness.calls).toHaveLength(1)
    expect(harness.store.get()).toBe('access-1')
  })

  it('render a rejected state answer in one alert with a retry button', async () => {
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () => {
        throw apiError(400, 'authn.oauth_state_invalid')
      },
    })
    renderWithProviders(
      <SocialCallbackHandler
        session={harness.session}
        provider="google"
        code={CODE}
        state={STATE}
      />,
    )
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.oauth_state_invalid,
      ),
    )
    expect(harness.store.get()).toBeNull()
    expect(
      screen.getByRole('button', { name: zhCN.socialCallback.retry }),
    ).toBeInTheDocument()
  })

  it('retry the same pair and succeed on the second exchange', async () => {
    let attempts = 0
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () => {
        attempts += 1
        if (attempts === 1) {
          throw apiError(400, 'authn.oauth_state_invalid')
        }
        return { tokens: makePair() }
      },
    })
    const onSignedIn = vi.fn()
    renderWithProviders(
      <SocialCallbackHandler
        session={harness.session}
        provider="google"
        code={CODE}
        state={STATE}
        onSignedIn={onSignedIn}
      />,
    )
    await waitFor(() =>
      expect(screen.getByRole('alert')).toBeInTheDocument(),
    )
    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: zhCN.socialCallback.retry }),
    )
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(harness.calls).toHaveLength(2)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(harness.store.get()).toBe('access-1')
  })

  it('start a fresh exchange when the pair changes', async () => {
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () => ({ tokens: makePair() }),
    })
    const onSignedIn = vi.fn()
    const { rerender } = renderWithProviders(
      <SocialCallbackHandler
        session={harness.session}
        provider="google"
        code={CODE}
        state={STATE}
        onSignedIn={onSignedIn}
      />,
    )
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    rerender(
      <SocialCallbackHandler
        session={harness.session}
        provider="google"
        code="oauth-code-2"
        state={STATE}
        onSignedIn={onSignedIn}
      />,
    )
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(2))
    expect(harness.calls).toHaveLength(2)
    expect(harness.calls[1]?.options?.body).toEqual({
      code: 'oauth-code-2',
      state: STATE,
    })
  })

  it('render the en-US pending notice on an English-starting instance', async () => {
    let resolveExchange: (value: unknown) => void = () => {}
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () =>
        new Promise((resolve) => {
          resolveExchange = resolve
        }),
    })
    renderWithProviders(
      <SocialCallbackHandler
        session={harness.session}
        provider="google"
        code={CODE}
        state={STATE}
      />,
      { language: 'en-US' },
    )
    expect(
      screen.getByText(enUS.socialCallback.pending),
    ).toBeInTheDocument()
    resolveExchange(makePair())
  })

  it('pass axe with no violations while pending', async () => {
    let resolveExchange: (value: unknown) => void = () => {}
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () =>
        new Promise((resolve) => {
          resolveExchange = resolve
        }),
    })
    renderWithProviders(
      <SocialCallbackHandler
        session={harness.session}
        provider="google"
        code={CODE}
        state={STATE}
      />,
    )
    await expectNoAxeViolations()
    resolveExchange(makePair())
  })
})
