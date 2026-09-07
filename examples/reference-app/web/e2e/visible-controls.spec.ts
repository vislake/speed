/**
 * That the controls a person needs are ones a person can actually see.
 *
 * This gate exists because the suite once passed while the product was
 * unusable. session-lifecycle.spec.ts clicked "sign out" and went green
 * for weeks, and all the while that button was rendered in the app bar's
 * own blue on the app bar's own blue -- contrast ratio 1:1, invisible.
 * The tenant switcher beside it, the only way a multi-location practice
 * changes which clinic it is working in, was invisible the same way.
 * Playwright finds a control by its role and its accessible name; it does
 * not look at it. A person looking at the screen saw an empty blue bar.
 *
 * So this spec asserts what the other specs cannot: that the text of
 * every control in the app's chrome stands out from what is behind it, by
 * the ratio WCAG 2.1 AA asks for. It is deliberately narrow -- the app
 * bar and the navigation, the chrome a person needs before they can do
 * anything else -- rather than a whole-page audit, because a gate that
 * fails for a decorative caption is a gate people learn to ignore.
 *
 * The ratio is computed in the page from resolved colours,
 * alpha-compositing every layer from the control upward exactly the way
 * a browser composites one (the naive "first non-transparent
 * background" walk that preceded this discarded the alpha of
 * translucent layers like the nav's selected-item tint and misreported
 * what a browser renders -- see backgroundOf below). That is what makes
 * it catch the real defect: both colours were fully legitimate on their
 * own, and only their pairing was wrong.
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER, DEMO_READER } from './test-utils/accounts.js'
import { signInAs } from './test-utils/journeys.js'

/** WCAG 2.1 AA for normal-sized text. */
const MINIMUM_CONTRAST = 4.5

/** One control's measured contrast, as the page reports it. */
interface ControlContrast {
  readonly label: string
  readonly ratio: number
  readonly color: string
  readonly background: string
}

/**
 * Measures every interactive control inside the app's chrome. Runs in the
 * page because only the browser knows what a colour resolved to.
 */
async function chromeContrast(page: import('@playwright/test').Page): Promise<ControlContrast[]> {
  return await page.evaluate(() => {
    const parseColor = (value: string): [number, number, number, number] | null => {
      const parts = value.match(/[\d.]+/g)
      if (parts === null || parts.length < 3) {
        return null
      }
      const [r = 0, g = 0, b = 0, a = 1] = parts.map(Number)
      return [r, g, b, a]
    }

    const relativeLuminance = ([r, g, b]: [number, number, number, number]): number => {
      const channel = (value: number): number => {
        const c = value / 255
        return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4)
      }
      return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b)
    }

    /**
     * The background a browser actually renders the control over:
     * every layer from the element itself upward, alpha-composited the
     * way the browser composites them, stopping at the first opaque
     * layer and finishing over white when the document background is
     * itself transparent. A naive "first non-transparent background"
     * walk is NOT the same thing, and this gate lived with that bug
     * once: MUI's selected nav item paints a semi-transparent
     * primary tint (rgba(37, 99, 235, 0.08)) over the white drawer,
     * and the naive walk treated that translucent layer as an opaque
     * primary background -- discarding its alpha for the luminance
     * math -- and reported the Home link at 3.45:1, where the browser
     * composites it to a pale tint under dark text at ~16:1. WCAG's
     * question is what the rendered pixel is, so the composited pixel
     * is what this measures.
     */
    const backgroundOf = (element: Element): [number, number, number, number] => {
      let node: Element | null = element
      // A premultiplied accumulator: each layer contributes its colour
      // times its own alpha times what is still uncovered.
      let red = 0
      let green = 0
      let blue = 0
      let alpha = 0
      while (node !== null && alpha < 1) {
        const parsed = parseColor(getComputedStyle(node).backgroundColor)
        if (parsed !== null && parsed[3] > 0) {
          const remaining = 1 - alpha
          red += parsed[0] * parsed[3] * remaining
          green += parsed[1] * parsed[3] * remaining
          blue += parsed[2] * parsed[3] * remaining
          alpha += parsed[3] * remaining
        }
        node = node.parentElement
      }
      if (alpha < 1) {
        // The document background is transparent: the browser paints
        // the page's own backdrop, white for this suite's context.
        const remaining = 1 - alpha
        red += 255 * remaining
        green += 255 * remaining
        blue += 255 * remaining
        alpha = 1
      }
      return [red / alpha, green / alpha, blue / alpha, 1]
    }

    const chrome = [
      ...document.querySelectorAll('header button, header a, nav button, nav a'),
    ]
    return chrome
      .map((element) => {
        const label = (element.getAttribute('aria-label') ?? element.textContent ?? '')
          .trim()
          .slice(0, 40)
        const style = getComputedStyle(element)
        const foreground = parseColor(style.color)
        const background = backgroundOf(element)
        if (foreground === null) {
          return null
        }
        const lighter = Math.max(relativeLuminance(foreground), relativeLuminance(background))
        const darker = Math.min(relativeLuminance(foreground), relativeLuminance(background))
        return {
          label,
          ratio: Math.round(((lighter + 0.05) / (darker + 0.05)) * 100) / 100,
          color: style.color,
          background: `rgb(${background[0]}, ${background[1]}, ${background[2]})`,
        }
      })
      .filter((entry): entry is ControlContrast => entry !== null && entry.label !== '')
  })
}

// These two tests were tagged @pending while the defect they found was
// open: on the day they were written both failed, naming the tenant
// switcher and the sign-out control at 1:1 -- both rendered in the
// AppBar's own primary blue on the AppBar's own primary blue, the exact
// silent-failure shape this suite exists for -- plus a third reading,
// the Home nav link at 3.45:1, which turned out to be this suite's own
// measurement bug (a translucent selected-item tint measured as if it
// were opaque; see backgroundOf above). The colours are fixed
// (tenancy-ui's trigger and auth-ui's sign-out button inherit the
// surface's contrastText) and the measurement now composites the way a
// browser does; both tests pass against the fixed tree (the closing
// round's verification). The tag stays until the acceptance session's
// re-run drops it: each test adds sign-ins to a suite that shares one
// demo server, and the go/authn per-account limit (five per minute)
// makes joining the default run a suite-budget decision, not a colour
// decision. Dropping @pending then turns them into ordinary regression
// gates.
test('every control in the signed-in chrome is legible against its background', {
  tag: ['@pending', '@deployment'],
}, async ({ page }) => {
  await signInAs(page, DEMO_OWNER)

  const measured = await chromeContrast(page)
  expect(measured.length, 'the chrome reported no controls to measure').toBeGreaterThan(0)

  const illegible = measured.filter((control) => control.ratio < MINIMUM_CONTRAST)
  expect(
    illegible,
    `these controls are below WCAG AA (${MINIMUM_CONTRAST}:1): ${illegible
      .map((c) => `"${c.label}" ${c.ratio}:1 (${c.color} on ${c.background})`)
      .join('; ')}`,
  ).toEqual([])
})

test('the chrome stays legible on a phone', { tag: ['@pending', '@deployment'] }, async ({ page }) => {
  // A different account from the test above, so neither approaches the
  // per-account sign-in budget when this runs against a long-lived
  // deployment (playwright.config.ts's deployment-mode note).
  // The same controls wrap onto a second row at this width, which is
  // where a colour problem tends to hide: the row a designer never
  // screenshotted.
  await page.setViewportSize({ width: 390, height: 844 })
  await signInAs(page, DEMO_READER)

  const illegible = (await chromeContrast(page)).filter(
    (control) => control.ratio < MINIMUM_CONTRAST,
  )
  expect(
    illegible,
    `these controls are below WCAG AA on a phone: ${illegible
      .map((c) => `"${c.label}" ${c.ratio}:1 (${c.color} on ${c.background})`)
      .join('; ')}`,
  ).toEqual([])
})
