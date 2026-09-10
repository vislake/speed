/**
 * RegisterForm: the registration channel of the sign-in family.
 *
 * One identifier field accepts an email or a phone number -- the '@'
 * heuristic decides which slot the request carries (the spec's separated
 * email/phone shape, never a single ambiguous identifier field). The
 * optional display name is trimmed and omitted when blank; the locale the
 * request declares is the session's current UI language, read at submit
 * time so a mid-flight language switch is honoured, and the timezone is
 * the browser's own report (guarded; see browserTimeZone) -- both are the
 * backend's registration initial-value chains' first tiers. Password policy and
 * identifier canonical form live on the backend, whose code-level
 * answers -- authn.password_too_short and friends, and the
 * identifier-format refusals authn.invalid_email / authn.invalid_phone --
 * render through the one InlineError banner.
 *
 * Registration never signs in (register is not a session operation): the
 * created user is handed to onRegistered -- the host navigates to its
 * sign-in screen -- or, when no callback is given, rendered as a success
 * panel in place of the form. The callback runs only after the
 * register verdict settled, and a throwing host callback is contained:
 * it is not a registration failure (the account exists server-side),
 * never renders the failure banner -- which would
 * invite a duplicate resubmission of an already-registered account --
 * and never escapes as an unhandled rejection. Nothing here navigates,
 * and the heading above the form is host content.
 */

import { useState } from 'react'
import Alert from '@mui/material/Alert'
import AlertTitle from '@mui/material/AlertTitle'
import Box from '@mui/material/Box'
import TextField from '@mui/material/TextField'
import Button from '@mui/material/Button'
import type { SxProps, Theme } from '@mui/material'
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

/** The visually-hidden recipe for the success live region while there is
 * nothing to announce (the clip technique, the family's shape -- see
 * product-shell's sr-only region and ui-kit's ConfirmDialog arming
 * region): the region must stay in the accessibility tree to announce,
 * so it is clipped, never display:none -- a hidden region would not be
 * live. */
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

interface RegisterFields {
  identifier: string
  password: string
  displayName: string
}

/** The '@' heuristic: an email carries one, a phone number never does. */
function isEmail(identifier: string): boolean {
  return identifier.includes('@')
}

/**
 * The browser's IANA timezone, reported as the registration's initial
 * timezone -- the backend's registration timezone chain's first tier (the
 * browser report), read at submit time beside the locale.
 *
 * Guarded, and undefined rather than fatal when the environment cannot
 * answer (a non-browser test renderer, an engine whose Intl is trimmed):
 * the backend validates this field leniently BY CONTRACT -- an absent or
 * unknown value is stored empty (the platform default UTC applies) and
 * never refuses the registration -- so failing or refusing to submit over
 * an unavailable timezone would be stricter than the backend itself. An
 * empty answer is treated the same as no answer: nothing to report.
 */
function browserTimeZone(): string | undefined {
  try {
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone
    return zone === '' ? undefined : zone
  } catch {
    return undefined
  }
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
    const timezone = browserTimeZone()
    if (timezone !== undefined) {
      request.timezone = timezone
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
    let user: AuthnUser
    try {
      user = await session.register(request)
    } catch (error) {
      setErrorCode(errorCodeOf(error))
      return
    }
    if (onRegistered !== undefined) {
      try {
        onRegistered(user)
      } catch {
        // A throwing host callback is not a registration failure (the
        // account was created): nothing here renders an error -- which
        // would invite a duplicate resubmission -- and the containment
        // keeps the throw out of this submit promise.
      }
      return
    }
    setCreated(user)
  }

  const fields = (
    <>
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
    </>
  )
  const formLayout = (
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
      {fields}
    </FormLayout>
  )

  // With a host callback the form announces nothing: registration hands
  // the created user to the host, which navigates away from this
  // surface, and no panel ever replaces the form.
  if (onRegistered !== undefined) {
    return formLayout
  }

  // The success live region. It must never mount together with its
  // text: a role="status" region announces only content changes that
  // follow its own insertion, so a success panel born in the same
  // commit as the region would be silent. The region therefore stands
  // for the whole no-callback life of the form -- mounted empty and
  // visually silent (clipped, never display:none) while there is
  // nothing to announce -- and the created commit fills the text into a
  // region the screen reader already knows, which is what makes the
  // announcement fire. The clip and the success panel never coexist:
  // the same node shows the full panel exactly while it carries the
  // text.
  return (
    <Box sx={{ width: '100%' }}>
      <Alert
        severity="success"
        role="status"
        sx={created !== null ? { width: '100%' } : srOnlyRegionSx}
      >
        {created !== null ? (
          <>
            <AlertTitle>{t('register.successTitle')}</AlertTitle>
            {t('register.successMessage')}
          </>
        ) : null}
      </Alert>
      {created !== null ? null : formLayout}
    </Box>
  )
}
