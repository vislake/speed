/**
 * SocialCallbackHandler behaviour: the handler starts one exchange per
 * (code, state) pair -- asserted on method, path and body -- shows the
 * pending notice while it is in flight, and fires onSignedIn once on
 * success; StrictMode's double effect invocation starts exactly one
 * exchange. A failed exchange renders its code text in one alert under a
 * retry button that re-runs the same pair, and a changed pair starts a
 * fresh exchange while the session is still anonymous. A mount that
 * finds the session already authenticated starts no exchange and fires
 * onSignedIn again -- the re-entry to the callback URL after a
 * completed exchange. The en-US bundle renders on an English-starting
 * instance and the pending tree passes axe. Text expectations read the
 * bundle values, never inline language.
 */

import { StrictMode } from 'react'
import { flushSync } from 'react-dom'
import { describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SocialCallbackHandler } from './SocialCallbackHandler.js'
import { renderWithProviders } from '../test-utils/render.js'
import {
  LOGIN_PASSWORD,
  SOCIAL_CALLBACK,
  apiError,
  makeHarness,
  makePair,
} from '@speed/test-utils/session-harness'
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

  it('mount the pending live region empty on a retry re-entry, then fill it (mount-with-text regression)', async () => {
    // The pending notice's role="status" region mounts empty on every
    // entry into the pending state -- the initial mount and a retry's
    // re-entry alike -- because a live region announces content changes
    // that follow its own existence, never text that mounts with it.
    // The text is gated behind a one-commit lag armed by an effect
    // after the pending phase's first commit, so the region's first
    // committed frame is empty whatever transition entered the phase,
    // and the text fills an existing region a commit later. The
    // assertion between the two commits below is what distinguishes
    // the shapes: the region's text content must still be empty the
    // moment the node re-appears on the retry. flushSync lands the
    // retry's one commit synchronously, and the act scope holds back
    // the post-commit effect that fills the region until this assertion
    // has seen the birth frame.
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
    // The failed state carries no live region: nothing to announce.
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    act(() => {
      flushSync(() => {
        screen
          .getByRole('button', { name: zhCN.socialCallback.retry })
          .click()
      })
      const region = screen.getByRole('status')
      expect(region.textContent).toBe('')
    })
    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        zhCN.socialCallback.pending,
      ),
    )
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
  })

  it('start a fresh exchange when the pair changes while anonymous', async () => {
    let attempts = 0
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () => {
        attempts += 1
        if (attempts === 1) {
          // The first pair is refused, so the session stays anonymous --
          // the state in which a fresh exchange is possible.
          throw apiError(400, 'authn.oauth_state_invalid')
        }
        return { tokens: makePair() }
      },
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
    await waitFor(() =>
      expect(screen.getByRole('alert')).toBeInTheDocument(),
    )
    rerender(
      <SocialCallbackHandler
        session={harness.session}
        provider="google"
        code="oauth-code-2"
        state={STATE}
        onSignedIn={onSignedIn}
      />,
    )
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(harness.calls).toHaveLength(2)
    expect(harness.calls[1]?.options?.body).toEqual({
      code: 'oauth-code-2',
      state: STATE,
    })
    expect(harness.store.get()).toBe('access-1')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('not re-exchange when a remount finds the session already signed in', async () => {
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () => ({ tokens: makePair() }),
    })
    const onSignedIn = vi.fn()
    const first = renderWithProviders(
      <SocialCallbackHandler
        session={harness.session}
        provider="google"
        code={CODE}
        state={STATE}
        onSignedIn={onSignedIn}
      />,
    )
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(harness.calls).toHaveLength(1)
    expect(harness.store.get()).toBe('access-1')

    // The viewer returns to the callback URL after the sign-in landed
    // (a back/forward re-entry in the same SPA instance): the mount
    // finds the session authenticated, starts no second exchange for
    // the consumed code and signals the host onward instead.
    first.unmount()
    renderWithProviders(
      <SocialCallbackHandler
        session={harness.session}
        provider="google"
        code={CODE}
        state={STATE}
        onSignedIn={onSignedIn}
      />,
    )
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(2))
    expect(harness.calls).toHaveLength(1)
    expect(harness.store.get()).toBe('access-1')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(
      screen.getByText(zhCN.socialCallback.pending),
    ).toBeInTheDocument()
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
    // Rendered under the page h1 a real host page supplies: the
    // handler renders no heading of its own, and page-has-heading-one
    // is determinate in jsdom now (see the axe helper header) -- a
    // scan without that context fails instead of passing by
    // indeterminacy.
    renderWithProviders(
      <div>
        <h1>Completing sign in</h1>
        <SocialCallbackHandler
          session={harness.session}
          provider="google"
          code={CODE}
          state={STATE}
        />
      </div>,
    )
    await expectNoAxeViolations()
    resolveExchange(makePair())
  })

  it('contain a throwing onSignedIn after a successful exchange: the success outcome never flips to the failed state', async () => {
    // The exchange verdict settles first; the host callback runs after.
    // A host callback that throws is not an exchange failure: it must
    // not flip the screen to the failed state -- which would offer a
    // retry of an already-consumed single-use code -- and must not
    // suppress the success outcome (the session holds the issued
    // token, and the containment keeps the throw from becoming an
    // unhandled rejection).
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () => ({ tokens: makePair() }),
    })
    const onSignedIn = vi.fn(() => {
      throw new Error('host navigation failed')
    })
    renderWithProviders(
      <SocialCallbackHandler
        session={harness.session}
        provider="google"
        code={CODE}
        state={STATE}
        onSignedIn={onSignedIn}
      />,
    )
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(harness.store.get()).toBe('access-1')
    // Still the success outcome: no failed-state banner, no retry of
    // the consumed pair, and the pending notice stays up.
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: zhCN.socialCallback.retry }),
    ).not.toBeInTheDocument()
    expect(
      screen.getByText(zhCN.socialCallback.pending),
    ).toBeInTheDocument()
    // Exactly one exchange ran.
    expect(harness.calls).toHaveLength(1)
  })

  it('treat a superseded exchange as the lost race it is: no failed state, no onSignedIn', async () => {
    // A sibling login committing through the same session while this
    // exchange is in flight makes auth-core reject it with
    // OperationSupersededError when its answer arrives: the session is
    // authenticated under the WINNER's identity, which is not
    // necessarily this exchange's. The handler must not flip to the
    // failed state -- its retry would re-submit the already-consumed
    // single-use code -- and must not fire onSignedIn, a one-shot side
    // effect reserved for the identity the session actually runs
    // under (the winning call fires its own). The pending notice stays
    // up for the host to observe the winner's session and navigate.
    let releaseSocial!: (value: unknown) => void
    const socialGate = new Promise((resolve) => {
      releaseSocial = resolve
    })
    const harness = makeHarness({
      [SOCIAL_CALLBACK]: () =>
        socialGate.then(() => ({ tokens: makePair() })),
      [LOGIN_PASSWORD]: () => makePair(),
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
    // The exchange is in flight (the gate holds its answer).
    await waitFor(() => expect(harness.calls).toHaveLength(1))
    // The winning password login commits while the exchange is in
    // flight.
    await act(async () => {
      await harness.session.loginWithPassword({
        identifier: 'alice@example.com',
        password: 'pw',
      })
    })
    expect(harness.store.get()).toBe('access-1')
    // The exchange's answer arrives after the winner committed:
    // auth-core rejects it as superseded. No failure banner, no retry
    // button, no onSignedIn, and the loser's tokens never applied.
    await act(async () => {
      releaseSocial(makePair({ access_token: 'access-social' }))
    })
    await waitFor(() =>
      expect(
        screen.getByText(zhCN.socialCallback.pending),
      ).toBeInTheDocument(),
    )
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: zhCN.socialCallback.retry }),
    ).not.toBeInTheDocument()
    expect(onSignedIn).not.toHaveBeenCalled()
    expect(harness.store.get()).toBe('access-1')
    expect(harness.calls).toHaveLength(2)
  })
})
