/**
 * AppShell contract: a fully controlled, props-driven app-chrome shell.
 *
 * Covers: nav item wiring (selected/href/onClick, host-computed only --
 * AppShell never path-matches), the desktop/mobile drawer split driven
 * by useMediaQuery (permanent at md+, temporary below, uncontrolled by
 * default and promotable to controlled via mobileOpen/onMobileOpenChange),
 * that header/headerActions/userMenu/children render only host content,
 * the header/nav/main landmarks in both languages via the shipped
 * bundles, a zero-violation axe scan with `region` left enabled
 * (AppShell is page-level chrome, unlike ui-kit's per-widget components),
 * and this round's narrow-viewport CSS protections (the capped mobile
 * drawer width, the nav content filling its paper rather than
 * re-declaring the width itself, the wrapping AppBar row) -- see the
 * "responsive protections" describe block for what each assertion does
 * and does not prove.
 */

import { act, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import enUS from '../locales/en-US.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { emittedStyleText } from '../../test-utils/emitted-css.js'
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

    it('makes the main landmark focusable so the skip link actually moves focus there', () => {
      mockMatchMedia(true)
      const { getByRole } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>content</AppShell>,
      )
      const skipLink = getByRole('link', { name: zhCN.appShell.skipToContent })
      const main = getByRole('main')
      // The skip link must target the main landmark by id ...
      expect(skipLink.getAttribute('href')).toBe(`#${main.id}`)
      // ... and that target must be programmatically focusable
      // (tabIndex={-1}), or a browser's fragment-navigation focusing
      // algorithm only scrolls to it without ever moving the assistive
      // -tech focus cursor. jsdom enforces the same focusability rule
      // as browsers, so calling .focus() directly on the target is a
      // faithful check of the mechanism the fix relies on.
      main.focus()
      expect(document.activeElement).toBe(main)
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

  describe('responsive protections', () => {
    // jsdom evaluates neither real layout nor `@media` conditions, so
    // these are property/snapshot assertions on the generated CSS text
    // or on a plain (non-breakpoint-gated) computed style value -- they
    // prove the intended declaration was wired into the render, not
    // that it looks correct at a real narrow viewport.
    it('caps the mobile (temporary) drawer paper width to a CSS min() of sidebarWidth and a viewport fraction', () => {
      mockMatchMedia(false)
      renderWithProviders(
        <AppShell navItems={NAV_ITEMS} sidebarWidth={280}>
          content
        </AppShell>,
      )
      expect(emittedStyleText()).toMatch(/width:\s*min\(280px,\s*85vw\)/)
    })

    it('leaves the desktop (permanent) drawer paper width as the plain sidebarWidth, uncapped', () => {
      mockMatchMedia(true)
      renderWithProviders(
        <AppShell navItems={NAV_ITEMS} sidebarWidth={280}>
          content
        </AppShell>,
      )
      // A precise rule-text match (not a "no 85vw anywhere" assertion,
      // since emotion's injected <style> accumulates every rule any
      // test in this file has rendered so far and never removes one on
      // unmount) -- this is the permanent Drawer's own docked-paper
      // rule, plain box-sizing/width with no CSS min() wrapping it.
      expect(emittedStyleText()).toMatch(
        /\.MuiDrawer-paper\{box-sizing:border-box;width:280px;\}/,
      )
    })

    it('fills its Drawer paper with the nav content instead of re-declaring sidebarWidth, so the content can never outgrow the mobile paper\'s capped width', () => {
      // Regression test: the nav Box rendered inside both Drawer variants
      // previously carried its own fixed `width: sidebarWidth`, independent
      // of the paper's width. That was harmless for the desktop
      // (permanent) Drawer, whose paper is the plain sidebarWidth too, but
      // on the mobile (temporary) Drawer the paper is capped to
      // `min(sidebarWidth, 85vw)` -- a second, independent sidebarWidth on
      // the content silently outgrew that cap and bled past the paper's
      // fixed-position edge (Drawer's paper sets no overflow-x). Asserting
      // `width: 100%` on the nav element itself (fills whichever paper it
      // is mounted into) is what makes that impossible by construction,
      // in both the closed-mobile and desktop cases.
      mockMatchMedia(false)
      const { getByRole } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS} sidebarWidth={280}>
          content
        </AppShell>,
      )
      expect(getByRole('navigation', { hidden: true })).toHaveStyle({ width: '100%' })
    })

    it('fills the desktop Drawer paper with the nav content the same way', () => {
      mockMatchMedia(true)
      const { getByRole } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS} sidebarWidth={280}>
          content
        </AppShell>,
      )
      expect(
        getByRole('navigation', { name: zhCN.appShell.navLabel }),
      ).toHaveStyle({ width: '100%' })
    })

    it('wraps the AppBar Toolbar row instead of squeezing header/actions/userMenu', () => {
      mockMatchMedia(true)
      const { container } = renderWithProviders(
        <AppShell
          navItems={NAV_ITEMS}
          header={<span>Brand</span>}
          headerActions={<button type="button">Search</button>}
          userMenu={<span>Jane Doe</span>}
        >
          content
        </AppShell>,
      )
      const toolbar = container.querySelector('.MuiToolbar-root')
      expect(toolbar).not.toBeNull()
      expect(toolbar).toHaveStyle({ flexWrap: 'wrap' })
    })

    it('wraps the headerActions group itself when the host supplies several actions', () => {
      mockMatchMedia(true)
      const { getByRole } = renderWithProviders(
        <AppShell
          navItems={NAV_ITEMS}
          headerActions={
            <>
              <button type="button">Search</button>
              <button type="button">Notifications</button>
            </>
          }
        >
          content
        </AppShell>,
      )
      const actionsGroup = getByRole('button', { name: 'Search' }).parentElement
      expect(actionsGroup).toHaveStyle({ flexWrap: 'wrap' })
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
