/**
 * InlineError: the shared failure banner for whole-attempt failures.
 *
 * A failed attempt resolves to one error code and renders it through
 * this banner instead of laying out per-field error text (field errors
 * belong to the fields; whole-attempt failures belong to one alert).
 * The banner itself is namespace-agnostic: the text comes from the
 * `resolve` prop, so each surface family binds its own namespace's
 * resolver and the English fallback an API returns for log triage can
 * never reach a user.
 *
 * errorCodeOf is the companion classifier: an ApiError-shaped failure
 * keeps its code, and anything else -- a bug-shaped throw, an
 * un-normalized answer -- collapses to a code that no resolver lists,
 * so the family renders its unknown fallback. The pair is fail-closed:
 * an operation that throws at all always has a code to show.
 */

import Alert from '@mui/material/Alert'

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
 * so a resolver treats it as unknown and renders its fallback.
 */
const UNKNOWN_FAILURE_CODE = 'client.unknown'

export interface InlineErrorProps {
  /** The code to render, or null for no error (renders nothing). */
  readonly code: string | null
  /**
   * Resolves a code to its current-language text. The surface family
   * binds its own namespace's resolver; the banner never guesses a
   * namespace or falls back across languages.
   */
  readonly resolve: (code: string) => string
}

/** The whole-attempt failure banner, one role="alert" per error. */
export function InlineError({ code, resolve }: InlineErrorProps) {
  if (code === null) {
    return null
  }
  return (
    <Alert severity="error" role="alert" sx={{ width: '100%' }}>
      {resolve(code)}
    </Alert>
  )
}
