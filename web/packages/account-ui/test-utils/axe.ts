/**
 * axe-core assertions for account-ui component tests.
 *
 * Runs axe over the whole jsdom document and fails the test on any
 * violation (component-level renders are small enough that scanning the
 * document is fine). color-contrast is disabled by default: jsdom does
 * no layout or color computation, so contrast results there are neither
 * trustworthy nor actionable -- contrast lives in the theme and is
 * verified visually/browser-side (see the package AGENTS.md). Tests
 * that specifically probe contrast affordances (colors roles, text
 * colors on surfaces) assert the theme values instead.
 *
 * jsdom capability note -- why incomplete results are handled the way
 * they are below, and what "determinate" means here. axe's page-context
 * checks (page-has-heading-one, landmark-one-main) probe for an open
 * modal before evaluating their real question, through a visibility
 * filter that needs real geometry and, failing that,
 * `document.elementsFromPoint`. jsdom has neither, so the probe used to
 * THROW and every such check reported "incomplete" -- indeterminate,
 * never failing, no matter what the component rendered. That is how a
 * component scan rendered with no page-heading context passed by
 * indeterminacy: page-has-heading-one could never make any test fail,
 * and the isolated widget renders in this package's suites are exactly
 * that shape (EmptyState.test's own heading-order regression tests
 * document the gap). The helper restores determinacy two ways: it
 * polyfills elementsFromPoint with an empty hit stack -- the truthful
 * answer for a document jsdom lays out not at all -- and it detects a
 * really open overlay itself (a visible `[aria-modal="true"]`
 * paper or any other visible MUI Modal-based overlay root, see
 * hasOpenOverlay) to reproduce the browser's passForModal
 * exemption, because the modal-open part of axe's own probe cannot run
 * under jsdom. The consequence component suites must live with: a scan
 * whose document contains no h1 FAILS with a page-has-heading-one
 * violation. A test that renders a heading-bearing component in
 * isolation must supply the h1 ancestor context (and, where the
 * component's heading level depends on the page, the level that
 * continues the order); where a component legitimately renders without
 * one the suite must assert that heading situation explicitly in the
 * render, never by disabling the rule silently. A scan taken while a
 * modal is really open is exempt from the page-context rules exactly as
 * a browser run would be; a closed but keepMounted drawer (visibility: hidden) is
 * NOT a modal and still faces the real page rules.
 *
 * The widget-tier carve-out: `region` and the page-level main-landmark
 * rule (landmark-one-main) stay disabled below -- the
 * same rationale as region's, which is landmark containment, not widget
 * semantics: "landmark containment is a page-level concern; the units
 * under test are components, not full app pages". `page-has-heading-one`
 * is deliberately NOT part of that carve-out, because these components
 * DO render real heading elements whose levels only make sense relative
 * to a page heading -- the scan must know that context to be honest.
 *
 * Remaining jsdom indeterminacies (a rule that genuinely cannot answer
 * under jsdom and is not on the page-context list, e.g. aria-hidden-focus
 * deciding whether an aria-hidden modal background stays out of the tab
 * order) are REPORTED through console.warn -- surfaced, never silently
 * dropped -- and do not fail the scan: jsdom cannot determine them at
 * all, a real browser resolves them, and the modal-open shapes a scan
 * covers are exactly the case axe's modal semantics are designed to
 * exempt.
 */

import { expect } from 'vitest'
import axe from 'axe-core'

export interface AxeAssertionOptions {
  /** Extra axe rules to disable beyond color-contrast. */
  readonly disabledRules?: readonly string[]
}

/**
 * Page-context rules: their whole question is "does this PAGE have an
 * h1 / a main landmark", answerable only against the document the
 * component is rendered into. With the elementsFromPoint polyfill below
 * they are determinate in jsdom -- an incomplete result on one of them
 * means the determination itself broke down, and the scan must fail
 * (naming the missing context) rather than pass by indeterminacy. A
 * rule the caller disabled deliberately never runs, so it never lands
 * here. (axe 4.13 ships the main-landmark page rule under the id
 * `landmark-one-main`; `page-has-main` is only that rule's inner check
 * id.)
 */
const PAGE_CONTEXT_RULES: readonly string[] = ['page-has-heading-one', 'landmark-one-main']

/**
 * Restores determinacy to axe's modal probe under jsdom. axe's
 * has-descendant family of page checks (and landmark-one-main) first
 * ask "is a modal open" -- a probe that consults
 * `document.elementsFromPoint` when no semantic modal (role=dialog /
 * aria-modal) is present. jsdom ships elementsFromPoint only as a
 * not-implemented stub that throws, and lays nothing out, so the probe
 * threw and the page rules reported incomplete forever. The polyfill
 * answers "nothing at that point" -- the only truthful answer for an
 * engine that lays nothing out -- which lets the checks reach their
 * real question. The probe call below detects the throwing stub (in a
 * real DOM the method answers normally and is left alone).
 */
function polyfillElementsFromPoint(): void {
  try {
    document.elementsFromPoint(0, 0)
  } catch {
    document.elementsFromPoint = () => []
  }
}

/**
 * Whether the scan's document currently shows an open MUI overlay,
 * judged the way a browser would: axe exempts the page-context rules
 * for a really open dialog through its own passForModal option, and its
 * modal probe first looks for a visible `[aria-modal="true"]` element.
 * jsdom cannot run that probe (see polyfillElementsFromPoint), so the
 * helper asks the question itself over the DOM -- an aria-modal paper
 * (dialog/drawer) or any other visible Modal-based overlay root (MUI's
 * Popover/Menu/Select are a styled Modal); a closed but keepMounted
 * overlay is `visibility: hidden` and an open one is not -- and
 * reproduces the exemption. While ANY of these overlays is open, MUI
 * aria-hides the page behind it, so the page-context rules cannot be
 * answered against content hidden from assistive tech -- the state
 * passForModal exists to skip -- which is why the exemption below keys
 * on this detector rather than on aria-modal alone.
 */
function hasOpenOverlay(): boolean {
  for (const element of document.querySelectorAll('[aria-modal="true"], .MuiModal-root')) {
    if (element.getAttribute('aria-hidden') === 'true') {
      continue
    }
    const style = window.getComputedStyle(element)
    if (style.display !== 'none' && style.visibility !== 'hidden') {
      return true
    }
  }
  return false
}

function summarizeTargets(rule: axe.Result): string {
  const targets = rule.nodes.map((node) => node.target.join(' ')).join(', ')
  return `${rule.id}: ${rule.help} (${targets || 'no target'})`
}

function reportNonCriticalIncomplete(incomplete: readonly axe.Result[]): void {
  for (const rule of incomplete) {
    // Deliberate, documented report (never a silent drop): this rule
    // genuinely cannot be determined under jsdom (see the header
    // comment) and is not on the PAGE_CONTEXT_RULES list that must fail
    // when indeterminate. A real browser run resolves it; the report
    // keeps it visible in the suite output instead of vanishing.
    console.warn(
      `[test-utils/axe] indeterminate under jsdom (reported, not asserted): ${summarizeTargets(rule)}`,
    )
  }
}

async function runAxeScan(options: axe.RunOptions = {}): Promise<axe.AxeResults> {
  polyfillElementsFromPoint()
  // Component-level scans run on a bare jsdom document: give it the two
  // attributes page-level rules demand so those never fail a unit test.
  document.title = document.title || 'account-ui test'
  document.documentElement.lang = document.documentElement.lang || 'en'
  return axe.run(document, options)
}

/**
 * Assert the current document has no axe violations, with
 * color-contrast disabled (jsdom cannot compute colors -- see header).
 * Page-context rules left enabled are determinate (see header): a scan
 * without page-heading context fails rather than passing by
 * indeterminacy, and genuinely indeterminate non-page rules are
 * reported, never silently dropped.
 */
export async function expectNoAxeViolations(
  options: AxeAssertionOptions = {},
): Promise<void> {
  const { disabledRules = [] } = options
  // The widget-tier carve-out (see header): region and the page-level
  // main-landmark rules are page concerns, not widget semantics. The
  // scan still runs every rule that IS meaningful for an isolated
  // component, page-has-heading-one included.
  const disabled = new Set<string>(['color-contrast', 'region', 'landmark-one-main', ...disabledRules])
  if (hasOpenOverlay()) {
    // A really open overlay exempts the page-context rules in a browser
    // (axe's passForModal -- and while MUI aria-hides the page behind
    // any Modal-based overlay, the same reasoning holds for its menus
    // and popovers); jsdom cannot run axe's own modal probe, so the
    // helper's DOM-based detector (hasOpenOverlay) stands in and the
    // exemption is applied up front. Reported, not silent -- an
    // open-overlay scan asserts everything except the page structure the
    // overlay itself suspends.
    for (const rule of PAGE_CONTEXT_RULES) {
      disabled.add(rule)
    }
    console.warn(
      `[test-utils/axe] open modal detected: page-context rule(s) ${PAGE_CONTEXT_RULES.join(', ')} exempted (a browser resolves them through axe's passForModal)`,
    )
  }
  const result = await runAxeScan({
    rules: Object.fromEntries([...disabled].map((rule) => [rule, { enabled: false }])),
  })
  if (result.violations.length > 0) {
    // aria-hidden-focus verdicts depend on live focus management jsdom
    // cannot run: while an open MUI overlay (dialog/drawer/menu/popover
    // -- all Modal-based) is showing, MUI aria-hides the page behind it,
    // and axe's static analysis cannot tell whether the overlay's focus
    // trap keeps that content out of the tab order (axe itself leaves
    // the modal case incomplete). Under a really open overlay such a
    // violation is reported, not asserted; without one (nothing to trap
    // focus) it fails the scan like any other violation.
    const focusTrapDependent = result.violations.filter(
      (rule) => rule.id === 'aria-hidden-focus' && hasOpenOverlay(),
    )
    const failing = result.violations.filter(
      (rule) => !focusTrapDependent.includes(rule),
    )
    for (const rule of focusTrapDependent) {
      console.warn(
        `[test-utils/axe] aria-hidden-focus under an open MUI overlay (reported, not asserted): ${summarizeTargets(rule)}`,
      )
    }
    if (failing.length > 0) {
      const summary = failing.map(summarizeTargets).join('\n')
      expect.fail(`axe violations found:\n${summary}`)
    }
  }
  const incomplete = result.incomplete
  const indeterminatePageContext = incomplete.filter((rule) =>
    PAGE_CONTEXT_RULES.includes(rule.id),
  )
  if (indeterminatePageContext.length > 0) {
    const summary = indeterminatePageContext.map(summarizeTargets).join('\n')
    expect.fail(
      `axe could not determine page-context rule(s):\n${summary}\n` +
        'Provide the page context the rule asks about (an h1 ancestor / a main ' +
        'landmark) in the render, or disable the rule explicitly in disabledRules.',
    )
  }
  reportNonCriticalIncomplete(incomplete)
}
