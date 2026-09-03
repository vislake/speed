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
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { PasswordSignInForm } from './PasswordSignInForm.js'
import { renderWithProviders } from '../test-utils/render.js'
import {
  LOGIN_PASSWORD,
  apiError,
  makeHarness,
  makePair,
} from '../test-utils/session-harness.js'
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
    renderWithProviders(<PasswordSignInForm session={harness.session} />)
    await expectNoAxeViolations()
  })
})
