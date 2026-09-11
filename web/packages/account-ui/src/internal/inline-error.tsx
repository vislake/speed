/**
 * InlineError: the code-level failure banner of the account surface.
 *
 * Every failure path of the surface resolves its failure to one error
 * code and renders it through this banner instead of laying out per-field
 * error text (frontend standards: field errors belong to the fields,
 * whole-attempt failures belong to one alert). The banner itself -- the
 * alert shell and the failure classifier -- is ui-kit's shared
 * InlineError / errorCodeOf; this module is the surface's binding of it
 * to the account-ui namespace's error-text resolver, so call sites keep
 * passing a bare code and the resolved text can only ever come from this
 * namespace (whitelisted codes have their own errors-section key,
 * anything else renders the errors.unknown fallback).
 */

import { InlineError as SharedInlineError } from '@speed/ui-kit'
import { useAccountUiErrorText } from './error-text.js'

export interface InlineErrorProps {
  /** The code to render, or null for no error (renders nothing). */
  readonly code: string | null
}

/** The surface's whole-attempt failure banner, one role="alert" per error. */
export function InlineError({ code }: InlineErrorProps) {
  const resolve = useAccountUiErrorText()
  return <SharedInlineError code={code} resolve={resolve} />
}
