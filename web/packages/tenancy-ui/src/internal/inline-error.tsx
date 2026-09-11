/**
 * InlineError: the code-level failure banner of the tenant-switch
 * affordance.
 *
 * A failed switch resolves to one error code and renders through this
 * banner instead of inline per-row text: the whole-attempt failure of a
 * switch belongs to one alert (frontend standards), the retry to the
 * same row. The banner itself -- the alert shell and the failure
 * classifier -- is ui-kit's shared InlineError / errorCodeOf; this
 * module is the affordance's binding of it to the tenancy-ui
 * namespace's error-text resolver, so call sites keep passing a bare
 * code and the resolved text can only ever come from this namespace.
 */

import { InlineError as SharedInlineError } from '@speed/ui-kit'
import { useTenancyUiErrorText } from './error-text.js'

export interface InlineErrorProps {
  /** The code to render, or null for no error (renders nothing). */
  readonly code: string | null
}

/** The switch failure banner, one role="alert" per error. */
export function InlineError({ code }: InlineErrorProps) {
  const resolve = useTenancyUiErrorText()
  return <SharedInlineError code={code} resolve={resolve} />
}
