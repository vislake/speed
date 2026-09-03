/**
 * MfaSection: the two-step-verification (TOTP authenticator + recovery
 * codes) surface of the account page.
 *
 * The authn module ships no factor-status operation and no disable
 * operation, and its sign-in path is deliberately not gated on a factor
 * -- verification is a pure step-up mechanism, used to prove the caller
 * is themselves before security-sensitive operations. The section
 * therefore never declares "enabled" or "disabled": state is discovered
 * through actions, and every action that the server gates behind a
 * step-up answers 403 authn.step_up_required when the caller's current
 * access token carries no fresh second-factor proof.
 *
 * Two entry actions, each running through the same discover-by-acting
 * machine:
 *
 *  - Set up an authenticator (useAuthnEnrollTOTP). A 200 answer means no
 *    active factor existed and the enrollment is pending: the wizard
 *    opens showing the secret and the provisioning URI (both rendered as
 *    text -- the package ships no QR dependency and no clipboard
 *    mechanism, so manual entry is the supported path), then a six-digit
 *    confirm (useAuthnConfirmTOTP) makes the factor active and the
 *    confirm answer's recovery codes open the show-once panel. A 403
 *    means an active factor does exist: the step-up dialog opens, and
 *    only its success path re-runs the enrollment -- that 403 is the one
 *    reliable signal an active factor exists, so the replacement warning
 *    ("this replaces your existing authenticator") renders only when the
 *    wizard was reached through the step-up, never on a first setup.
 *
 *  - Regenerate recovery codes (useAuthnRegenerateRecoveryCodes). The
 *    handler gates this unconditionally, so an unelevated caller gets 403
 *    whether or not a factor exists: the step-up dialog opens, and its
 *    success path re-runs the regeneration. The retry then answers
 *    either 200 (the show-once codes panel opens) or -- when no active
 *    factor actually backs the account, a race the step-up cannot rule
 *    out -- 404 authn.mfa_not_enrolled, which renders its guide text and
 *    points at the setup entry above.
 *
 * Confirm answers that end the wizard (409 authn.mfa_already_enrolled
 * when another session confirmed the pending factor first, 404 when it
 * vanished) close the wizard and render their code text; a wrong code is
 * a field-level error and stays retryable; everything else (the shared
 * rate limiter, the session-lifecycle family, client.*) renders its code
 * text banner. The show-once recovery-codes panel is the only place the
 * codes ever appear (they are served in plaintext exactly once): it
 * shows the ten codes and a single "I have saved them" exit, and leaving
 * it resets all state -- nothing in this package caches or re-fetches
 * codes, so the only way to see codes again is to regenerate.
 *
 * Orchestration with the challenge dialog lives entirely inside this
 * section: StepUpChallenge is controlled ({open, session, onSuccess,
 * onCancel}) and the pending action is remembered so a success retries
 * exactly the gated operation and a cancel retries nothing. The session
 * prop exists for that dialog; the enroll/confirm/regenerate calls are
 * plain generated mutations travelling on the access token in the
 * shared store.
 */

import { useState } from 'react'
import type { FormEvent } from 'react'
import Alert from '@mui/material/Alert'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import TextField from '@mui/material/TextField'
import Typography from '@mui/material/Typography'
import {
  useAuthnConfirmTOTP,
  useAuthnEnrollTOTP,
  useAuthnRegenerateRecoveryCodes,
} from '@speed/api-sdk'
import type { AuthSession } from '@speed/auth-core'
import { useAccountUiErrorText } from './internal/error-text.js'
import { errorCodeOf, InlineError } from './internal/inline-error.js'
import { StepUpChallenge } from './internal/step-up-challenge.js'
import { useAccountUiTranslation } from './internal/translation.js'

export interface MfaSectionProps {
  /** The session that verifies the step-up challenge. */
  readonly session: AuthSession
}

/** The action a step-up challenge, once won, re-runs. */
type GatedAction = 'enroll' | 'regenerate'

/** The in-progress enrollment: what the wizard shows and how it was
 * reached (the replacement warning renders only for the 403-reached
 * wizard). The fields are text because no QR/clipboard mechanism
 * ships; the server always answers both, kept optional-string so an
 * empty value simply renders no row. */
interface EnrollWizard {
  readonly secret: string
  readonly provisioningUri: string
  readonly replacing: boolean
}

export function MfaSection({ session }: MfaSectionProps) {
  const { t } = useAccountUiTranslation()
  const resolve = useAccountUiErrorText()
  const enrollMutation = useAuthnEnrollTOTP()
  const confirmMutation = useAuthnConfirmTOTP()
  const regenerateMutation = useAuthnRegenerateRecoveryCodes()

  // The step-up gate: non-null while the challenge dialog is open and
  // names the action its success re-runs.
  const [gate, setGate] = useState<GatedAction | null>(null)
  // The open enrollment, if any.
  const [wizard, setWizard] = useState<EnrollWizard | null>(null)
  // The show-once recovery codes, if the confirm/regenerate answered.
  const [codes, setCodes] = useState<readonly string[] | null>(null)
  // The wizard's confirm field.
  const [confirmCode, setConfirmCode] = useState('')
  // Wrong-code answer: renders under the field, stays retryable.
  const [confirmFieldError, setConfirmFieldError] = useState<string | null>(
    null,
  )
  // Every other failure answer: code text above the current view.
  const [banner, setBanner] = useState<string | null>(null)

  const confirmBusy = confirmMutation.isPending
  const entryBusy =
    enrollMutation.isPending ||
    confirmMutation.isPending ||
    regenerateMutation.isPending

  /**
   * Run the enroll operation. A 403 on the idle entry opens the gate
   * (the retry then runs with replacing=true and never re-gates); a 403
   * on the retry itself is not gateable again and renders its text.
   */
  async function startEnroll(afterStepUp: boolean): Promise<void> {
    setBanner(null)
    setConfirmCode('')
    setConfirmFieldError(null)
    try {
      const response = await enrollMutation.mutateAsync()
      setWizard({
        secret: response.secret ?? '',
        provisioningUri: response.provisioning_uri ?? '',
        replacing: afterStepUp,
      })
    } catch (error) {
      const failure = errorCodeOf(error)
      if (failure === 'authn.step_up_required' && !afterStepUp) {
        setGate('enroll')
        return
      }
      setBanner(failure)
    }
  }

  /** Run the regenerate operation; 403 on the idle entry opens the gate. */
  async function regenerateCodes(afterStepUp: boolean): Promise<void> {
    setBanner(null)
    try {
      const response = await regenerateMutation.mutateAsync()
      setCodes(response.recovery_codes ?? [])
    } catch (error) {
      const failure = errorCodeOf(error)
      if (failure === 'authn.step_up_required' && !afterStepUp) {
        setGate('regenerate')
        return
      }
      setBanner(failure)
    }
  }

  function handleStepUpSuccess(): void {
    const action = gate
    setGate(null)
    if (action === 'enroll') {
      void startEnroll(true)
    } else if (action === 'regenerate') {
      void regenerateCodes(true)
    }
  }

  function handleStepUpCancel(): void {
    // No retry: the gated action stays unrun and the section returns to
    // its idle entries.
    setGate(null)
  }

  async function handleConfirmSubmit(event: FormEvent): Promise<void> {
    event.preventDefault()
    if (confirmBusy) {
      return
    }
    const code = confirmCode.trim()
    if (code === '') {
      return
    }
    setConfirmFieldError(null)
    try {
      const response = await confirmMutation.mutateAsync({ data: { code } })
      setWizard(null)
      setConfirmCode('')
      setBanner(null)
      setCodes(response.recovery_codes ?? [])
    } catch (error) {
      const failure = errorCodeOf(error)
      if (failure === 'authn.mfa_invalid_code') {
        // Wrong code: the wizard stays, the field error is retryable.
        setConfirmFieldError(failure)
        return
      }
      if (
        failure === 'authn.mfa_already_enrolled' ||
        failure === 'authn.mfa_not_enrolled'
      ) {
        // The pending factor is gone: another session confirmed it (409)
        // or it no longer exists (404). Nothing in this wizard can be
        // retried -- close it and render the answer's code text.
        setWizard(null)
        setConfirmCode('')
      }
      setBanner(failure)
    }
  }

  function closeWizard(): void {
    setWizard(null)
    setConfirmCode('')
    setConfirmFieldError(null)
  }

  /** "I have saved them": the only exit of the show-once panel. */
  function handleCodesSaved(): void {
    setCodes(null)
    setBanner(null)
  }

  return (
    <Box>
      <Box sx={{ mb: 2 }}>
        <Typography variant="h5" component="h2">
          {t('mfa.title')}
        </Typography>
      </Box>

      <InlineError code={banner} />

      {codes !== null ? (
        <Box sx={{ py: 1.5 }}>
          <Typography variant="body1" sx={{ fontWeight: 500 }}>
            {t('mfa.recoveryCodes.showOnceTitle')}
          </Typography>
          <Typography
            variant="body2"
            color="text.secondary"
            sx={{ mt: 0.5, mb: 1.5 }}
          >
            {t('mfa.recoveryCodes.showOnceDescription')}
          </Typography>
          <Box
            component="ul"
            sx={{ m: 0, mb: 2, pl: 0, listStyle: 'none' }}
          >
            {codes.map((code) => (
              <Box component="li" key={code}>
                <Typography
                  variant="body2"
                  sx={{ fontFamily: 'monospace' }}
                >
                  {code}
                </Typography>
              </Box>
            ))}
          </Box>
          <Button variant="contained" onClick={handleCodesSaved}>
            {t('mfa.recoveryCodes.savedLabel')}
          </Button>
        </Box>
      ) : wizard !== null ? (
        <Box sx={{ py: 1.5 }}>
          {wizard.replacing && (
            <Alert severity="warning" sx={{ mb: 2 }}>
              {t('mfa.authenticator.replacingNotice')}
            </Alert>
          )}
          <Typography variant="body2" color="text.secondary" sx={{ mb: 1.5 }}>
            {t('mfa.authenticator.enrollDescription')}
          </Typography>
          {wizard.secret !== '' && (
            <Box sx={{ mb: 1.5 }}>
              <Typography variant="body2" color="text.secondary">
                {t('mfa.authenticator.secretLabel')}
              </Typography>
              <Typography
                variant="body2"
                sx={{
                  fontFamily: 'monospace',
                  wordBreak: 'break-all',
                  mt: 0.5,
                }}
              >
                {wizard.secret}
              </Typography>
            </Box>
          )}
          {wizard.provisioningUri !== '' && (
            <Box sx={{ mb: 1.5 }}>
              <Typography variant="body2" color="text.secondary">
                {t('mfa.authenticator.uriLabel')}
              </Typography>
              <Typography
                variant="body2"
                sx={{
                  fontFamily: 'monospace',
                  wordBreak: 'break-all',
                  mt: 0.5,
                }}
              >
                {wizard.provisioningUri}
              </Typography>
            </Box>
          )}
          <Box component="form" onSubmit={handleConfirmSubmit}>
            <TextField
              label={t('mfa.authenticator.codeLabel')}
              value={confirmCode}
              onChange={(event) => setConfirmCode(event.target.value)}
              error={confirmFieldError !== null}
              helperText={
                confirmFieldError !== null
                  ? resolve(confirmFieldError)
                  : undefined
              }
              disabled={confirmBusy}
              autoComplete="one-time-code"
              fullWidth
              sx={{ maxWidth: 360 }}
            />
            <Box sx={{ display: 'flex', gap: 1.5, mt: 2 }}>
              <Button
                type="submit"
                variant="contained"
                disabled={confirmBusy || confirmCode.trim() === ''}
              >
                {t('mfa.authenticator.confirmLabel')}
              </Button>
              <Button onClick={closeWizard} disabled={confirmBusy}>
                {t('mfa.authenticator.cancelLabel')}
              </Button>
            </Box>
          </Box>
        </Box>
      ) : (
        <Box>
          <Box sx={{ py: 1.5 }}>
            <Typography variant="h6" component="h3">
              {t('mfa.authenticator.title')}
            </Typography>
            <Typography
              variant="body2"
              color="text.secondary"
              sx={{ mt: 0.5, mb: 1.5 }}
            >
              {t('mfa.authenticator.description')}
            </Typography>
            <Button
              variant="outlined"
              disabled={entryBusy}
              onClick={() => void startEnroll(false)}
            >
              {t('mfa.authenticator.enrollButton')}
            </Button>
          </Box>
          <Box
            sx={{
              py: 1.5,
              borderTop: '1px solid',
              borderColor: 'divider',
            }}
          >
            <Typography variant="h6" component="h3">
              {t('mfa.recoveryCodes.title')}
            </Typography>
            <Typography
              variant="body2"
              color="text.secondary"
              sx={{ mt: 0.5, mb: 1.5 }}
            >
              {t('mfa.recoveryCodes.description')}
            </Typography>
            <Button
              variant="outlined"
              disabled={entryBusy}
              onClick={() => void regenerateCodes(false)}
            >
              {t('mfa.recoveryCodes.regenerateButton')}
            </Button>
          </Box>
        </Box>
      )}

      <StepUpChallenge
        open={gate !== null}
        session={session}
        onSuccess={handleStepUpSuccess}
        onCancel={handleStepUpCancel}
      />
    </Box>
  )
}
