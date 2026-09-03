/**
 * SMSSignInForm behaviour: the happy path runs the two steps -- the code
 * request answers 202 and the flow moves to the code step, whose submit
 * logs in with the number the code was sent to -- and each scripted
 * request is asserted on method, path and body. Resend repeats the
 * request against the same number without leaving the code step; editing
 * the phone returns to the first step. A rate-limited code request
 * renders its code text in one alert and stays on the phone step; a bad
 * code answer does the same on the code step and signs nothing in. Both
 * steps' buttons disable for their flight, empty submits never reach the
 * network, the en-US bundle renders on an English-starting instance, and
 * the tree passes axe. Text expectations read the bundle values, never
 * inline language.
 */

import { describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SMSSignInForm } from './SMSSignInForm.js'
import { renderWithProviders } from '../test-utils/render.js'
import {
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
})
