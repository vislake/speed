/**
 * The validation-error text contract of the form family (FormField,
 * FormLayout): one resolution rule, applied at render time so the
 * displayed text always follows the current language.
 *
 * An error message coming out of react-hook-form is either:
 *
 *  - a key that exists in the ui-kit namespace (the form family's own
 *    built-ins -- 'form.required', 'form.invalid' -- or any other ui-kit
 *    key), which renders as its translation in the active language, or
 *
 *  - anything else -- already-localized text a host validation function
 *    returned, a host-specific error code, a value from a future
 *    generated-types resolver -- which renders verbatim. ui-kit owns one
 *    namespace and resolves keys in it only; it never guesses another
 *    namespace's codes. Host error codes arrive as their own text when
 *    the resolver layer that turns codes into text exists (a later
 *    milestone, alongside validation derived from generated types).
 */

export interface ValidationErrorLookup {
  /** Key-existence check scoped to the ui-kit namespace. */
  readonly exists: (key: string) => boolean
  /** Translation renderer for the ui-kit namespace in the active language. */
  readonly t: (key: string) => string
}

/**
 * Resolve a field error message to its display text: null for no
 * error, the ui-kit-namespace translation when the message is such a
 * key, the message itself otherwise.
 */
export function resolveValidationError(
  message: string | undefined | null,
  { exists, t }: ValidationErrorLookup,
): string | null {
  if (message === undefined || message === null || message === '') {
    return null
  }
  return exists(message) ? t(message) : message
}
