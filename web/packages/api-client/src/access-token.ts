/**
 * The access-token seam.
 *
 * The access token lives in memory only (docs/internal/12-frontend.md):
 * a token in localStorage is a credential an XSS walks away with. The
 * refresh token is the session's business, not the store's: the authn
 * API returns it in the token-issuing response bodies and sets no
 * refresh cookie, so a session layer (@speed/auth-core) holds it in
 * its closure and drives the session-refresh operation. The store
 * carries the access token alone, and refreshing is the host's
 * business through `refreshAccessToken` -- an absent hook leaves 401s
 * to surface directly.
 *
 * The store is synchronous on purpose: it is a memory cell, not an
 * API, and the request path reads it on every send with no async
 * indirection. Nothing in this package touches localStorage or any
 * other persistence -- there is no storage seam to bind, by design,
 * so a host cannot accidentally configure token persistence.
 */

/** Where the current bearer token lives between requests. */
export interface AccessTokenStore {
  /** The current token, or null when signed out / not yet signed in. */
  get(): string | null
  /** Store a fresh token; pass null to sign out (clears the memory cell). */
  set(token: string | null): void
}

/**
 * The in-memory AccessTokenStore. Process-lifetime only: reloading the
 * page signs the user out until the host's session layer re-establishes
 * a session (@speed/auth-core has no restore -- a reload means signing
 * in again). Tests and hosts both inject this (or their own store)
 * through ClientOptions; the client never creates one.
 */
export function createMemoryAccessTokenStore(): AccessTokenStore {
  let token: string | null = null
  return {
    get(): string | null {
      return token
    },
    set(next: string | null): void {
      token = next
    },
  }
}
