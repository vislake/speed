/**
 * summarizeUserAgent, the raw-User-Agent-to-readable-summary mapping of
 * the sessions surface.
 *
 * The corpus pins the two hazards that would silently break the surface
 * it feeds: detection ORDER (an iPhone or iPad UA embeds "like Mac OS
 * X", an Android UA opens "(Linux; ...", and a Chromium UA carries a
 * Safari/ token of its own -- each precedence rule exists because a
 * wrong order answers the wrong label, which the raw strings below all
 * contain) and the bounded-vocabulary rule (every answer is a mapped
 * label, never a fragment of the input). The gate that accepted the
 * original defect names the machine substrings that only ever appear in
 * a raw UA (Mozilla/, AppleWebKit, Gecko, curl/, Safari/); the
 * corpus-wide check below holds the same line for every sample this
 * module claims to understand, so a future marker that parsed into a
 * row would fail here first.
 */

import { describe, expect, it } from 'vitest'
import { summarizeUserAgent } from './ua-summary.js'

/** Substrings that only ever appear in a raw User-Agent string -- the
 * e2e gate's own vocabulary (examples/reference-app/web/e2e/
 * sessions-are-distinguishable.spec.ts), mirrored here so the unit tier
 * holds the same line as the browser tier. */
const MACHINE_STRINGS = ['Mozilla/', 'AppleWebKit', 'Gecko', 'curl/', 'Safari/']

/**
 * Real user agents in the shapes servers actually store, each with the
 * label the surface should show instead. The order traps are inside
 * the strings: the iPhone and iPad samples embed "like Mac OS X", the
 * Android samples embed "(Linux; ", and the Chromium samples carry a
 * Safari/ token of their own.
 */
const CORPUS: ReadonlyArray<readonly [string, string]> = [
  // Desktop Chrome -- the e2e chromium project's own shape.
  [
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36',
    'Chrome · macOS',
  ],
  // Desktop Safari -- the e2e webkit project's own shape.
  [
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15',
    'Safari · macOS',
  ],
  // iPhone Safari -- an iPad-shaped trap: "like Mac OS X" must not win.
  [
    'Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1',
    'Safari · iPhone',
  ],
  // iPad Safari, the e2e ipad project's own shape.
  [
    'Mozilla/5.0 (iPad; CPU OS 14_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.7 Mobile/15E148 Safari/604.1',
    'Safari · iPad',
  ],
  // Android Chrome -- a Linux-shaped trap: the "(Linux; " opener must
  // not win over the Android token behind it.
  [
    'Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/116.0.0.0 Mobile Safari/537.36',
    'Chrome · Android',
  ],
  // Desktop Edge -- a Chromium UA, but Edge is what a person calls it.
  [
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.2592.61',
    'Edge · Windows',
  ],
  // Firefox on macOS -- its UA carries the Gecko token, which must
  // never surface in the answer.
  [
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:127.0) Gecko/20100101 Firefox/127.0',
    'Firefox · macOS',
  ],
  // Firefox on Android -- the Android token sits at the very front.
  [
    'Mozilla/5.0 (Android 14; Mobile; rv:126.0) Gecko/126.0 Firefox/126.0',
    'Firefox · Android',
  ],
  // Opera (Chromium-era).
  [
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36 OPR/111.0.0.0',
    'Opera · Windows',
  ],
  // Samsung Internet (Chromium-based).
  [
    'Mozilla/5.0 (Linux; Android 13; SM-G991B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/23.0 Chrome/115.0.0.0 Mobile Safari/537.36',
    'Samsung Internet · Android',
  ],
  // Chrome OS.
  [
    'Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36',
    'Chrome · ChromeOS',
  ],
  // Plain Linux desktop Chrome.
  [
    'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36',
    'Chrome · Linux',
  ],
  // A command-line client: a browser-only answer, never "curl/8.4.0".
  ['curl/8.4.0', 'curl'],
]

describe('summarizeUserAgent', () => {
  it.each(CORPUS)('summarises %s', (ua, expected) => {
    expect(summarizeUserAgent(ua)).toBe(expected)
  })

  it('answers null when nothing in the string is recognised, never the raw string', () => {
    expect(summarizeUserAgent('')).toBeNull()
    expect(summarizeUserAgent('  ')).toBeNull()
    expect(summarizeUserAgent('Go-http-client/1.1')).toBeNull()
    expect(summarizeUserAgent('SomeWeirdClient/1.0 (unknown platform)')).toBeNull()
    expect(summarizeUserAgent('Mozilla/5.0 (X11; FreeBSD amd64)')).toBeNull()
  })

  it('never lets a raw-UA machine substring into any answer the corpus parses', () => {
    for (const [ua] of CORPUS) {
      const summary = summarizeUserAgent(ua)
      expect(summary).not.toBeNull()
      for (const token of MACHINE_STRINGS) {
        expect(summary, `"${summary}" (from "${ua}") contains ${token}`).not.toContain(
          token,
        )
      }
    }
  })
})
