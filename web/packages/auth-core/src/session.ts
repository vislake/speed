/**
 * The browser session state machine: createAuthSession wires the
 * generated authn surface of @speed/api-sdk (login, logout, tenant
 * switch, step-up, refresh -- real server round-trips through the
 * host-bound client) into one memory-only session.
 *
 * What lives where:
 * - the access token lives in the caller-supplied AccessTokenStore
 *   (the same store the host's @speed/api-client reads on every send,
 *   so a login here is immediately visible to every request);
 * - the refresh token lives in this closure only. It never enters the
 *   store and nothing here writes storage: the authn API returns the
 *   refresh token in the token-issuing response bodies (no refresh
 *   cookie exists -- the only HttpOnly cookie authn ever sets is the
 *   social-binding pre-auth one), and the refresh ENDPOINT takes the
 *   held token in the request body, which is why the session keeps
 *   its own copy.
 *
 * The failure contract: every user operation (login, logout, tenant
 * switch, step-up) rejects with the raw ApiError from the request
 * (distinguishable with isApiError), and a failed operation leaves
 * both the store and the snapshot exactly as they were. refresh()
 * never rejects for an invalid or expired refresh token -- that is the
 * silent path the client's refreshAccessToken hook calls; it returns
 * false and signs the session out locally (the server has already
 * terminated the token family).
 *
 * The generation guard: a user operation captures the generation it
 * started under and bumps the counter only when it commits -- a
 * successful login, switch or step-up bumps after applying its
 * tokens, a successful logout bumps after clearing. (Bumping on entry
 * would let a failed operation strand an in-flight refresh: the
 * refresh's writes would be dropped as stale even though nothing
 * changed.) refresh() never bumps: it captures the generation it
 * started under, and every one of its writes -- store, held refresh
 * token, snapshot -- is dropped when that generation no longer holds.
 * That makes a completed logout win over a refresh that resolves
 * after it (a refresh cannot resurrect an ended session) and a
 * committed user operation win over a refresh that resolves after it;
 * a token-issuing response that lost the race is discarded, never
 * applied. refresh() is also single-flight per generation: concurrent
 * callers -- the api-client silent-401 hook and an application timer
 * -- share one in-flight request, because the authn server treats
 * parallel refreshes as token theft and rotates the whole family.
 *
 * Host-attached permission sets: the snapshot additionally carries
 * setPermissionSet(domain, list) data -- lists the host attaches (the
 * /me-derived permissions of the current tenant and, for platform
 * staff, the tenant-independent system set). The session never
 * evaluates a list; it only applies the survival rules when a
 * principal change commits: same user and tenant (a silent refresh or
 * a step-up) keeps both lists, a tenant switch drops the tenant-domain
 * list and keeps the system-domain one, and a different user or an
 * anonymous transition clears both domains. A failed operation
 * changes nothing, lists included.
 */

import {
  ApiError,
  ERROR_CODE_PROTOCOL,
  isApiError,
} from '@speed/api-client'
import type { AccessTokenStore } from '@speed/api-client'
import {
  authnLoginWithPassword,
  authnLoginWithSMSCode,
  authnLogout,
  authnRefreshToken,
  authnSwitchTenant,
  authnVerifyStepUp,
} from '@speed/api-sdk'
import type {
  AuthnLoginWithPasswordRequest,
  AuthnLoginWithSMSCodeRequest,
  AuthnPrincipal,
  AuthnTokenPair,
} from '@speed/api-sdk'

/** The permission domain a host-attached permission set belongs to.
 * 'tenant' names the permissions the principal holds inside its
 * current tenant; 'system' names platform-staff permissions that are
 * tenant-independent. */
export type AuthDomain = 'tenant' | 'system'

/** The host-attached permission lists per domain: pure data the host
 * attaches through setPermissionSet. A null list means nothing is
 * known for that domain (checks against it fail closed). The session
 * never evaluates a list -- set membership is decided by the hooks
 * and the shells, never here. */
export interface AuthPermissionSets {
  tenant: readonly string[] | null
  system: readonly string[] | null
}

/** The observable session state. Immutable by convention: hosts read
 * it, they never mutate it. */
export interface AuthSnapshot {
  state: 'anonymous' | 'authenticated'
  /** The authenticated principal, or null while anonymous. */
  principal: AuthnPrincipal | null
  /** The host-attached permission lists per domain (see
   * setPermissionSet). Always present; a null list means no set is
   * known for that domain. Empty in both domains while anonymous. */
  permissionSets: AuthPermissionSets
}

/** Receives the new snapshot on every state change. */
export type AuthSessionListener = (snapshot: AuthSnapshot) => void

/** The session handle createAuthSession returns. */
export interface AuthSession {
  /** The current snapshot; read it in render paths and in
   * useSyncExternalStore's getSnapshot. */
  getSnapshot(): AuthSnapshot
  /** Subscribe to state changes; returns the unsubscribe function. */
  subscribe(listener: AuthSessionListener): () => void
  /** Signs in with an email-or-phone identifier and a password. */
  loginWithPassword(
    request: AuthnLoginWithPasswordRequest,
  ): Promise<AuthSnapshot>
  /** Signs in with a phone number and the SMS code sent to it. */
  loginWithSMSCode(request: AuthnLoginWithSMSCodeRequest): Promise<AuthSnapshot>
  /** Ends the session: the server revokes it, the store empties and
   * the snapshot returns to anonymous. */
  logout(): Promise<void>
  /** Switches the session to another tenant the principal belongs to;
   * the server mints a new access token and the held refresh token
   * keeps rotating into it. */
  switchTenant(tenantId: string): Promise<AuthSnapshot>
  /** Re-proves a second factor for the current session; the elevation
   * lives in the access token just minted. */
  verifyStepUp(code: string): Promise<AuthSnapshot>
  /** Host-attached permission list for one domain: replaces the list
   * the snapshot carries with a defensive copy (mutating the caller's
   * array later changes nothing here); passing null clears the domain.
   * Notifies subscribers like any other snapshot change. What the
   * lists mean -- and which host attaches them -- is the host's
   * business; the session only carries them and applies the survival
   * rules in the file header when a principal change commits. */
  setPermissionSet(domain: AuthDomain, perms: readonly string[] | null): void
  /** Silently refreshes the access token. Resolves true when a fresh
   * pair was stored, false when there is nothing to refresh or the
   * refresh token was refused (the session is over). Never throws for
   * a refused token; a transport/server failure rethrows the raw
   * ApiError and leaves the held tokens in place. Concurrent calls
   * under one generation share a single in-flight request. */
  refresh(): Promise<boolean>
}

/** A validated token-issuing response. */
interface IssuedTokens {
  accessToken: string
  /** The minted refresh token; null when the response did not mint
   * one (a tenant switch or step-up reuses the caller's). */
  refreshToken: string | null
  principal: AuthnPrincipal
}

/** Fail-closed response validation: a token-issuing endpoint that
 * answers a 2xx without the tokens or the principal it must carry is
 * a protocol violation, never a successful login. */
function protocolViolation(detail: string): ApiError {
  return new ApiError({
    status: 200,
    code: ERROR_CODE_PROTOCOL,
    attempts: 1,
    cause: new Error(detail),
  })
}

function parseIssued(
  body: AuthnTokenPair,
  expectRefreshToken: boolean,
): IssuedTokens {
  const accessToken = body.access_token
  if (typeof accessToken !== 'string' || accessToken === '') {
    throw protocolViolation(
      'token-issuing response carries no access_token string',
    )
  }
  const principal = body.principal
  if (
    typeof principal !== 'object' ||
    principal === null ||
    typeof principal.user_id !== 'string' ||
    principal.user_id === ''
  ) {
    throw protocolViolation(
      'token-issuing response carries no principal with a user_id',
    )
  }
  const refreshToken = body.refresh_token
  if (expectRefreshToken) {
    if (typeof refreshToken !== 'string' || refreshToken === '') {
      throw protocolViolation(
        'token-issuing response carries no refresh_token string',
      )
    }
    return { accessToken, refreshToken, principal }
  }
  if (refreshToken === undefined) {
    // A switch or a step-up mints no new refresh token (the spec says
    // so): the caller's existing one keeps rotating.
    return { accessToken, refreshToken: null, principal }
  }
  if (typeof refreshToken !== 'string' || refreshToken === '') {
    throw protocolViolation(
      'token-issuing response carries a malformed refresh_token',
    )
  }
  return { accessToken, refreshToken, principal }
}

/** The permission-set shape that carries nothing: every anonymous
 * snapshot and every cross-user transition uses it. */
const NO_PERMISSION_SETS: AuthPermissionSets = {
  tenant: null,
  system: null,
}

/** What survives from the previous snapshot's permission sets into
 * the next authenticated one (see the file header). A silent refresh
 * and a step-up keep the same user and tenant, so the host-attached
 * lists stay -- an automatic refresh must not flash permissions away.
 * A tenant switch keeps the user but changes the tenant: the
 * tenant-domain list belonged to the old tenant and must not leak
 * into the new one, while the tenant-independent system-domain list
 * survives (platform-staff impersonation semantics). A different
 * user -- or no previous principal at all -- clears both domains: a
 * login never inherits another session's lists. */
function nextPermissionSets(
  previous: AuthSnapshot,
  principal: AuthnPrincipal,
): AuthPermissionSets {
  const prevPrincipal = previous.principal
  if (prevPrincipal === null || prevPrincipal.user_id !== principal.user_id) {
    return NO_PERMISSION_SETS
  }
  if (prevPrincipal.tenant_id !== principal.tenant_id) {
    return { tenant: null, system: previous.permissionSets.system }
  }
  return previous.permissionSets
}

/**
 * Creates the session over the caller's access-token store. The store
 * is the single bridge into @speed/api-client: a host that built its
 * client with this same store and refreshAccessToken: () =>
 * session.refresh() gets silent refresh for free -- an expired-token
 * 401 on any request runs one refresh, and the retried request carries
 * the fresh token.
 */
export function createAuthSession(store: AccessTokenStore): AuthSession {
  let snapshot: AuthSnapshot = {
    state: 'anonymous',
    principal: null,
    permissionSets: NO_PERMISSION_SETS,
  }
  // The held refresh token: closure-only, never stored, never exposed.
  let refreshToken: string | null = null
  // The generation guard (see the file header). User operations bump
  // it when they commit; refresh never does. Writes are applied only
  // when the captured generation still holds.
  let generation = 0
  // The single-flight refresh (see the file header): at most one
  // in-flight request per generation.
  let refreshFlight: { generation: number; promise: Promise<boolean> } | null =
    null
  const listeners = new Set<AuthSessionListener>()

  function notify(): void {
    for (const listener of listeners) {
      listener(snapshot)
    }
  }

  /** Clears everything local: the store, the held refresh token and
   * the snapshot. Used by logout and by refresh when the server
   * refuses the held token. */
  function clearLocal(): void {
    store.set(null)
    refreshToken = null
    snapshot = {
      state: 'anonymous',
      principal: null,
      permissionSets: NO_PERMISSION_SETS,
    }
  }

  /** Applies a validated token-issuing response. */
  function commitIssued(issued: IssuedTokens): void {
    store.set(issued.accessToken)
    if (issued.refreshToken !== null) {
      refreshToken = issued.refreshToken
    }
    snapshot = {
      state: 'authenticated',
      principal: issued.principal,
      permissionSets: nextPermissionSets(snapshot, issued.principal),
    }
  }

  /** The shared tail of every token-issuing user operation: the
   * response is validated first (a contract-violating 2xx rejects
   * even when a newer operation has taken over -- the caller deserves
   * to know its own request failed), then applied only if no other
   * user operation committed meanwhile. A committed operation bumps
   * the generation so an in-flight refresh that resolves later is
   * dropped instead of overwriting the fresher tokens. */
  function settleIssued(
    opGeneration: number,
    body: AuthnTokenPair,
    expectRefreshToken: boolean,
  ): AuthSnapshot {
    const issued = parseIssued(body, expectRefreshToken)
    if (generation !== opGeneration) {
      // Another user operation committed while this one was in
      // flight: it owns the session now, and this stale result is
      // dropped -- its freshly minted tokens are never applied.
      return snapshot
    }
    commitIssued(issued)
    generation += 1
    notify()
    return snapshot
  }

  /** The one actual refresh request (see refreshFlight). */
  async function runRefresh(
    held: string,
    capturedGeneration: number,
  ): Promise<boolean> {
    // The refresh request travels credential-less by declaration: the
    // generated authnRefreshToken operation carries omitAccessToken
    // (orval's speedRequestCredentialless mutator), so it goes out
    // without an Authorization header and the store is never read --
    // let alone cleared -- for it. That is load-bearing: the client's
    // silent-401 refresh engages only for a refused request that
    // presented a bearer token, so a refused refresh token surfaces
    // here instead of re-entering the refresh path and awaiting itself
    // (see the api-client README's bearer-only rule). Clearing the
    // store to make the request credential-less would momentarily
    // strip the token from every concurrent request, turning their
    // 401s into spurious auth failures under that same rule; the store
    // is never touched on the way out.
    let pair: AuthnTokenPair
    try {
      pair = await authnRefreshToken({ refresh_token: held })
    } catch (error) {
      if (generation !== capturedGeneration) {
        // A user operation committed while the refresh was in flight:
        // it owns the session now; its state stands untouched.
        return false
      }
      if (
        isApiError(error) &&
        (error.status === 401 || error.code === ERROR_CODE_PROTOCOL)
      ) {
        // The server refused the held token (401: invalid, expired
        // or replayed -- the token family is terminated) or consumed
        // it against a contract-violating 2xx: the session is over.
        clearLocal()
        notify()
        return false
      }
      // Transport failure or a server-side error: the held token may
      // still be valid. Nothing was cleared on the way out, so the
      // session stands exactly as it was; rethrow and let the caller
      // surface the error raw.
      throw error
    }
    if (generation !== capturedGeneration) {
      // A user operation committed while the refresh was in flight:
      // the freshly minted pair belongs to a session that no longer
      // exists here. The old held token was already consumed by the
      // server, so nothing is restored.
      return false
    }
    let issued: IssuedTokens
    try {
      issued = parseIssued(pair, true)
    } catch {
      // A 2xx consumed the held token but violated the contract: the
      // session cannot continue on an unverifiable pair.
      clearLocal()
      notify()
      return false
    }
    commitIssued(issued)
    notify()
    return true
  }

  return {
    getSnapshot(): AuthSnapshot {
      return snapshot
    },

    subscribe(listener: AuthSessionListener): () => void {
      listeners.add(listener)
      return () => {
        listeners.delete(listener)
      }
    },

    async loginWithPassword(
      request: AuthnLoginWithPasswordRequest,
    ): Promise<AuthSnapshot> {
      const opGeneration = generation
      const pair = await authnLoginWithPassword(request)
      return settleIssued(opGeneration, pair, true)
    },

    async loginWithSMSCode(
      request: AuthnLoginWithSMSCodeRequest,
    ): Promise<AuthSnapshot> {
      const opGeneration = generation
      const pair = await authnLoginWithSMSCode(request)
      return settleIssued(opGeneration, pair, true)
    },

    async logout(): Promise<void> {
      const opGeneration = generation
      await authnLogout()
      if (generation !== opGeneration) {
        // Another user operation committed while the logout request
        // was in flight: the local state belongs to that operation --
        // the server revoked the old session, but this logout must
        // not clear a session that superseded it.
        return
      }
      clearLocal()
      // Bump so that a refresh that started under this generation is
      // dropped when it resolves: a refresh must not resurrect the
      // session this logout just ended.
      generation += 1
      notify()
    },

    async switchTenant(tenantId: string): Promise<AuthSnapshot> {
      const opGeneration = generation
      const pair = await authnSwitchTenant({ tenant_id: tenantId })
      return settleIssued(opGeneration, pair, false)
    },

    async verifyStepUp(code: string): Promise<AuthSnapshot> {
      const opGeneration = generation
      const pair = await authnVerifyStepUp({ code })
      return settleIssued(opGeneration, pair, false)
    },

    setPermissionSet(
      domain: AuthDomain,
      perms: readonly string[] | null,
    ): void {
      // The stored list is a defensive copy, never an alias of the
      // caller's array: a host that reuses one array across calls (or
      // mutates it after attaching) cannot change the snapshot.
      const copy: readonly string[] | null =
        perms === null ? null : [...perms]
      const current = snapshot.permissionSets
      snapshot = {
        ...snapshot,
        permissionSets:
          domain === 'tenant'
            ? { tenant: copy, system: current.system }
            : { tenant: current.tenant, system: copy },
      }
      notify()
    },

    refresh(): Promise<boolean> {
      const held = refreshToken
      if (held === null) {
        // Nothing to refresh; not even a store read -- and certainly
        // no state change.
        return Promise.resolve(false)
      }
      const capturedGeneration = generation
      const inFlight = refreshFlight
      if (
        inFlight !== null &&
        inFlight.generation === capturedGeneration
      ) {
        // Same generation, same held token: share the in-flight
        // request. A second concurrent refresh would read as token
        // theft to the server and rotate the whole family out from
        // under the first.
        return inFlight.promise
      }
      const promise = runRefresh(held, capturedGeneration)
      refreshFlight = { generation: capturedGeneration, promise }
      // Clear the flight slot on both paths. (promise.finally(...)
      // would hand us a second promise that rejects unhandled when
      // the refresh does -- the caller's handler covers only the
      // promise it awaits -- so both branches are handled here.)
      const clearFlight = (): void => {
        if (refreshFlight?.promise === promise) {
          refreshFlight = null
        }
      }
      promise.then(clearFlight, clearFlight)
      return promise
    },
  }
}
