/**
 * share-errors.ts -- the share surfaces' reachable-error
 * whitelists: one map per surface from every code that surface can be
 * answered with to the app-namespace key carrying its current-language
 * text, plus the classifiers the two surfaces share with the rest of
 * the host.
 *
 * The two share surfaces sit on opposite sides of go/sharing's HTTP
 * surface, and each can be answered with a different slice of the
 * module's code set:
 *
 *   - SHARE_ACTION_ERROR_TEXT_KEYS covers the clinic's share action on
 *     the case page (POST /api/v1/sharing/shares, the owner-facing
 *     route): the rbac gate every protected route of this host sits
 *     behind (a colleague whose role holds no sharing:create answers
 *     rbac.permission_denied), the module's per-tenant creation rate
 *     limit, and the internal envelope. The module's input-validation
 *     codes (sharing.resource_ref_required and friends) are
 *     deliberately absent: this surface always posts a non-empty
 *     resource_ref with no forever/maxViews/password fields, so none
 *     of those answers is reachable from it -- the no-dead-entries
 *     direction of the codes-alignment suite keeps the list honest.
 *
 *   - SHARE_VIEW_ERROR_TEXT_KEYS covers the patient's page (GET
 *     /api/v1/sharing/access, the public route): sharing.not_accessible
 *     is the module's ONE outward answer for every refusal of a
 *     recognized token -- revoked, expired, view-exhausted or
 *     password-refused are indistinguishable by design, the module's
 *     outward answers being identical for every refusal reason -- so
 *     one honest text serves the expired link and the revoked one
 *     identically; sharing.resource_unavailable answers a granted
 *     share whose bytes could not be opened (the simulation object
 *     deleted or reclaimed); sharing.rate_limited answers a visitor
 *     over the route's per-IP/per-token budget; sharing.internal_error
 *     covers a store failure. sharing.invalid_request (the
 *     parameter-binder envelope) is deliberately absent: the page only
 *     ever requests with the well-formed token its own URL carried.
 *
 * Both surfaces whitelist the transport's client.* trio; an answer
 * outside the list (a future server code, a client.http.<status>
 * transport answer) resolves to its surface's unknown fallback, so
 * neither surface ever shows a raw key or another language's text --
 * the identical discipline every other whitelist of this host follows.
 * Exported so both whitelists can be deep-imported by the
 * codes-alignment suite.
 */
import { apiErrorCodeOf } from './cases-errors.js'

/** The clinic share action's whitelist (POST /api/v1/sharing/shares). */
export const SHARE_ACTION_ERROR_TEXT_KEYS: Readonly<Record<string, string>> = {
  // The rbac gate this host's router puts in front of the owner-facing
  // route: a practice member whose role holds no sharing:create.
  'rbac.permission_denied': 'cases.share.errors.permissionDenied',
  // The module's per-tenant creation budget (go/sharing/ratelimit.go's
  // checkCreateRateLimit, sharing.rate_limited on denial).
  'sharing.rate_limited': 'cases.share.errors.rateLimited',
  // The handler-level internal envelope every operation can write.
  'sharing.internal_error': 'cases.share.errors.internal',
  // The transport's own codes (the client.* trio the request function
  // can emit); a client.http.<status> answer is not whitelisted and
  // degrades to the unknown fallback, exactly as on the cases surface.
  'client.network': 'cases.share.errors.client',
  'client.timeout': 'cases.share.errors.client',
  'client.protocol': 'cases.share.errors.client',
}

/** The patient page's whitelist (GET /api/v1/sharing/access). */
export const SHARE_VIEW_ERROR_TEXT_KEYS: Readonly<Record<string, string>> = {
  // The module's one outward refusal for a recognized token that is no
  // longer accessible -- expired, revoked, view-exhausted or
  // password-refused are indistinguishable by design, the outward
  // answer being identical for every refusal reason, so one honest text
  // covers the expired link and the revoked one alike.
  'sharing.not_accessible': 'shareView.errors.notAccessible',
  // The access route's per-IP/per-token rate limit.
  'sharing.rate_limited': 'shareView.errors.rateLimited',
  // A granted share whose bytes could not be opened (the simulation
  // object deleted or reclaimed by storage's expiry sweep).
  'sharing.resource_unavailable': 'shareView.errors.resourceUnavailable',
  // The handler-level internal envelope.
  'sharing.internal_error': 'shareView.errors.internal',
  // The transport's own codes; client.protocol is the probe's answer
  // for a share that IS live (the access route answered real bytes,
  // which the request function cannot parse as JSON) -- the patient
  // page's retry signal, never rendered text.
  'client.network': 'shareView.errors.client',
  'client.timeout': 'shareView.errors.client',
  'client.protocol': 'shareView.errors.client',
}

/** The text key of a code the share action was answered with, falling
 * back to the unknown key for codes outside the whitelist. */
export function shareActionErrorTextKey(code: string): string {
  return SHARE_ACTION_ERROR_TEXT_KEYS[code] ?? 'cases.share.errors.unknown'
}

/** The text key of a code the patient page was answered with, falling
 * back to the unknown key for codes outside the whitelist. */
export function shareViewErrorTextKey(code: string): string {
  return SHARE_VIEW_ERROR_TEXT_KEYS[code] ?? 'shareView.errors.unknown'
}

/**
 * The surfaces' shared failure classifier (the cases surface's generic
 * one, imported rather than re-implemented): an ApiError-shaped
 * failure keeps its code, anything else -- a bug-shaped throw, an
 * un-normalized answer -- collapses to a code that is deliberately not
 * whitelisted, so the resolver renders the unknown fallback.
 */
export function shareErrorCodeOf(error: unknown): string {
  return apiErrorCodeOf(error) ?? 'client.unknown'
}
