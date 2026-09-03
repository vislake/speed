/**
 * PasswordSignInForm: the password channel of the sign-in family.
 *
 * A controlled react-hook-form flow over the session's
 * loginWithPassword operation: identifier (email or phone -- the backend
 * decides which) and password, rendered through ui-kit's FormLayout and
 * FormField skeleton. The skeleton owns the <form>, the validation-error
 * rendering and the actions row; this component owns the submission, the
 * busy state (RHF's isSubmitting while the login is in flight) and the
 * whole-attempt failure banner (one InlineError per failed submit, code
 * resolved to current-language text).
 *
 * Nothing here navigates and nothing reads the session state: a
 * successful login fires onSignedIn once and the host decides what
 * happens next. The heading above the form is host content -- the
 * component renders no title of its own.
 */

import { useState } from 'react'
import TextField from '@mui/material/TextField'
import Button from '@mui/material/Button'
import { useForm } from 'react-hook-form'
import type { SubmitHandler } from 'react-hook-form'
import { FormLayout, FormField } from '@speed/ui-kit'
import type { AuthSession } from '@speed/auth-core'
import { useAuthUiTranslation } from './internal/translation.js'
import { InlineError, errorCodeOf } from './internal/inline-error.js'

export interface PasswordSignInFormProps {
  /** The session the password login drives. */
  readonly session: AuthSession
  /** Fired once after a password login commits; the host navigates. */
  readonly onSignedIn?: () => void
}

interface PasswordFields {
  identifier: string
  password: string
}

export function PasswordSignInForm({
  session,
  onSignedIn,
}: PasswordSignInFormProps) {
  const { t } = useAuthUiTranslation()
  const form = useForm<PasswordFields>({
    defaultValues: { identifier: '', password: '' },
  })
  const [errorCode, setErrorCode] = useState<string | null>(null)

  const onSubmit: SubmitHandler<PasswordFields> = async ({
    identifier,
    password,
  }) => {
    setErrorCode(null)
    try {
      await session.loginWithPassword({ identifier, password })
      onSignedIn?.()
    } catch (error) {
      setErrorCode(errorCodeOf(error))
    }
  }

  return (
    <FormLayout
      form={form}
      onSubmit={onSubmit}
      actions={
        <Button
          type="submit"
          variant="contained"
          disabled={form.formState.isSubmitting}
        >
          {t('passwordSignIn.submit')}
        </Button>
      }
    >
      <FormField
        name="identifier"
        required
        render={({ field, invalid, errorText }) => (
          <TextField
            {...field}
            label={t('passwordSignIn.identifierLabel')}
            autoComplete="username"
            fullWidth
            error={invalid}
            helperText={errorText ?? undefined}
          />
        )}
      />
      <FormField
        name="password"
        required
        render={({ field, invalid, errorText }) => (
          <TextField
            {...field}
            label={t('passwordSignIn.passwordLabel')}
            type="password"
            autoComplete="current-password"
            fullWidth
            error={invalid}
            helperText={errorText ?? undefined}
          />
        )}
      />
      <InlineError code={errorCode} />
    </FormLayout>
  )
}
