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
 * and does not prove. The a11y- and state-hardening regressions live
 * alongside: the skip link activating without touching the host hash
 * (a hash-routed host would lose the route to fragment navigation),
 * the uncontrolled temporary drawer closing itself on nav-item
 * activation with the scrim cleared and focus released (controlled
 * contract untouched), the open state resetting when the drawer leaves
 * the temporary variant for the permanent one -- the uncontrolled state
 * cleared, a controlled host told through onMobileOpenChange on the
 * same crossing -- and the header-spacer placeholders deriving from
 * the measured header height ("measured header spacer" describe block
 * states what jsdom can and cannot prove about the wrap case).
 */

import { useCallback, useState } from 'react'
import { act, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import enUS from '../locales/en-US.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { emittedStyleText } from '../../test-utils/emitted-css.js'
import { mockMatchMedia } from '../../test-utils/matchMedia.js'
import { renderWithProviders } from '../../test-utils/render.js'
import { stubResizeObserver } from '../../test-utils/resize-observer.js'
import { AppShell, type AppShellNavItem } from './AppShell.js'

// Some harnesses below put an app route into location.hash the way a
// hash-routed host would; nothing else in this file reads the hash, but
// restore it after every test so no test inherits another's route.
afterEach(() => {
  window.location.hash = ''
})

const NAV_ITEMS: readonly AppShellNavItem[] = [
  { id: 'home', label: 'Home', href: '/home', selected: true },
  { id: 'reports', label: 'Reports', href: '/reports' },
]

// The controlled drawer's host, the shape a real controlled consumer
// renders: the open state lives in the host component, AppShell reports
// every change through onMobileOpenChange, and nothing else writes it.
// The widen-and-narrow regression below must be driven by that callback
// alone -- a host that closed itself at the crossing would test nothing.
function ControlledDrawerHost({
  initialOpen,
  onMobileOpenChange,
}: {
  readonly initialOpen: boolean
  readonly onMobileOpenChange: (open: boolean) => void
}) {
  const [open, setOpen] = useState(initialOpen)
  const reportOpenChange = useCallback(
    (next: boolean) => {
      onMobileOpenChange(next)
      setOpen(next)
    },
    [onMobileOpenChange],
  )
  return (
    <AppShell
      navItems={NAV_ITEMS}
      mobileOpen={open}
      onMobileOpenChange={reportOpenChange}
    >
      content
    </AppShell>
  )
}

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

    it('closes the uncontrolled temporary drawer itself when a nav item is activated, scrim cleared and focus released', async () => {
      // Regression: activating a nav item in the mobile drawer used to
      // leave the drawer open -- the temporary variant owns its open
      // state in uncontrolled mode, so it owns the close-on-navigation
      // transition too (the scrim clearing and the focus returning to
      // the document are part of that close, not separate mechanisms).
      mockMatchMedia(false)
      const user = userEvent.setup()
      const { getByRole } = renderWithProviders(
        <AppShell
          navItems={[
            { id: 'home', label: 'Home', href: '#/home' },
            { id: 'notes', label: 'Notes', href: '#/notes' },
          ]}
        >
          content
        </AppShell>,
      )
      await user.click(getByRole('button', { name: zhCN.appShell.openNav }))
      const homeLink = getByRole('link', { name: 'Home' })
      await user.click(homeLink)

      // The drawer closes by itself (the toggle is back to openNav and
      // aria-expanded false -- the suite's established close marker) ...
      await waitFor(() => {
        expect(getByRole('button', { name: zhCN.appShell.openNav })).toHaveAttribute(
          'aria-expanded',
          'false',
        )
      })
      // ... and the scrim is cleared: MUI keeps the closed modal's
      // backdrop mounted (keepMounted) but invisible, so "cleared" is a
      // visibility assertion, not an absence one.
      const backdrop = document.querySelector('.MuiBackdrop-root')
      expect(backdrop).not.toBeNull()
      expect(backdrop).not.toBeVisible()
      // ... the host navigation still ran (jsdom performs the anchor's
      // fragment navigation; AppShell never blocks a nav item) ...
      expect(window.location.hash).toBe('#/home')
      // ... and focus is back in the document instead of stranded
      // inside the closed drawer's aria-hidden region.
      const paper = document.querySelector('.MuiDrawer-paper')
      expect(paper).not.toBeNull()
      expect(paper?.contains(document.activeElement)).toBe(false)
    })

    it('keeps the controlled contract on nav activation: reports nothing and stays open until the host closes', async () => {
      // The close-on-navigation transition is AppShell's own only while
      // AppShell owns the open state. A controlled host keeps the
      // existing contract untouched: no callback fires from a nav-item
      // click and the drawer stays open until the host's own next
      // render closes it.
      mockMatchMedia(false)
      const onMobileOpenChange = vi.fn()
      const user = userEvent.setup()
      const { getByRole } = renderWithProviders(
        <AppShell
          navItems={[{ id: 'home', label: 'Home', href: '#/home' }]}
          mobileOpen
          onMobileOpenChange={onMobileOpenChange}
        >
          content
        </AppShell>,
      )
      await user.click(getByRole('link', { name: 'Home' }))

      expect(onMobileOpenChange).not.toHaveBeenCalled()
      expect(
        getByRole('button', { name: zhCN.appShell.closeNav, hidden: true }),
      ).toHaveAttribute('aria-expanded', 'true')
    })

    it('does not bring a portrait-open drawer back open after a widen-and-narrow round trip', async () => {
      // Regression: the uncontrolled open state used to survive a
      // breakpoint crossing -- a drawer opened in portrait stayed true
      // while the layout grew into the permanent (always-open) variant
      // and re-popped open when the viewport narrowed back. The open
      // state belongs to the temporary variant alone; leaving temporary
      // resets it.
      const media = mockMatchMedia(false)
      const user = userEvent.setup()
      const { getByRole, queryByRole } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>content</AppShell>,
      )

      // Portrait: open the temporary drawer.
      await user.click(getByRole('button', { name: zhCN.appShell.openNav }))
      expect(
        getByRole('button', { name: zhCN.appShell.closeNav, hidden: true }),
      ).toHaveAttribute('aria-expanded', 'true')

      // Widen past md: the toggle leaves and the permanent drawer takes
      // over. (queryByRole, not getByRole: an absence assertion must not
      // throw while the element is still disappearing -- waitFor would
      // retry the thrown query until timeout.)
      act(() => {
        media.changeMatches(true)
      })
      await waitFor(() => {
        expect(
          queryByRole('button', { name: zhCN.appShell.openNav, hidden: true }),
        ).not.toBeInTheDocument()
      })
      expect(getByRole('navigation', { name: zhCN.appShell.navLabel })).toBeInTheDocument()

      // Narrow back below md: the temporary drawer must come back
      // closed -- pre-fix it re-pops open, because the portrait-open
      // state rode through the permanent spell untouched.
      act(() => {
        media.changeMatches(false)
      })
      await waitFor(() => {
        expect(getByRole('button', { name: zhCN.appShell.openNav })).toHaveAttribute(
          'aria-expanded',
          'false',
        )
      })
    })

    it('tells a controlled host that a portrait-open drawer must close when the viewport widens, so narrowing back finds it closed', async () => {
      // Regression: the widen-and-narrow reset above only cleared the
      // uncontrolled state. The controlled half of the same
      // configuration was never told that the variant had left
      // temporary -- the reset effect was gated off by !isControlled and
      // no other path fires on a breakpoint crossing -- so a host
      // holding mobileOpen=true rode through the permanent spell
      // untouched and its drawer re-popped open (scrim and all) when the
      // viewport narrowed back. The breakpoint is AppShell's own
      // useMediaQuery knowledge, so onMobileOpenChange is the only
      // channel the host can reset through; the host below stores the
      // prop and is driven by that callback alone.
      const media = mockMatchMedia(false)
      const onMobileOpenChange = vi.fn()
      const { getByRole } = renderWithProviders(
        <ControlledDrawerHost initialOpen onMobileOpenChange={onMobileOpenChange} />,
      )

      // Portrait: the host's drawer starts open.
      expect(
        getByRole('button', { name: zhCN.appShell.closeNav, hidden: true }),
      ).toHaveAttribute('aria-expanded', 'true')

      // Widen past md: leaving temporary means open should become false,
      // reported exactly once -- the host applies it, and no re-fire
      // follows on the renders the reset itself causes.
      act(() => {
        media.changeMatches(true)
      })
      expect(onMobileOpenChange).toHaveBeenCalledTimes(1)
      expect(onMobileOpenChange).toHaveBeenCalledWith(false)

      // Narrow back below md: the temporary drawer comes back closed,
      // because the host's state followed the notification -- pre-fix
      // the host never heard about the crossing and the drawer re-popped
      // open.
      act(() => {
        media.changeMatches(false)
      })
      await waitFor(() => {
        expect(getByRole('button', { name: zhCN.appShell.openNav })).toHaveAttribute(
          'aria-expanded',
          'false',
        )
      })
    })

    it('does not hand a controlled host a meaningless close when the viewport widens past a drawer that is already closed', () => {
      // The reset notification is gated on the drawer actually being
      // open: a host already at false crossing into the permanent
      // variant has nothing to learn from the crossing, so no callback
      // fires -- the gate keeps the crossing silent for the closed case
      // while still reporting it for the open one.
      const media = mockMatchMedia(false)
      const onMobileOpenChange = vi.fn()
      renderWithProviders(
        <ControlledDrawerHost initialOpen={false} onMobileOpenChange={onMobileOpenChange} />,
      )
      act(() => {
        media.changeMatches(true)
      })
      act(() => {
        media.changeMatches(false)
      })
      expect(onMobileOpenChange).not.toHaveBeenCalled()
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
      // The skip link keeps an anchor identity (and, with it, an href) ...
      expect(skipLink.getAttribute('href')).toBe(`#${main.id}`)
      // ... and the target must be programmatically focusable
      // (tabIndex={-1}) for the skip mechanism's own programmatic focus
      // to land, or a browser's fragment-navigation focusing algorithm
      // only scrolls to it without ever moving the assistive-tech focus
      // cursor. jsdom enforces the same focusability rule as browsers,
      // so calling .focus() directly on the target is a faithful check
      // of the mechanism the fix relies on.
      main.focus()
      expect(document.activeElement).toBe(main)
    })

    it('activates the skip link without touching the host hash and moves focus into main', async () => {
      // Regression: the skip link used to be a plain `href="#target"`
      // anchor, and activating it ran real fragment navigation -- which
      // REWRITES location.hash. For a host that routes through the
      // fragment (the reference app's own router parses location.hash
      // into its surfaces), that replaces the app route with the
      // generated target id and the route is lost. The harness below
      // mimics such a host: the hash carries an app route, and the skip
      // link must leave it alone while still moving focus -- driven the
      // way a real keyboard user would (Tab to it, Enter to activate).
      mockMatchMedia(true)
      window.location.hash = '/notes'
      const user = userEvent.setup()
      const { getByRole } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>content</AppShell>,
      )
      const main = getByRole('main')
      const skipLink = getByRole('link', { name: zhCN.appShell.skipToContent })

      await user.tab()
      expect(skipLink).toHaveFocus()
      await user.keyboard('{Enter}')

      // The app route survives the activation ...
      expect(window.location.hash).toBe('#/notes')
      // ... and focus moved into the main landmark. jsdom performs the
      // fragment navigation itself (it rewrites the hash and never
      // moves focus), so both halves of this pair fail on the old
      // mechanism: the hash is replaced by the target id and focus
      // stays on the link.
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

  describe('measured header spacer', () => {
    afterEach(() => {
      vi.unstubAllGlobals()
    })

    it('derives every spacer from the measured header height, so a wrapped header cannot cover content below it', async () => {
      // The AppBar row wraps under real overflow pressure (the flexWrap
      // protection), which makes its height taller than the theme
      // toolbar default the placeholders used to assume -- a taller
      // fixed header then covered the top of main and the drawer's
      // first item. jsdom evaluates no real layout, so this test cannot
      // observe an actual wrap or measure actual coverage; what it can
      // and does prove is the mechanism that keeps the two in lockstep:
      // the header's rendered height is measured (ResizeObserver on the
      // banner) and every spacer placeholder (one in the drawer paper,
      // one in main) derives its min-height from that measurement, and
      // the drawer's nav follows its spacer in document order. In a
      // real browser that height equality is what keeps the fixed
      // AppBar from covering main and the drawer when the toolbar
      // wraps onto a second row.
      mockMatchMedia(true)
      const resizeObserver = stubResizeObserver()
      const { getByRole } = renderWithProviders(
        <AppShell
          navItems={NAV_ITEMS}
          header={<span>Brand</span>}
          headerActions={<button type="button">Search</button>}
          userMenu={<span>Jane Doe</span>}
        >
          content
        </AppShell>,
      )
      // The observer watches the header/banner itself ...
      expect(resizeObserver.observedElement()).toBe(getByRole('banner'))

      // ... and reports a wrapped, two-row header height.
      resizeObserver.emitHeight(112)

      // The main landmark's spacer carries the measured height ...
      const main = getByRole('main')
      const mainSpacer = main.querySelector('.MuiToolbar-root')
      expect(mainSpacer).not.toBeNull()
      expect(mainSpacer).toHaveStyle({ minHeight: '112px' })

      // ... and so does the drawer paper's spacer ...
      const paper = document.querySelector('.MuiDrawer-paper')
      expect(paper).not.toBeNull()
      const drawerSpacer = paper?.querySelector('.MuiToolbar-root') ?? null
      expect(drawerSpacer).not.toBeNull()
      expect(drawerSpacer).toHaveStyle({ minHeight: '112px' })

      // ... so the first drawer item sits below the spacer in the
      // paper, exactly where the fixed header's overlay ends when the
      // spacer matches the header's height.
      const nav = paper?.querySelector('nav') ?? null
      expect(nav).not.toBeNull()
      expect(
        (drawerSpacer as Node).compareDocumentPosition(nav as Node) &
          Node.DOCUMENT_POSITION_FOLLOWING,
      ).toBeTruthy()
    })
  })

  describe('accessibility', () => {
    // Both closed-layout scans render the shell with the page content a
    // real host puts inside `main` -- including the page's h1, which
    // chrome deliberately does not render itself: page-has-heading-one
    // is determinate in jsdom now (see the axe helper header), so a
    // scan document without an h1 fails instead of passing by
    // indeterminacy. The shell's own landmark structure (a real main)
    // answers landmark-one-main; region stays enabled.
    it('has no axe violations on the desktop layout, with region enabled', async () => {
      mockMatchMedia(true)
      renderWithProviders(
        <AppShell
          navItems={NAV_ITEMS}
          header={<span>Speed Admin</span>}
          userMenu={<span>Jane Doe</span>}
        >
          <h1>Main content</h1>
        </AppShell>,
      )
      await expectNoAxeViolations()
    })

    it('has no axe violations on the mobile layout, with region enabled', async () => {
      mockMatchMedia(false)
      renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>
          <h1>Main content</h1>
        </AppShell>,
      )
      await expectNoAxeViolations()
    })

    it('has no axe violations on the mobile layout with the drawer OPEN, with region enabled', async () => {
      // Regression: the axe runs above scan the mobile drawer closed --
      // the only state in which the scans used to run -- while OPEN the
      // temporary drawer is a real modal (role=dialog aria-modal) and
      // the only usable state of mobile navigation. Pre-fix the open
      // drawer's paper had no accessible name: axe's aria-dialog-name
      // rule failed the paper (impact serious) and no scan measured it.
      // The drawer's paper now carries the nav label as its dialog name
      // (see AppShell.tsx), and this scan runs over the open state.
      // Page-context rules are exempted while the modal is open, the
      // same passForModal semantics a browser applies.
      mockMatchMedia(false)
      const user = userEvent.setup()
      const { getByRole } = renderWithProviders(
        <AppShell navItems={NAV_ITEMS}>
          <h1>Main content</h1>
        </AppShell>,
      )
      await user.click(getByRole('button', { name: zhCN.appShell.openNav }))
      expect(
        document.querySelector('.MuiDrawer-paper')?.getAttribute('aria-modal'),
      ).toBe('true')
      await expectNoAxeViolations()
    })
  })
})
