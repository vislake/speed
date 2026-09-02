/**
 * Canonical language codes and the negotiation chain that picks a start
 * language for a new i18n instance.
 *
 * The chain (priority order, mirroring docs/internal/11-cross-cutting.md's
 * frontend rules): URL parameter, manually persisted choice, the signed-in
 * user's profile locale, navigator languages, then the default language.
 * Every source that matches no supported language is skipped; the default
 * language wins only when every source misses, and the default is always a
 * member of the supported set -- an unknown language never selects anything
 * (createI18n validates the default up front). Matching is by canonical tag
 * (case-insensitive, region-exact first), then by a unique language-only
 * tag ("en" selects "en-US" only when no other supported tag shares the
 * primary subtag).
 */

/** The canonical language tags the platform ships. zh-CN + en-US in M0. */
export const DEFAULT_SUPPORTED_LANGUAGES = ['zh-CN', 'en-US'] as const

/** Negotiation default: an unknown language resolves to zh-CN, never silently to English. */
export const DEFAULT_LANGUAGE = 'zh-CN'

/** True when the input parses as a BCP-47-style tag the negotiator can reason about. */
const TAG_PATTERN = /^[A-Za-z]{2,3}(?:[-_][A-Za-z0-9]{2,8})*$/

/**
 * Canonicalize a raw language string, or null when it is not a usable tag.
 * Underscores become hyphens ("zh_CN" -> "zh-CN"); casing is preserved and
 * compared case-insensitively, so "EN-us" still matches "en-US".
 */
export function normalizeLanguageTag(input: string): string | null {
  const trimmed = input.trim()
  if (!TAG_PATTERN.test(trimmed)) {
    return null
  }
  return trimmed.replaceAll('_', '-')
}

/**
 * Match a candidate against the supported set, or null when nothing
 * supports it. Exact canonical match first (case-insensitive); otherwise a
 * bare primary subtag ("en") selects the supported tag with that primary
 * subtag, but only when the choice is unambiguous.
 */
export function matchSupportedLanguage(
  candidate: string,
  supported: readonly string[],
): string | null {
  const normalized = normalizeLanguageTag(candidate)
  if (normalized === null) {
    return null
  }
  const lower = normalized.toLowerCase()
  for (const language of supported) {
    if (language.toLowerCase() === lower) {
      return language
    }
  }
  const primary = lower.split('-')[0] ?? lower
  const matches = supported.filter((language) =>
    language.toLowerCase().startsWith(`${primary}-`),
  )
  return matches.length === 1 ? matches[0]! : null
}

/** A source feeding the negotiation chain, in priority order. */
export interface DetectLanguageOptions {
  readonly supportedLanguages: readonly string[]
  /** Negotiation fallback; must be a member of supportedLanguages. */
  readonly defaultLanguage: string
  /** URL parameter value, already extracted; null/empty when absent. */
  readonly urlLanguage?: string | null
  /** Manually persisted choice, already extracted; null/empty when absent. */
  readonly storedLanguage?: string | null
  /** Signed-in user profile locale, resolved by the host; null/empty when absent. */
  readonly profileLanguage?: string | null
  /** Navigator language preferences, most preferred first. */
  readonly navigatorLanguages?: readonly string[]
}

/**
 * Run the negotiation chain and return the language to start with: the
 * first source that matches a supported language, else defaultLanguage.
 */
export function detectLanguage(options: DetectLanguageOptions): string {
  const { supportedLanguages, defaultLanguage } = options
  const candidates = [
    options.urlLanguage,
    options.storedLanguage,
    options.profileLanguage,
    ...(options.navigatorLanguages ?? []),
  ]
  for (const candidate of candidates) {
    if (candidate === null || candidate === undefined) {
      continue
    }
    const matched = matchSupportedLanguage(candidate, supportedLanguages)
    if (matched !== null) {
      return matched
    }
  }
  return defaultLanguage
}

/** Minimal structural view of an i18next instance's supported-language option. */
export interface SupportedLanguagesCarrier {
  readonly options: {
    readonly supportedLngs?: unknown
  }
}

/**
 * Read the supported-language set an instance was created with. Returns []
 * when the instance declares none -- the state registerNamespace and
 * switchLanguage both refuse, because per-language coverage and
 * no-cross-language-fallback guarantees are only meaningful against a
 * pinned set (createI18n always pins one).
 *
 * i18next internally extends a pinned supportedLngs with "cimode" (its
 * render-the-key-as-is meta language); that entry is a runtime escape
 * hatch, not a language the platform ships, so it is filtered out here --
 * coverage and switch validation must not demand cimode bundles.
 */
export function readSupportedLanguages(
  instance: SupportedLanguagesCarrier,
): readonly string[] {
  const raw = instance.options.supportedLngs
  const entries =
    Array.isArray(raw) ? (raw as readonly string[]) : typeof raw === 'string' && raw.length > 0 ? [raw] : []
  return entries.filter((entry) => entry.toLowerCase() !== 'cimode')
}
