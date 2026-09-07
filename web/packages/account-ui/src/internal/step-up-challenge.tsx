/**
 * StepUpChallenge: the re-verification dialog of the step-up-gated
 * surfaces (MfaSection opens it when an operation answers 403
 * authn.step_up_required).
 *
 * The dialog is controlled -- {open, session, onSuccess, onCancel} -- and
 * asks one thing: a single code that verifies the caller is themselves
 * right now. The field accepts either a six-digit TOTP code or one
 * recovery code; the authn module's step-up operation shape-dispatches
 * between them, so the component validates nothing about the value
 * client-side. Submitting drives session.verifyStepUp(code), the session
 * operation that owns the token lifecycle: a successful verification
 * settles a fresh access token whose amr carries the just-verified factor
 * and calls onSuccess (nothing about the token is the dialog's business;
 * it never promises that verification will not be asked again, because
 * the elevation lives only in that access token's lifetime).
 *
 * Failure answers resolve through the code whitelist. The server splits
 * spent codes from wrong ones: a code that was never valid
 * (authn.mfa_invalid_code -- a wrong TOTP code, or a recovery code no
 * issued row matches) renders as a field-level error and stays
 * retryable; a code that WAS valid but is consumed (authn.mfa_code_used
 * -- the replay guard already advanced past its step, or its
 * recovery-code row is marked used) renders its code text above the
 * field through the InlineError banner, the field is cleared, and the
 * dialog asks for a fresh code -- the spent code itself is never
 * retryable, and never called invalid. Every other reachable code --
 * authn.rate_limited, the session-lifecycle family, authn.mfa_not_enrolled,
 * the client.* transport codes -- renders its code text above the field
 * through the InlineError banner too, never a raw key and never an API
 * message. The dialog never signs in and never navigates: a dead session
 * shows its code text and the host's session gate converges.
 *
 * While a verification is in flight the field, the verify button and the
 * cancel affordance (button, Escape, backdrop) are all disabled, so an
 * in-flight answer cannot be double-submitted or abandoned mid-rotation.
 * Re-opening the dialog resets every piece of per-attempt state.
 *
 * A verification that lost a concurrent-operation race (a sibling
 * step-up -- or a tenant switch -- committed to the session while this
 * code was in flight) answers with auth-core's
 * OperationSupersededError: the submitted code genuinely verified
 * server-side -- which is exactly why it is now SPENT, its single-use
 * guard consumed by this very 2xx -- but the elevation never landed:
 * the winner's session stands without it, and the losing pair was never
 * applied. No onSuccess fires (re-running the gated operation under an
 * elevation that does not exist would only draw a fresh 403 from the
 * server). The dialog renders the same used-code text a re-submitted
 * consumed code draws and clears the field: the attempt is NOT
 * retryable with the same code -- the next submit can only verify with
 * a fresh one. (Pre-fix, the dialog stayed silent and told the caller
 * the code was intact and retryable; the code was in fact consumed, and
 * the ensuing re-submit drew the invalid-code answer forever.)
 */

import { useEffect, useId, useState } from 'react'
import type { FormEvent } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Dialog from '@mui/material/Dialog'
import DialogActions from '@mui/material/DialogActions'
import DialogContent from '@mui/material/DialogContent'
import DialogTitle from '@mui/material/DialogTitle'
import TextField from '@mui/material/TextField'
import Typography from '@mui/material/Typography'
import type { AuthSession } from '@speed/auth-core'
import { isOperationSuperseded } from '@speed/auth-core'
import { useAccountUiErrorText } from './error-text.js'
import { errorCodeOf, InlineError } from './inline-error.js'
import { useAccountUiTranslation } from './translation.js'

export interface StepUpChallengeProps {
  /** Whether the dialog is showing. Opening resets its per-attempt state. */
  readonly open: boolean
  /** The session whose verifyStepUp drives the verification. */
  readonly session: AuthSession
  /** Fired once when a submitted code verifies. The host closes the dialog. */
  readonly onSuccess: () => void
  /** Fired on cancel while nothing is in flight. The host does not retry. */
  readonly onCancel: () => void
}

export function StepUpChallenge({
  open,
  session,
  onSuccess,
  onCancel,
}: StepUpChallengeProps) {
  const { t } = useAccountUiTranslation()
  const resolve = useAccountUiErrorText()
  const titleId = useId()
  // The submit action sits outside the form -- the MUI dialog layout
  // keeps the actions below the content -- and reaches it through this
  // id, so a plain submit button still drives the form's submit path.
  // The id is useId-derived, never hand-written, like titleId above:
  // two challenge dialogs on one page (tests, or a host rendering two
  // surfaces) can never collide on a shared document id.
  const formId = useId()

  const [code, setCode] = useState('')
  const [submitting, setSubmitting] = useState(false)
  // The wrong-code answer renders under the field; any other failure
  // renders as the banner above it.
  const [fieldError, setFieldError] = useState<string | null>(null)
  const [banner, setBanner] = useState<string | null>(null)

  // Each opening starts a clean attempt: the previous run's code, errors
  // and in-flight state must never leak into the next challenge.
  useEffect(() => {
    if (open) {
      setCode('')
      setSubmitting(false)
      setFieldError(null)
      setBanner(null)
    }
  }, [open])

  if (!open) {
    return null
  }

  async function handleSubmit(event: FormEvent): Promise<void> {
    event.preventDefault()
    if (submitting) {
      return
    }
    const value = code.trim()
    if (value === '') {
      return
    }
    setSubmitting(true)
    setFieldError(null)
    setBanner(null)
    try {
      await session.verifyStepUp(value)
      onSuccess()
    } catch (error) {
      if (isOperationSuperseded(error)) {
        // This verification lost a concurrent-operation race (see the
        // file header): the code verified server-side -- and is
        // therefore spent -- but the elevation never landed. The code
        // is never retryable: render the used-code answer, clear the
        // field and ask for a fresh code. No onSuccess: re-running the
        // gated operation under an elevation that does not exist would
        // only draw a fresh 403 from the server.
        setCode('')
        setBanner('authn.mfa_code_used')
        return
      }
      const failure = errorCodeOf(error)
      if (failure === 'authn.mfa_invalid_code') {
        // A never-valid code: wrong, or no issued row matches. The
        // attempt is retryable with a fresh code -- the field error
        // stays, the typed code stays for editing.
        setFieldError(failure)
      } else if (failure === 'authn.mfa_code_used') {
        // A code that was valid and is spent: its single-use guard
        // already consumed it, so it can never verify again. Clear the
        // field and render the used-code answer -- never "invalid",
        // never retryable-with-this-code.
        setCode('')
        setBanner(failure)
      } else {
        setBanner(failure)
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      open={open}
      // Any close request (Escape, backdrop) is a cancel while nothing
      // is in flight; an in-flight verification cannot be abandoned.
      onClose={() => {
        if (!submitting) {
          onCancel()
        }
      }}
      aria-labelledby={titleId}
      maxWidth="xs"
      fullWidth
    >
      <DialogTitle id={titleId}>{t('mfa.stepUp.title')}</DialogTitle>
      <DialogContent>
        <Typography variant="body2" color="text.secondary">
          {t('mfa.stepUp.description')}
        </Typography>
        <Box
          component="form"
          id={formId}
          onSubmit={handleSubmit}
          sx={{ mt: 2 }}
        >
          <TextField
            label={t('mfa.stepUp.codeLabel')}
            value={code}
            onChange={(event) => setCode(event.target.value)}
            error={fieldError !== null}
            helperText={
              fieldError !== null ? resolve(fieldError) : undefined
            }
            disabled={submitting}
            autoComplete="one-time-code"
            fullWidth
            autoFocus
          />
          <InlineError code={banner} />
        </Box>
      </DialogContent>
      <DialogActions>
        <Button onClick={onCancel} disabled={submitting}>
          {t('mfa.stepUp.cancelLabel')}
        </Button>
        <Button
          type="submit"
          form={formId}
          variant="contained"
          disabled={submitting || code.trim() === ''}
        >
          {t('mfa.stepUp.confirmLabel')}
        </Button>
      </DialogActions>
    </Dialog>
  )
}
