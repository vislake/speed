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
 * The hooks (hooks.ts) read one host-attached session: attachSession
 * binds it once at bootstrap (last bind wins), useAuthState exposes
 * the snapshot, useCurrentTenant the principal's tenant and
 * usePermission a fail-closed set lookup over the host-attached
 * per-domain permission lists (a UX affordance, never a security
 * boundary). react is a peer dependency of the main entry: any host
 * of the hooks already renders React.
 *
 * No storage is written anywhere: the authn API returns the refresh
 * token in the response body and sets no refresh cookie, so the held
 * token survives only inside the session closure, and persistence of
 * the session across page loads is out of scope here (see README's
 * Known limitations).
 */

export { createAuthSession } from './session.js'
export {
  attachSession,
  useAuthState,
  useCurrentTenant,
  usePermission,
} from './hooks.js'
export type {
  AuthDomain,
  AuthPermissionSets,
  AuthSession,
  AuthSessionListener,
  AuthSnapshot,
} from './session.js'
