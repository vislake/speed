/**
 * SMSSignInForm: the SMS-code channel of the sign-in family.
 *
 * A two-step react-hook-form flow over the session's SMS operations: the
 * phone step requests a code (requestSMSCode, whose 202 acceptance -- and
 * the account-existence ambiguity the endpoint answers with -- is the
 * phone step's terminal state), then the code step completes the sign-in
 * with loginWithSMSCode. The sent notice announces the receiving number;
 * resend repeats the request against the same number, and changing the
 * phone returns to the first step. The request step renders the code the
 * server answers: authn.invalid_phone when the number has no E.164 form
 * (no leading '+' and country code) and authn.rate_limited when the
 * attempt trips the send policy, each through the one InlineError
 * banner.
 *
 * Busy states: the code-request button disables for the request's flight,
 * the code step's submit disables while the login commits (RHF's
 * isSubmitting). A fresh code starts the code field empty: a successful
 * request (the first send, a resend, or a request for a changed phone)
 * resets any code typed against the code it invalidates, so a stale
 * code can never ride along to a new code session. Nothing navigates; a
 * successful login fires onSignedIn once and the host decides what
 * follows -- the callback runs only after the login verdict settled,
 * and a throwing host callback is contained (it is not a login
 * failure, never renders an error, never escapes as an unhandled
 * rejection). The heading above the form is host content.
 *
 * A code-step submit whose login lost a concurrent race (the password
 * channel's, a social exchange, a second instance of a sign-in form
 * committed to the session while this one was in flight) answers with
 * auth-core's OperationSupersededError, not a failure: the losing
 * submit fires no onSignedIn (the winning call fires its own exactly
 * once), and the session being authenticated now is the host's own
 * snapshot to observe through its auth-core hooks. One consequence the
 * password channel never faces, the SMS code step must hear: a
 * phone-login code is single-use server-side, so the losing submit's
 * own 2xx spent the very code it verified even though the login never
 * landed. The form therefore never advertises that code as retryable:
 * it renders the used-code notice, clears the code field, and
 * remembers the exact spent code -- re-submitting that string is
 * answered locally with the same notice rather than sent into the
 * server's deliberately collapsed invalid-code refusal (authn answers
 * a spent login code exactly like a wrong one), and only a fresh code
 * -- a resend, or a request for a changed phone -- can sign in.
 *
 * The collapse is deliberate, and it is the pre-authentication half of
 * an asymmetry the step-up/MFA channel's honest spent-vs-never-valid
 * split (go/authn's ErrMFACodeUsed against ErrMFAInvalidCode) must not
 * be copied onto this endpoint: step-up answers a caller who is
 * already authenticated, so telling that caller a code was used rather
 * than wrong discloses nothing an outsider can use. A phone-login code
 * verifies a caller who is not yet anyone -- the server cannot tell
 * the code's true holder, whose earlier submit consumed it, from a
 * caller who merely holds a candidate code -- and an answer
 * distinguishing "used" from "wrong" would certify to an
 * unauthenticated caller that the code they submitted was the genuine
 * live one, an oracle that confirms an intercepted or phished code.
 * The honest "used" verdict can therefore live only where the truth is
 * already known without asking the server: this form's spent-code
 * memory, whose reach the comment on spentCode below states.
 * go/authn's ErrVerificationCodeInvalid (errors.go) carries the same
 * analysis from the server's side and points back here.
 */

import { useCallback, useState } from 'react'
import Alert from '@mui/material/Alert'
import Box from '@mui/material/Box'
import TextField from '@mui/material/TextField'
import Button from '@mui/material/Button'
import { useForm } from 'react-hook-form'
import type { SubmitHandler } from 'react-hook-form'
import { FormLayout, FormField } from '@speed/ui-kit'
import type { AuthSession } from '@speed/auth-core'
import { isOperationSuperseded } from '@speed/auth-core'
import { useAuthUiTranslation } from './internal/translation.js'
import { InlineError, errorCodeOf } from './internal/inline-error.js'

export interface SMSSignInFormProps {
  /** The session the SMS sign-in drives. */
  readonly session: AuthSession
  /** Fired once after an SMS-code login commits; the host navigates. */
  readonly onSignedIn?: () => void
}

interface SmsFields {
  phone: string
  code: string
}

type Step = 'phone' | 'code'

export function SMSSignInForm({ session, onSignedIn }: SMSSignInFormProps) {
  const { t } = useAuthUiTranslation()
  const form = useForm<SmsFields>({
    defaultValues: { phone: '', code: '' },
  })
  const [step, setStep] = useState<Step>('phone')
  const [sentTo, setSentTo] = useState<string | null>(null)
  const [sendingCode, setSendingCode] = useState(false)
  const [errorCode, setErrorCode] = useState<string | null>(null)
  // The used-code notice: the current code session's last submit lost
  // a concurrent sign-in race after its code verified server-side --
  // the code is SPENT, and only a fresh one can sign in (see the file
  // header) -- or re-submitted the very code one did. Renders in the
  // code step, never while an error is showing.
  const [codeUsed, setCodeUsed] = useState(false)
  // The exact code a superseded answer proved spent, kept until the
  // next code request starts a new code session: a re-submission of
  // this string is refused locally with the used-code notice instead
  // of a doomed round-trip into the server's collapsed invalid-code
  // refusal. That reach -- this browser, until the next code request --
  // is the whole boundary of this mitigation, a design cost rather
  // than a defect to fix by splitting (see the file header): the
  // server cannot tell this client its code was used without telling
  // every caller the same, so a refresh, a new tab or another device
  // re-submitting this same exhausted code gets the server's collapsed
  // answer, and the honest verdict is payable only locally, only while
  // the truth is still in this memory.
  const [spentCode, setSpentCode] = useState<string | null>(null)

  const requestCode = useCallback(
    async (phone: string) => {
      setErrorCode(null)
      // A fresh code starts a new code session: the previous one's
      // used-code notice and spent-code memory describe a code the new
      // request invalidates.
      setCodeUsed(false)
      setSpentCode(null)
      setSendingCode(true)
      try {
        await session.requestSMSCode({ phone })
        // A fresh code was just issued: any code typed against the one
        // it replaces (a changed phone, a resend) is stale, so the code
        // field starts empty for the new session. Harmless no-op on
        // the first send, where no code was ever typed.
        form.resetField('code', { defaultValue: '' })
        setSentTo(phone)
        setStep('code')
      } catch (error) {
        setErrorCode(errorCodeOf(error))
      } finally {
        setSendingCode(false)
      }
    },
    [form, session],
  )

  const onSubmit: SubmitHandler<SmsFields> = async (values) => {
    if (step === 'phone') {
      // The phone step's submit is the code request; the code field is
      // not mounted yet, so RHF validates the phone alone.
      await requestCode(values.phone)
      return
    }
    setErrorCode(null)
    setCodeUsed(false)
    if (values.code !== '' && values.code === spentCode) {
      // The very code the superseded answer proved spent (see the file
      // header) is being submitted again: answer with the used-code
      // notice and clear it -- never a doomed round-trip into the
      // server's collapsed invalid-code refusal, never an endless
      // retry of a dead code.
      form.resetField('code', { defaultValue: '' })
      setCodeUsed(true)
      return
    }
    try {
      await session.loginWithSMSCode({
        phone: sentTo ?? values.phone,
        code: values.code,
      })
    } catch (error) {
      if (isOperationSuperseded(error)) {
        // This submit lost a concurrent sign-in race (see the file
        // header): its own request genuinely verified the code
        // server-side -- which is exactly why it is now SPENT, its
        // single-use guard consumed by this very 2xx -- but the login
        // never landed. The code is never retryable: render the
        // used-code notice, clear the field and remember the spent
        // code, so a re-submission of this exact string is answered
        // locally instead of drawing the server's collapsed
        // invalid-code refusal. No onSignedIn: the winning call fired
        // its own exactly once, and the session being authenticated
        // now is the host's own snapshot to observe.
        form.resetField('code', { defaultValue: '' })
        setSpentCode(values.code)
        setCodeUsed(true)
        return
      }
      setErrorCode(errorCodeOf(error))
      return
    }
    try {
      onSignedIn?.()
    } catch {
      // A throwing host callback is not a login failure: the login
      // committed, nothing here renders the host's error, and the
      // containment keeps the throw out of this submit promise.
    }
  }

  const editPhone = (): void => {
    setErrorCode(null)
    // Leaving the code step clears its attempt state: a re-entry
    // always runs requestCode, which starts a fresh code session.
    setCodeUsed(false)
    setSpentCode(null)
    setStep('phone')
  }

  return (
    <FormLayout
      form={form}
      onSubmit={onSubmit}
      actions={
        step === 'phone' ? (
          <Button type="submit" variant="contained" disabled={sendingCode}>
            {t('smsSignIn.sendCode')}
          </Button>
        ) : (
          <Button
            type="submit"
            variant="contained"
            disabled={form.formState.isSubmitting}
          >
            {t('smsSignIn.submit')}
          </Button>
        )
      }
    >
      {step === 'phone' ? (
        <FormField
          name="phone"
          required
          render={({ field, invalid, errorText }) => (
            <TextField
              {...field}
              label={t('smsSignIn.phoneLabel')}
              type="tel"
              autoComplete="tel"
              fullWidth
              error={invalid}
              helperText={errorText ?? undefined}
            />
          )}
        />
      ) : (
        <>
          <Alert severity="info" role="status" sx={{ width: '100%' }}>
            {t('smsSignIn.sentNotice', { phone: sentTo ?? '' })}
          </Alert>
          <FormField
            name="code"
            required
            render={({ field, invalid, errorText }) => (
              <TextField
                {...field}
                label={t('smsSignIn.codeLabel')}
                inputMode="numeric"
                autoComplete="one-time-code"
                fullWidth
                error={invalid}
                helperText={errorText ?? undefined}
              />
            )}
          />
          <Box sx={{ display: 'flex', gap: 1 }}>
            <Button size="small" onClick={() => void requestCode(sentTo ?? '')} disabled={sendingCode}>
              {t('smsSignIn.resendCode')}
            </Button>
            <Button size="small" onClick={editPhone}>
              {t('smsSignIn.editPhone')}
            </Button>
          </Box>
        </>
      )}
      {codeUsed ? (
        <Alert severity="error" role="alert" sx={{ width: '100%' }}>
          {t('smsSignIn.codeUsed')}
        </Alert>
      ) : (
        <InlineError code={errorCode} />
      )}
    </FormLayout>
  )
}
