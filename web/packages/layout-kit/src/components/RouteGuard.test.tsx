/**
 * RouteGuard contract: a fully controlled gate over an injected
 * `status` value, never a callback RouteGuard invokes itself.
 *
 * Covers: `'allowed'` renders children and nothing else; `'pending'`
 * renders the default labelled spinner or a host override;
 * `'denied'` renders the default `@speed/ui-kit` `noPermission`
 * EmptyState (asserted against ui-kit's own shipped bundle strings,
 * never a re-typed literal) or a host override, the default
 * composition's title a page-level `h1` unless the host forwards a
 * `headingLevel` of its own; `onDenied` fires exactly once per
 * transition INTO `'denied'` -- re-rendering with the same `'denied'`
 * status twice fires it only once, and leaving and re-entering
 * `'denied'` fires it again; and a zero-violation axe scan on each
 * status's rendered subtree, with `region` disabled -- unlike
 * AppShell, RouteGuard is a per-widget gate around host content, not
 * page-level chrome with its own landmarks, so it falls under the same
 * "component tests, not page tests" carve-out ui-kit's own axe helper
 * documents.
 */

import { act } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import enUS from '../locales/en-US.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import uiKitZhCN from '../../../ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { renderWithProviders } from '../../test-utils/render.js'
import { RouteGuard } from './RouteGuard.js'

describe('RouteGuard', () => {
  describe('allowed', () => {
    it('renders children and nothing else', () => {
      const { getByText, queryByText } = renderWithProviders(
        <RouteGuard status="allowed">
          <p>Protected content</p>
        </RouteGuard>,
      )
      expect(getByText('Protected content')).toBeInTheDocument()
      expect(queryByText(uiKitZhCN.emptyState.noPermission.title)).not.toBeInTheDocument()
    })

    it('has no axe violations', async () => {
      // The scan mounts the guard inside the page skeleton a real host
      // supplies -- the main landmark and the page's h1 above the
      // gated content -- because page-has-heading-one and
      // landmark-one-main are determinate in jsdom now (see the axe
      // helper header): a scan without that context fails instead of
      // passing by indeterminacy. region stays disabled: the gate is a
      // per-widget fragment whose host content it does not structure.
      renderWithProviders(
        <main>
          <h1>Protected page</h1>
          <RouteGuard status="allowed">
            <p>Protected content</p>
          </RouteGuard>
        </main>,
      )
      await expectNoAxeViolations({ disabledRules: ['region'] })
    })
  })

  describe('pending', () => {
    it('renders the default labelled spinner and no children', () => {
      const { getByRole, queryByText } = renderWithProviders(
        <RouteGuard status="pending">
          <p>Protected content</p>
        </RouteGuard>,
      )
      expect(getByRole('progressbar', { name: zhCN.routeGuard.pending })).toBeInTheDocument()
      expect(queryByText('Protected content')).not.toBeInTheDocument()
    })

    it('relabels the default spinner when the language switches to en-US', async () => {
      const { getByRole, i18n } = renderWithProviders(<RouteGuard status="pending" />)
      await act(async () => {
        await switchLanguage(i18n, 'en-US')
      })
      expect(getByRole('progressbar', { name: enUS.routeGuard.pending })).toBeInTheDocument()
    })

    it('renders a host-supplied pendingFallback instead of the default', () => {
      const { getByText, queryByRole } = renderWithProviders(
        <RouteGuard status="pending" pendingFallback={<p>Loading your access…</p>} />,
      )
      expect(getByText('Loading your access…')).toBeInTheDocument()
      expect(queryByRole('progressbar')).not.toBeInTheDocument()
    })

    it('has no axe violations', async () => {
      renderWithProviders(
        <main>
          <h1>Protected page</h1>
          <RouteGuard status="pending" />
        </main>,
      )
      await expectNoAxeViolations({ disabledRules: ['region'] })
    })
  })

  describe('denied', () => {
    it('renders the default ui-kit noPermission EmptyState and no children', () => {
      const { getByText, queryByText } = renderWithProviders(
        <RouteGuard status="denied">
          <p>Protected content</p>
        </RouteGuard>,
      )
      expect(getByText(uiKitZhCN.emptyState.noPermission.title)).toBeInTheDocument()
      expect(getByText(uiKitZhCN.emptyState.noPermission.description)).toBeInTheDocument()
      expect(queryByText('Protected content')).not.toBeInTheDocument()
    })

    it('renders a host-supplied deniedFallback instead of the default', () => {
      const { getByText, queryByText } = renderWithProviders(
        <RouteGuard status="denied" deniedFallback={<p>Ask your admin for access.</p>} />,
      )
      expect(getByText('Ask your admin for access.')).toBeInTheDocument()
      expect(queryByText(uiKitZhCN.emptyState.noPermission.title)).not.toBeInTheDocument()
    })

    it('renders the default denied fallback title as the page-level h1', () => {
      // The denied gate replaces the page content it guards, so the
      // default composition's title is the page's own heading -- an
      // h1, the same page-level default the sibling whole-page
      // placeholder (auth-ui's SessionEndedScreen) chose. Pre-fix the
      // stock h6 rendered a heading level no page can start at.
      const { getByRole } = renderWithProviders(<RouteGuard status="denied" />)
      expect(
        getByRole('heading', { level: 1, name: uiKitZhCN.emptyState.noPermission.title }),
      ).toBeInTheDocument()
    })

    it('renders the default denied fallback title at the host-forwarded headingLevel', () => {
      // A host mounting the gate under a heading of its own passes the
      // level that continues the page's order; pre-fix there was no
      // host control over the default composition at all and the
      // fallback stayed at ui-kit's stock h6 whatever the page above.
      const { getByRole } = renderWithProviders(
        <main>
          <h1>Protected page</h1>
          <RouteGuard status="denied" headingLevel="h3" />
        </main>,
      )
      expect(
        getByRole('heading', { level: 3, name: uiKitZhCN.emptyState.noPermission.title }),
      ).toBeInTheDocument()
    })

    it('fires onDenied exactly once for a single transition into denied, not on every re-render', () => {
      const onDenied = vi.fn()
      const { rerender } = renderWithProviders(
        <RouteGuard status="pending" onDenied={onDenied} />,
      )
      expect(onDenied).not.toHaveBeenCalled()

      rerender(<RouteGuard status="denied" onDenied={onDenied} />)
      expect(onDenied).toHaveBeenCalledTimes(1)

      // Re-rendering with the SAME denied status must not re-fire.
      rerender(<RouteGuard status="denied" onDenied={onDenied} />)
      expect(onDenied).toHaveBeenCalledTimes(1)
    })

    it('fires onDenied again on a later, separate transition into denied', () => {
      const onDenied = vi.fn()
      const { rerender } = renderWithProviders(
        <RouteGuard status="denied" onDenied={onDenied} />,
      )
      expect(onDenied).toHaveBeenCalledTimes(1)

      rerender(<RouteGuard status="allowed" onDenied={onDenied} />)
      expect(onDenied).toHaveBeenCalledTimes(1)

      rerender(<RouteGuard status="denied" onDenied={onDenied} />)
      expect(onDenied).toHaveBeenCalledTimes(2)
    })

    it('has no axe violations', async () => {
      // The default denied fallback composes ui-kit's EmptyState at the
      // page-level h1 (the headingLevel default): the denied gate
      // replaces the page content it guards, so the fallback's title IS
      // the page's own heading and the scan document needs no host
      // heading of its own -- page-has-heading-one stays enabled
      // (pre-fix the stock h6 left the page heading-less and this rule
      // failed the scan). A host mounting the gate under a heading of
      // its own forwards headingLevel, proven by the scan below.
      renderWithProviders(
        <main>
          <RouteGuard status="denied" />
        </main>,
      )
      await expectNoAxeViolations({ disabledRules: ['region'] })
    })

    it('has no axe violations when the host forwards headingLevel under its own page heading', async () => {
      // The host-forwarding leg of the heading fix: a page h1 of the
      // host's own, and the gate's default denied composition below it
      // at the level that continues the page's order. Pre-fix,
      // headingLevel reached no composition and the stock h6 fell out
      // of the page's h1 -- a heading-order skip this scan flags.
      renderWithProviders(
        <main>
          <h1>Protected page</h1>
          <RouteGuard status="denied" headingLevel="h2" />
        </main>,
      )
      await expectNoAxeViolations({ disabledRules: ['region'] })
    })
  })
})
