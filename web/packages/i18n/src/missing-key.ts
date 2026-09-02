/**
 * The missing-key discipline: a translation key that does not exist must
 * never silently render another language's text, and it must be visible.
 *
 * Mechanics: createI18n pins supportedLngs, disables cross-language
 * fallback (fallbackLng: false, load: "currentOnly") and installs a
 * missingKeyHandler, so a missing key renders as the key itself and fires
 * this package's handler -- the visible warning below by default, whatever
 * the host passes as onMissingKey otherwise. registerNamespace's parity and
 * coverage checks are the companion guarantee: keys cannot go missing in
 * one language while another has them. This mirrors go/pkgcore/i18n's
 * design (never let the underlying catalog silently fall back across
 * languages); see README.md's "Missing keys".
 */

/** Everything the handler knows about one missing translation key. */
export interface MissingKeyDetails {
  /** Canonical language tags the lookup ran for. */
  readonly languages: readonly string[]
  /** The namespace the key was looked up in. */
  readonly namespace: string
  /** The key as requested, dot-joined when nested. */
  readonly key: string
}

/** Default handler: a console warning, prefixed so it is greppable. */
export function defaultMissingKeyHandler(details: MissingKeyDetails): void {
  const { languages, namespace, key } = details
  console.warn(
    `[speed-i18n] missing translation key "${key}" in namespace "${namespace}" ` +
      `for language(s): ${languages.join(', ')}; no fallback to another language is ` +
      'performed and the key is rendered as-is. Add the key to every language ' +
      'resource of the namespace.',
  )
}

/**
 * Adapt the host's onMissingKey option (or the default handler) to
 * i18next's missingKeyHandler signature.
 */
export function missingKeyHandlerFactory(
  onMissingKey?: (details: MissingKeyDetails) => void,
): (lngs: readonly string[], namespace: string, key: string) => void {
  const handle = onMissingKey ?? defaultMissingKeyHandler
  return (lngs, namespace, key): void => {
    handle({ languages: lngs, namespace, key })
  }
}
