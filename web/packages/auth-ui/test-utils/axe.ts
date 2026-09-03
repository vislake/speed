/**
 * axe-core assertions for auth-ui component tests.
 *
 * Runs axe over the whole jsdom document and fails the test on any
 * violation (component-level renders are small enough that scanning the
 * document is fine). color-contrast is disabled by default: jsdom does
 * no layout or color computation, so contrast results there are neither
 * trustworthy nor actionable -- contrast lives in the theme and is
 * verified visually/browser-side. Tests that specifically probe contrast
 * affordances assert the theme values instead.
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
 */
export async function expectNoAxeViolations(
  options: AxeAssertionOptions = {},
): Promise<void> {
  const { disabledRules = [] } = options
  // Component-level scans run on a bare jsdom document: give it the two
  // attributes page-level rules demand so those never fail a unit test.
  document.title = document.title || 'auth-ui test'
  document.documentElement.lang = document.documentElement.lang || 'en'
  const result = await axe.run(document, {
    rules: {
      'color-contrast': { enabled: false },
      // Landmark containment is a page-level concern; the units under
      // test are components, not full app pages.
      region: { enabled: false },
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
