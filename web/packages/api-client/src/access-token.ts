/**
 * The access-token seam.
 *
 * The access token lives in memory only (docs/internal/12-frontend.md):
 * a token in localStorage is a credential an XSS walks away with. The
 * refresh token, by contrast, is an httpOnly+Secure+SameSite cookie that
 * JavaScript never sees -- so the store carries the access token alone,
 * and refreshing is the host's business through `refreshAccessToken`
 * (the M1 authn round implements it against the session-refresh
 * endpoint; until then hosts leave the hook out and 401s surface
 * directly).
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
 * page signs the user out until the M1 authn round restores the session
 * from the refresh-token cookie. Tests and hosts both inject this (or
 * their own store) through ClientOptions; the client never creates one.
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
