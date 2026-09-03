/**
 * SMSSignInForm: the SMS-code channel of the sign-in family.
 *
 * A two-step react-hook-form flow over the session's SMS operations: the
 * phone step requests a code (requestSMSCode, whose 202 acceptance -- and
 * the account-existence ambiguity the endpoint answers with -- is the
 * phone step's terminal state), then the code step completes the sign-in
 * with loginWithSMSCode. The sent notice announces the receiving number;
 * resend repeats the request against the same number, and changing the
 * phone returns to the first step. The request step answers the code the
 * server accepts or does not: authn.rate_limited is its only code-shaped
 * failure, and every failure renders through the one InlineError banner.
 *
 * Busy states: the code-request button disables for the request's flight,
 * the code step's submit disables while the login commits (RHF's
 * isSubmitting). Nothing navigates; a successful login fires onSignedIn
 * once and the host decides what follows. The heading above the form is
 * host content.
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

  const requestCode = useCallback(
    async (phone: string) => {
      setErrorCode(null)
      setSendingCode(true)
      try {
        await session.requestSMSCode({ phone })
        setSentTo(phone)
        setStep('code')
      } catch (error) {
        setErrorCode(errorCodeOf(error))
      } finally {
        setSendingCode(false)
      }
    },
    [session],
  )

  const onSubmit: SubmitHandler<SmsFields> = async (values) => {
    if (step === 'phone') {
      // The phone step's submit is the code request; the code field is
      // not mounted yet, so RHF validates the phone alone.
      await requestCode(values.phone)
      return
    }
    setErrorCode(null)
    try {
      await session.loginWithSMSCode({
        phone: sentTo ?? values.phone,
        code: values.code,
      })
      onSignedIn?.()
    } catch (error) {
      setErrorCode(errorCodeOf(error))
    }
  }

  const editPhone = (): void => {
    setErrorCode(null)
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
      <InlineError code={errorCode} />
    </FormLayout>
  )
}
