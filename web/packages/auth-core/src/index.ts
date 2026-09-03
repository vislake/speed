/**
 * @speed/auth-core -- the browser session lifecycle as a memory-only
 * state machine over the generated authn surface of @speed/api-sdk.
 *
 * createAuthSession wires the generated operations (password and SMS
 * login, logout, tenant switch, step-up, refresh) to one observable
 * session: an AccessTokenStore holds the access token (the bridge
 * into @speed/api-client, which reads the same store on every send),
 * the refresh token lives only inside the session closure, and
 * subscribers observe the AuthSnapshot transitions. The failure
 * contract is ApiError-based: user operations reject raw (tell apart
 * with isApiError) and change nothing on failure; refresh() is the
 * silent path -- false for a refused token (the session signs out),
 * a raw rethrow for a transport/server failure.
 *
 * No storage is written anywhere: an httpOnly cookie is the M1
 * server-side home of the refresh token, and persistence of the
 * session across page loads is out of scope here (see README's Known
 * limitations).
 */

export { createAuthSession } from './session.js'
export type {
  AuthSession,
  AuthSessionListener,
  AuthSnapshot,
} from './session.js'
