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
 *
 * A submit whose login lost a concurrent race (another sign-in -- the
 * SMS channel's, a social exchange, a second instance of this form --
 * committed to the session while this one was in flight) answers with
 * auth-core's OperationSupersededError, not a failure: the losing
 * submit renders no error banner and fires no onSignedIn (the winning
 * call fires its own exactly once), and the session being
 * authenticated now is the host's own snapshot to observe through its
 * auth-core hooks.
 */

import { useState } from 'react'
import TextField from '@mui/material/TextField'
import Button from '@mui/material/Button'
import { useForm } from 'react-hook-form'
import type { SubmitHandler } from 'react-hook-form'
import { errorCodeOf, FormField, FormLayout } from '@speed/ui-kit'
import type { AuthSession } from '@speed/auth-core'
import { isOperationSuperseded } from '@speed/auth-core'
import { useAuthUiTranslation } from './internal/translation.js'
import { InlineError } from './internal/inline-error.js'

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
    } catch (error) {
      if (isOperationSuperseded(error)) {
        // This submit lost a concurrent sign-in race (see the file
        // header): a lost race is not a failure -- no error banner,
        // and no onSignedIn, which the winning call already fired
        // exactly once. The session being authenticated now is the
        // host's own snapshot to observe.
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
