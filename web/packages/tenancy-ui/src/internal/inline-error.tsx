/**
 * InlineError: the code-level failure banner of the tenant-switch
 * affordance.
 *
 * A failed switch resolves to one error code and renders through this
 * banner instead of inline per-row text: the whole-attempt failure of a
 * switch belongs to one alert (frontend standards), the retry to the
 * same row. The code resolves to current-language text through the
 * error-text resolver -- whitelisted codes (authn.*, client.*) have their
 * own errors-section key, anything else renders the errors.unknown
 * fallback, so no raw key and no cross-language fallback can ever appear.
 *
 * This file is a deliberate copy of auth-ui's inline-error.tsx at the
 * same tier (packages at one tier cannot import one another, so each
 * ships its own banner): errorCodeOf and InlineError behave identically,
 * resolving through this package's own error-text hook. A fix to the
 * classifier belongs in both copies.
 */

import Alert from '@mui/material/Alert'
import { useTenancyUiErrorText } from './error-text.js'

/** Collapses an arbitrary thrown value to the code InlineError renders. */
export function errorCodeOf(error: unknown): string {
  if (typeof error !== 'object' || error === null) {
    return UNKNOWN_FAILURE_CODE
  }
  const code = (error as { code?: unknown }).code
  return typeof code === 'string' && code.length > 0
    ? code
    : UNKNOWN_FAILURE_CODE
}

/**
 * The collapse target of a failure that is not an ApiError-shaped
 * answer. Not a code @speed/api-client ever emits (its failures are
 * client.network / client.timeout / client.protocol / client.http.*),
 * so the error-text resolver treats it as unknown and renders the
 * errors.unknown fallback.
 */
const UNKNOWN_FAILURE_CODE = 'client.unknown'

export interface InlineErrorProps {
  /** The code to render, or null for no error (renders nothing). */
  readonly code: string | null
}

/** The switch failure banner, one role="alert" per error. */
export function InlineError({ code }: InlineErrorProps) {
  const resolve = useTenancyUiErrorText()
  if (code === null) {
    return null
  }
  return (
    <Alert severity="error" role="alert" sx={{ width: '100%' }}>
      {resolve(code)}
    </Alert>
  )
}
