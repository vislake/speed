/**
 * EmptyState contract: every variant carries bilingual built-in title
 * and description from the ui-kit namespace (asserted against the
 * bundles, never against inlined language text), title/description/
 * action/icon are host overrides, and a key missing from the namespace
 * renders as the key itself -- never another language's text.
 */

import { act } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import axe from 'axe-core'
import { useTranslation, switchLanguage } from '@speed/i18n'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../../test-utils/render.js'
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { EmptyState } from './EmptyState.js'

/**
 * Runs ONLY axe-core's heading-order rule over the current document and
 * returns its violations, unlike expectNoAxeViolations (test-utils/axe.js)
 * which runs the full page-oriented rule set and asserts none exist. This
 * component's own tests otherwise render EmptyState with no ancestor
 * heading at all, so the full run above can never reach a heading-order
 * violation regardless of what level EmptyState's title renders as -- the
 * exact gap the account-ui audit named. Scoping to one rule lets these
 * two tests supply a real h1 ancestor and assert on the skip directly,
 * without adopting the page-level rules (document-title, html-has-lang,
 * region) the shared harness disables for good reason (see its own
 * header comment) but that are irrelevant to heading-order anyway.
 */
async function runHeadingOrderCheck(): Promise<readonly axe.Result[]> {
  const result = await axe.run(document, {
    runOnly: { type: 'rule', values: ['heading-order'] },
  })
  return result.violations
}

describe('EmptyState', () => {
  it('renders the empty variant with its built-in zh-CN texts by default', () => {
    const { getByText } = renderWithProviders(<EmptyState />)
    expect(getByText(zhCN.emptyState.empty.title)).toBeInTheDocument()
    expect(getByText(zhCN.emptyState.empty.description)).toBeInTheDocument()
  })

  it.each(['empty', 'noPermission', 'error'] as const)(
    'renders the %s variant with its own built-in texts',
    (variant) => {
      const { getByText } = renderWithProviders(<EmptyState variant={variant} />)
      expect(getByText(zhCN.emptyState[variant].title)).toBeInTheDocument()
      expect(getByText(zhCN.emptyState[variant].description)).toBeInTheDocument()
    },
  )

  it('switches built-in texts to the en-US bundle when the language changes', async () => {
    const { i18n, getByText, queryByText } = renderWithProviders(<EmptyState />)
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    expect(getByText(enUS.emptyState.empty.title)).toBeInTheDocument()
    expect(getByText(enUS.emptyState.empty.description)).toBeInTheDocument()
    expect(queryByText(zhCN.emptyState.empty.title)).not.toBeInTheDocument()
  })

  it('lets title and description overrides replace the built-in texts', () => {
    const { getByText, queryByText } = renderWithProviders(
      <EmptyState title="Tenant at capacity" description="Upgrade to store more scans." />,
    )
    expect(getByText('Tenant at capacity')).toBeInTheDocument()
    expect(getByText('Upgrade to store more scans.')).toBeInTheDocument()
    expect(queryByText(zhCN.emptyState.empty.title)).not.toBeInTheDocument()
    expect(queryByText(zhCN.emptyState.empty.description)).not.toBeInTheDocument()
  })

  it('renders an action below the description when given', () => {
    const { getByRole } = renderWithProviders(
      <EmptyState action={<button type="button">Create first scan</button>} />,
    )
    expect(getByRole('button', { name: 'Create first scan' })).toBeInTheDocument()
  })

  it('renders the custom icon in place of the variant stock icon', () => {
    const { getByTestId, container } = renderWithProviders(
      <EmptyState icon={<div data-testid="custom-icon" />} />,
    )
    expect(getByTestId('custom-icon')).toBeInTheDocument()
    expect(container.querySelectorAll('svg')).toHaveLength(0)
  })

  it('keeps the stock icons decorative (aria-hidden, no focus)', () => {
    const { container } = renderWithProviders(<EmptyState variant="noPermission" />)
    for (const svg of container.querySelectorAll('svg')) {
      expect(svg).toHaveAttribute('aria-hidden', 'true')
      expect(svg).toHaveAttribute('focusable', 'false')
    }
  })

  it('renders a missing namespace key as the key itself, warning visibly', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    function MissingProbe() {
      const { t } = useTranslation('ui-kit')
      return <div data-testid="probe">{t('emptyState.empty.title.typo')}</div>
    }
    const { getByTestId } = renderWithProviders(<MissingProbe />)
    try {
      expect(getByTestId('probe')).toHaveTextContent('emptyState.empty.title.typo')
      expect(warn).toHaveBeenCalledTimes(1)
      const message = warn.mock.calls[0]?.[0] as string
      expect(message).toContain('emptyState.empty.title.typo')
      expect(message).toContain('"ui-kit"')
    } finally {
      warn.mockRestore()
    }
  })

  it('passes axe over every variant', async () => {
    for (const variant of ['empty', 'noPermission', 'error'] as const) {
      renderWithProviders(<EmptyState variant={variant} />)
    }
    await expectNoAxeViolations()
  })

  // Regression for the account-ui audit's CONFIRMED P2-1 finding: the
  // title used to render unconditionally as Typography variant="h6" with
  // no component override, which is a REAL <h6> DOM element regardless of
  // its visual style. A real page renders EmptyState under real
  // heading ancestors (an h1 page title, an h2 section header a hidden-
  // header empty/error branch stands in for -- see account-ui's
  // SessionsSection/LoginHistorySection/SocialBindingsSection), so that
  // fixed h6 skipped straight from h1/h2 to h6, a genuine axe
  // heading-order violation the package's own component-level axe run
  // above can never see: it renders EmptyState in total isolation, with
  // no ancestor heading at all, so the very defect this test targets
  // produces zero violations there. This test supplies the ancestor
  // itself so the skip is actually reachable.
  it('does not skip a heading level under a real h1/h2 ancestor', async () => {
    // PRE-FIX: EmptyState always rendered an <h6>, so a page whose most
    // recent heading is h1 (as here) jumped straight to h6 -- a genuine
    // heading-order skip axe's heading-order rule flags. POST-FIX: the
    // default is UNCHANGED (still h6) precisely so an un-migrated caller
    // keeps its exact old behaviour -- so a caller that does not pass
    // headingLevel still skips here, and only supplying the correct
    // level (as account-ui's own call sites now do) fixes it. Both halves
    // are asserted below.
    renderWithProviders(
      <div>
        <h1>Page title</h1>
        <EmptyState />
      </div>,
    )
    const heading = document.querySelector('h6')
    expect(heading).not.toBeNull()

    const violations = await runHeadingOrderCheck()
    expect(violations.length).toBeGreaterThan(0)
  })

  it('continues the real page heading order when headingLevel is supplied', async () => {
    renderWithProviders(
      <div>
        <h1>Page title</h1>
        <EmptyState headingLevel="h2" />
      </div>,
    )
    expect(document.querySelector('h6')).toBeNull()
    expect(document.querySelector('h2')).not.toBeNull()

    const violations = await runHeadingOrderCheck()
    expect(violations).toHaveLength(0)
  })
})
