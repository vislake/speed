/**
 * StepUpChallenge behaviour, driven over the real-client rig: the dialog
 * is controlled ({open, session, onSuccess, onCancel}) and its one
 * operation is session.verifyStepUp(code) -- a real session call over a
 * real api-client, so the 401-refresh leg a scripted answer triggers is
 * exercised by the client machinery itself. Success fires onSuccess
 * exactly once; cancel (button, Escape, backdrop) fires onCancel and
 * nothing else while nothing is in flight, and an in-flight verification
 * cannot be cancelled at all. Failure answers resolve through the code
 * whitelist: authn.mfa_invalid_code renders as the field's helper text
 * and stays retryable; every other reachable code (rate_limited,
 * mfa_not_enrolled, the session family) renders its code text in the
 * banner. Re-opening resets every piece of per-attempt state. Every
 * scenario ends with an axe pass.
 */

import { describe, expect, it } from 'vitest'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import {
  errorResponse,
  jsonResponse,
  makePair,
  makeRealClientRig,
  signInWithPassword,
} from '../../test-utils/real-client.js'
import { renderWithProviders } from '../../test-utils/render.js'
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { StepUpChallenge } from './step-up-challenge.js'

const LOGIN_PATH = '/api/v1/authn/login/password'
const REFRESH_PATH = '/api/v1/authn/token/refresh'
const STEP_UP_PATH = '/api/v1/authn/mfa/step-up'
const CODE = '123456'

/** The step-up 200: a fresh access token and no refresh token -- the
 * spec says a step-up reuses the caller's existing refresh token, so the
 * answer's token-issuing body simply omits it. */
const STEP_UP_BODY = {
  access_token: 'access-2',
  principal: {
    user_id: 'user-1',
    tenant_id: 'tenant-1',
    session_id: 'session-1',
  },
}

function verifyButtons() {
  const confirm = screen.getByRole('button', {
    name: zhCN.mfa.stepUp.confirmLabel,
  })
  const cancel = screen.getByRole('button', {
    name: zhCN.mfa.stepUp.cancelLabel,
  })
  return { confirm, cancel }
}

describe('StepUpChallenge', () => {
  it('verify a submitted code and fire onSuccess exactly once', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === STEP_UP_PATH) {
        return jsonResponse(200, STEP_UP_BODY)
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)

    let successCount = 0
    let cancelCount = 0
    renderWithProviders(
      <StepUpChallenge
        open
        session={rig.session}
        onSuccess={() => {
          successCount += 1
        }}
        onCancel={() => {
          cancelCount += 1
        }}
      />,
    )
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      CODE,
    )
    await userEvent.click(verifyButtons().confirm)
    await waitFor(() => expect(successCount).toBe(1))
    expect(cancelCount).toBe(0)
    expect(
      rig.calls.filter(
        (call) => call.method === 'POST' && call.path === STEP_UP_PATH,
      ),
    ).toHaveLength(1)
    expect(screen.queryByRole('alert')).toBeNull()

    await expectNoAxeViolations()
  })

  it('let Escape cancel an idle dialog and send nothing', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    let successCount = 0
    let cancelCount = 0
    renderWithProviders(
      <StepUpChallenge
        open
        session={rig.session}
        onSuccess={() => {
          successCount += 1
        }}
        onCancel={() => {
          cancelCount += 1
        }}
      />,
    )
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' })
    expect(cancelCount).toBe(1)
    expect(successCount).toBe(0)
    expect(
      rig.calls.filter(
        (call) => call.method === 'POST' && call.path === STEP_UP_PATH,
      ),
    ).toHaveLength(0)

    await expectNoAxeViolations()
  })

  it('close on the cancel button and send nothing', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    let successCount = 0
    let cancelCount = 0
    renderWithProviders(
      <StepUpChallenge
        open
        session={rig.session}
        onSuccess={() => {
          successCount += 1
        }}
        onCancel={() => {
          cancelCount += 1
        }}
      />,
    )
    await userEvent.click(verifyButtons().cancel)
    expect(cancelCount).toBe(1)
    expect(successCount).toBe(0)
    expect(
      rig.calls.filter(
        (call) => call.method === 'POST' && call.path === STEP_UP_PATH,
      ),
    ).toHaveLength(0)

    await expectNoAxeViolations()
  })

  it('lock the field, both actions and Escape while a verification is in flight', async () => {
    let releaseVerify: () => void = () => undefined
    const verifyGate = new Promise<void>((resolve) => {
      releaseVerify = resolve
    })
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === STEP_UP_PATH) {
        await verifyGate
        return jsonResponse(200, STEP_UP_BODY)
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    let successCount = 0
    let cancelCount = 0
    renderWithProviders(
      <StepUpChallenge
        open
        session={rig.session}
        onSuccess={() => {
          successCount += 1
        }}
        onCancel={() => {
          cancelCount += 1
        }}
      />,
    )
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      CODE,
    )
    fireEvent.click(verifyButtons().confirm)
    await waitFor(() =>
      expect(screen.getByRole('textbox')).toBeDisabled(),
    )
    expect(verifyButtons().confirm).toBeDisabled()
    expect(verifyButtons().cancel).toBeDisabled()
    // Neither Escape nor a second submit can abandon or duplicate the
    // in-flight verification.
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' })
    fireEvent.click(verifyButtons().cancel)
    fireEvent.click(verifyButtons().confirm)
    expect(cancelCount).toBe(0)
    await waitFor(() =>
      expect(screen.getByRole('button', { name: zhCN.mfa.stepUp.confirmLabel })).toBeDisabled(),
    )
    releaseVerify()
    await waitFor(() => expect(successCount).toBe(1))
    expect(cancelCount).toBe(0)
    expect(
      rig.calls.filter(
        (call) => call.method === 'POST' && call.path === STEP_UP_PATH,
      ),
    ).toHaveLength(1)

    await expectNoAxeViolations()
  })

  it('render a wrong code as the field error and stay retryable through the 401-refresh leg', async () => {
    let stepUpAttempts = 0
    let refreshCount = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === REFRESH_PATH) {
        refreshCount += 1
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === STEP_UP_PATH) {
        stepUpAttempts += 1
        // The wrong code is refused twice: the 401-refresh leg re-sends
        // the same request once with the fresh token, and the server
        // refuses it again -- only a genuinely different code succeeds.
        if (stepUpAttempts <= 2) {
          return errorResponse(401, 'authn.mfa_invalid_code')
        }
        return jsonResponse(200, STEP_UP_BODY)
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    let successCount = 0
    renderWithProviders(
      <StepUpChallenge
        open
        session={rig.session}
        onSuccess={() => {
          successCount += 1
        }}
        onCancel={() => {
          throw new Error('unexpected cancel')
        }}
      />,
    )
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      '000000',
    )
    await userEvent.click(verifyButtons().confirm)
    // The 401 rode the silent refresh leg before the answer surfaced.
    await waitFor(() =>
      expect(
        screen.getByText(zhCN.errors.authn.mfa_invalid_code),
      ).toBeTruthy(),
    )
    expect(refreshCount).toBe(1)
    expect(successCount).toBe(0)
    // A retry with the fresh code succeeds in the same dialog.
    await userEvent.clear(screen.getByLabelText(zhCN.mfa.stepUp.codeLabel))
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      CODE,
    )
    await userEvent.click(verifyButtons().confirm)
    await waitFor(() => expect(successCount).toBe(1))
    expect(stepUpAttempts).toBe(3)
    expect(screen.queryByText(zhCN.errors.authn.mfa_invalid_code)).toBeNull()

    await expectNoAxeViolations()
  })

  it('render the rate limiter text in the banner and keep the dialog open', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === STEP_UP_PATH) {
        return errorResponse(429, 'authn.rate_limited')
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    let successCount = 0
    renderWithProviders(
      <StepUpChallenge
        open
        session={rig.session}
        onSuccess={() => {
          successCount += 1
        }}
        onCancel={() => {
          throw new Error('unexpected cancel')
        }}
      />,
    )
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      CODE,
    )
    await userEvent.click(verifyButtons().confirm)
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(zhCN.errors.authn.rate_limited)
    expect(successCount).toBe(0)
    // The dialog stays: the answer can be retried once the limiter opens.
    expect(screen.getByRole('dialog')).toBeTruthy()
    expect(screen.getByRole('textbox')).not.toBeDisabled()

    await expectNoAxeViolations()
  })

  it('render mfa_not_enrolled in the banner when no factor backs the account', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === STEP_UP_PATH) {
        return errorResponse(404, 'authn.mfa_not_enrolled')
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    let successCount = 0
    renderWithProviders(
      <StepUpChallenge
        open
        session={rig.session}
        onSuccess={() => {
          successCount += 1
        }}
        onCancel={() => {
          throw new Error('unexpected cancel')
        }}
      />,
    )
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      CODE,
    )
    await userEvent.click(verifyButtons().confirm)
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(zhCN.errors.authn.mfa_not_enrolled)
    expect(successCount).toBe(0)

    await expectNoAxeViolations()
  })

  it('render the original code text when the session died under the attempt', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === REFRESH_PATH) {
        // The session is gone server-side: the refresh itself is refused.
        return errorResponse(401, 'authn.refresh_token_invalid')
      }
      if (call.method === 'POST' && call.path === STEP_UP_PATH) {
        return errorResponse(401, 'authn.token_expired')
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    let successCount = 0
    renderWithProviders(
      <StepUpChallenge
        open
        session={rig.session}
        onSuccess={() => {
          successCount += 1
        }}
        onCancel={() => {
          throw new Error('unexpected cancel')
        }}
      />,
    )
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      CODE,
    )
    await userEvent.click(verifyButtons().confirm)
    const alert = await screen.findByRole('alert')
    // The refused request's own code, not the refresh's: the api-client
    // surfaces the original envelope when the refresh leg fails.
    expect(alert.textContent).toBe(zhCN.errors.authn.token_expired)
    expect(successCount).toBe(0)

    await expectNoAxeViolations()
  })

  it('reset every piece of per-attempt state on re-open', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === REFRESH_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'POST' && call.path === STEP_UP_PATH) {
        return errorResponse(401, 'authn.mfa_invalid_code')
      }
      return errorResponse(500, 'internal')
    })
    await signInWithPassword(rig)
    let successCount = 0
    let cancelCount = 0
    const { rerender } = renderWithProviders(
      <StepUpChallenge
        open
        session={rig.session}
        onSuccess={() => {
          successCount += 1
        }}
        onCancel={() => {
          cancelCount += 1
        }}
      />,
    )
    await userEvent.type(
      screen.getByLabelText(zhCN.mfa.stepUp.codeLabel),
      '000000',
    )
    await userEvent.click(verifyButtons().confirm)
    await waitFor(() =>
      expect(
        screen.getByText(zhCN.errors.authn.mfa_invalid_code),
      ).toBeTruthy(),
    )
    // Close and re-open: the typed code and the field error are gone, so
    // the next attempt starts clean.
    rerender(
      <StepUpChallenge
        open={false}
        session={rig.session}
        onSuccess={() => {
          successCount += 1
        }}
        onCancel={() => {
          cancelCount += 1
        }}
      />,
    )
    expect(screen.queryByRole('dialog')).toBeNull()
    rerender(
      <StepUpChallenge
        open
        session={rig.session}
        onSuccess={() => {
          successCount += 1
        }}
        onCancel={() => {
          cancelCount += 1
        }}
      />,
    )
    const input = screen.getByLabelText(
      zhCN.mfa.stepUp.codeLabel,
    ) as HTMLInputElement
    expect(input.value).toBe('')
    expect(
      screen.queryByText(zhCN.errors.authn.mfa_invalid_code),
    ).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
    // The confirm stays inert on an empty code: the 401 attempt plus its
    // silent-refresh retry is all the traffic the first opening produced.
    fireEvent.click(verifyButtons().confirm)
    await waitFor(() => expect(cancelCount).toBe(0))
    expect(successCount).toBe(0)
    expect(
      rig.calls.filter(
        (call) => call.method === 'POST' && call.path === STEP_UP_PATH,
      ),
    ).toHaveLength(2)

    await expectNoAxeViolations()
  })
})
