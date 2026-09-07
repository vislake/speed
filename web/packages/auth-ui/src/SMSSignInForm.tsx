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
 * -- a resend, or a request for a changed phone -- can sign in. The
 * spent-code memory clears only when a fresh code is actually issued:
 * a resend the send policy refuses (authn.rate_limited being the
 * likeliest refusal) issues none, and the exhausted code stays refused
 * locally rather than becoming resubmittable.
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
import type { SxProps, Theme } from '@mui/material'
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

/** The visually-hidden recipe for the sent-notice live region while
 * there is nothing to announce (the clip technique, the family's shape
 * -- see product-shell's sr-only region and ui-kit's ConfirmDialog
 * arming region): the region must stay in the accessibility tree to
 * announce, so it is clipped, never display:none -- a hidden region
 * would not be live. */
const srOnlyRegionSx: SxProps<Theme> = {
  clip: 'rect(0 0 0 0)',
  clipPath: 'inset(50%)',
  height: 1,
  margin: 0,
  overflow: 'hidden',
  position: 'absolute',
  whiteSpace: 'nowrap',
  width: 1,
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
  // next code request ISSUES a fresh code -- a request the server
  // refuses (rate-limited, transport) issues none, and this memory
  // survives it, for the old code is no less spent -- at which point a
  // re-submission of this string is refused locally with the used-code
  // notice instead of a doomed round-trip into the server's collapsed
  // invalid-code refusal. That reach -- this browser, until a fresh
  // code arrives -- is the whole boundary of this mitigation, a design
  // cost rather than a defect to fix by splitting (see the file
  // header): the server cannot tell this client its code was used
  // without telling every caller the same, so a refresh, a new tab or
  // another device re-submitting this same exhausted code gets the
  // server's collapsed answer, and the honest verdict is payable only
  // locally, only while the truth is still in this memory.
  const [spentCode, setSpentCode] = useState<string | null>(null)

  const requestCode = useCallback(
    async (phone: string) => {
      setErrorCode(null)
      // A request does not yet open a new code session: whether one
      // opens is the server's answer. The used-code notice clears for
      // the attempt, but the spent-code memory below is released only
      // once a fresh code was actually issued -- in the success path --
      // because a refused request (rate-limited, transport) leaves the
      // previous code spent: forgetting it here would send the
      // exhausted code into a doomed round-trip against the server's
      // collapsed invalid-code refusal on the very next submit.
      setCodeUsed(false)
      setSendingCode(true)
      try {
        await session.requestSMSCode({ phone })
        // A fresh code was just issued: a new code session opens now.
        // The previous session's spent-code memory describes a code
        // this request invalidates, and only on the issued code may it
        // clear -- never at request start, or a refused resend would
        // forget the old code is spent. Any code typed against the one
        // the new code replaces (a changed phone, a resend) is stale
        // too, so the code field starts empty for the new session.
        // Harmless no-op on the first send, where no code was ever
        // typed.
        setSpentCode(null)
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
    // Leaving the code step clears its attempt state: the code-entry
    // surface unmounts with it, and re-entering requires a code
    // request the server accepts -- a fresh code session, whatever the
    // requestCode attempt below starts, only opens on the issued code.
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
      {/* The sent-notice live region. It must never mount together with
          its text: a role="status" region announces only content changes
          that follow its own insertion, so a notice born in the same
          commit as the region (the accepted code request's step flip)
          would be silent. The region therefore stands for the whole
          life of the form -- mounted empty and visually silent (clipped,
          never display:none) while nothing was sent -- and the accepted
          request fills the text into a region the screen reader already
          knows, which is what makes the announcement fire. The clip and
          the notice never coexist: the same node shows the full info
          notice exactly while it carries the text, and a changed phone
          or a re-entry from the phone step fills the same standing node
          again. */}
      <Alert
        severity="info"
        role="status"
        sx={step === 'code' ? { width: '100%' } : srOnlyRegionSx}
      >
        {step === 'code'
          ? t('smsSignIn.sentNotice', { phone: sentTo ?? '' })
          : ''}
      </Alert>
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
