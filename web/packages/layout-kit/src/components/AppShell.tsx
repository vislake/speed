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
 */

import { useId, useState } from 'react'
import type { MouseEventHandler, ReactNode } from 'react'
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

function NavList({ navItems }: { readonly navItems: readonly AppShellNavItem[] }) {
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
                onClick={item.onClick}
                selected={item.selected ?? false}
                aria-current={item.selected === true ? 'page' : undefined}
              >
                {label}
              </ListItemButton>
            ) : (
              <ListItemButton
                onClick={item.onClick}
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

  const navContent = (
    <Box component="nav" aria-label={t('appShell.navLabel')} sx={{ width: sidebarWidth }}>
      <NavList navItems={navItems} />
    </Box>
  )

  return (
    <Box
      sx={[
        { display: 'flex', minHeight: '100vh' },
        ...(Array.isArray(sx) ? sx : sx ? [sx] : []),
      ]}
    >
      <AppBar position="fixed" sx={{ zIndex: theme.zIndex.drawer + 1 }}>
        {/* Visually hidden until focused; the first focusable element in
            the shell, so it must live before the nav toggle in DOM order.
            It stays inside the header/banner landmark so it never counts
            as page content outside a landmark. */}
        <Link
          href={`#${mainContentId}`}
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
        <Toolbar>
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
            <Toolbar />
            {navContent}
          </Drawer>
        ) : (
          <Drawer
            variant="temporary"
            open={mobileOpen}
            onClose={() => setMobileOpen(false)}
            ModalProps={{ keepMounted: true }}
            sx={{ '& .MuiDrawer-paper': { boxSizing: 'border-box', width: sidebarWidth } }}
          >
            <Toolbar />
            {navContent}
          </Drawer>
        )}
      </Box>
      <Box
        component="main"
        id={mainContentId}
        sx={{
          flexGrow: 1,
          minWidth: 0,
          width: { md: `calc(100% - ${sidebarWidth}px)` },
        }}
      >
        <Toolbar />
        {children}
      </Box>
    </Box>
  )
}
