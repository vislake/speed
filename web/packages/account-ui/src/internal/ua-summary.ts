/**
 * User-agent summarization for the session rows of the account surface.
 *
 * The sessions list exists for one job -- a person suspects their
 * account is being used by someone else, opens the list, finds the
 * session that is not theirs and ends it -- and a raw User-Agent string
 * cannot do that job: three sign-ins from one browser render as three
 * identical, truncated walls of text. The server answers the original
 * UA header of each sign-in verbatim, so this module turns that raw
 * string into the short readable form a row shows in its place: a
 * browser and an operating system from a fixed vocabulary -- "Chrome ·
 * macOS", "Safari · iPhone", "curl" -- the shape a person recognises a
 * session by.
 *
 * The vocabulary is deliberately bounded: the extractor matches stable
 * UA markers and answers only the mapped label, never a fragment of the
 * input, so no piece of the raw string can leak into a row. Detection
 * order is load-bearing twice over -- the Apple mobile tokens precede
 * the macOS marker because an iPhone or iPad UA embeds "like Mac OS X",
 * Android precedes Linux for the same embedded-token reason, and Safari
 * comes last among the browsers because a Chromium UA carries a
 * Safari/ token of its own -- and the order is pinned by the test
 * corpus. Every label is a proper noun that reads the same in every
 * language, so the summary carries no translatable text; a string in
 * which nothing is recognised answers null, and the caller falls back
 * to its own unknown-device label rather than to the raw string.
 */

/** Joins the recognised browser and OS into one row label. */
const SEPARATOR = ' · '

/**
 * Browser markers, most specific first. Each entry's label is the whole
 * answer -- never a substring of the UA -- so a row can never read as
 * the machine's own string.
 */
const BROWSER_MARKERS: ReadonlyArray<readonly [RegExp, string]> = [
  [/Edg(?:e|A|iOS)?\/\d/, 'Edge'],
  [/SamsungBrowser\//, 'Samsung Internet'],
  [/OPR\/|Opera\//, 'Opera'],
  [/FxiOS\/|Firefox\//, 'Firefox'],
  [/Chromium\/|CriOS\/|Chrome\//, 'Chrome'],
  [/curl\//, 'curl'],
  // Safari must come last: a Chromium UA carries its own Safari/ token,
  // so every earlier marker wins over it.
  [/Safari\//, 'Safari'],
]

/**
 * OS markers, most specific first. iPhone and iPad precede macOS
 * because their UA embeds "like Mac OS X"; Android precedes Linux
 * because an Android UA opens "(Linux; Android ...)".
 */
const OS_MARKERS: ReadonlyArray<readonly [RegExp, string]> = [
  [/Windows NT \d/, 'Windows'],
  [/CrOS/, 'ChromeOS'],
  [/\(iPhone;/, 'iPhone'],
  [/\(iPad;/, 'iPad'],
  [/Mac OS X|Macintosh/, 'macOS'],
  [/Android\b/, 'Android'],
  [/Linux\b/, 'Linux'],
]

/**
 * Summarises a raw User-Agent string into the short label a session row
 * shows: "Chrome · macOS", "Safari · iPhone", "curl". Answers null when
 * the string answers no recognisable browser or OS, so the caller can
 * fall back to its own unknown-device label -- the raw string is never
 * a fallback.
 */
export function summarizeUserAgent(ua: string): string | null {
  const parts: string[] = []
  for (const [marker, label] of BROWSER_MARKERS) {
    if (marker.test(ua)) {
      parts.push(label)
      break
    }
  }
  for (const [marker, label] of OS_MARKERS) {
    if (marker.test(ua)) {
      parts.push(label)
      break
    }
  }
  return parts.length > 0 ? parts.join(SEPARATOR) : null
}
