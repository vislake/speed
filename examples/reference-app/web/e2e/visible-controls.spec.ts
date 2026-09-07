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
     * The pixel a translucent colour actually becomes over an opaque
     * one. WCAG asks about rendered pixels, and TEXT carries alpha in
     * this design system as routinely as backgrounds do: MUI's light
     * palette states its text colours as black at an opacity --
     * text.primary at 0.87, text.secondary at 0.6, the disabled tier at
     * 0.38, action.active at 0.54 -- so a foreground read straight off
     * getComputedStyle and measured as if opaque is not the colour on
     * the screen.
     *
     * It is measured in the lenient direction, which is why this matters
     * rather than being a rounding quibble: dark text treated as fully
     * opaque looks DARKER than it renders, so the ratio comes out too
     * high and a control passes on a number the browser never produced.
     * Over white, text.secondary really renders at 5.74:1 and the
     * disabled tier at 2.65:1 -- a genuine AA failure -- while both
     * measure 21:1 when their alpha is dropped. This gate would have
     * reported the failing one as excellent.
     *
     * The background half of exactly this mistake was found and fixed by
     * the round that closed the colour defects (see backgroundOf below);
     * the foreground half was left, so this closes the same error on the
     * other side of the ratio.
     */
    const flatten = (
      colour: [number, number, number, number],
      over: [number, number, number, number],
    ): [number, number, number, number] => {
      const [r, g, b, a] = colour
      if (a >= 1) {
        return colour
      }
      return [
        r * a + over[0] * (1 - a),
        g * a + over[1] * (1 - a),
        b * a + over[2] * (1 - a),
        1,
      ]
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
        const declared = parseColor(style.color)
        const background = backgroundOf(element)
        if (declared === null) {
          return null
        }
        // The text over the background it sits on, both as pixels.
        const foreground = flatten(declared, background)
        const lighter = Math.max(relativeLuminance(foreground), relativeLuminance(background))
        const darker = Math.min(relativeLuminance(foreground), relativeLuminance(background))
        return {
          label,
          ratio: Math.round(((lighter + 0.05) / (darker + 0.05)) * 100) / 100,
          // The declared colour, not the composited one: a person fixing
          // a reading needs the value written in the code, while the
          // ratio is what the screen does with it.
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
// were opaque; see backgroundOf above).
//
// Both are closed, and measured closed on the real deployment across
// all three engines in the acceptance session: the two invisible
// controls now read 5.17:1 (white on the AppBar's blue) and the Home
// link 15.99:1, the nav's other links 17.85:1. Those numbers are worth
// keeping here because they corroborate each other from opposite
// directions -- 3.45:1 is exactly what near-black text over an OPAQUE
// primary blue measures, and ~16:1 is exactly what the same text over
// that blue at 8% over white measures, so the old reading and the new
// one are the same pixel described by a broken and a working
// instrument.
//
// The tag is @budget now, not @pending: verified, and out of the default
// run only because its sign-ins do not fit the suite's per-account
// budget (see e2e/README.md).
test('every control in the signed-in chrome is legible against its background', {
  tag: ['@budget', '@deployment'],
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

test('the chrome stays legible on a phone', { tag: ['@budget', '@deployment'] }, async ({ page }) => {
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
