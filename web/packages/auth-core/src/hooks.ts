/**
 * hooks.ts -- the React bindings over the @speed/auth-core session.
 *
 * The hooks read one session that the host attaches once, at
 * bootstrap: attachSession(session) is the module-level seam, last
 * bind wins (the same discipline as the bindRequestFn seam in
 * @speed/api-sdk). Before any session is attached -- and after a
 * logout -- every hook fails closed: useAuthState reports the
 * anonymous snapshot, useCurrentTenant returns null and usePermission
 * returns false. No hook ever throws because no session is attached.
 *
 * The snapshot is the single subscribable store: attachSession
 * forwards the attached session's notifications to every subscribed
 * component, and the selectors below recompute on every notification
 * (any bump re-renders every hook consumer -- a permission-set change
 * is a snapshot change like any other).
 *
 * Permission checks are pure set lookup, never evaluation: the host
 * attaches the /me-derived lists through session.setPermissionSet,
 * the session's survival rules decide which lists a switch or a
 * refresh keeps, and usePermission answers "is this string in that
 * domain's list" only -- a domain whose set is absent is false, and a
 * 'tenant' check never consults the 'system' list or vice versa.
 * These checks are a UX affordance, not a security boundary: the
 * server authorizes.
 */

import { useSyncExternalStore } from 'react'
import type {
  AuthDomain,
  AuthSession,
  AuthSnapshot,
} from './session.js'

/** The snapshot hooks report before any session is attached. A module
 * constant: referentially stable, so useSyncExternalStore never sees
 * spurious changes while nothing is attached. */
const NO_SESSION_SNAPSHOT: AuthSnapshot = {
  state: 'anonymous',
  principal: null,
  permissionSets: { tenant: null, system: null },
}

/** The attached session; null before the first attachSession call. */
let attachedSession: AuthSession | null = null
/** Unsubscribes the notification bridge on the attached session. */
let unattachBridge: (() => void) | null = null
/** The subscribed components (one listener per useSyncExternalStore
 * subscription). */
const listeners = new Set<() => void>()

function notifyListeners(): void {
  for (const listener of listeners) {
    listener()
  }
}

/** The getSnapshot every hook shares: the attached session's current
 * snapshot, or the stable anonymous one. Referentially stable between
 * changes on both paths. */
function getSnapshot(): AuthSnapshot {
  return attachedSession === null
    ? NO_SESSION_SNAPSHOT
    : attachedSession.getSnapshot()
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

/**
 * Attaches the session the hooks read. Last bind wins: attaching a new
 * session unsubscribes the bridge on the previous one (its later
 * transitions no longer reach the components) and wakes every
 * subscribed component so it re-reads -- a swap may change the
 * snapshot identity. Attaching the session already attached is a
 * no-op. The hooks fail closed before the first call.
 */
export function attachSession(session: AuthSession): void {
  if (session === attachedSession) {
    return
  }
  if (unattachBridge !== null) {
    unattachBridge()
    unattachBridge = null
  }
  attachedSession = session
  // The bridge forwards every session notification to the components;
  // each listener's re-read then picks up the session's new snapshot.
  unattachBridge = session.subscribe(() => {
    notifyListeners()
  })
  // The newly attached session may already hold a different snapshot
  // than the components last saw (or than the no-session one): wake
  // them so the first render after attach is not stale.
  notifyListeners()
}

/** The full auth snapshot. Re-renders on every snapshot change --
 * state, principal or permission sets. Before attachSession, returns
 * the stable anonymous snapshot. */
export function useAuthState(): AuthSnapshot {
  return useSyncExternalStore(subscribe, getSnapshot)
}

/** The tenant the principal is currently signed into, or null while
 * anonymous and when the principal carries no tenant_id (a
 * system-domain principal, for example). */
export function useCurrentTenant(): { tenantId: string } | null {
  const snapshot = useAuthState()
  const tenantId = snapshot.principal?.tenant_id
  if (typeof tenantId !== 'string' || tenantId === '') {
    return null
  }
  return { tenantId }
}

/** Whether the principal holds the permission in the domain's
 * host-attached set. Pure set lookup that fails closed: false while
 * anonymous, when the domain's set is absent (null), and when the
 * permission is not in it. The 'tenant' set never satisfies a
 * 'system' check and vice versa. A UX affordance only -- the server
 * authorizes. */
export function usePermission(
  domain: AuthDomain,
  permission: string,
): boolean {
  const snapshot = useAuthState()
  if (snapshot.state !== 'authenticated') {
    return false
  }
  const set = snapshot.permissionSets[domain]
  return set !== null && set.includes(permission)
}
