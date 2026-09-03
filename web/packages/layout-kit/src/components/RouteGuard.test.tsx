/**
 * RouteGuard contract: a fully controlled gate over an injected
 * `status` value, never a callback RouteGuard invokes itself.
 *
 * Covers: `'allowed'` renders children and nothing else; `'pending'`
 * renders the default labelled spinner or a host override;
 * `'denied'` renders the default `@speed/ui-kit` `noPermission`
 * EmptyState (asserted against ui-kit's own shipped bundle strings,
 * never a re-typed literal) or a host override; `onDenied` fires
 * exactly once per transition INTO `'denied'` -- re-rendering with the
 * same `'denied'` status twice fires it only once, and leaving and
 * re-entering `'denied'` fires it again; and a zero-violation axe scan
 * on each status's rendered subtree, with `region` disabled -- unlike
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
      renderWithProviders(
        <RouteGuard status="allowed">
          <p>Protected content</p>
        </RouteGuard>,
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
      renderWithProviders(<RouteGuard status="pending" />)
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
      renderWithProviders(<RouteGuard status="denied" />)
      await expectNoAxeViolations({ disabledRules: ['region'] })
    })
  })
})
