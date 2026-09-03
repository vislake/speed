/**
 * axe-core assertions for product-shell component tests.
 *
 * Runs axe over the whole jsdom document and fails the test on any
 * violation. color-contrast is disabled: jsdom does no layout or color
 * computation, so contrast results there are neither trustworthy nor
 * actionable -- contrast lives in the theme and is verified visually/
 * browser-side (same rationale as ui-kit's test-utils/axe.ts).
 *
 * Unlike ui-kit's own helper, `region` stays ENABLED here by default:
 * ProductShell's authenticated branch is page-level chrome with repeated
 * landmarks (banner, nav, main), so the axe rule that checks all content
 * is contained by a landmark is directly relevant -- ui-kit's blanket
 * disable is reserved for its per-widget component tests, not this
 * package's page-shaped ones. This is the same helper layout-kit's suite
 * uses (packages cannot share test utilities: each carries its own copy,
 * since the helpers a suite needs are not published artifacts); scans
 * run only on the authenticated frame, whose pre-auth branches are
 * deliberately widget-shaped and belong to the slotted packages' own
 * suites.
 */

import { expect } from 'vitest'
import axe from 'axe-core'

export interface AxeAssertionOptions {
  /** Extra axe rules to disable beyond color-contrast. */
  readonly disabledRules?: readonly string[]
}

/**
 * Assert the current document has no axe violations, with
 * color-contrast disabled (jsdom cannot compute colors -- see header).
 * `region` is left enabled; pass it in `disabledRules` for the rare test
 * that renders a deliberately incomplete fragment.
 */
export async function expectNoAxeViolations(
  options: AxeAssertionOptions = {},
): Promise<void> {
  const { disabledRules = [] } = options
  // Component-level scans run on a bare jsdom document: give it the two
  // attributes page-level rules demand so those never fail a unit test.
  document.title = document.title || 'product-shell test'
  document.documentElement.lang = document.documentElement.lang || 'en'
  const result = await axe.run(document, {
    rules: {
      'color-contrast': { enabled: false },
      ...Object.fromEntries(disabledRules.map((rule) => [rule, { enabled: false }])),
    },
  })
  if (result.violations.length > 0) {
    const summary = result.violations
      .map((v) => `${v.id}: ${v.help} (${v.nodes.map((n) => n.target.join(' ')).join(', ')})`)
      .join('\n')
    expect.fail(`axe violations found:\n${summary}`)
  }
}
