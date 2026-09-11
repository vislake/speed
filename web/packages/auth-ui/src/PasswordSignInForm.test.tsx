/**
 * PasswordSignInForm behaviour: the happy path drives one password-login
 * call with the entered identifier and password (scripted through the
 * bindRequestFn harness, asserted on the request body), a busy submit
 * disables the button for the flight's duration, an invalid-credentials
 * answer renders the code's own text in one alert and changes nothing,
 * an empty submit never reaches the network, the en-US bundle renders on
 * an English-starting instance, and the tree passes axe. Text
 * expectations read the bundle values, never inline language.
 */

import { describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { PasswordSignInForm } from './PasswordSignInForm.js'
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

const SUBMIT_ZH = zhCN.passwordSignIn.submit
const SUBMIT_EN = enUS.passwordSignIn.submit

interface SignInLabels {
  readonly identifierLabel: string
  readonly passwordLabel: string
}

async function fillAndSubmit(
  identifier: string,
  password: string,
  labels: SignInLabels,
  submitName: string = SUBMIT_ZH,
) {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText(labels.identifierLabel), identifier)
  await user.type(screen.getByLabelText(labels.passwordLabel), password)
  await user.click(screen.getByRole('button', { name: submitName }))
}

const ZH_LABELS: SignInLabels = {
  identifierLabel: zhCN.passwordSignIn.identifierLabel,
  passwordLabel: zhCN.passwordSignIn.passwordLabel,
}

describe('PasswordSignInForm', () => {
  it('sign in with the entered identifier and password on submit', async () => {
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    const onSignedIn = vi.fn()
    renderWithProviders(
      <PasswordSignInForm session={harness.session} onSignedIn={onSignedIn} />,
    )
    await fillAndSubmit('alice@example.com', 's3cret-pass', ZH_LABELS)
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(harness.calls).toHaveLength(1)
    expect(harness.calls[0]?.method).toBe('POST')
    expect(harness.calls[0]?.path).toBe('/api/v1/authn/login/password')
    expect(harness.calls[0]?.options?.body).toEqual({
      identifier: 'alice@example.com',
      password: 's3cret-pass',
    })
    expect(harness.store.get()).toBe('access-1')
  })

  it('disable the submit button while the login is in flight, re-enable after', async () => {
    let resolveLogin: (value: unknown) => void = () => {}
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () =>
        new Promise((resolve) => {
          resolveLogin = resolve
        }),
    })
    renderWithProviders(<PasswordSignInForm session={harness.session} />)
    const user = userEvent.setup()
    await user.type(
      screen.getByLabelText(zhCN.passwordSignIn.identifierLabel),
      'alice@example.com',
    )
    await user.type(
      screen.getByLabelText(zhCN.passwordSignIn.passwordLabel),
      's3cret-pass',
    )
    const submit = screen.getByRole('button', { name: SUBMIT_ZH })
    await user.click(submit)
    await waitFor(() => expect(submit).toBeDisabled())
    resolveLogin(makePair())
    await waitFor(() => expect(submit).toBeEnabled())
  })

  it('render the code text of an invalid-credentials answer and change nothing', async () => {
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => {
        throw apiError(401, 'authn.invalid_credentials')
      },
    })
    renderWithProviders(<PasswordSignInForm session={harness.session} />)
    await fillAndSubmit('alice@example.com', 'wrong-password', ZH_LABELS)
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.invalid_credentials,
      ),
    )
    expect(harness.store.get()).toBeNull()
    expect(harness.calls).toHaveLength(1)
  })

  it('never reach the network for an empty submit', async () => {
    const harness = makeHarness({})
    renderWithProviders(<PasswordSignInForm session={harness.session} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: SUBMIT_ZH }))
    await waitFor(() =>
      expect(screen.queryByRole('alert')).not.toBeInTheDocument(),
    )
    expect(harness.calls).toHaveLength(0)
  })

  it('render the en-US bundle on an English-starting instance', async () => {
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    renderWithProviders(
      <PasswordSignInForm session={harness.session} />,
      { language: 'en-US' },
    )
    expect(
      screen.getByLabelText(enUS.passwordSignIn.identifierLabel),
    ).toBeInTheDocument()
    expect(
      screen.getByLabelText(enUS.passwordSignIn.passwordLabel),
    ).toBeInTheDocument()
    await fillAndSubmit('alice@example.com', 's3cret-pass', {
      identifierLabel: enUS.passwordSignIn.identifierLabel,
      passwordLabel: enUS.passwordSignIn.passwordLabel,
    }, SUBMIT_EN)
    await waitFor(() => expect(harness.calls).toHaveLength(1))
  })

  it('pass axe with no violations', async () => {
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    // Rendered under the page h1 a real host page supplies: the form
    // renders no heading of its own (the page above it is host
    // content), and page-has-heading-one is determinate in jsdom now
    // (see the axe helper header) -- a scan without that context fails
    // instead of passing by indeterminacy.
    renderWithProviders(
      <div>
        <h1>Sign in</h1>
        <PasswordSignInForm session={harness.session} />
      </div>,
    )
    await expectNoAxeViolations()
  })

  it('contain a throwing onSignedIn: the committed login never looks like a failure', async () => {
    // A host callback that throws is not a login failure (the login
    // committed -- the store holds the issued token): it must not paint
    // the failure banner, and the containment keeps the throw out of
    // the submit promise as an unhandled rejection.
    const harness = makeHarness({ [LOGIN_PASSWORD]: () => makePair() })
    const onSignedIn = vi.fn(() => {
      throw new Error('host navigation failed')
    })
    renderWithProviders(
      <PasswordSignInForm session={harness.session} onSignedIn={onSignedIn} />,
    )
    await fillAndSubmit('alice@example.com', 's3cret-pass', ZH_LABELS)
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(harness.store.get()).toBe('access-1')
  })

  it('treat a superseded submit as the lost race it is, rendering no error', async () => {
    // A concurrent sign-in (here a social exchange committed through
    // the same session while this form's own login was still in
    // flight) makes auth-core reject this submit with
    // OperationSupersededError when its answer arrives: the losing
    // submit must not render the generic error banner -- it is not a
    // failure, and the session being authenticated now is the host's
    // own snapshot to observe.
    let releasePassword: (value: unknown) => void = () => {}
    const passwordGate = new Promise((resolve) => {
      releasePassword = resolve
    })
    const harness = makeHarness({
      [LOGIN_PASSWORD]: () => passwordGate,
      [SOCIAL_CALLBACK]: () => ({ tokens: makePair() }),
    })
    const onSignedIn = vi.fn()
    renderWithProviders(
      <PasswordSignInForm session={harness.session} onSignedIn={onSignedIn} />,
    )
    const user = userEvent.setup()
    await user.type(
      screen.getByLabelText(zhCN.passwordSignIn.identifierLabel),
      'alice@example.com',
    )
    await user.type(
      screen.getByLabelText(zhCN.passwordSignIn.passwordLabel),
      's3cret-pass',
    )
    const submit = screen.getByRole('button', { name: SUBMIT_ZH })
    await user.click(submit)
    await waitFor(() => expect(submit).toBeDisabled())
    // The winning sign-in commits while the password login is in
    // flight.
    await act(async () => {
      await harness.session.completeSocialLogin('google', {
        code: 'oauth-code-1',
        state: 'csrf-state-1',
      })
    })
    expect(harness.store.get()).toBe('access-1')
    // The password answer arrives after the winner committed: auth-core
    // rejects it as superseded, and the form treats that as the lost
    // race it is -- quiet, retryable, no error banner.
    await act(async () => {
      releasePassword(makePair())
    })
    await waitFor(() => expect(submit).toBeEnabled())
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(onSignedIn).not.toHaveBeenCalled()
    expect(harness.store.get()).toBe('access-1')
  })
})
