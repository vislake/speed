/**
 * InlineError: the code-level failure banner of the account surface.
 *
 * Every failure path of the surface resolves its failure to one error
 * code and renders it through this banner instead of laying out per-field
 * error text (frontend standards: field errors belong to the fields,
 * whole-attempt failures belong to one alert). The code resolves to
 * current-language text through the error-text resolver -- whitelisted
 * codes (authn.*, client.*) have their own errors-section key, anything
 * else renders the errors.unknown fallback, so no raw key and no
 * cross-language fallback can ever appear.
 *
 * errorCodeOf is the surface's failure classifier: an ApiError-shaped
 * failure keeps its code, and anything else -- a bug-shaped throw, an
 * un-normalized answer -- collapses to a code that is deliberately not
 * whitelisted, so the resolver renders the unknown fallback. The banner
 * is fail-closed: an operation that throws at all always has a code to
 * show.
 */

import Alert from '@mui/material/Alert'
import { useAccountUiErrorText } from './error-text.js'

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

/** The surface's whole-attempt failure banner, one role="alert" per error. */
export function InlineError({ code }: InlineErrorProps) {
  const resolve = useAccountUiErrorText()
  if (code === null) {
    return null
  }
  return (
    <Alert severity="error" role="alert" sx={{ width: '100%' }}>
      {resolve(code)}
    </Alert>
  )
}
