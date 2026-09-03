/**
 * RegisterForm: the registration channel of the sign-in family.
 *
 * One identifier field accepts an email or a phone number -- the '@'
 * heuristic decides which slot the request carries (the spec's separated
 * email/phone shape, never a single ambiguous identifier field). The
 * optional display name is trimmed and omitted when blank; the locale the
 * request declares is the session's current UI language, read at submit
 * time so a mid-flight language switch is honoured. Password policy
 * lives on the backend, whose code-level answers (authn.password_too_short
 * and friends) render through the one InlineError banner.
 *
 * Registration never signs in (register is not a session operation): the
 * created user is handed to onRegistered -- the host navigates to its
 * sign-in screen -- or, when no callback is given, rendered as a success
 * panel in place of the form. Nothing here navigates, and the heading
 * above the form is host content.
 */

import { useState } from 'react'
import Alert from '@mui/material/Alert'
import AlertTitle from '@mui/material/AlertTitle'
import TextField from '@mui/material/TextField'
import Button from '@mui/material/Button'
import { useForm } from 'react-hook-form'
import type { SubmitHandler } from 'react-hook-form'
import { FormLayout, FormField } from '@speed/ui-kit'
import type { AuthSession } from '@speed/auth-core'
import type { AuthnRegisterRequest, AuthnUser } from '@speed/api-sdk'
import { useAuthUiTranslation } from './internal/translation.js'
import { InlineError, errorCodeOf } from './internal/inline-error.js'

export interface RegisterFormProps {
  /** The session the registration drives. */
  readonly session: AuthSession
  /**
   * Receives the created user once; with a callback the form stays quiet
   * (the host navigates to its sign-in screen). Without one the form
   * renders a success panel in place.
   */
  readonly onRegistered?: (user: AuthnUser) => void
}

interface RegisterFields {
  identifier: string
  password: string
  displayName: string
}

/** The '@' heuristic: an email carries one, a phone number never does. */
function isEmail(identifier: string): boolean {
  return identifier.includes('@')
}

export function RegisterForm({ session, onRegistered }: RegisterFormProps) {
  const { t, i18n } = useAuthUiTranslation()
  const form = useForm<RegisterFields>({
    defaultValues: { identifier: '', password: '', displayName: '' },
  })
  const [created, setCreated] = useState<AuthnUser | null>(null)
  const [errorCode, setErrorCode] = useState<string | null>(null)

  const onSubmit: SubmitHandler<RegisterFields> = async ({
    identifier,
    password,
    displayName,
  }) => {
    setErrorCode(null)
    const request: AuthnRegisterRequest = {
      password,
      locale: i18n.language,
    }
    if (isEmail(identifier)) {
      request.email = identifier
    } else {
      request.phone = identifier
    }
    const name = displayName.trim()
    if (name.length > 0) {
      request.display_name = name
    }
    try {
      const user = await session.register(request)
      if (onRegistered !== undefined) {
        onRegistered(user)
      } else {
        setCreated(user)
      }
    } catch (error) {
      setErrorCode(errorCodeOf(error))
    }
  }

  if (created !== null) {
    return (
      <Alert severity="success" role="status" sx={{ width: '100%' }}>
        <AlertTitle>{t('register.successTitle')}</AlertTitle>
        {t('register.successMessage')}
      </Alert>
    )
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
          {t('register.submit')}
        </Button>
      }
    >
      <FormField
        name="identifier"
        required
        render={({ field, invalid, errorText }) => (
          <TextField
            {...field}
            label={t('register.identifierLabel')}
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
            label={t('register.passwordLabel')}
            type="password"
            autoComplete="new-password"
            fullWidth
            error={invalid}
            helperText={errorText ?? undefined}
          />
        )}
      />
      <FormField
        name="displayName"
        render={({ field, invalid, errorText }) => (
          <TextField
            {...field}
            label={t('register.displayNameLabel')}
            autoComplete="nickname"
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
