/**
 * The README usage example, compiled and executed by the suite.
 *
 * The README's Quick start composes an AppShell around a
 * RouteGuard-wrapped content region, backed by a local `useState`
 * standing in for the authorization source no `auth-core` round has
 * shipped yet (see the package AGENTS.md Deferrals). This file renders
 * that exact composition through the same host tree
 * (renderWithProviders) and asserts what it shows, so the documented
 * usage cannot drift from the API -- the package suite compiles and
 * runs it. Host-content strings (nav labels, the app title) are English
 * fixtures on purpose: they stand in for a host's own translations and
 * are data in a test file (exempt from the no-literal-text rule), not
 * rendered product text. Assertions derive every built-in string from
 * the locale bundles, never inline translations.
 */

import { act, fireEvent } from '@testing-library/react'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }
import uiKitZhCN from '../../ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import { mockMatchMedia } from '../test-utils/matchMedia.js'
import { AppShell } from './components/AppShell.js'
import { RouteGuard, type RouteGuardStatus } from './components/RouteGuard.js'

const NAV_ITEMS = [
  { id: 'home', label: 'Home', href: '/home', selected: true },
  { id: 'reports', label: 'Reports', href: '/reports' },
] as const

/**
 * The README's example host page: it owns the authorization status
 * exactly the way it owns every other piece of state passed to these
 * components -- computed here, flowing down through props.
 */
function AppContent({ initialStatus }: { readonly initialStatus: RouteGuardStatus }) {
  const [status] = useState<RouteGuardStatus>(initialStatus)
  return (
    <AppShell navItems={NAV_ITEMS} header="My App">
      <RouteGuard status={status}>
        <p>Protected screen content</p>
      </RouteGuard>
    </AppShell>
  )
}

describe('README usage example', () => {
  it('renders the quick-start composition with the zh-CN built-in texts', async () => {
    mockMatchMedia(true)
    const utils = renderWithProviders(<AppContent initialStatus="allowed" />)
    expect(utils.getByText('My App')).toBeInTheDocument()
    expect(utils.getByRole('link', { name: 'Home' })).toHaveAttribute('href', '/home')
    expect(utils.getByRole('link', { name: 'Home' })).toHaveAttribute('aria-current', 'page')
    expect(utils.getByRole('link', { name: 'Reports' })).toHaveAttribute('href', '/reports')
    expect(utils.getByText('Protected screen content')).toBeInTheDocument()
    expect(
      utils.getByRole('navigation', { name: zhCN.appShell.navLabel }),
    ).toBeInTheDocument()
    expect(utils.getByRole('banner')).toBeInTheDocument()
    expect(utils.getByRole('main')).toBeInTheDocument()
    await expectNoAxeViolations()
  })

  it('follows a language switch into the en-US built-in texts', async () => {
    mockMatchMedia(true)
    const utils = renderWithProviders(<AppContent initialStatus="allowed" />)
    expect(
      utils.getByRole('navigation', { name: zhCN.appShell.navLabel }),
    ).toBeInTheDocument()
    await act(async () => {
      await switchLanguage(utils.i18n, 'en-US')
    })
    expect(
      utils.getByRole('navigation', { name: enUS.appShell.navLabel }),
    ).toBeInTheDocument()
    expect(
      utils.queryByRole('navigation', { name: zhCN.appShell.navLabel }),
    ).not.toBeInTheDocument()
  })

  it('gates the content region on RouteGuard status, reusing ui-kit noPermission for denied', async () => {
    mockMatchMedia(true)
    const utils = renderWithProviders(<AppContent initialStatus="denied" />)
    expect(utils.queryByText('Protected screen content')).not.toBeInTheDocument()
    expect(
      utils.getByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
  })

  it('opens and closes the mobile nav drawer via the shipped toggle labels', async () => {
    mockMatchMedia(false)
    const utils = renderWithProviders(<AppContent initialStatus="allowed" />)
    const toggle = utils.getByRole('button', { name: zhCN.appShell.openNav })
    fireEvent.click(toggle)
    // The now-open temporary drawer is a real modal: MUI aria-hides the
    // rest of the page, this toggle button included, while it traps
    // focus -- `hidden: true` looks past that, same as AppShell's own
    // suite documents.
    expect(
      utils.getByRole('button', { name: zhCN.appShell.closeNav, hidden: true }),
    ).toBeInTheDocument()
  })
})
