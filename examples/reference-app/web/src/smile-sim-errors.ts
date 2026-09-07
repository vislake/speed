/**
 * smile-sim-errors.ts -- the smile-simulation surface's reachable-error
 * whitelist: one map from every code the surface's four routes (simulate,
 * job-status, per-photo enumeration and simulation content) can be
 * answered with to the app-namespace key carrying its current-language
 * text, plus the small classifiers the surface shares with the cases
 * whitelist.
 *
 * The code set is the server's own: the smilesim fragment's documented
 * answers (internal/smilesim's option-validation codes, the simulate
 * route's request-shape refusals and the recipient gate, the surface's
 * handler-level envelopes, the poll and content routes' not-found
 * answers) plus the two gateway-level gates a simulate call passes
 * through (go/ai-gateway's entitlement gate and go/billing's credit
 * reservation) and go/jobs' own not-found answer. The chain-level codes
 * (authn.token_invalid, tenancy.tenant_unresolved) are deliberately
 * absent, exactly as on every other surface of this host: a surface only
 * renders after authentication, and a token that dies mid-session is the
 * session layer's business, never a surface error. An answer that is not
 * on the list (a future server code, a client.http.<status> transport
 * answer) resolves to the unknown fallback, so the surface never shows a
 * raw key or another language's text -- the identical discipline the
 * cases and notes whitelists follow. One code has one text wherever it
 * surfaces.
 *
 * Exported so this surface's whitelist can be deep-imported by the
 * codes-alignment suite like the other surfaces' whitelists are.
 */
import { apiErrorCodeOf } from './cases-errors.js'

export const SMILE_SIM_ERROR_TEXT_KEYS: Readonly<Record<string, string>> = {
  // The option-validation refusals (internal/smilesim/options.go).
  'smilesim.unsupported_smile_style': 'cases.sim.errors.unsupportedSmileStyle',
  'smilesim.unsupported_tooth_shade': 'cases.sim.errors.unsupportedToothShade',
  'smilesim.strength_out_of_range': 'cases.sim.errors.strengthOutOfRange',
  // The simulate route's request-shape refusals (cmd/server/smilesim.go).
  'smilesim.invalid_request_body': 'cases.sim.errors.invalidRequest',
  'smilesim.photo_object_id_required': 'cases.sim.errors.photoObjectIdRequired',
  'smilesim.recipient_not_in_tenant': 'cases.sim.errors.recipientNotInTenant',
  // The two gateway-level gates a simulate call passes through before
  // any job is enqueued (go/ai-gateway's entitlement gate, go/billing's
  // credit reservation).
  'aigateway.entitlement_denied': 'cases.sim.errors.entitlementDenied',
  'billing.insufficient_credits': 'cases.sim.errors.insufficientCredits',
  // The poll route's not-found answer (go/jobs' own sentinel, passed
  // through by the job-status handler).
  'jobs.job_not_found': 'cases.sim.errors.jobNotFound',
  // The simulation-content route's refusals (cmd/server/smilesim.go).
  'smilesim.simulation_not_found': 'cases.sim.errors.simulationNotFound',
  'smilesim.output_not_ready': 'cases.sim.errors.outputNotReady',
  'smilesim.output_not_found': 'cases.sim.errors.outputNotFound',
  // The handler-level envelope every operation can write.
  'smilesim.internal_error': 'cases.sim.errors.internalError',
  // The transport's own codes (the client.* trio the request function
  // can emit); a client.http.<status> answer is not whitelisted and
  // degrades to the unknown fallback, exactly as on the cases surface.
  'client.network': 'cases.sim.errors.client',
  'client.timeout': 'cases.sim.errors.client',
  'client.protocol': 'cases.sim.errors.client',
}

/** The text key of a code the surface was answered with, falling back
 * to the unknown key for codes outside the whitelist. */
export function smileSimErrorTextKey(code: string): string {
  return SMILE_SIM_ERROR_TEXT_KEYS[code] ?? 'cases.sim.errors.unknown'
}

/**
 * The submit path's failure classifier: an ApiError-shaped failure
 * keeps its code, anything else -- a bug-shaped throw, an un-normalized
 * answer -- collapses to a code that is deliberately not whitelisted,
 * so the resolver renders the unknown fallback. A submit that throws at
 * all always has a code to show. The classifier itself is the cases
 * surface's generic one, imported rather than re-implemented.
 */
export function smileSimErrorCodeOf(error: unknown): string {
  return apiErrorCodeOf(error) ?? 'client.unknown'
}
