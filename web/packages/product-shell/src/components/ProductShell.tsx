/**
 * ProductShell: the tenant-facing assembly shell over an @speed/auth-core
 * session.
 *
 * ProductShell is the view machine between "who is looking at this page"
 * and the app frame, in exactly the shape auth-ui's suite has been
 * driving since its first journey (the SessionGate pattern of that
 * package's test-utils, shipped here as package code because the shell
 * tier is the one place the session hooks are meant to be consumed --
 * the tiers below never do):
 *
 *   authenticated            -> the AppShell frame around the app
 *                               children (navItems and the other chrome
 *                               props pass through unchanged), and the
 *                               shell remembers that the app was reached
 *   anonymous, app reached   -> the sessionEnded slot, or auth-ui's own
 *                               SessionEndedScreen whose action resets
 *                               the memory and returns to the sign-in
 *                               branch
 *   anonymous, app never
 *   reached (or unattached)  -> the signIn slot, or nothing when the
 *                               host supplies none
 *
 * The reachedApp memory is the whole difference between a fresh visitor
 * (sign-in view, always) and a user whose session died mid-use
 * (session-ended view, with a way back). It is transition memory only:
 * the snapshot itself comes from useAuthState, so the machine renders
 * from the session the host attached with attachSession (last bind wins)
 * and fails closed before any attach -- an unattached shell can never
 * show the frame or the session-ended screen, whatever the slots say.
 *
 * Every view except the frame is a host slot: signIn is the host's own
 * sign-in surface (auth-ui's SignInScreen family is the natural fit but
 * never imported here), and sessionEnded may replace the default
 * ended screen wholesale -- a supplied node renders as-is and owns its
 * own way back (typically by signing in again, which the machine
 * observes through the snapshot flip). The shell itself renders no text
 * of its own: the frame's built-in strings come from layout-kit's
 * namespace, the default ended screen's from auth-ui's, and everything
 * else is host content -- so the package ships no locale files and no
 * namespace, and the host registers exactly the three namespaces the
 * views it actually renders read.
 *
 * ProductShell performs no session operations, navigates nowhere and
 * makes no requests: the session's own transitions drive it through the
 * hooks, login and logout are called by the views the host slotted in
 * (auth-ui's forms and SignOutButton), and route-level authorization
 * (layout-kit's RouteGuard fed by a real status source) is a later,
 * host-side concern inside the children.
 */

import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { useAuthState } from '@speed/auth-core'
import { SessionEndedScreen } from '@speed/auth-ui'
import { AppShell } from '@speed/layout-kit'
import type { AppShellProps } from '@speed/layout-kit'

/** The AppShell chrome props the authenticated frame is built from,
 * re-declared here by Pick so a chrome change lands in AppShellProps
 * only (single source of truth) while the shell keeps shipping the
 * shell-shaped surface below. */
type AppShellChromeProps = Pick<
  AppShellProps,
  | 'navItems'
  | 'header'
  | 'headerActions'
  | 'userMenu'
  | 'mobileOpen'
  | 'onMobileOpenChange'
  | 'sidebarWidth'
  | 'sx'
>

export interface ProductShellProps extends AppShellChromeProps {
  /** The view for the anonymous-and-never-in-the-app branch (a fresh
   * visitor, or a shell nothing has been attached to yet): the host's
   * sign-in surface. Omitted, that branch renders nothing. */
  readonly signIn?: ReactNode
  /** The view for the anonymous-but-the-app-was-reached branch (a
   * session that ended mid-use). Omitted, auth-ui's SessionEndedScreen
   * renders with its sign-in-again action wired to return to the
   * signIn branch. A supplied node renders as-is, machine state
   * included: it is the host's own ended view and it owns its own way
   * out (signing in again flips the snapshot and the frame returns). */
  readonly sessionEnded?: ReactNode
  /** The app content, rendered inside the frame's main landmark only
   * while the snapshot is authenticated. Route-level gates (layout-kit
   * RouteGuard) and the tenant-switch affordance compose here, both
   * host-side. */
  readonly children: ReactNode
}

export function ProductShell({
  navItems,
  header,
  headerActions,
  userMenu,
  mobileOpen,
  onMobileOpenChange,
  sidebarWidth,
  sx,
  signIn,
  sessionEnded,
  children,
}: ProductShellProps) {
  const snapshot = useAuthState()
  const [reachedApp, setReachedApp] = useState(false)

  useEffect(() => {
    // Once the authenticated frame has appeared, remember that it did:
    // the anonymous branch after a logout must show the session-ended
    // view, not a fresh-visitor sign-in. Keyed on the state value, so
    // re-renders that leave it unchanged never re-fire. The memory is
    // deliberately not reset on logout -- that is the SessionEndedScreen
    // action's job, or the host's own ended view's.
    if (snapshot.state === 'authenticated') {
      setReachedApp(true)
    }
  }, [snapshot.state])

  if (snapshot.state === 'authenticated') {
    return (
      <AppShell
        navItems={navItems}
        header={header}
        headerActions={headerActions}
        userMenu={userMenu}
        mobileOpen={mobileOpen}
        onMobileOpenChange={onMobileOpenChange}
        sidebarWidth={sidebarWidth}
        sx={sx}
      >
        {children}
      </AppShell>
    )
  }
  if (reachedApp) {
    if (sessionEnded !== undefined) {
      return <>{sessionEnded}</>
    }
    return <SessionEndedScreen onSignIn={() => setReachedApp(false)} />
  }
  return <>{signIn}</>
}
