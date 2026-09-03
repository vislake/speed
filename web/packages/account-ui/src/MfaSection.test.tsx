/**
 * MfaSection behaviour, driven over the real-client rig: the section
 * discovers MFA state by acting on it, so every scenario walks one of the
 * two gated journeys (enroll / regenerate) through the composed wiring a
 * host builds -- real api-client, real session, generated mutations.
 *
 * The suite pins the plan's discover-by-acting machine: a first setup
 * answers 200 straight into the wizard (no warning), the confirm opens
 * the show-once codes, and saving resets everything -- re-entering needs
 * a brand-new enroll request, nothing is cached. An enroll or regenerate
 * refused 403 authn.step_up_required opens the challenge dialog, whose
 * success re-runs exactly the gated action: enroll then shows the
 * replacement warning (the 403 is the one reliable signal an active
 * factor exists), regenerate opens the codes panel or -- a race the
 * server cannot fully rule out -- answers 404 authn.mfa_not_enrolled and
 * renders its guide text. A cancel retries nothing. Confirm answers: a
 * wrong code is a field error that stays retryable through the silent
 * 401-refresh leg, 429 renders its code text over the still-open wizard,
 * and a 409 authn.mfa_already_enrolled race closes the wizard; a dead
 * session renders the original request's code text. Entry and confirm
 * are single-flight: both idle buttons lock during an entry, both wizard
 * actions lock during a confirm. Every scenario ends with an axe pass.
 */

import { describe, expect, it } from 'vitest'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import {
  errorResponse,
  jsonResponse,
  makePair,
  makeRealClientRig,
  signInWithPassword,
} from '../test-utils/real-client.js'
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import { MfaSection } from './MfaSection.js'
import type { AuthSession } from '@speed/auth-core'

const LOGIN_PATH = '/api/v1/authn/login/password'
const REFRESH_PATH = '/api/v1/authn/token/refresh'
const ENROLL_PATH = '/api/v1/authn/mfa/totp/enroll'
const CONFIRM_PATH = '/api/v1/authn/mfa/totp/confirm'
const REGENERATE_PATH = '/api/v1/authn/mfa/recovery-codes/regenerate'
const STEP_UP_PATH = '/api/v1/authn/mfa/step-up'

const SECRET = 'JBSWY3DPEHPK3PXP'
const PROVISIONING_URI =
  'otpauth://totp/speed:owner@example.test?issuer=speed'
const CONFIRM_CODE = '123456'
const WRONG_CODE = '000000'
const RECOVERY_CODES = [
  'able-able-1111',
  'beta-beta-2222',
  'charlie-3333',
  'delta-4444',
  'echo-5555',
  'foxtrot-6666',
  'golf-7777',
  'hotel-8888',
  'india-9999',
  'juliet-0000',
]
const ENROLL_BODY = {
  secret: SECRET,
  provisioning_uri: PROVISIONING_URI,
}
const CODES_BODY = { recovery_codes: RECOVERY_CODES }

/** The step-up 200: a fresh access token and no refresh token (a step-up
 * reuses the caller's existing one, so the body omits it). */
const STEP_UP_BODY = {
  access_token: 'access-2',
  principal: {
    user_id: 'user-1',
    tenant_id: 'tenant-1',
    session_id: 'session-1',
  },
}

async function renderSection(session: AuthSession) {
  const result = renderWithProviders(<MfaSection session={session} />)
  await screen.findByText(zhCN.mfa.title)
  return result
}

async function openEnrollWizard() {
  await userEvent.click(
    screen.getByRole('button', {
      name: zhCN.mfa.authenticator.enrollButton,
    }),
  )
  await screen.findByText(SECRET)
}

describe('MfaSection', () => {
  it('set up an authenticator end to end: wizard without warning, show-once codes, saved resets, no cache', async () => {
    let enrollCalls = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === ENROLL_PATH) {
        enrollCalls += 1
        return jsonResponse(200, ENROLL_BODY)
      }
      if (call.method === 'POST' && call.path === CONFIRM_PATH) {
        return jsonResponse(200, CODES_BODY)
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    await renderSection(rig.session)

    // A first setup answers 200 straight into the wizard -- and renders
    // no replacement warning, which belongs to the 403 path only.
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.enrollButton,
      }),
    )
    expect(await screen.findByText(SECRET)).toBeTruthy()
    expect(screen.getByText(PROVISIONING_URI)).toBeTruthy()
    expect(screen.getByText(zhCN.mfa.authenticator.secretLabel)).toBeTruthy()
    expect(screen.getByText(zhCN.mfa.authenticator.uriLabel)).toBeTruthy()
    expect(
      screen.queryByText(zhCN.mfa.authenticator.replacingNotice),
    ).toBeNull()

    // The six-digit confirm opens the show-once codes panel.
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.authenticator.codeLabel),
      CONFIRM_CODE,
    )
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.confirmLabel,
      }),
    )
    expect(
      await screen.findByText(zhCN.mfa.recoveryCodes.showOnceTitle),
    ).toBeTruthy()
    for (const code of RECOVERY_CODES) {
      expect(screen.getByText(code)).toBeTruthy()
    }

    // Saving resets to the idle view.
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.recoveryCodes.savedLabel,
      }),
    )
    expect(
      screen.queryByText(zhCN.mfa.recoveryCodes.showOnceTitle),
    ).toBeNull()
    expect(enrollCalls).toBe(1)

    // Re-entering needs a brand-new enroll request: nothing in this
    // section caches the codes or the enrollment.
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.enrollButton,
      }),
    )
    expect(await screen.findByText(SECRET)).toBeTruthy()
    expect(enrollCalls).toBe(2)

    await expectNoAxeViolations()
  })

  it('open the challenge on a 403 enroll and, once verified, auto-retry into the replacement wizard', async () => {
    let enrollCalls = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === ENROLL_PATH) {
        enrollCalls += 1
        if (enrollCalls === 1) {
          // An active factor exists: only the step-up can open the door.
          return errorResponse(403, 'authn.step_up_required')
        }
        return jsonResponse(200, ENROLL_BODY)
      }
      if (call.method === 'POST' && call.path === STEP_UP_PATH) {
        return jsonResponse(200, STEP_UP_BODY)
      }
      if (call.method === 'POST' && call.path === CONFIRM_PATH) {
        return jsonResponse(200, CODES_BODY)
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    await renderSection(rig.session)

    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.enrollButton,
      }),
    )
    // The refusal opens the challenge dialog, not a wizard.
    expect(await screen.findByRole('dialog')).toBeTruthy()
    expect(screen.getByText(zhCN.mfa.stepUp.title)).toBeTruthy()
    expect(screen.queryByText(SECRET)).toBeNull()
    expect(enrollCalls).toBe(1)

    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      CONFIRM_CODE,
    )
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.stepUp.confirmLabel,
      }),
    )
    // The success re-ran the gated enroll; the dialog is gone and the
    // wizard now carries the replacement warning.
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(await screen.findByText(SECRET)).toBeTruthy()
    expect(
      screen.getByText(zhCN.mfa.authenticator.replacingNotice),
    ).toBeTruthy()
    expect(enrollCalls).toBe(2)

    await expectNoAxeViolations()
  })

  it('let the challenge cancel retry nothing', async () => {
    let enrollCalls = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === ENROLL_PATH) {
        enrollCalls += 1
        return errorResponse(403, 'authn.step_up_required')
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    await renderSection(rig.session)

    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.enrollButton,
      }),
    )
    expect(await screen.findByRole('dialog')).toBeTruthy()
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.stepUp.cancelLabel,
      }),
    )
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    // No retry: the gated action stays unrun, the section is idle and a
    // fresh entry would need a fresh click.
    expect(enrollCalls).toBe(1)
    expect(screen.queryByText(SECRET)).toBeNull()
    expect(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.enrollButton,
      }),
    ).toBeTruthy()

    await expectNoAxeViolations()
  })

  it('render a wrong confirm code as the field error and complete on the next code', async () => {
    let confirmCalls = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === REFRESH_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === ENROLL_PATH) {
        return jsonResponse(200, ENROLL_BODY)
      }
      if (call.method === 'POST' && call.path === CONFIRM_PATH) {
        confirmCalls += 1
        // The wrong code is refused twice: the 401-refresh leg re-sends
        // the same request once, and the server refuses it again.
        if (confirmCalls <= 2) {
          return errorResponse(401, 'authn.mfa_invalid_code')
        }
        return jsonResponse(200, CODES_BODY)
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    await renderSection(rig.session)
    await openEnrollWizard()

    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.authenticator.codeLabel),
      WRONG_CODE,
    )
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.confirmLabel,
      }),
    )
    expect(
      await screen.findByText(zhCN.errors.authn.mfa_invalid_code),
    ).toBeTruthy()
    // The wizard stays: the field error is retryable.
    expect(screen.getByText(SECRET)).toBeTruthy()
    expect(confirmCalls).toBe(2)

    await userEvent.clear(
      screen.getByLabelText(zhCN.mfa.authenticator.codeLabel),
    )
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.authenticator.codeLabel),
      CONFIRM_CODE,
    )
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.confirmLabel,
      }),
    )
    expect(
      await screen.findByText(zhCN.mfa.recoveryCodes.showOnceTitle),
    ).toBeTruthy()
    expect(confirmCalls).toBe(3)
    expect(
      screen.queryByText(zhCN.errors.authn.mfa_invalid_code),
    ).toBeNull()

    await expectNoAxeViolations()
  })

  it('render a rate-limited confirm code over the still-open wizard', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === ENROLL_PATH) {
        return jsonResponse(200, ENROLL_BODY)
      }
      if (call.method === 'POST' && call.path === CONFIRM_PATH) {
        return errorResponse(429, 'authn.rate_limited')
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    await renderSection(rig.session)
    await openEnrollWizard()

    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.authenticator.codeLabel),
      CONFIRM_CODE,
    )
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.confirmLabel,
      }),
    )
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(zhCN.errors.authn.rate_limited)
    // The wizard stays and the field is usable again once the limiter
    // opens; the notice persists until the next action clears it.
    expect(screen.getByText(SECRET)).toBeTruthy()
    expect(screen.getByLabelText(zhCN.mfa.authenticator.codeLabel)).not.toBeDisabled()
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.cancelLabel,
      }),
    )
    expect(screen.getByRole('alert')).toBeTruthy()

    await expectNoAxeViolations()
  })

  it('regenerate behind the challenge: verified, the retry opens the show-once panel', async () => {
    let regenerateCalls = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === REGENERATE_PATH) {
        regenerateCalls += 1
        if (regenerateCalls === 1) {
          // The handler gates regeneration unconditionally.
          return errorResponse(403, 'authn.step_up_required')
        }
        return jsonResponse(200, CODES_BODY)
      }
      if (call.method === 'POST' && call.path === STEP_UP_PATH) {
        return jsonResponse(200, STEP_UP_BODY)
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    await renderSection(rig.session)

    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.recoveryCodes.regenerateButton,
      }),
    )
    expect(await screen.findByRole('dialog')).toBeTruthy()
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      CONFIRM_CODE,
    )
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.stepUp.confirmLabel,
      }),
    )
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    // The retry landed: the fresh batch shows once, no wizard involved.
    expect(await screen.findByText(zhCN.mfa.recoveryCodes.showOnceTitle)).toBeTruthy()
    for (const code of RECOVERY_CODES) {
      expect(screen.getByText(code)).toBeTruthy()
    }
    expect(regenerateCalls).toBe(2)
    expect(
      screen.queryByText(zhCN.mfa.authenticator.replacingNotice),
    ).toBeNull()

    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.recoveryCodes.savedLabel,
      }),
    )
    expect(
      screen.queryByText(zhCN.mfa.recoveryCodes.showOnceTitle),
    ).toBeNull()

    await expectNoAxeViolations()
  })

  it('render the mfa_not_enrolled guide text when the verified regenerate retry finds no factor', async () => {
    // A defensive leg: the authn module has no factor-disable operation,
    // so a step-up victory racing a factor that then vanished is not
    // reachable through today's API -- but the retry answers it, and the
    // section renders its guide text rather than a dead panel.
    let regenerateCalls = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === REGENERATE_PATH) {
        regenerateCalls += 1
        if (regenerateCalls === 1) {
          return errorResponse(403, 'authn.step_up_required')
        }
        return errorResponse(404, 'authn.mfa_not_enrolled')
      }
      if (call.method === 'POST' && call.path === STEP_UP_PATH) {
        return jsonResponse(200, STEP_UP_BODY)
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    await renderSection(rig.session)

    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.recoveryCodes.regenerateButton,
      }),
    )
    expect(await screen.findByRole('dialog')).toBeTruthy()
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      CONFIRM_CODE,
    )
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.stepUp.confirmLabel,
      }),
    )
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(zhCN.errors.authn.mfa_not_enrolled)
    // No codes panel and no wizard: the idle entries stay, the setup
    // entry is the path the guide text points at.
    expect(
      screen.queryByText(zhCN.mfa.recoveryCodes.showOnceTitle),
    ).toBeNull()
    expect(regenerateCalls).toBe(2)
    expect(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.enrollButton,
      }),
    ).toBeTruthy()
    expect(
      screen.getByRole('button', {
        name: zhCN.mfa.recoveryCodes.regenerateButton,
      }),
    ).toBeTruthy()

    await expectNoAxeViolations()
  })

  it('close the wizard and render its code text when the confirm answers a 409 race', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === ENROLL_PATH) {
        return jsonResponse(200, ENROLL_BODY)
      }
      if (call.method === 'POST' && call.path === CONFIRM_PATH) {
        // Another session confirmed the pending factor first.
        return errorResponse(409, 'authn.mfa_already_enrolled')
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    await renderSection(rig.session)
    await openEnrollWizard()

    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.authenticator.codeLabel),
      CONFIRM_CODE,
    )
    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.confirmLabel,
      }),
    )
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(zhCN.errors.authn.mfa_already_enrolled)
    // Nothing in the dead wizard can be retried: it closed and the
    // section returned to its idle entries.
    await waitFor(() => expect(screen.queryByText(SECRET)).toBeNull())
    expect(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.enrollButton,
      }),
    ).toBeTruthy()

    await expectNoAxeViolations()
  })

  it('render the original code text when the session died under an entry', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === REFRESH_PATH) {
        return errorResponse(401, 'authn.refresh_token_invalid')
      }
      if (call.method === 'POST' && call.path === ENROLL_PATH) {
        return errorResponse(401, 'authn.token_expired')
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    await renderSection(rig.session)

    await userEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.enrollButton,
      }),
    )
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(zhCN.errors.authn.token_expired)
    // No wizard, no dialog: the section stays idle for the host's
    // session gate to converge.
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.queryByText(SECRET)).toBeNull()

    await expectNoAxeViolations()
  })

  it('lock every action of a view while its mutation is in flight', async () => {
    let releaseEnroll: () => void = () => undefined
    const enrollGate = new Promise<void>((resolve) => {
      releaseEnroll = resolve
    })
    let releaseConfirm: () => void = () => undefined
    const confirmGate = new Promise<void>((resolve) => {
      releaseConfirm = resolve
    })
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === ENROLL_PATH) {
        await enrollGate
        return jsonResponse(200, ENROLL_BODY)
      }
      if (call.method === 'POST' && call.path === CONFIRM_PATH) {
        await confirmGate
        return jsonResponse(200, CODES_BODY)
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    await renderSection(rig.session)

    // An in-flight entry locks both idle actions.
    const enrollButton = screen.getByRole('button', {
      name: zhCN.mfa.authenticator.enrollButton,
    })
    const regenerateButton = screen.getByRole('button', {
      name: zhCN.mfa.recoveryCodes.regenerateButton,
    })
    fireEvent.click(enrollButton)
    await waitFor(() => expect(enrollButton).toBeDisabled())
    expect(regenerateButton).toBeDisabled()
    releaseEnroll()
    await screen.findByText(SECRET)

    // An in-flight confirm locks the wizard's confirm and cancel.
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.authenticator.codeLabel),
      CONFIRM_CODE,
    )
    fireEvent.click(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.confirmLabel,
      }),
    )
    const confirmButton = await screen.findByRole('button', {
      name: zhCN.mfa.authenticator.confirmLabel,
    })
    await waitFor(() => expect(confirmButton).toBeDisabled())
    expect(
      screen.getByRole('button', {
        name: zhCN.mfa.authenticator.cancelLabel,
      }),
    ).toBeDisabled()
    releaseConfirm()
    expect(
      await screen.findByText(zhCN.mfa.recoveryCodes.showOnceTitle),
    ).toBeTruthy()

    await expectNoAxeViolations()
  })
})
