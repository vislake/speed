/**
 * The browser session state machine: createAuthSession wires the
 * generated authn surface of @speed/api-sdk (login, logout, tenant
 * switch, step-up, refresh, plus the pre-session operations the
 * sign-up and social flows need -- sms-code request, register, social
 * authorize and social callback; real server round-trips through the
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
 * both the store and the snapshot exactly as they were. The
 * pre-session operations -- requestSMSCode, register,
 * socialAuthorizeUrl -- run under the same contract and never change
 * state even on success: a registration is not a session, an SMS
 * request is an acceptance, and a built authorize URL has nothing to
 * commit. completeSocialLogin is a login operation with the full login
 * contract, plus one extra refusal: a sign-in flow answered with a
 * binding-shaped response (user and identity, no tokens -- the
 * server's answer to an already-authenticated caller binding a new
 * identity) is a client.protocol violation, rejected before any state
 * change. refresh()
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
 * started under, and its writes -- the store, the snapshot -- apply
 * only while that generation still holds. That makes a completed
 * logout win over a refresh that resolves after it (a refresh cannot
 * resurrect an ended session) and a committed user operation win over
 * a refresh that resolves after it: the losing pair's access token
 * and principal are never applied over the winner's. The one
 * exception is the held refresh token itself: when the operation that
 * won the race kept it -- a tenant switch or a step-up mints no new
 * refresh token -- the refresh's success has still consumed it
 * server-side, so the session adopts only the rotated token from the
 * losing pair; without that, the session's next refresh would present
 * a consumed token, read as a replay and die. (A losing pair that
 * violates the contract is dropped, never cleared: the winner's
 * session stands.) refresh() is also single-flight per held token:
 * concurrent callers -- the api-client silent-401 hook and an
 * application timer -- share one in-flight request, because the authn
 * server treats parallel refreshes presenting the same token as theft
 * and rotates the whole family. The flight is keyed on the token
 * rather than the generation so a refresh started after a switch or
 * step-up committed -- which kept the token -- shares the request
 * still in flight instead of firing a second presentation of it.
 *
 * Host-attached permission sets: the snapshot additionally carries
 * setPermissionSet(domain, list) data -- lists the host attaches (the
 * /me-derived permissions of the current tenant and, for platform
 * staff, the tenant-independent system set). The session never
 * evaluates a list; it only applies the survival rules when a
 * principal change commits: a silent refresh or a step-up (the same
 * session continuing) keeps both lists, a tenant switch drops the
 * tenant-domain list and keeps the system-domain one, and a login --
 * even by the same user in the same tenant, whose previous session
 * this login replaced -- or an anonymous transition clears both
 * domains: the (user_id, tenant_id) key cannot tell a silent refresh
 * from a brand-new login, and a login starts a server session whose
 * lists the host has not fetched yet, so nothing may ride over from
 * the previous session's. A failed operation changes nothing, lists
 * included.
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
  authnRegister,
  authnRequestSMSCode,
  authnSocialAuthorize,
  authnSocialCallback,
  authnSwitchTenant,
  authnVerifyStepUp,
} from '@speed/api-sdk'
import type {
  AuthnLoginWithPasswordRequest,
  AuthnLoginWithSMSCodeRequest,
  AuthnPrincipal,
  AuthnRegisterRequest,
  AuthnRequestSMSCodeRequest,
  AuthnSocialAuthorizeParams,
  AuthnSocialCallbackRequest,
  AuthnTokenPair,
  AuthnUser,
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
  /** Requests a one-time SMS sign-in code for a phone number -- the
   * 202 that precedes a loginWithSMSCode. The endpoint always answers
   * 202 whether or not the phone number belongs to an account, and
   * this method mirrors it: it resolves on acceptance and changes no
   * state -- nothing is committed, nothing is notified. Rejects the
   * raw ApiError on failure (authn.rate_limited, which carries
   * Retry-After). */
  requestSMSCode(request: AuthnRequestSMSCodeRequest): Promise<void>
  /** Registers a new account. Deliberately not a session operation:
   * the response is the created user, never a token pair, and no state
   * changes here -- the host follows up with a login when the new
   * account signs in. Rejects the raw ApiError on failure
   * (authn.email_already_registered, authn.phone_already_registered,
   * authn.rate_limited). */
  register(request: AuthnRegisterRequest): Promise<AuthnUser>
  /** Builds the authorization URL for one social sign-in channel. A
   * pure request: no state changes, and nothing here ever navigates --
   * the caller decides what the URL is for (the auth-ui layer reports
   * it upward, never jumping the browser itself). A 2xx that carries
   * no usable authorize_url rejects with a client.protocol ApiError,
   * the same fail-closed answer a contract-violating token response
   * gets. Rejects the raw ApiError on failure (authn.provider_unknown,
   * authn.redirect_uri_not_allowed). */
  socialAuthorizeUrl(
    provider: string,
    params: AuthnSocialAuthorizeParams,
  ): Promise<string>
  /** Completes a social sign-in flow: exchanges the provider's
   * authorization response -- the code and state it redirected back --
   * for a session, exactly like the other login operations: the token
   * pair is validated, both host-attached permission domains clear,
   * the store is written and subscribers are notified. The provider is
   * a path segment of the callback endpoint and is threaded through
   * verbatim. A sign-in flow answered with a binding-shaped response
   * (user and identity but no tokens -- the server's answer to an
   * already-authenticated caller binding a new identity) is refused
   * with a client.protocol ApiError before any state change: this
   * surface is a sign-in surface, its caller is anonymous by
   * construction, and there is nothing to bind to. Binding semantics
   * belong to a later round. */
  completeSocialLogin(
    provider: string,
    request: AuthnSocialCallbackRequest,
  ): Promise<AuthSnapshot>
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
   * rules in the file header when a principal change commits (a
   * login or a tenant switch clears what must not survive). Because
   * the session never correlates a list with the principal it was
   * fetched under, a host that attaches /me-derived lists should
   * refetch them after every login and tenant switch rather than
   * reuse lists captured under an earlier commit. */
  setPermissionSet(domain: AuthDomain, perms: readonly string[] | null): void
  /** Silently refreshes the access token. Resolves true when the
   * session holds a current token afterwards -- a fresh pair was
   * stored, or (when a tenant switch or step-up committed while the
   * refresh was in flight, keeping the held token) the rotated
   * refresh token was adopted onto the winner's session. Resolves
   * false when there is nothing to refresh or the refresh token was
   * refused (the session is over). Never throws for a refused token;
   * a transport/server failure rethrows the raw ApiError and leaves
   * the held tokens in place. Concurrent calls presenting the same
   * held token share a single in-flight request. */
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
 * survives (platform-staff impersonation semantics). A login clears
 * both domains unconditionally (freshLogin): its response starts a
 * brand-new server session, so even a same-user same-tenant login
 * must not inherit lists that were fetched under the session it
 * replaced -- the host re-attaches fresh /me-derived lists after the
 * commit. A different user -- or no previous principal at all --
 * clears both domains too: a login never inherits another session's
 * lists. */
function nextPermissionSets(
  previous: AuthSnapshot,
  principal: AuthnPrincipal,
  freshLogin: boolean,
): AuthPermissionSets {
  const prevPrincipal = previous.principal
  if (
    freshLogin ||
    prevPrincipal === null ||
    prevPrincipal.user_id !== principal.user_id
  ) {
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
  // in-flight request per held refresh token.
  let refreshFlight: { held: string; promise: Promise<boolean> } | null = null
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

  /** Applies a validated token-issuing response. freshLogin marks a
   * login operation, whose response starts a new server session:
   * both host-attached permission domains clear even when the
   * principal is unchanged. A silent refresh or a step-up -- the
   * same session continuing -- passes false and the lists keep. */
  function commitIssued(issued: IssuedTokens, freshLogin: boolean): void {
    store.set(issued.accessToken)
    if (issued.refreshToken !== null) {
      refreshToken = issued.refreshToken
    }
    snapshot = {
      state: 'authenticated',
      principal: issued.principal,
      permissionSets: nextPermissionSets(snapshot, issued.principal, freshLogin),
    }
  }

  /** The shared tail of every token-issuing user operation: the
   * response is validated first (a contract-violating 2xx rejects
   * even when a newer operation has taken over -- the caller deserves
   * to know its own request failed), then applied only if no other
   * user operation committed meanwhile. A committed operation bumps
   * the generation so an in-flight refresh that resolves later is
   * dropped instead of overwriting the fresher tokens. freshLogin is
   * true only for the login operations, whose commits clear both
   * host-attached permission domains (see nextPermissionSets). */
  function settleIssued(
    opGeneration: number,
    body: AuthnTokenPair,
    expectRefreshToken: boolean,
    freshLogin: boolean,
  ): AuthSnapshot {
    const issued = parseIssued(body, expectRefreshToken)
    if (generation !== opGeneration) {
      // Another user operation committed while this one was in
      // flight: it owns the session now, and this stale result is
      // dropped -- its freshly minted tokens are never applied.
      return snapshot
    }
    commitIssued(issued, freshLogin)
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
      // A user operation committed while the refresh was in flight and
      // owns the session now. If it kept the held token -- a tenant
      // switch or a step-up mints no new refresh token -- this
      // refresh's success still consumed the token server-side, so the
      // session must adopt the rotated one or its next refresh reads
      // as a replay and dies. Only the rotated token is adopted: the
      // winner's access token, principal and snapshot stand, and the
      // losing pair was minted against the pre-commit context. A
      // commit that replaced the token (a different login) or ended
      // the session (a logout) makes the pair stale: dropped outright.
      if (refreshToken === held) {
        try {
          const rotated = parseIssued(pair, true)
          refreshToken = rotated.refreshToken
          // Nothing observable changed: no store write, no snapshot
          // change and no notify -- the winner's state already stands
          // and subscribers saw it commit. Resolving true tells the
          // silent-401 caller that the store's current token is worth
          // a retry.
          return true
        } catch {
          // A contract-violating 2xx consumed the held token with
          // nothing adoptable: the session keeps the winner's tokens
          // but can no longer refresh. Degrade silently -- clearing
          // would kill the session a user operation just committed.
        }
      }
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
    commitIssued(issued, false)
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
      return settleIssued(opGeneration, pair, true, true)
    },

    async loginWithSMSCode(
      request: AuthnLoginWithSMSCodeRequest,
    ): Promise<AuthSnapshot> {
      const opGeneration = generation
      const pair = await authnLoginWithSMSCode(request)
      return settleIssued(opGeneration, pair, true, true)
    },

    async requestSMSCode(
      request: AuthnRequestSMSCodeRequest,
    ): Promise<void> {
      // 202 acceptance, never a state change: nothing to commit, no
      // store write, no snapshot change and no notify.
      await authnRequestSMSCode(request)
    },

    async register(request: AuthnRegisterRequest): Promise<AuthnUser> {
      // The created user, never a token pair: registering is not
      // signing in (see the interface docs), so the session stands
      // exactly as it was on either outcome.
      return authnRegister(request)
    },

    async socialAuthorizeUrl(
      provider: string,
      params: AuthnSocialAuthorizeParams,
    ): Promise<string> {
      const response = await authnSocialAuthorize(provider, params)
      const authorizeUrl = response.authorize_url
      if (typeof authorizeUrl !== 'string' || authorizeUrl === '') {
        // A 2xx whose whole purpose is the URL answers without one: a
        // protocol violation, not a successful authorize.
        throw protocolViolation(
          'social authorize 2xx carries no authorize_url string',
        )
      }
      return authorizeUrl
    },

    async completeSocialLogin(
      provider: string,
      request: AuthnSocialCallbackRequest,
    ): Promise<AuthSnapshot> {
      const opGeneration = generation
      const response = await authnSocialCallback(provider, request)
      const tokens = response.tokens
      if (typeof tokens !== 'object' || tokens === null) {
        // The callback answered a binding-shaped response -- identity
        // bound, no tokens minted -- which is the server's answer to
        // an already-authenticated caller. This surface is a sign-in
        // surface, so refuse before any state change rather than
        // "commit" a session that carries no credentials.
        throw protocolViolation(
          'social callback 2xx carries no tokens for a sign-in flow',
        )
      }
      return settleIssued(opGeneration, tokens, true, true)
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
      return settleIssued(opGeneration, pair, false, false)
    },

    async verifyStepUp(code: string): Promise<AuthSnapshot> {
      const opGeneration = generation
      const pair = await authnVerifyStepUp({ code })
      return settleIssued(opGeneration, pair, false, false)
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
      if (inFlight !== null && inFlight.held === held) {
        // Same held token: share the in-flight request. The flight is
        // keyed on the token, not the generation, because a tenant
        // switch or a step-up commits without replacing the held
        // token (their responses mint no new one): a refresh started
        // after such a commit would otherwise fire a second request
        // presenting the same token while the first is still in
        // flight -- which the authn server reads as token theft and
        // answers by rotating the whole family out from under the
        // session.
        return inFlight.promise
      }
      const promise = runRefresh(held, capturedGeneration)
      refreshFlight = { held, promise }
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
