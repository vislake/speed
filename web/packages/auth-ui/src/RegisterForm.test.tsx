/**
 * RegisterForm behaviour: an email identifier fills the request's email
 * slot and a phone number its phone slot (the spec's separated shape),
 * with the display name trimmed and omitted when blank; the locale the
 * request declares follows the active UI language, re-read at submit time
 * so a mid-flight switch is honoured. Registration never signs in: an
 * email-already-registered answer or an identifier-format refusal
 * (authn.invalid_email from the email slot's canonical-form gate,
 * authn.invalid_phone for a phone number with no E.164 form) renders its
 * code text in one alert, and a created user goes to onRegistered or --
 * without a callback -- renders the success panel in place of the form. An empty submit never reaches
 * the network, the en-US bundle renders on an English-starting instance,
 * and the tree passes axe. Text expectations read the bundle values,
 * never inline language.
 */

import { describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { switchLanguage } from '@speed/i18n'
import type { AuthnUser } from '@speed/api-sdk'
import { RegisterForm } from './RegisterForm.js'
import { renderWithProviders } from '../test-utils/render.js'
import {
  REGISTER,
  apiError,
  makeHarness,
} from '../test-utils/session-harness.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

const ALICE: AuthnUser = {
  id: 'user-1',
  email: 'alice@example.com',
  display_name: 'Alice',
  locale: 'zh-CN',
  email_verified: false,
  phone_verified: false,
}

const PHONE = '+8613800138000'

const SUBMIT_ZH = zhCN.register.submit
const SUBMIT_EN = enUS.register.submit

interface RegisterLabels {
  readonly identifierLabel: string
  readonly passwordLabel: string
  readonly displayNameLabel: string
}

async function fillAndSubmit(
  identifier: string,
  password: string,
  labels: RegisterLabels,
  submitName: string = SUBMIT_ZH,
  displayName?: string,
) {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText(labels.identifierLabel), identifier)
  await user.type(screen.getByLabelText(labels.passwordLabel), password)
  if (displayName !== undefined) {
    await user.type(screen.getByLabelText(labels.displayNameLabel), displayName)
  }
  await user.click(screen.getByRole('button', { name: submitName }))
}

const ZH_LABELS: RegisterLabels = {
  identifierLabel: zhCN.register.identifierLabel,
  passwordLabel: zhCN.register.passwordLabel,
  displayNameLabel: zhCN.register.displayNameLabel,
}

describe('RegisterForm', () => {
  it('register an email identifier into the email slot and render the success panel', async () => {
    const harness = makeHarness({ [REGISTER]: () => ALICE })
    renderWithProviders(<RegisterForm session={harness.session} />)
    await fillAndSubmit('alice@example.com', 's3cret-pass', ZH_LABELS)
    await waitFor(() =>
      expect(
        screen.getByRole('status'),
      ).toHaveTextContent(zhCN.register.successMessage),
    )
    expect(
      screen.getByText(zhCN.register.successTitle),
    ).toBeInTheDocument()
    // The form is gone, replaced by the panel.
    expect(
      screen.queryByLabelText(zhCN.register.identifierLabel),
    ).not.toBeInTheDocument()
    expect(harness.calls).toHaveLength(1)
    expect(harness.calls[0]?.method).toBe('POST')
    expect(harness.calls[0]?.path).toBe('/api/v1/authn/register')
    expect(harness.calls[0]?.options?.body).toEqual({
      email: 'alice@example.com',
      password: 's3cret-pass',
      locale: 'zh-CN',
    })
    // Registration never issues tokens.
    expect(harness.store.get()).toBeNull()
  })

  it('register a phone identifier into the phone slot with a display name', async () => {
    const harness = makeHarness({
      [REGISTER]: () => ({ id: 'user-2', phone: PHONE }),
    })
    renderWithProviders(<RegisterForm session={harness.session} />)
    await fillAndSubmit(PHONE, 's3cret-pass', ZH_LABELS, SUBMIT_ZH, 'Li Lei')
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    expect(harness.calls[0]?.options?.body).toEqual({
      phone: PHONE,
      password: 's3cret-pass',
      display_name: 'Li Lei',
      locale: 'zh-CN',
    })
  })

  it('omit a blank display name from the request', async () => {
    const harness = makeHarness({ [REGISTER]: () => ALICE })
    renderWithProviders(<RegisterForm session={harness.session} />)
    const user = userEvent.setup()
    await user.type(
      screen.getByLabelText(ZH_LABELS.identifierLabel),
      'alice@example.com',
    )
    await user.type(
      screen.getByLabelText(ZH_LABELS.passwordLabel),
      's3cret-pass',
    )
    await user.type(screen.getByLabelText(ZH_LABELS.displayNameLabel), '   ')
    await user.click(screen.getByRole('button', { name: SUBMIT_ZH }))
    await waitFor(() =>
      expect(screen.getByRole('status')).toBeInTheDocument(),
    )
    const body = harness.calls[0]?.options?.body as Record<string, unknown>
    expect(body).toEqual({
      email: 'alice@example.com',
      password: 's3cret-pass',
      locale: 'zh-CN',
    })
    expect('display_name' in body).toBe(false)
  })

  it('hand the created user to onRegistered when given, rendering no panel', async () => {
    const harness = makeHarness({ [REGISTER]: () => ALICE })
    const onRegistered = vi.fn()
    renderWithProviders(
      <RegisterForm session={harness.session} onRegistered={onRegistered} />,
    )
    await fillAndSubmit('alice@example.com', 's3cret-pass', ZH_LABELS)
    await waitFor(() => expect(onRegistered).toHaveBeenCalledTimes(1))
    expect(onRegistered).toHaveBeenCalledWith(ALICE)
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('render an email-already-registered answer and keep the form up', async () => {
    const harness = makeHarness({
      [REGISTER]: () => {
        throw apiError(409, 'authn.email_already_registered')
      },
    })
    renderWithProviders(<RegisterForm session={harness.session} />)
    await fillAndSubmit('taken@example.com', 's3cret-pass', ZH_LABELS)
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.email_already_registered,
      ),
    )
    expect(harness.store.get()).toBeNull()
    // Still on the form, retry possible.
    expect(
      screen.getByLabelText(zhCN.register.identifierLabel),
    ).toBeInTheDocument()
  })

  it('render an invalid-phone answer for a phone identifier and keep the form up', async () => {
    const harness = makeHarness({
      [REGISTER]: () => {
        throw apiError(400, 'authn.invalid_phone')
      },
    })
    renderWithProviders(<RegisterForm session={harness.session} />)
    // No '@': the identifier goes into the phone slot, and a number
    // without the E.164 '+' prefix and country code is what the
    // backend's canonical-form gate refuses.
    await fillAndSubmit('13800138000', 's3cret-pass', ZH_LABELS)
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.invalid_phone,
      ),
    )
    expect(harness.store.get()).toBeNull()
    // Still on the form, retry possible.
    expect(
      screen.getByLabelText(zhCN.register.identifierLabel),
    ).toBeInTheDocument()
  })

  it('render an invalid-email answer for an email identifier and keep the form up', async () => {
    const harness = makeHarness({
      [REGISTER]: () => {
        throw apiError(400, 'authn.invalid_email')
      },
    })
    renderWithProviders(<RegisterForm session={harness.session} />)
    await fillAndSubmit('bad@example', 's3cret-pass', ZH_LABELS)
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        zhCN.errors.authn.invalid_email,
      ),
    )
    expect(harness.store.get()).toBeNull()
    // Still on the form, retry possible.
    expect(
      screen.getByLabelText(zhCN.register.identifierLabel),
    ).toBeInTheDocument()
  })

  it('declare the UI language switched mid-flight on the next submit', async () => {
    const harness = makeHarness({ [REGISTER]: () => ALICE })
    const { i18n } = renderWithProviders(
      <RegisterForm session={harness.session} />,
    )
    await switchLanguage(i18n, 'en-US')
    await fillAndSubmit(
      'alice@example.com',
      's3cret-pass',
      {
        identifierLabel: enUS.register.identifierLabel,
        passwordLabel: enUS.register.passwordLabel,
        displayNameLabel: enUS.register.displayNameLabel,
      },
      SUBMIT_EN,
    )
    await waitFor(() => expect(harness.calls).toHaveLength(1))
    expect(harness.calls[0]?.options?.body).toEqual({
      email: 'alice@example.com',
      password: 's3cret-pass',
      locale: 'en-US',
    })
  })

  it('render the en-US bundle on an English-starting instance', async () => {
    const harness = makeHarness({ [REGISTER]: () => ALICE })
    renderWithProviders(<RegisterForm session={harness.session} />, {
      language: 'en-US',
    })
    expect(
      screen.getByLabelText(enUS.register.identifierLabel),
    ).toBeInTheDocument()
    await fillAndSubmit(
      'alice@example.com',
      's3cret-pass',
      {
        identifierLabel: enUS.register.identifierLabel,
        passwordLabel: enUS.register.passwordLabel,
        displayNameLabel: enUS.register.displayNameLabel,
      },
      SUBMIT_EN,
    )
    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        enUS.register.successMessage,
      ),
    )
    expect(harness.calls[0]?.options?.body).toEqual({
      email: 'alice@example.com',
      password: 's3cret-pass',
      locale: 'en-US',
    })
  })

  it('never reach the network for an empty submit', async () => {
    const harness = makeHarness({})
    renderWithProviders(<RegisterForm session={harness.session} />)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: SUBMIT_ZH }))
    await waitFor(() =>
      expect(screen.queryByRole('alert')).not.toBeInTheDocument(),
    )
    expect(harness.calls).toHaveLength(0)
  })

  it('pass axe with no violations', async () => {
    const harness = makeHarness({ [REGISTER]: () => ALICE })
    renderWithProviders(<RegisterForm session={harness.session} />)
    await expectNoAxeViolations()
  })
})
