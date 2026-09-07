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
 * The ratio is computed in the page from resolved colours, walking up for
 * the first non-transparent background the way a browser composites one.
 * That is what makes it catch the real defect: both colours were fully
 * legitimate on their own, and only their pairing was wrong.
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
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

    /** The first ancestor background a browser would actually composite onto. */
    const backgroundOf = (element: Element): [number, number, number, number] => {
      let node: Element | null = element
      while (node !== null) {
        const parsed = parseColor(getComputedStyle(node).backgroundColor)
        if (parsed !== null && parsed[3] > 0) {
          return parsed
        }
        node = node.parentElement
      }
      return [255, 255, 255, 1]
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

// Tagged @pending only because the defect they found is still open: on
// the day this was written both tests failed, naming the tenant switcher
// and the sign-out control at 1:1 and the Home nav link at 3.45:1. They
// leave @pending the moment the colours are fixed, and from then on they
// are an ordinary regression gate. (playwright.config.ts's grepInvert
// keeps a known-failing gate out of the default run; asking for it by
// name is `E2E_INCLUDE_PENDING=1 pnpm test:e2e --grep @pending`.)
test('every control in the signed-in chrome is legible against its background', {
  tag: '@pending',
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

test('the chrome stays legible on a phone', { tag: '@pending' }, async ({ page }) => {
  // The same controls wrap onto a second row at this width, which is
  // where a colour problem tends to hide: the row a designer never
  // screenshotted.
  await page.setViewportSize({ width: 390, height: 844 })
  await signInAs(page, DEMO_OWNER)

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
