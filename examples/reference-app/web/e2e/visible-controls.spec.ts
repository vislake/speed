/**
 * That the controls a person needs are ones a person can actually see.
 *
 * The other specs click controls by role and accessible name; they do
 * not look at them. A control rendered in the app bar's own blue on the
 * app bar's own blue -- the sign-out button, and the tenant switcher
 * beside it, the only way a multi-location practice changes which
 * clinic it is working in -- is as clickable to Playwright as a legible
 * one, while a person looking at the screen sees an empty blue bar.
 *
 * So this spec asserts what the other specs cannot: that the text of
 * every control in the app's chrome stands out from what is behind it,
 * by the ratio WCAG 2.1 AA asks for. It is deliberately narrow -- the
 * app bar and the navigation, the chrome a person needs before they
 * can do anything else -- rather than a whole-page audit, because a
 * gate that fails for a decorative caption is a gate people learn to
 * ignore.
 *
 * The ratio is computed in the page from resolved colours,
 * alpha-compositing every layer from the control upward exactly the way
 * a browser composites one (a naive "first non-transparent background"
 * walk discards the alpha of translucent layers like the nav's
 * selected-item tint and misreports what a browser renders -- see
 * backgroundOf below). That is what makes it catch the real defect:
 * two fully legitimate colours whose pairing is wrong.
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
     * palette states its text colours as black at an opacity -- the
     * text.primary, text.secondary and disabled tiers and action.active
     * each carry their own -- so a foreground read straight off
     * getComputedStyle and measured as if opaque is not the colour on
     * the screen.
     *
     * It is measured in the lenient direction, which is why this matters
     * rather than being a rounding quibble: dark text treated as fully
     * opaque looks DARKER than it renders, so the ratio comes out too
     * high and a control passes on a number the browser never produced
     * -- a semi-transparent text tier that genuinely fails AA over
     * white measures as 21:1 when its alpha is dropped, and this gate
     * would report the failing one as excellent. The background half of
     * the same mistake is what backgroundOf below corrects; this closes
     * the error on the foreground side of the ratio.
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
     * walk is NOT the same thing: MUI's selected nav item paints a
     * semi-transparent primary tint over the white drawer, and treating
     * that translucent layer as an opaque primary background --
     * discarding its alpha for the luminance math -- reports a ratio no
     * browser ever produced, where the browser composites the tint
     * under dark text to a pale layer that passes AA comfortably.
     * WCAG's question is what the rendered pixel is, so the composited
     * pixel is what this measures.
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

// The tenant switcher and the sign-out control are legible against the
// AppBar (white text on the AppBar's blue), and the backgroundOf walk
// exists because the Home nav link's translucent selected-item tint,
// measured as if it were opaque, once reported a ratio no browser ever
// produced -- the same pixel described by a broken and a working
// instrument.
//
// The tags are @budget and @deployment: verified, and out of the
// default local run only because its sign-ins do not fit the suite's
// per-account budget (see e2e/README.md).
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
