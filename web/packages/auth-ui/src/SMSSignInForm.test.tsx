/**
 * SMSSignInForm behaviour: the happy path runs the two steps -- the code
 * request answers 202 and the flow moves to the code step, whose submit
 * logs in with the number the code was sent to -- and each scripted
 * request is asserted on method, path and body. Resend repeats the
 * request against the same number without leaving the code step; editing
 * the phone returns to the first step. A refused code request (rate
 * limited, or a number with no E.164 form) renders its code text in one
 * alert and stays on the phone step; a bad code answer does the same on
 * the code step and signs nothing in. Both
 * steps' buttons disable for their flight, empty submits never reach the
 * network, the en-US bundle renders on an English-starting instance, and
 * the tree passes axe. Text expectations read the bundle values, never
 * inline language.
 */

import { describe, expect, it, vi } from 'vitest'
import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SMSSignInForm } from './SMSSignInForm.js'
import { renderWithProviders } from '../test-utils/render.js'
import {
  LOGIN_PASSWORD,
  LOGIN_SMS,
  REQUEST_SMS_CODE,
  apiError,
  makeHarness,
  makePair,
} from '../test-utils/session-harness.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

const PHONE = '+8613800138000'
const CODE = '123456'

function sentNoticeOf(phone: string, template: string): string {
  return template.replace('{{phone}}', phone)
}

/** Types the phone and submits the code request (phone step). */
async function requestCode(phone: string, language: 'zh-CN' | 'en-US' = 'zh-CN') {
  const user = userEvent.setup()
  const bundle = language === 'zh-CN' ? zhCN : enUS
  await user.type(
    screen.getByLabelText(bundle.smsSignIn.phoneLabel),
    phone,
  )
  await user.click(
    screen.getByRole('button', { name: bundle.smsSignIn.sendCode }),
  )
}

/** Types the code and submits the login (code step). */
async function completeCode(
  code: string,
  language: 'zh-CN' | 'en-US' = 'zh-CN',
) {
  const user = userEvent.setup()
  const bundle = language === 'zh-CN' ? zhCN : enUS
  await user.type(screen.getByLabelText(bundle.smsSignIn.codeLabel), code)
  await user.click(
    screen.getByRole('button', { name: bundle.smsSignIn.submit }),
  )
}

describe('SMSSignInForm', () => {
  it('request a code, then sign in with the code sent to that number', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
      [LOGIN_SMS]: () => makePair(),
    })
    const onSignedIn = vi.fn()
    renderWithProviders(
      <SMSSignInForm session={harness.session} onSignedIn={onSignedIn} />,
    )
    await requestCode(PHONE)
    await waitFor(() =>
      expect(
        screen.getByRole('status'),
      ).toHaveTextContent(
        sentNoticeOf(PHONE, zhCN.smsSignIn.sentNotice),
      ),
    )
    expect(screen.getByLabelText(zhCN.smsSignIn.codeLabel)).toBeInTheDocument()
    await completeCode(CODE)
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(harness.calls).toHaveLength(2)
    expect(harness.calls[0]?.method).toBe('POST')
    expect(harness.calls[0]?.path).toBe('/api/v1/authn/login/sms/request')
    expect(harness.calls[0]?.options?.body).toEqual({ phone: PHONE })
    expect(harness.calls[1]?.method).toBe('POST')
    expect(harness.calls[1]?.path).toBe('/api/v1/authn/login/sms')
    expect(harness.calls[1]?.options?.body).toEqual({
      phone: PHONE,
      code: CODE,
    })
    expect(harness.store.get()).toBe('access-1')
  })

  it('resend the code against the same number, staying on the code step', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
      [LOGIN_SMS]: () => makePair(),
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />)
    await requestCode(PHONE)
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: zhCN.smsSignIn.resendCode }),
    )
    await waitFor(() => expect(harness.calls).toHaveLength(2))
    expect(harness.calls[1]?.options?.body).toEqual({ phone: PHONE })
    // The code step stays put: the code field and the notice remain.
    expect(screen.getByLabelText(zhCN.smsSignIn.codeLabel)).toBeInTheDocument()
    expect(
      screen.queryByLabelText(zhCN.smsSignIn.phoneLabel),
    ).not.toBeInTheDocument()
  })

  it('return to the phone step when the number is edited', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />)
    await requestCode(PHONE)
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: zhCN.smsSignIn.editPhone }),
    )
    expect(screen.getByLabelText(zhCN.smsSignIn.phoneLabel)).toBeInTheDocument()
    expect(
      screen.queryByLabelText(zhCN.smsSignIn.codeLabel),
    ).not.toBeInTheDocument()
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    expect(harness.calls).toHaveLength(1)
  })

  it('render a rate-limited code request text and stay on the phone step', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => {
        throw apiError(429, 'authn.rate_limited')
      },
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />)
    await requestCode(PHONE)
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.rate_limited,
      ),
    )
    // Still on the phone step: no notice, no code field, nothing sent.
    expect(screen.getByLabelText(zhCN.smsSignIn.phoneLabel)).toBeInTheDocument()
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    expect(harness.store.get()).toBeNull()
    expect(harness.calls).toHaveLength(1)
  })

  it('render an invalid-phone answer text and stay on the phone step', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => {
        throw apiError(400, 'authn.invalid_phone')
      },
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />)
    // A domestic-style number without the E.164 '+' prefix and country
    // code is exactly what the backend's canonical-form gate refuses.
    await requestCode('13800138000')
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.invalid_phone,
      ),
    )
    // Still on the phone step: no notice, no code field, nothing sent.
    expect(screen.getByLabelText(zhCN.smsSignIn.phoneLabel)).toBeInTheDocument()
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    expect(harness.store.get()).toBeNull()
    expect(harness.calls).toHaveLength(1)
  })

  it('render a rejected code text on the code step and sign nothing in', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
      [LOGIN_SMS]: () => {
        throw apiError(401, 'authn.verification_code_invalid')
      },
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />)
    await requestCode(PHONE)
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    await completeCode('000000')
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.verification_code_invalid,
      ),
    )
    expect(harness.store.get()).toBeNull()
    expect(harness.calls).toHaveLength(2)
  })

  it('disable the code-request button while the request is in flight', async () => {
    let resolveRequest: (value: unknown) => void = () => {}
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () =>
        new Promise((resolve) => {
          resolveRequest = resolve
        }),
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />)
    const user = userEvent.setup()
    await user.type(
      screen.getByLabelText(zhCN.smsSignIn.phoneLabel),
      PHONE,
    )
    const sendCode = screen.getByRole('button', {
      name: zhCN.smsSignIn.sendCode,
    })
    await user.click(sendCode)
    await waitFor(() => expect(sendCode).toBeDisabled())
    resolveRequest(undefined)
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
  })

  it('disable the login button while the login is in flight', async () => {
    let resolveLogin: (value: unknown) => void = () => {}
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
      [LOGIN_SMS]: () =>
        new Promise((resolve) => {
          resolveLogin = resolve
        }),
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />)
    await requestCode(PHONE)
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    const user = userEvent.setup()
    await user.type(
      screen.getByLabelText(zhCN.smsSignIn.codeLabel),
      CODE,
    )
    const submit = screen.getByRole('button', { name: zhCN.smsSignIn.submit })
    await user.click(submit)
    await waitFor(() => expect(submit).toBeDisabled())
    resolveLogin(makePair())
    await waitFor(() => expect(submit).toBeEnabled())
  })

  it('never reach the network for an empty submit on either step', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />)
    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: zhCN.smsSignIn.sendCode }),
    )
    await waitFor(() =>
      expect(screen.queryByRole('alert')).not.toBeInTheDocument(),
    )
    expect(harness.calls).toHaveLength(0)
    // The empty submit on the code step is equally refused.
    await user.type(
      screen.getByLabelText(zhCN.smsSignIn.phoneLabel),
      PHONE,
    )
    await user.click(
      screen.getByRole('button', { name: zhCN.smsSignIn.sendCode }),
    )
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    await user.click(
      screen.getByRole('button', { name: zhCN.smsSignIn.submit }),
    )
    await waitFor(() =>
      expect(screen.queryByRole('alert')).not.toBeInTheDocument(),
    )
    // The code request went out; the empty-code login never did.
    expect(harness.calls).toHaveLength(1)
  })

  it('render the en-US bundle on an English-starting instance', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
      [LOGIN_SMS]: () => makePair(),
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />, {
      language: 'en-US',
    })
    expect(
      screen.getByLabelText(enUS.smsSignIn.phoneLabel),
    ).toBeInTheDocument()
    await requestCode(PHONE, 'en-US')
    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        sentNoticeOf(PHONE, enUS.smsSignIn.sentNotice),
      ),
    )
    expect(screen.getByLabelText(enUS.smsSignIn.codeLabel)).toBeInTheDocument()
    await completeCode(CODE, 'en-US')
    await waitFor(() => expect(harness.calls).toHaveLength(2))
  })

  it('pass axe with no violations', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />)
    await expectNoAxeViolations()
  })

  it('contain a throwing onSignedIn: the committed SMS login never looks like a failure', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
      [LOGIN_SMS]: () => makePair(),
    })
    const onSignedIn = vi.fn(() => {
      throw new Error('host navigation failed')
    })
    renderWithProviders(
      <SMSSignInForm session={harness.session} onSignedIn={onSignedIn} />,
    )
    await requestCode(PHONE)
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    await completeCode(CODE)
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledTimes(1))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(harness.store.get()).toBe('access-1')
  })

  it('start the code field empty after the phone changed and a new code was requested', async () => {
    const SECOND_PHONE = '+8613900139000'
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />)
    const user = userEvent.setup()
    // A code for the first phone is typed on the code step.
    await requestCode(PHONE)
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    await user.type(screen.getByLabelText(zhCN.smsSignIn.codeLabel), CODE)
    // The viewer changes the number and requests a code for it.
    await user.click(
      screen.getByRole('button', { name: zhCN.smsSignIn.editPhone }),
    )
    await user.clear(screen.getByLabelText(zhCN.smsSignIn.phoneLabel))
    await user.type(screen.getByLabelText(zhCN.smsSignIn.phoneLabel), SECOND_PHONE)
    await user.click(
      screen.getByRole('button', { name: zhCN.smsSignIn.sendCode }),
    )
    await waitFor(() => expect(harness.calls).toHaveLength(2))
    expect(harness.calls[1]?.options?.body).toEqual({ phone: SECOND_PHONE })
    // The stale code for the old phone and the old code session is
    // gone: the field starts empty for the freshly issued code.
    await waitFor(() =>
      expect(screen.getByLabelText(zhCN.smsSignIn.codeLabel)).toHaveValue(''),
    )
  })

  it('clear the typed code when a new code is requested over the resend button', async () => {
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
    })
    renderWithProviders(<SMSSignInForm session={harness.session} />)
    const user = userEvent.setup()
    await requestCode(PHONE)
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    await user.type(screen.getByLabelText(zhCN.smsSignIn.codeLabel), CODE)
    await user.click(
      screen.getByRole('button', { name: zhCN.smsSignIn.resendCode }),
    )
    await waitFor(() => expect(harness.calls).toHaveLength(2))
    // A resent code invalidates whatever code was typed: the field
    // starts empty again.
    await waitFor(() =>
      expect(screen.getByLabelText(zhCN.smsSignIn.codeLabel)).toHaveValue(''),
    )
  })

  it('treat a superseded code-step submit as the lost race it is, rendering no error', async () => {
    // A concurrent sign-in (here the password channel committing
    // through the same session while the SMS login is still in
    // flight) makes auth-core reject the SMS submit with
    // OperationSupersededError when its answer arrives: the losing
    // submit must not render the generic error banner (it is not a
    // failure) and must not fire onSignedIn (the winning call fired
    // its own exactly once, for the identity the session actually
    // runs under).
    let releaseSms!: (value: unknown) => void
    const smsGate = new Promise((resolve) => {
      releaseSms = resolve
    })
    const harness = makeHarness({
      [REQUEST_SMS_CODE]: () => undefined,
      [LOGIN_SMS]: () => smsGate,
      [LOGIN_PASSWORD]: () => makePair(),
    })
    const onSignedIn = vi.fn()
    renderWithProviders(
      <SMSSignInForm session={harness.session} onSignedIn={onSignedIn} />,
    )
    await requestCode(PHONE)
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    const user = userEvent.setup()
    await user.type(screen.getByLabelText(zhCN.smsSignIn.codeLabel), CODE)
    await user.click(
      screen.getByRole('button', { name: zhCN.smsSignIn.submit }),
    )
    // The winning password login commits while the SMS login is in
    // flight.
    await act(async () => {
      await harness.session.loginWithPassword({
        identifier: 'alice@example.com',
        password: 'pw',
      })
    })
    expect(harness.store.get()).toBe('access-1')
    // The SMS answer arrives after the winner committed: auth-core
    // rejects it as superseded, and the form treats that as the lost
    // race it is -- quiet, retryable, no error banner, no onSignedIn.
    await act(async () => {
      releaseSms(makePair())
    })
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: zhCN.smsSignIn.submit }),
      ).toBeEnabled(),
    )
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(onSignedIn).not.toHaveBeenCalled()
    expect(harness.store.get()).toBe('access-1')
  })
})
