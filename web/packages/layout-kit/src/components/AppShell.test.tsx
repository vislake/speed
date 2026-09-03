/**
 * AppShell contract: a fully controlled, props-driven app-chrome shell.
 *
 * Covers: nav item wiring (selected/href/onClick, host-computed only --
 * AppShell never path-matches), the desktop/mobile drawer split driven
 * by useMediaQuery (permanent at md+, temporary below, uncontrolled by
 * default and promotable to controlled via mobileOpen/onMobileOpenChange),
 * that header/headerActions/userMenu/children render only host content,
 * the header/nav/main landmarks in both languages via the shipped
 * bundles, and a zero-violation axe scan with `region` left enabled
 * (AppShell is page-level chrome, unlike ui-kit's per-widget components).
 */

import { act, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import enUS from '../locales/en-US.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { mockMatchMedia } from '../../test-utils/matchMedia.js'
import { renderWithProviders } from '../../test-utils/render.js'
import { AppShell, type AppShellNavItem } from './AppShell.js'

const NAV_ITEMS: readonly AppShellNavItem[] = [
  { id: 'home', label: 'Home', href: '/home', selected: true },
  { id: 'reports', label: 'Reports', href: '/reports' },
]

describe('AppShell', () => {
  describe('nav items', () => {
    it('renders each item label and wires href/selected from the host, never computing them itself', () => {
      const { getByRole } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>content</AppShell>,
      )
      const homeLink = getByRole('link', { name: 'Home' })
      expect(homeLink).toHaveAttribute('href', '/home')
      expect(homeLink).toHaveAttribute('aria-current', 'page')

      const reportsLink = getByRole('link', { name: 'Reports' })
      expect(reportsLink).toHaveAttribute('href', '/reports')
      expect(reportsLink).not.toHaveAttribute('aria-current')
    })

    it('renders an item without href as a button and fires onClick', async () => {
      const onClick = vi.fn()
      const user = userEvent.setup()
      const { getByRole } = renderWithProviders(
        <AppShell navItems={[{ id: 'logout', label: 'Log out', onClick }]}>content</AppShell>,
      )
      const button = getByRole('button', { name: 'Log out' })
      await user.click(button)
      expect(onClick).toHaveBeenCalledTimes(1)
    })
  })

  describe('responsive drawer', () => {
    it('renders a permanent drawer with no toggle button at md and up', () => {
      mockMatchMedia(true)
      const { getByRole, queryByRole } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>content</AppShell>,
      )
      expect(getByRole('navigation', { name: zhCN.appShell.navLabel })).toBeInTheDocument()
      expect(
        queryByRole('button', { name: zhCN.appShell.openNav }),
      ).not.toBeInTheDocument()
    })

    it('renders a closed temporary drawer below md and opens/closes it, uncontrolled by default', async () => {
      mockMatchMedia(false)
      const user = userEvent.setup()
      const { getByRole } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>content</AppShell>,
      )
      const toggle = getByRole('button', { name: zhCN.appShell.openNav })
      expect(toggle).toHaveAttribute('aria-expanded', 'false')

      await user.click(toggle)
      // Once open, the temporary drawer is a real modal: MUI aria-hides
      // the rest of the page (this same header button included) from
      // assistive tech while it traps focus inside the drawer, exactly
      // as a modal should. `hidden: true` looks past that to assert the
      // toggle state a mouse click still reaches -- screen-reader users
      // close through Escape or the backdrop instead, both of which stay
      // inside the modal's own accessible subtree.
      const closeToggle = getByRole('button', {
        name: zhCN.appShell.closeNav,
        hidden: true,
      })
      expect(closeToggle).toHaveAttribute('aria-expanded', 'true')

      await user.click(closeToggle)
      // The background's aria-hidden marker lifts only once the drawer's
      // exit transition finishes (react-transition-group's onExited),
      // which runs on a real timer even under jsdom -- waitFor polls
      // past that instead of asserting on an animation still in flight.
      await waitFor(() => {
        expect(getByRole('button', { name: zhCN.appShell.openNav })).toHaveAttribute(
          'aria-expanded',
          'false',
        )
      })
    })

    it('defers the open state to the host when mobileOpen/onMobileOpenChange are supplied', async () => {
      mockMatchMedia(false)
      const onMobileOpenChange = vi.fn()
      const user = userEvent.setup()
      const { getByRole } = renderWithProviders(
        <AppShell
          navItems={NAV_ITEMS}
          mobileOpen={false}
          onMobileOpenChange={onMobileOpenChange}
        >
          content
        </AppShell>,
      )
      const toggle = getByRole('button', { name: zhCN.appShell.openNav })
      await user.click(toggle)

      expect(onMobileOpenChange).toHaveBeenCalledTimes(1)
      expect(onMobileOpenChange).toHaveBeenCalledWith(true)
      // Controlled: the prop did not change, so the shell stays closed
      // until the host re-renders it with mobileOpen={true}.
      expect(getByRole('button', { name: zhCN.appShell.openNav })).toHaveAttribute(
        'aria-expanded',
        'false',
      )
    })
  })

  describe('slots', () => {
    it('renders only the content passed to each slot, nothing implicit', () => {
      mockMatchMedia(true)
      const { getByText, queryByText, getByRole } = renderWithProviders(
        <AppShell
          navItems={NAV_ITEMS}
          header={<span>Speed Admin</span>}
          headerActions={<button type="button">Search</button>}
          userMenu={<span>Jane Doe</span>}
        >
          <p>Main content</p>
        </AppShell>,
      )
      expect(getByText('Speed Admin')).toBeInTheDocument()
      expect(getByRole('button', { name: 'Search' })).toBeInTheDocument()
      expect(getByText('Jane Doe')).toBeInTheDocument()
      expect(getByText('Main content')).toBeInTheDocument()
      // No stand-in placeholder text ever renders for an absent slot.
      expect(queryByText(/placeholder/i)).not.toBeInTheDocument()
    })

    it('renders no headerActions box when the slot is omitted', () => {
      mockMatchMedia(true)
      const { queryByRole } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>content</AppShell>,
      )
      expect(queryByRole('button', { name: 'Search' })).not.toBeInTheDocument()
    })
  })

  describe('landmarks', () => {
    it('exposes header (banner), nav and main landmarks in zh-CN', () => {
      mockMatchMedia(true)
      const { getByRole } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>content</AppShell>,
      )
      expect(getByRole('banner')).toBeInTheDocument()
      expect(getByRole('navigation', { name: zhCN.appShell.navLabel })).toBeInTheDocument()
      expect(getByRole('main')).toBeInTheDocument()
    })

    it('relabels the nav landmark and toggle button when the language switches to en-US', async () => {
      mockMatchMedia(false)
      const { getByRole, i18n } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>content</AppShell>,
      )
      await act(async () => {
        await switchLanguage(i18n, 'en-US')
      })
      // The temporary drawer starts closed, and MUI keeps a closed (but
      // keepMounted) modal's own root aria-hidden -- an aria-hidden
      // subtree contributes no accessible name at all (by the accname
      // spec), so `hidden: true` finds the node but a `name` filter on
      // it never matches. Assert the built-in text landed on the
      // attribute directly instead; open/close accessibility is covered
      // separately in the "responsive drawer" tests above.
      expect(getByRole('navigation', { hidden: true })).toHaveAttribute(
        'aria-label',
        enUS.appShell.navLabel,
      )
      expect(getByRole('button', { name: enUS.appShell.openNav })).toBeInTheDocument()
    })
  })

  describe('accessibility', () => {
    it('has no axe violations on the desktop layout, with region enabled', async () => {
      mockMatchMedia(true)
      renderWithProviders(
        <AppShell
          navItems={NAV_ITEMS}
          header={<span>Speed Admin</span>}
          userMenu={<span>Jane Doe</span>}
        >
          <p>Main content</p>
        </AppShell>,
      )
      await expectNoAxeViolations()
    })

    it('has no axe violations on the mobile layout, with region enabled', async () => {
      mockMatchMedia(false)
      renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>
          <p>Main content</p>
        </AppShell>,
      )
      await expectNoAxeViolations()
    })
  })
})
