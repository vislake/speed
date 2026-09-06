/**
 * AppShell: the responsive app-chrome shell.
 *
 * Renders a fixed `AppBar` (the `header`/banner landmark), a responsive
 * navigation drawer (a `nav` landmark: permanent at `md`+, temporary and
 * overlaid below `md`), and a `main` content landmark. The desktop/mobile
 * split is a JS boolean from `useMediaQuery(theme.breakpoints.up('md'))`
 * -- no new breakpoint is introduced, and the two Drawer variants are
 * chosen in JS rather than toggled through CSS-only breakpoint sx, so a
 * host that queries "is the desktop drawer showing" gets one deterministic
 * answer instead of two DOM nodes racing on paint.
 *
 * AppShell carries no navigation or auth semantics: `navItems` is fully
 * host-computed (including which item is `selected` -- AppShell never
 * does path matching, since different hosts use different routers) and
 * the mobile drawer's open state is the one interaction-local exception
 * (uncontrolled by default, like ConfirmDialog's double-confirm arm),
 * promotable to fully controlled via `mobileOpen` / `onMobileOpenChange`.
 *
 * Slots (`header`, `headerActions`, `userMenu`, `children`) render
 * exactly the host content passed in and nothing implicit -- an absent
 * slot renders nothing, never a placeholder.
 *
 * Narrow-viewport protection (additive, CSS-only, no prop changes):
 * the Toolbar row and the `headerActions` group both carry a responsive
 * `flexWrap: 'wrap'` so a host that supplies several header actions
 * wraps onto a second line under real overflow pressure instead of
 * being squeezed or clipped -- AppShell still renders only what the
 * host gives it (no auto-collapsing overflow menu is invented here,
 * consistent with this package's host-injected-everything design). The
 * mobile (temporary) Drawer's paper width is capped to
 * `min(sidebarWidth, 85vw)`, and the nav content rendered inside it
 * fills that paper (`width: 100%`) rather than re-declaring
 * `sidebarWidth` itself, so a very narrow viewport (~320px and below)
 * never gets a near-full-screen or overflowing drawer; the desktop
 * (permanent) Drawer's width, and its nav content, are untouched.
 *
 * Two interaction-local states live inside AppShell, both only when
 * the host keeps the drawer uncontrolled. First, the skip link focuses
 * the `main` landmark programmatically instead of navigating to a
 * fragment: fragment navigation rewrites `location.hash`, which in a
 * hash-routed host (the reference app routes exactly that way) replaces
 * the app route with the generated target id and loses the route.
 * Second, activating a nav item in the temporary drawer closes it --
 * the drawer owns its open state in uncontrolled mode, so it owns the
 * close-on-navigation transition (MUI's own close machinery clears the
 * scrim and restores focus). A controlled host keeps the existing
 * contract untouched: nav activation reports nothing and closing stays
 * the host's own next render.
 *
 * Because the header row can wrap, its rendered height is measured
 * (ResizeObserver on the banner, where the browser supports it) and
 * the three spacer placeholders -- the one inside the drawer paper and
 * the one inside `main` for each drawer variant -- derive their height
 * from that measurement rather than assuming the theme's single-row
 * toolbar height, so a wrapped header can never cover the top of the
 * content it overlays. Where no ResizeObserver exists (jsdom), the
 * spacers keep the plain theme-height Toolbar.
 */

import { useEffect, useId, useRef, useState } from 'react'
import type { MouseEvent, MouseEventHandler, ReactNode } from 'react'
import type { SxProps, Theme } from '@mui/material/styles'
import { useTheme } from '@mui/material/styles'
import AppBar from '@mui/material/AppBar'
import Box from '@mui/material/Box'
import Drawer from '@mui/material/Drawer'
import IconButton from '@mui/material/IconButton'
import Link from '@mui/material/Link'
import List from '@mui/material/List'
import ListItem from '@mui/material/ListItem'
import ListItemButton from '@mui/material/ListItemButton'
import ListItemIcon from '@mui/material/ListItemIcon'
import ListItemText from '@mui/material/ListItemText'
import Toolbar from '@mui/material/Toolbar'
import useMediaQuery from '@mui/material/useMediaQuery'
import { useLayoutKitTranslation } from '../internal/translation.js'
import { CloseIcon, MenuIcon } from '../internal/icons.js'

/**
 * One navigation entry. The host owns `selected` (computed from
 * whichever router it uses) and either `href` (renders as a link) or
 * `onClick` (renders as a button) -- both may be supplied together, for
 * example to intercept a client-side router link's default navigation.
 */
export interface AppShellNavItem {
  /** Stable identity for the React key; not rendered. */
  readonly id: string
  /** The item's visible label; host content, already translated. */
  readonly label: ReactNode
  /** Optional leading icon; host content. */
  readonly icon?: ReactNode
  /** Renders the item as a link to this destination when set. */
  readonly href?: string
  /** Click handler; fires whether or not `href` is also set. */
  readonly onClick?: MouseEventHandler<HTMLElement>
  /** Whether this is the current page/section, computed by the host. */
  readonly selected?: boolean
}

export interface AppShellProps {
  /** The navigation entries rendered in the drawer, in order. */
  readonly navItems: readonly AppShellNavItem[]
  /** Start-of-AppBar content: typically a logo or product name. */
  readonly header?: ReactNode
  /** End-of-AppBar content, before `userMenu`: search, notifications, and the like. */
  readonly headerActions?: ReactNode
  /** Far end-of-AppBar content: typically an account/user menu trigger. */
  readonly userMenu?: ReactNode
  /** The content region, rendered inside the `main` landmark. */
  readonly children: ReactNode
  /**
   * Mobile drawer open state. Omit both this and `onMobileOpenChange` to
   * let AppShell manage the toggle internally (uncontrolled); pass both
   * for a fully host-controlled drawer.
   */
  readonly mobileOpen?: boolean
  /** Fired whenever the mobile drawer's open state should change. */
  readonly onMobileOpenChange?: (open: boolean) => void
  /** Drawer width in px, both variants. Defaults to 280. */
  readonly sidebarWidth?: number
  /** Extra styling merged onto the root layout box. */
  readonly sx?: SxProps<Theme>
}

const DEFAULT_SIDEBAR_WIDTH = 280

function NavList({
  navItems,
  onNavigate,
}: {
  readonly navItems: readonly AppShellNavItem[]
  /**
   * Fired after the item's own onClick, when it has one, on every item
   * activation. The drawer variants decide what this means: the
   * temporary drawer closes itself on it (uncontrolled), the permanent
   * drawer passes none.
   */
  readonly onNavigate?: () => void
}) {
  const activate = (event: MouseEvent<HTMLElement>, item: AppShellNavItem): void => {
    item.onClick?.(event)
    onNavigate?.()
  }
  return (
    <List>
      {navItems.map((item) => {
        const label = (
          <>
            {item.icon !== undefined && <ListItemIcon>{item.icon}</ListItemIcon>}
            <ListItemText primary={item.label} />
          </>
        )
        return (
          <ListItem key={item.id} disablePadding>
            {item.href !== undefined ? (
              <ListItemButton
                component="a"
                href={item.href}
                onClick={(event) => activate(event, item)}
                selected={item.selected ?? false}
                aria-current={item.selected === true ? 'page' : undefined}
              >
                {label}
              </ListItemButton>
            ) : (
              <ListItemButton
                onClick={(event) => activate(event, item)}
                selected={item.selected ?? false}
                aria-current={item.selected === true ? 'page' : undefined}
              >
                {label}
              </ListItemButton>
            )}
          </ListItem>
        )
      })}
    </List>
  )
}

/**
 * The responsive app-chrome shell: fixed header, responsive nav drawer,
 * `main` content region. See the module doc comment for the full
 * contract.
 */
export function AppShell({
  navItems,
  header,
  headerActions,
  userMenu,
  children,
  mobileOpen: mobileOpenProp,
  onMobileOpenChange,
  sidebarWidth = DEFAULT_SIDEBAR_WIDTH,
  sx,
}: AppShellProps) {
  const { t } = useLayoutKitTranslation()
  const theme = useTheme()
  const isDesktop = useMediaQuery(theme.breakpoints.up('md'))
  const mainContentId = useId()

  // Interaction-only state, the same carve-out ConfirmDialog's double-
  // confirm arm relies on: promoted to fully controlled the moment the
  // host supplies mobileOpen, otherwise AppShell tracks it itself.
  const [uncontrolledOpen, setUncontrolledOpen] = useState(false)
  const isControlled = mobileOpenProp !== undefined
  const mobileOpen = isControlled ? mobileOpenProp : uncontrolledOpen

  const setMobileOpen = (next: boolean) => {
    if (!isControlled) {
      setUncontrolledOpen(next)
    }
    onMobileOpenChange?.(next)
  }

  // The uncontrolled drawer's open state belongs to the temporary
  // variant: a portrait-open drawer that grows into the permanent
  // (always-open) variant at md+ must not come back open when the
  // viewport narrows again. Reset it the moment the variant leaves
  // temporary. Controlled hosts own the state themselves; nothing is
  // reset on their behalf.
  useEffect(() => {
    if (isDesktop && !isControlled) {
      setUncontrolledOpen(false)
    }
  }, [isDesktop, isControlled])

  // The header row wraps under real overflow pressure, so its rendered
  // height is measured and every spacer placeholder derives its height
  // from the measurement (see the "spacer" note at the placeholders).
  // Without a ResizeObserver (jsdom) the measurement never arrives and
  // the spacers keep the theme toolbar height, exactly as before.
  const [headerHeight, setHeaderHeight] = useState<number | null>(null)
  const appBarRef = useRef<HTMLDivElement | null>(null)
  useEffect(() => {
    const appBar = appBarRef.current
    if (appBar === null || typeof ResizeObserver === 'undefined') {
      return undefined
    }
    const observer = new ResizeObserver((entries) => {
      const entry = entries[entries.length - 1]
      if (entry !== undefined) {
        setHeaderHeight(Math.round(entry.contentRect.height))
      }
    })
    observer.observe(appBar)
    return () => observer.disconnect()
  }, [])

  // The skip link's activation target. Kept in a ref rather than
  // resolved by id so the handler never touches the document: the
  // element the link labels is the element it focuses.
  const mainContentRef = useRef<HTMLDivElement | null>(null)

  const focusMainContent = (): void => {
    mainContentRef.current?.focus()
  }

  // The nav content mounts into either Drawer paper. It fills whichever
  // paper it lands in (`width: 100%`) rather than re-declaring
  // sidebarWidth itself -- the permanent (desktop) paper is exactly
  // sidebarWidth, but the temporary (mobile) paper is capped to
  // `min(sidebarWidth, 85vw)` below, and a second, independent
  // sidebarWidth here would silently outgrow that cap and bleed past
  // the paper's right edge (Drawer's paper is `position: fixed` with no
  // overflow-x, so the overflow is not clipped, just visible). The
  // permanent variant passes no onNavigate (nothing to close); the
  // temporary variant passes the close transition -- see the module doc
  // comment for who owns it in which mode.
  const navContent = (onNavigate?: () => void) => (
    <Box component="nav" aria-label={t('appShell.navLabel')} sx={{ width: '100%', boxSizing: 'border-box' }}>
      <NavList navItems={navItems} onNavigate={onNavigate} />
    </Box>
  )

  // The three offset placeholders that keep content clear of the fixed
  // AppBar: one inside the drawer paper and one inside `main`, for
  // whichever drawer variant is mounted. When the header height has
  // been measured, each spacer carries that exact height; before any
  // measurement exists it is a plain theme-height Toolbar (the
  // single-row case, which is also the whole jsdom case).
  const spacer = (
    <Toolbar sx={headerHeight !== null ? { minHeight: headerHeight } : undefined} />
  )

  return (
    <Box
      sx={[
        { display: 'flex', minHeight: '100vh' },
        ...(Array.isArray(sx) ? sx : sx ? [sx] : []),
      ]}
    >
      <AppBar position="fixed" ref={appBarRef} sx={{ zIndex: theme.zIndex.drawer + 1 }}>
        {/* Visually hidden until focused; the first focusable element in
            the shell, so it must live before the nav toggle in DOM order.
            It stays inside the header/banner landmark so it never counts
            as page content outside a landmark. Activating it focuses the
            main landmark directly instead of navigating to the fragment:
            fragment navigation rewrites location.hash, and in a
            hash-routed host the fragment IS the route (the reference app
            routes exactly that way) -- a skip link that navigates would
            replace the app route with the generated target id. The
            keyboard path is unchanged: Tab reaches the link, Enter
            activates it, and the focus visibly moves into main (browsers
            scroll a focused element into view, matching what fragment
            navigation would have scrolled to). */}
        <Link
          href={`#${mainContentId}`}
          onClick={(event) => {
            event.preventDefault()
            focusMainContent()
          }}
          sx={{
            position: 'absolute',
            width: 1,
            height: 1,
            padding: 0,
            margin: -1,
            overflow: 'hidden',
            whiteSpace: 'nowrap',
            clipPath: 'inset(50%)',
            color: 'inherit',
            '&:focus': {
              position: 'fixed',
              top: 8,
              left: 8,
              width: 'auto',
              height: 'auto',
              margin: 0,
              padding: 1,
              overflow: 'visible',
              whiteSpace: 'normal',
              clipPath: 'none',
              zIndex: theme.zIndex.tooltip,
              backgroundColor: 'background.paper',
              color: 'text.primary',
            },
          }}
        >
          {t('appShell.skipToContent')}
        </Link>
        <Toolbar sx={{ flexWrap: 'wrap', rowGap: 1 }}>
          {!isDesktop && (
            <IconButton
              color="inherit"
              edge="start"
              aria-label={mobileOpen ? t('appShell.closeNav') : t('appShell.openNav')}
              aria-expanded={mobileOpen}
              onClick={() => setMobileOpen(!mobileOpen)}
              sx={{ marginRight: 2 }}
            >
              {mobileOpen ? <CloseIcon /> : <MenuIcon />}
            </IconButton>
          )}
          <Box sx={{ flexGrow: 1, display: 'flex', alignItems: 'center', minWidth: 0 }}>
            {header}
          </Box>
          {headerActions !== undefined && (
            <Box
              sx={{
                display: 'flex',
                alignItems: 'center',
                flexWrap: 'wrap',
                gap: 1,
                marginRight: userMenu !== undefined ? 2 : 0,
              }}
            >
              {headerActions}
            </Box>
          )}
          {userMenu}
        </Toolbar>
      </AppBar>
      <Box
        sx={{
          width: { md: sidebarWidth },
          flexShrink: { md: 0 },
        }}
      >
        {isDesktop ? (
          <Drawer
            variant="permanent"
            open
            sx={{ '& .MuiDrawer-paper': { boxSizing: 'border-box', width: sidebarWidth } }}
          >
            {spacer}
            {navContent()}
          </Drawer>
        ) : (
          <Drawer
            variant="temporary"
            open={mobileOpen}
            onClose={() => setMobileOpen(false)}
            ModalProps={{ keepMounted: true }}
            sx={{
              '& .MuiDrawer-paper': {
                boxSizing: 'border-box',
                // Capped to the viewport so a very narrow screen (~320px
                // and below) never gets a near-full-screen or
                // overflowing drawer; the permanent (desktop) Drawer
                // above is untouched and keeps the plain sidebarWidth.
                width: `min(${sidebarWidth}px, 85vw)`,
              },
            }}
          >
            {spacer}
            {navContent(() => {
              // The temporary drawer owns the close-on-navigation
              // transition when it owns the open state. In controlled
              // mode this fires nothing and closes nothing: the host
              // observes the navigation itself and closes through its
              // own next render, the unchanged contract.
              if (!isControlled) {
                setUncontrolledOpen(false)
              }
            })}
          </Drawer>
        )}
      </Box>
      <Box
        component="main"
        id={mainContentId}
        ref={mainContentRef}
        // A bare `main` has no tabindex, so it is not natively focusable
        // and neither native fragment navigation nor the skip link's
        // programmatic focus could land on it. tabIndex={-1} makes it a
        // valid, non-tab-order focus target without adding a Tab stop.
        tabIndex={-1}
        sx={{
          flexGrow: 1,
          minWidth: 0,
          width: { md: `calc(100% - ${sidebarWidth}px)` },
        }}
      >
        {spacer}
        {children}
      </Box>
    </Box>
  )
}
