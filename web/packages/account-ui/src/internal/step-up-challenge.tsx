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
 * Failure answers resolve through the code whitelist: a wrong code
 * (authn.mfa_invalid_code, collapsed with replay and unknown-recovery-
 * code answers on the server side) renders as a field-level error and
 * stays retryable; every other reachable code -- authn.rate_limited,
 * the session-lifecycle family, authn.mfa_not_enrolled, the client.*
 * transport codes -- renders its code text above the field through the
 * InlineError banner, never a raw key and never an API message. The
 * dialog never signs in and never navigates: a dead session shows its
 * code text and the host's session gate converges.
 *
 * While a verification is in flight the field, the verify button and the
 * cancel affordance (button, Escape, backdrop) are all disabled, so an
 * in-flight answer cannot be double-submitted or abandoned mid-rotation.
 * Re-opening the dialog resets every piece of per-attempt state.
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
      const failure = errorCodeOf(error)
      if (failure === 'authn.mfa_invalid_code') {
        setFieldError(failure)
      } else {
        setBanner(failure)
      }
    } finally {
      setSubmitting(false)
    }
  }

  // The form lives in the content; the actions sit below it and reach it
  // by id, keeping the MUI dialog layout while a plain submit button
  // still drives the form's submit path.
  const formId = 'step-up-challenge-form'

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
