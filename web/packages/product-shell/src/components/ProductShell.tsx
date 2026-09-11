/**
 * ProductShell: the tenant-facing assembly shell over an @speed/auth-core
 * session.
 *
 * ProductShell is the view machine between "who is looking at this page"
 * and the app frame, in exactly the shape auth-ui's suite drives through
 * its session gate (the SessionGate pattern of that package's
 * test-utils, shipped here as package code because the shell
 * tier is the one place the session hooks are meant to be consumed --
 * the tiers below never do):
 *
 *   authenticated            -> the AppShell frame around the app
 *                               children (navItems and the other chrome
 *                               props pass through unchanged), and the
 *                               shell remembers that the app was reached
 *   anonymous, app reached   -> the sessionEnded slot, or auth-ui's own
 *                               SessionEndedScreen whose action returns
 *                               to the sign-in branch -- when the host
 *                               supplied one (below)
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
 * observes through the snapshot flip). The shell itself renders one
 * string of its own, the polite transition announcement of the
 * session-ended flip, from the product-shell namespace
 * (PRODUCT_SHELL_NAMESPACE, registered by the host like every
 * sibling's); the frame's built-in strings come from layout-kit's
 * namespace, the default ended screen's from auth-ui's, and everything
 * else is host content.
 *
 * The machine is a whole-page switch, so it owns the two a11y duties a
 * page swap carries, in the sibling family's shape: every branch flip
 * moves focus into the branch's own container (the branches render
 * inside a focusable, non-tab-stop wrapper -- never into a slot the
 * host may not be able to reach), and every flip into the session-ended
 * view -- the one destination the shell cannot distinguish by cause, a
 * server-side session death and an explicit sign-out being the same
 * snapshot flip -- additionally announces itself through a role="status"
 * region (the family convention; the flips the user's own action aimed
 * at, whose destinations announce themselves, move focus only). The
 * announcement renders only where the product-shell namespace is
 * registered: an unregistered host gets the focus transfer and no raw
 * key text -- the missing-key noise of an unregistered namespace must
 * never regress into view (the focus half is namespace-independent and
 * always on).
 *
 * ProductShell performs no session operations, navigates nowhere and
 * makes no requests: the session's own transitions drive it through the
 * hooks, login and logout are called by the views the host slotted in
 * (auth-ui's forms and SignOutButton), and route-level authorization
 * (layout-kit's RouteGuard fed by a status source) is composed
 * host-side inside the children.
 */

import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import Box from '@mui/material/Box'
import { useAuthState } from '@speed/auth-core'
import { SessionEndedScreen } from '@speed/auth-ui'
import { useTranslation } from '@speed/i18n'
import { AppShell } from '@speed/layout-kit'
import type { AppShellProps } from '@speed/layout-kit'
import { visuallyHiddenSx } from '@speed/ui-kit'
import { PRODUCT_SHELL_NAMESPACE } from '../resources.js'

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
   * sign-in surface. Omitted, that branch renders nothing. The default
   * ended screen's action needs this view to exist: it returns the
   * viewer to the sign-in branch, and the machine refuses to reset into
   * a branch that would render nothing -- a host without a signIn view
   * whose session ends mid-use stays on the ended screen, whose only
   * way back into the app is the host's own (its blank-branch pairing,
   * or a sessionEnded node of its own). */
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

/** The branch the machine is currently on -- the identity a flip is
 * measured against, so a re-render that leaves it unchanged never moves
 * focus or announces. */
type Branch = 'frame' | 'ended' | 'signIn'

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
  const { t, i18n } = useTranslation(PRODUCT_SHELL_NAMESPACE)
  // The ended branch's polite announcement, stored structurally and
  // rendered at render time in the active language -- the FileUploader
  // pattern (a standing role="status" region whose text flips). It is
  // set by the flip effect only after the ended branch has committed
  // with an empty region, so the text lands as an insertion into a live
  // region that is already in the tree -- the one reliable announce.
  const [announced, setAnnounced] = useState(false)
  // The focus target of whichever branch renders: conditional rendering
  // mounts exactly one branch, so this ref always names the current
  // branch's container.
  const branchContainerRef = useRef<HTMLDivElement | null>(null)
  const previousBranchRef = useRef<Branch | null>(null)

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

  const branch: Branch =
    snapshot.state === 'authenticated'
      ? 'frame'
      : reachedApp
        ? 'ended'
        : 'signIn'

  // The branch-flip effect: on a real change of branch -- never on the
  // first render, whose focus belongs to the host's page load -- move
  // focus into the new branch's container, and manage the polite
  // announcement: a flip into the ended view announces (session death
  // and explicit sign-out are the same snapshot flip; the destination
  // screen the shell itself placed there is the flip worth a polite
  // word), and any flip that leaves the ended view clears the standing
  // announcement so the next arrival inserts fresh text into the live
  // region instead of finding it pre-filled.
  useEffect(() => {
    const previous = previousBranchRef.current
    previousBranchRef.current = branch
    if (previous === null || previous === branch) {
      return
    }
    branchContainerRef.current?.focus()
    if (branch === 'ended') {
      setAnnounced(true)
    } else if (previous === 'ended') {
      setAnnounced(false)
    }
  }, [branch])

  // The shell's own text needs its own namespace registration, exactly
  // like every sibling's; an unregistered host renders no region and
  // no raw keys, and keeps the focus half of the a11y contract.
  const announcementsRegistered = i18n.hasResourceBundle(
    i18n.language ?? '',
    PRODUCT_SHELL_NAMESPACE,
  )

  if (snapshot.state === 'authenticated') {
    return (
      <Box ref={branchContainerRef} tabIndex={-1}>
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
      </Box>
    )
  }
  if (reachedApp) {
    return (
      // The ended branch replaces the whole app page, so its container
      // IS the page's main landmark (axe's landmark-one-main rule
      // requires one -- measured by the suite's ended-branch scans; the
      // authenticated branch's main comes from AppShell, and only one
      // branch renders at a time). Focus still lands on it through the
      // same tabIndex={-1} mechanism AppShell's own main uses.
      <Box ref={branchContainerRef} component="main" tabIndex={-1}>
        {announcementsRegistered && (
          <Box component="p" role="status" sx={visuallyHiddenSx}>
            {announced ? t('announcements.sessionEnded') : ''}
          </Box>
        )}
        {sessionEnded !== undefined ? (
          sessionEnded
        ) : (
          <SessionEndedScreen
            onSignIn={() => {
              // Only reset into a branch that renders. The action's
              // promise is "return to the sign-in view"; without a
              // signIn view there is no such destination, and resetting
              // would land the viewer on the deliberately-blank
              // fresh-visitor branch -- a whitescreen dead end. Such a
              // host's way back into the app is its own (see the signIn
              // prop's doc), so the action keeps the viewer on the
              // ended screen.
              if (signIn !== undefined) {
                setReachedApp(false)
              }
            }}
          />
        )}
      </Box>
    )
  }
  if (signIn === undefined) {
    return null
  }
  return (
    <Box ref={branchContainerRef} tabIndex={-1}>
      {signIn}
    </Box>
  )
}
