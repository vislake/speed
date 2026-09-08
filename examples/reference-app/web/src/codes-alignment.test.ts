/**
 * codes-alignment.test.ts -- the reference-app shell's reachable-error
 * alignment suite: every server-emittable error code the shell's ten
 * surfaces can be answered with (the auth-ui sign-in/session family, the
 * account-ui signed-in family, the tenancy-ui switch family, the notes
 * create surface, the cases surface, the smile-simulation surface the
 * block-B round added, the two block-C share surfaces: the clinic's
 * share action on the case page and the patient's share page, the
 * credits surface the block-D round added, and the team surface the
 * add-a-colleague round added -- its invite send resolving go/org's
 * create-operation sentinels through the same whitelist shape) is
 * rendered through a reachable-error whitelist, and this suite pins the
 * whitelists to the server codes themselves.
 *
 * The server side of the comparison is GO_PINNED below: a hand-maintained
 * enumeration of the codes the Go side of this app can answer with on
 * these ten surfaces. "Can answer with" is the reachability criterion
 * the enumeration is audited against, judged from the surfaces' own
 * flows rather than from the Go side's full answerable vocabulary: a
 * code earns an entry when some request one of the ten surfaces' flows
 * genuinely makes is answered with it on the composed stack -- the
 * operations the surface invokes, the request shapes its code sends
 * through the typed generated client, and this app's own middleware
 * chain all decide what is reachable. A code no such request can draw
 * stays out, and the boundary rulings are recorded where they fall:
 * request shapes a flow cannot produce (the notes create form submits
 * typed client output whose text is capped well inside the notes
 * handler's body bound, so the malformed-body answer
 * notes.invalid_request_body has no requesting flow on that surface);
 * parameters a surface never sends (the credits view never sends a
 * limit, so billing.invalid_limit has no answering request -- see its
 * entry below); operations and requests no listed surface produces
 * (the consult route is invoked by none of the ten surfaces, and
 * reference_app.method_not_allowed answers a request whose method a
 * hand-mounted route does not serve, which the typed operations of
 * these surfaces never send); and answers a preceding layer preempts
 * on the only request that could reach them
 * (authn.authentication_required loses to this app's tenancy gate on
 * the anonymous switch route -- verified on the composed stack, the
 * reasoning recorded in its exemption entry below). Each entry
 * carries the source citation of the sentinel that defines it:
 * go/authn/errors.go for the authn codes, every other entry's own
 * citation naming its file and sentinel. The shell side is derived,
 * never copied: the auth-ui / account-ui / tenancy-ui whitelists are
 * deep-imported from the packages' own error-text modules (never from
 * package entry points -- each package's whitelist is its own, and
 * this app-level suite reads them where they live), and the remaining
 * seven surfaces' whitelists come from the app's own modules:
 * notes-view for the notes create surface, cases-errors for the cases
 * one, smile-sim-errors for the smile-simulation one, share-errors
 * for both block-C share surfaces, team-view for the team one and
 * credits-view for the credits one. Ten per-surface whitelists drawn
 * from nine source modules; the imports below are the complete set.
 * The two directions are asserted separately so a drift names its
 * side:
 *
 *   - every GO_PINNED code is whitelisted by at least one surface -- a
 *     server code with no surface text would render a raw key (a code
 *     added server-side and forgotten here fails with the code in hand);
 *   - every non-client code a surface whitelists has a GO_PINNED
 *     citation or a recorded WHITELISTED_BEYOND_THIS_APP exemption --
 *     a whitelist entry for a code no server answers here is dead copy
 *     and fails with its surface named. The exemption list exists
 *     because a package whitelist is consumer-facing: it serves every
 *     deployment that can be answered with the code, which can be a
 *     wider composition than THIS app wires. Exempting such a code from
 *     the no-dead-entries direction -- instead of adding a citation
 *     this app's routes cannot earn -- keeps the two hand lists honest:
 *     GO_PINNED stays the set of codes the app itself can answer with,
 *     and the exemption records the reasoning that keeps a code out of
 *     it.
 *
 * The client.network / client.timeout / client.protocol codes are the
 * @speed/api-client transport contract, not server answers, so they sit
 * outside the comparison: the suite asserts each surface whitelists
 * exactly that trio (and never a client.http.<status> family, which the
 * api-client contract leaves dynamic and every surface resolves to its
 * unknown fallback by design).
 *
 * The GO_PINNED enumeration is deliberately hand-maintained, and the
 * machine-extracted server-side code census the deferral below once
 * deferred is now HALF-real: tools/gen_error_code_index.py extracts
 * every code constructed in Go source with a literal code argument
 * into docs/error-codes.md and its machine twin docs/error-codes.json
 * (one row per code, file/line/identifier carried, regenerated by the
 * same drift-gated generator), and the machine-verification test below
 * reads that JSON back: every GO_PINNED code must exist in the census
 * as a construction at exactly the file its citation names, bound to
 * the cited identifier. What stays hand-maintained, and why, is the
 * REACHABLE half of the census the old deferral framed ("every
 * answerable code of every route this app mounts"): reachability is a
 * flow property judged from the surfaces' own requests through the
 * composed stack -- no line-based extractor can derive which codes a
 * form's requests can draw, so membership in GO_PINNED remains the
 * audited, citation-carrying judgment this header describes, and the
 * suite's two directions keep it honest -- a code the server gains and
 * the whitelists do not cover, or a whitelist entry with no citation,
 * both fail here. A code whose CITATION is wrong (the sentinel's code
 * string changed, the citation names the wrong file) now fails the
 * machine leg, not just review.
 */

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { ERROR_TEXT_CODES as AUTH_UI_ERROR_TEXT_CODES } from '../../../../web/packages/auth-ui/src/internal/error-text.js'
import { ERROR_TEXT_CODES as ACCOUNT_UI_ERROR_TEXT_CODES } from '../../../../web/packages/account-ui/src/internal/error-text.js'
import { ERROR_TEXT_CODES as TENANCY_UI_ERROR_TEXT_CODES } from '../../../../web/packages/tenancy-ui/src/internal/error-text.js'
import { CASES_ERROR_TEXT_KEYS } from './cases-errors.js'
import { SMILE_SIM_ERROR_TEXT_KEYS } from './smile-sim-errors.js'
import {
  SHARE_ACTION_ERROR_TEXT_KEYS,
  SHARE_VIEW_ERROR_TEXT_KEYS,
} from './share-errors.js'
import { NOTE_ERROR_TEXT_KEYS } from './views/notes-view.js'
import { TEAM_ERROR_TEXT_KEYS } from './views/team-view.js'
import { CREDITS_ERROR_TEXT_KEYS } from './views/credits-view.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

/** Reads one dotted key out of a bundle, or '' when the key is missing
 * or holds a non-string -- the shared text lookup the bilingual
 * regression tests below run against both app bundles. */
function textOf(bundle: Record<string, unknown>, key: string): string {
  const value = key
    .split('.')
    .reduce<unknown>(
      (node, segment) =>
        typeof node === 'object' && node !== null
          ? (node as Record<string, unknown>)[segment]
          : undefined,
      bundle,
    )
  return typeof value === 'string' ? value : ''
}

/**
 * The server-emittable codes of this app's surfaces, each cited to
 * the source of the sentinel that defines it. The authn sentinels all
 * live in go/authn/errors.go; rbac.permission_denied is go/rbac's
 * ErrPermissionDenied, and the notes codes are the notes module
 * handler's own sentinels.
 *
 * Each citation names the file, the sentinel identifier that defines
 * the code, and the parenthesised line where the sentinel was declared
 * when the annotation was last written. The identifier is the
 * load-bearing half; the line is a human-audit aid only, NOT an
 * assertion. Both halves of that history matter: a citation carrying
 * only a line number cannot be checked without re-opening the Go file,
 * one carrying the identifier can -- which is how the
 * reference-app-web.md P2-1 drift (a sentinel inserted above the authn
 * block shifted 25 of 30 line citations and stayed silent until an
 * audit re-measured them) became visible at all. But once the suite
 * began asserting the annotated line mechanically, every unrelated Go
 * edit that inserted or removed a line above a sentinel reddened it:
 * the retellings that once annotated each block below recorded
 * twenty-odd re-pin episodes, the latest a docs round that never
 * touched behaviour -- 89f3077, docs(jobs) -- moving ErrJobNotFound
 * from go/jobs/job.go:186 to :201 (the jobs-3 round that landed in the
 * same window took the blame; 0eb0644, the round's other real mover,
 * shifted billing's ErrInsufficientCredits and ErrInternal the same
 * three lines). Each re-pin round discovered the further drifted
 * citations only one at a time, since the line check reported the
 * first mismatch in traversal order and stopped.
 *
 * The deep check below therefore asserts the property worth keeping --
 * the cited FILE declares the identifier exactly once. That still
 * fails when a sentinel is deleted or renamed (the identifier vanishes
 * from the file) or when a citation points at the wrong file (the
 * identifier is not declared there), with the code, the file and the
 * identifier named; it no longer fails when a sentinel merely moves
 * within its file. Where the annotation drifts from the re-measured
 * declaration site, the check prints the found line instead -- visible
 * information in the run output for whoever next reads the
 * annotation, never a red. All 73 annotations were re-measured
 * accurate by the re-pin round this change replaces (a6b2be8); none
 * of them is asserted after it.
 */
const GO_PINNED: Readonly<Record<string, string>> = {
  // go/authn/errors.go -- the authn module's error sentinels, in current
  // file order (the enumeration carries only the codes these surfaces
  // can be answered with; authn.token_invalid's reachability reasoning
  // sits with its entry below, and authn.authentication_required --
  // once cited here -- moved to WHITELISTED_BEYOND_THIS_APP, see its
  // entry).
  'authn.invalid_credentials': 'go/authn/errors.go:37 (ErrInvalidCredentials)',
  'authn.identifier_required': 'go/authn/errors.go:41 (ErrIdentifierRequired)',
  'authn.invalid_email': 'go/authn/errors.go:45 (ErrInvalidEmail)',
  'authn.invalid_phone': 'go/authn/errors.go:49 (ErrInvalidPhone)',
  'authn.email_already_registered': 'go/authn/errors.go:62 (ErrEmailAlreadyRegistered)',
  'authn.phone_already_registered': 'go/authn/errors.go:66 (ErrPhoneAlreadyRegistered)',
  'authn.password_too_short': 'go/authn/errors.go:69 (ErrPasswordTooShort)',
  'authn.password_too_long': 'go/authn/errors.go:72 (ErrPasswordTooLong)',
  'authn.display_name_too_long': 'go/authn/errors.go:82 (ErrDisplayNameTooLong)',
  'authn.password_too_weak': 'go/authn/errors.go:86 (ErrPasswordTooWeak)',
  // authn.token_invalid -- the composed authn.Middleware's answer for a
  // request that presented an access token the verifier refused (a
  // tampered or otherwise invalid token; an expired one is answered
  // authn.token_expired below). The middleware is optional
  // authentication (go/authn/middleware.go): a request with no
  // credential passes through anonymous -- which is why it never writes
  // authn.authentication_required -- but a token the verifier refuses
  // is 401'd immediately, on every protected route of this app, the
  // tenant-switch one included. The no-credential code
  // (authn.authentication_required) is written only by per-operation
  // and per-route guards -- RequireAuthenticated (go/authn/middleware.go)
  // and the handler's requirePrincipal (go/authn/handler.go) -- which
  // cannot answer the switch route in this app: the route is not in the
  // pre-auth allowlist, so tenancy.Middleware's fail-closed gate refuses
  // the anonymous request first with tenancy.tenant_unresolved, verified
  // on the composed stack; that code therefore lives in
  // WHITELISTED_BEYOND_THIS_APP below, not here.
  'authn.token_invalid': 'go/authn/errors.go:98 (ErrTokenInvalid)',
  'authn.token_expired': 'go/authn/errors.go:105 (ErrTokenExpired)',
  'authn.session_revoked': 'go/authn/errors.go:109 (ErrSessionRevoked)',
  'authn.refresh_token_invalid': 'go/authn/errors.go:113 (ErrRefreshTokenInvalid)',
  'authn.refresh_token_reused': 'go/authn/errors.go:119 (ErrRefreshTokenReused)',
  'authn.tenant_membership_required': 'go/authn/errors.go:125 (ErrTenantMembershipRequired)',
  'authn.tenant_membership_unavailable': 'go/authn/errors.go:131 (ErrTenantMembershipUnavailable)',
  'authn.oauth_state_invalid': 'go/authn/errors.go:157 (ErrOAuthStateInvalid)',
  'authn.redirect_uri_not_allowed': 'go/authn/errors.go:161 (ErrRedirectURINotAllowed)',
  'authn.provider_unknown': 'go/authn/errors.go:166 (ErrProviderUnknown)',
  'authn.social_exchange_failed': 'go/authn/errors.go:173 (ErrSocialExchangeFailed)',
  'authn.identity_requires_binding': 'go/authn/errors.go:206 (ErrIdentityRequiresBinding)',
  'authn.identity_already_bound': 'go/authn/errors.go:210 (ErrIdentityAlreadyBound)',
  'authn.identity_not_found': 'go/authn/errors.go:216 (ErrIdentityNotFound)',
  'authn.last_login_method': 'go/authn/errors.go:222 (ErrLastLoginMethod)',
  'authn.rate_limited': 'go/authn/errors.go:285 (ErrRateLimited)',
  'authn.account_locked': 'go/authn/errors.go:293 (ErrAccountLocked)',
  'authn.channel_disabled': 'go/authn/errors.go:303 (ErrChannelDisabled)',
  'authn.verification_code_invalid': 'go/authn/errors.go:330 (ErrVerificationCodeInvalid)',
  'authn.mfa_not_enrolled': 'go/authn/errors.go:347 (ErrMFANotEnrolled)',
  'authn.mfa_already_enrolled': 'go/authn/errors.go:351 (ErrMFAAlreadyEnrolled)',
  // authn.mfa_code_used -- the honest-split round's new sentinel: the
  // spent-code answer (a code that passed the real check but whose
  // single-use guard already consumed it) that authn now distinguishes
  // from the never-valid authn.mfa_invalid_code.
  'authn.mfa_invalid_code': 'go/authn/errors.go:369 (ErrMFAInvalidCode)',
  'authn.mfa_code_used': 'go/authn/errors.go:384 (ErrMFACodeUsed)',
  'authn.step_up_required': 'go/authn/errors.go:388 (ErrStepUpRequired)',
  'authn.session_not_found': 'go/authn/errors.go:397 (ErrSessionNotFound)',
  // go/rbac/errors.go -- the permission-denied sentinel the notes route's
  // rbac gate answers with.
  'rbac.permission_denied': 'go/rbac/errors.go:56 (ErrPermissionDenied)',
  // examples/reference-app/internal/notes/handler.go -- the notes module
  // handler's own sentinels.
  'notes.text_required': 'examples/reference-app/internal/notes/handler.go:31 (ErrTextRequired)',
  // The two declarations below sit after the maxRequestBodyBytes constant
  // block the request-body-cap round added above ErrTextTooLong.
  'notes.text_too_long': 'examples/reference-app/internal/notes/handler.go:80 (ErrTextTooLong)',
  'notes.internal_error': 'examples/reference-app/internal/notes/handler.go:84 (errInternal)',
  // examples/reference-app/internal/cases/service.go -- the cases
  // domain layer's own sentinels (the block-A round's clinic-wide list
  // left the code set unchanged: these are the create/read refusals
  // the cases fragment documents, now reachable text on the cases
  // surface's routes).
  'cases.patient_name_required': 'examples/reference-app/internal/cases/service.go:64 (ErrPatientNameRequired)',
  'cases.patient_name_too_long': 'examples/reference-app/internal/cases/service.go:69 (ErrPatientNameTooLong)',
  'cases.photo_object_id_required': 'examples/reference-app/internal/cases/service.go:81 (ErrPhotoObjectIDRequired)',
  'cases.photo_object_id_too_long': 'examples/reference-app/internal/cases/service.go:86 (ErrPhotoObjectIDTooLong)',
  'cases.duplicate_photo_object': 'examples/reference-app/internal/cases/service.go:91 (ErrDuplicatePhotoObject)',
  'cases.too_many_photos': 'examples/reference-app/internal/cases/service.go:95 (ErrTooManyPhotos)',
  'cases.photo_already_attached': 'examples/reference-app/internal/cases/service.go:103 (ErrPhotoAlreadyAttached)',
  'cases.not_found': 'examples/reference-app/internal/cases/service.go:109 (ErrNotFound)',
  'cases.subject_unresolved': 'examples/reference-app/internal/cases/service.go:122 (ErrSubjectUnresolved)',
  // examples/reference-app/cmd/server/cases_photos.go -- the photo
  // upload/content route sentinels the block-A round added (surface
  // orchestration codes; the photo_content_too_large answers both the
  // upload route and the content route).
  'cases.photo_content_required': 'examples/reference-app/cmd/server/cases_photos.go:73 (ErrPhotoContentRequired)',
  'cases.photo_content_invalid': 'examples/reference-app/cmd/server/cases_photos.go:78 (ErrPhotoContentInvalid)',
  'cases.photo_content_too_large': 'examples/reference-app/cmd/server/cases_photos.go:85 (ErrPhotoContentTooLarge)',
  'cases.photo_rejected': 'examples/reference-app/cmd/server/cases_photos.go:93 (ErrPhotoRejected)',
  'cases.photo_not_found': 'examples/reference-app/cmd/server/cases_photos.go:100 (ErrPhotoNotFound)',
  // examples/reference-app/cmd/server/cases.go -- the case surface's
  // handler-level envelopes (the internal fallback writeCasesError
  // folds every non-apperr error into, and the shared malformed-body
  // sentinel every body-reading cases route writes -- both kept as
  // named declarations so the audits that cite them have a stable
  // site).
  'cases.internal_error': 'examples/reference-app/cmd/server/cases.go:58 (casesErrInternal)',
  'cases.invalid_request_body': 'examples/reference-app/cmd/server/cases.go:66 (casesInvalidRequestBody)',
  // examples/reference-app/internal/smilesim/options.go -- the option
  // validation sentinels (named declarations the block-B round's web
  // surface made reachable text for the first time).
  'smilesim.unsupported_smile_style': 'examples/reference-app/internal/smilesim/options.go:154 (ErrUnsupportedSmileStyle)',
  'smilesim.unsupported_tooth_shade': 'examples/reference-app/internal/smilesim/options.go:158 (ErrUnsupportedToothShade)',
  'smilesim.strength_out_of_range': 'examples/reference-app/internal/smilesim/options.go:162 (ErrStrengthOutOfRange)',
  // examples/reference-app/cmd/server/smilesim.go -- the smile-simulation
  // surface's handler-level sentinels (the internal envelope, the
  // simulate route's request-shape refusals, the recipient gate, the
  // poll/content routes' not-found answers and the simulation-content
  // route's refusals the block-B round added).
  'smilesim.internal_error': 'examples/reference-app/cmd/server/smilesim.go:55 (smileSimErrInternal)',
  'smilesim.invalid_request_body': 'examples/reference-app/cmd/server/smilesim.go:66 (smilesimErrInvalidRequestBody)',
  'smilesim.photo_object_id_required': 'examples/reference-app/cmd/server/smilesim.go:70 (smilesimErrPhotoObjectIDRequired)',
  'smilesim.simulation_not_found': 'examples/reference-app/cmd/server/smilesim.go:82 (smileSimErrSimulationNotFound)',
  'smilesim.output_not_ready': 'examples/reference-app/cmd/server/smilesim.go:86 (smileSimErrOutputNotReady)',
  'smilesim.output_not_found': 'examples/reference-app/cmd/server/smilesim.go:91 (smileSimErrOutputNotFound)',
  'smilesim.recipient_not_in_tenant': 'examples/reference-app/cmd/server/smilesim.go:446 (smilesimErrRecipientNotInTenant)',
  // go/jobs/job.go -- the not-found sentinel the job-status handler
  // passes through for an unknown or another tenant's job id.
  'jobs.job_not_found': 'go/jobs/job.go:201 (ErrJobNotFound)',
  // go/billing/errors.go -- the credit-reservation refusal a simulate
  // answers when the tenant's balance cannot cover one generation.
  'billing.insufficient_credits': 'go/billing/errors.go:73 (ErrInsufficientCredits)',
  // go/billing/errors.go -- the handler-level envelope the credits
  // surface's two GETs fold an unclassifiable failure into (the block-D
  // round's own surface addition; billing.invalid_limit and
  // billing.invalid_request stay out of the enumeration because the
  // credits view never sends a limit -- the server's default window is
  // the read it needs -- so neither 400 is reachable on this surface).
  'billing.internal_error': 'go/billing/errors.go:174 (ErrInternal)',
  // go/ai-gateway/errors.go -- the entitlement-gate refusal a simulate
  // answers for a tenant whose subscription lacks the image model.
  'aigateway.entitlement_denied': 'go/ai-gateway/errors.go:42 (ErrEntitlementDenied)',
  // go/sharing/errors.go and go/sharing/ratelimit.go -- the sharing
  // module's sentinels the block-C round's two surfaces made reachable
  // text (the clinic share action's POST /api/v1/sharing/shares can be
  // answered with the create rate limit and the internal envelope; the
  // patient page's GET /api/v1/sharing/access can be answered with the
  // outward-identical not-accessible refusal, the per-IP/per-token rate
  // limit, the granted-but-unopenable 502 and the internal envelope.
  // The route-level rbac answer the share action can draw is the
  // already-pinned rbac.permission_denied above).
  'sharing.internal_error': 'go/sharing/errors.go:92 (ErrInternal)',
  'sharing.not_accessible': 'go/sharing/errors.go:60 (ErrNotAccessible)',
  'sharing.resource_unavailable': 'go/sharing/errors.go:111 (ErrResourceUnavailable)',
  'sharing.rate_limited': 'go/sharing/ratelimit.go:79 (ErrRateLimited)',
  // go/org/errors.go -- the org-module sentinels the team surface's
  // invite send and its reads can be answered with (the add-a-colleague
  // round's own addition). org.invalid_email is the create's address
  // refusal (an address the caller typed), org.node_not_found the
  // create's answer for a binding target that went stale under the
  // form, org.invitation_rate_limited and org.invitations_disabled the
  // two invitation gates, org.internal_error the envelope every
  // unclassifiable org failure folds into -- and the route-level rbac
  // answer the surface's reads and its send can draw is the
  // already-pinned rbac.permission_denied above (go/rbac/errors.go:56).
  'org.invalid_email': 'go/org/errors.go:222 (ErrInvalidEmail)',
  'org.invitation_rate_limited': 'go/org/errors.go:215 (ErrInvitationRateLimited)',
  'org.invitations_disabled': 'go/org/errors.go:226 (ErrInvitationsDisabled)',
  'org.node_not_found': 'go/org/errors.go:33 (ErrNodeNotFound)',
  'org.internal_error': 'go/org/errors.go:121 (ErrInternal)',
}

/**
 * Whitelisted codes this app's own surfaces cannot be answered with --
 * the deliberate, reasoning-carrying exceptions to the no-dead-entries
 * direction (see the file header). Each entry names the code and why it
 * has no in-app answer, so it must not gain a GO_PINNED citation either:
 * the two hand lists would otherwise start confirming each other for a
 * code no surface here can reach.
 */
const WHITELISTED_BEYOND_THIS_APP: Readonly<Record<string, string>> = {
  'authn.social_identity_incomplete':
    'auth-ui whitelists the social exchange answers for its SocialCallbackHandler ' +
    'surface, which any host with a wired, enabled social channel can be answered ' +
    'with (the provider authorized the person but reported no stable identifier). ' +
    'This app wires no social channel: cfg.SocialProviders is empty, so ' +
    'openConfiguredAuthnChannels opens no flag rows and every social feature flag ' +
    'stays OFF -- an exchange attempt here refuses at the channel gate (go/authn/' +
    'identity.go, SocialCallback\'s gate, which runs before any identity analysis) ' +
    'with authn.channel_disabled. The code therefore has no in-app answer.',
  // go/authn/errors.go:93 (ErrAuthenticationRequired) -- the sentinel
  // citation moved here with the code when composed-stack verification
  // overturned its in-app reachability (see the token_invalid citation
  // above for who writes each and why the switch route cannot draw the
  // no-credential answer).
  'authn.authentication_required':
    'tenancy-ui whitelists the token-verification answers of authn\'s per-operation ' +
    'and per-route guards, and any host whose guard mounts without a preceding ' +
    'tenant gate can be answered with this code: RequireAuthenticated (go/authn/' +
    'middleware.go) and the handler\'s requirePrincipal (go/authn/handler.go) both ' +
    'write it for a request that presented no credential. This app cannot be ' +
    'answered with it: authn.Middleware is optional authentication (a request with ' +
    'no credential passes through anonymous, never writing this code), and the ' +
    'switch route is not in the pre-auth allowlist, so tenancy.Middleware\'s ' +
    'fail-closed gate refuses the anonymous switch request first with 403 ' +
    'tenancy.tenant_unresolved -- verified on the composed stack (anonymous POST ' +
    '/api/v1/authn/tenant/switch answers tenancy.tenant_unresolved, not this ' +
    'code). The code therefore has no in-app answer.',
}

/** The transport codes of the @speed/api-client contract: reserved to
 * the client layer, never a server answer, and the one reserved family
 * every surface whitelists exactly (client.http.<status> stays dynamic
 * and resolves to each surface's unknown fallback). */
const CLIENT_RESERVED_CODES: readonly string[] = [
  'client.network',
  'client.timeout',
  'client.protocol',
]

function nonClientCodes(codes: readonly string[]): string[] {
  return codes.filter((code) => !code.startsWith('client.'))
}

/** The ten whitelists by surface, for failure messages that name the
 * list a drift was found in. */
const SURFACE_WHITELISTS: Readonly<Record<string, readonly string[]>> = {
  'auth-ui sign-in/session': AUTH_UI_ERROR_TEXT_CODES,
  'account-ui signed-in family': ACCOUNT_UI_ERROR_TEXT_CODES,
  'tenancy-ui tenant switch': TENANCY_UI_ERROR_TEXT_CODES,
  'notes create surface': Object.keys(NOTE_ERROR_TEXT_KEYS),
  'cases surface': Object.keys(CASES_ERROR_TEXT_KEYS),
  'smile-simulation surface': Object.keys(SMILE_SIM_ERROR_TEXT_KEYS),
  'share action (case detail)': Object.keys(SHARE_ACTION_ERROR_TEXT_KEYS),
  'patient share page': Object.keys(SHARE_VIEW_ERROR_TEXT_KEYS),
  'credits surface': Object.keys(CREDITS_ERROR_TEXT_KEYS),
  'team invite surface': Object.keys(TEAM_ERROR_TEXT_KEYS),
}

/** A citation's path, annotated line and sentinel identifier. The
 * annotated line is the human-audit aid carried in the citation; it is
 * never asserted -- the deep check re-measures the declaration site and
 * prints it when the two differ. */
interface SentinelCitation {
  readonly path: string
  readonly annotatedLine: number
  readonly identifier: string
}

/** Parses a citation of the shape 'path:line (Identifier)'. */
function parseCitation(code: string, citation: string): SentinelCitation {
  const match = /^([^:]+):(\d+) \(([^)]+)\)$/.exec(citation)
  const path = match?.[1]
  const lineText = match?.[2]
  const identifier = match?.[3]
  if (path === undefined || lineText === undefined || identifier === undefined) {
    throw new Error(`malformed citation for ${code}: ${citation}`)
  }
  return { path, annotatedLine: Number(lineText), identifier }
}

/** The repository root as a filesystem path, derived from this test
 * file's own URL (the app's src sits four levels under the root:
 * examples/reference-app/web/src). The path is assembled as plain
 * string arithmetic rather than `new URL(..., import.meta.url)`,
 * because vite's transform rewrites that pattern as an asset
 * reference; the filesystem resolves the '..' segments at open time. */
function repoRootPath(): string {
  const url = import.meta.url
  const withoutScheme = url.startsWith('file://') ? url.slice(7) : url
  const srcDir = withoutScheme.slice(0, withoutScheme.lastIndexOf('/'))
  return `${srcDir}/../../../../`
}

describe('reachable-error whitelists vs the server code set', () => {
  it('declares every GO_PINNED sentinel exactly once in the file its citation names', () => {
    // The machine half of the identifier-citation discipline (see the
    // GO_PINNED doc comment): the cited FILE must declare the cited
    // sentinel exactly once, so a sentinel deleted or renamed (the
    // identifier vanishes from the file) and a citation pointed at the
    // wrong file (the identifier is not declared there) both fail this
    // suite with the code, the file and the identifier named. The line
    // number is deliberately not asserted -- an unrelated Go edit that
    // merely moves a sentinel inside its file must not redden the
    // suite (the re-pin episodes the GO_PINNED doc comment recounts
    // all began that way); the re-measured declaration line is printed
    // instead whenever it differs from the citation's annotation, so a
    // drift is visible information in the run output, never a red.
    // The files are read relative to this test file (the app lives at
    // examples/reference-app/web, four levels under the repository
    // root, and the citations are repository-root-relative paths).
    for (const [code, citation] of Object.entries(GO_PINNED)) {
      const { path, annotatedLine, identifier } = parseCitation(code, citation)
      const source = readFileSync(`${repoRootPath()}${path}`, 'utf8')
      const declaration = new RegExp(`^\\s*(?:var\\s+)?${identifier}\\s*=`)
      const declaredLines = source
        .split('\n')
        .flatMap((line, index) => (declaration.test(line) ? [index + 1] : []))
      const message =
        declaredLines.length === 0
          ? `${code} is cited to ${path} (${identifier}), but that file does not declare the sentinel (deleted or renamed?)`
          : `${code} is cited to ${path} (${identifier}), but that file declares the sentinel ${declaredLines.length} times (lines ${declaredLines.join(', ')}), not exactly once`
      expect(declaredLines.length, message).toBe(1)
      const declaredLine = declaredLines[0]
      if (declaredLine !== annotatedLine) {
        // The drift goes to the raw stdout stream rather than
        // console.log because vitest swallows a passing test's console
        // output and echoes it only attached to a failing test -- and a
        // passing suite is the common case here, the whole point of the
        // file-level assertion being that a moved sentinel no longer
        // reddens it. The raw line stays in the run log either way.
        process.stdout.write(
          `GO_PINNED drift: ${code} is declared at ${path}:${declaredLine} (${identifier}); its citation annotation says ${path}:${annotatedLine}\n`,
        )
      }
    }
  })

  it('machine-verifies every GO_PINNED code against the extracted error-code census', () => {
    // The census half of the machine bridge (see the GO_PINNED doc
    // comment above): tools/gen_error_code_index.py extracts every code
    // constructed in Go source into docs/error-codes.json, one row per
    // code with its construction file, line and bound identifier --
    // regenerated by the same drift-gated generator the docs-check
    // pipeline runs, so a stale JSON reddens there before this suite is
    // even reached. The identifier-level test above proves the cited
    // sentinel is declared in the cited file; THIS test proves the
    // cited (code, file) pair is a real construction of that code: a
    // sentinel whose code string changed (ErrFoo now builds a different
    // code) or a citation pointed at the wrong file would leave the
    // identifier check green (the identifier is still declared there)
    // while the census loses the pinned code's row at that file -- the
    // drift the machine leg exists to catch. The cited identifier must
    // also match the census row's bound identifier when the row binds
    // one (inline constructions bind none; code+file agreement is then
    // the whole check).
    const index = JSON.parse(
      readFileSync(`${repoRootPath()}docs/error-codes.json`, 'utf8'),
    ) as {
      rows: ReadonlyArray<{
        readonly code: string
        readonly file: string
        readonly ident: string
      }>
    }
    const rowsByCodeAndFile = new Map<string, (typeof index.rows)[number]>()
    for (const row of index.rows) {
      rowsByCodeAndFile.set(`${row.code}\u0000${row.file}`, row)
    }
    for (const [code, citation] of Object.entries(GO_PINNED)) {
      const { path, identifier } = parseCitation(code, citation)
      const row = rowsByCodeAndFile.get(`${code}\u0000${path}`)
      expect(
        row,
        `${code} is cited to ${path} (${identifier}), but the extracted census (docs/error-codes.json, generated by tools/gen_error_code_index.py) carries no construction of code ${code} at ${path} -- the sentinel's code string changed, or the citation names the wrong file`,
      ).toBeDefined()
      if (row !== undefined && row.ident !== '') {
        expect(
          row.ident,
          `${code} is cited to ${path} (${identifier}), but the census binds ${path} to the identifier ${row.ident} -- the sentinel was renamed?`,
        ).toBe(identifier)
      }
    }
  })

  it('keeps the hand-maintained enumeration at its audited size', () => {
    // 35 authn sentinels (the 34 of the previous audit -- the 33 of
    // the audit before plus authn.token_invalid, the middleware-era
    // addition that stayed -- plus this round's authn.mfa_code_used,
    // the honest-split spent-code answer; the other middleware-era
    // addition, authn.authentication_required, has already left
    // GO_PINNED again: composed-stack verification overturned its
    // in-app reachability, and its citation now lives in
    // WHITELISTED_BEYOND_THIS_APP with the reasoning) +
    // rbac.permission_denied + the three notes sentinels + the
    // sixteen cases sentinels the block-A round's cases surface added
    // (nine domain codes in internal/cases/service.go, five photo
    // route codes in cmd/server/cases_photos.go, and the two
    // handler-level envelopes in cmd/server/cases.go) + the thirteen
    // smile-simulation sentinels the block-B round's web surface added
    // (seven app-side handler sentinels in cmd/server/smilesim.go, the
    // three option-validation sentinels in internal/smilesim/options.go
    // that round named, and the three gateway/queue sentinels a
    // simulate call can surface from go/jobs, go/billing and
    // go/ai-gateway) + the four sharing sentinels the block-C round's
    // two surfaces added (three in go/sharing/errors.go and the
    // creation/access rate-limit sentinel in go/sharing/ratelimit.go) +
    // billing.internal_error, the one handler-level envelope the
    // block-D round's credits surface can be answered with + the five
    // org-module sentinels in go/org/errors.go the add-a-colleague
    // round's team surface added (its invite send can draw the address
    // refusal, the node gone stale and the two invitation gates, and
    // its reads and send fold an unclassifiable failure into the
    // internal envelope; org's other codes, which no operation this
    // surface calls can answer with, stay out of the enumeration
    // exactly as billing.invalid_limit does). The
    // size guard makes a GO_PINNED edit (in either direction) fail
    // loudly here rather than silently through the subset assertions
    // below.
    expect(Object.keys(GO_PINNED)).toHaveLength(78)
  })

  it('renders a bilingual text for every reachable smile-simulation code', () => {
    // Regression (c) of the block-B gate: a code this surface can be
    // answered with must resolve to HUMAN text in both languages --
    // never a missing key that would render a raw code. The whitelist
    // itself is pinned to the server sentinels by the directions above;
    // this test pins the other half, that every whitelisted code's text
    // key actually exists in both app bundles, resolves to a non-empty
    // string, and is never the unknown fallback (an entry that maps to
    // the fallback is a code with no text of its own).
    const unknownEn = textOf(enUS, 'cases.sim.errors.unknown')
    const unknownZh = textOf(zhCN, 'cases.sim.errors.unknown')
    expect(unknownEn).not.toBe('')
    expect(unknownZh).not.toBe('')
    for (const [code, textKey] of Object.entries(SMILE_SIM_ERROR_TEXT_KEYS)) {
      if (code.startsWith('client.')) {
        continue
      }
      const en = textOf(enUS, textKey)
      const zh = textOf(zhCN, textKey)
      expect(en, `${code} maps to ${textKey}, which is missing or empty in en-US`).not.toBe('')
      expect(zh, `${code} maps to ${textKey}, which is missing or empty in zh-CN`).not.toBe('')
      expect(en, `${code} maps to ${textKey}, which resolves to the unknown fallback`).not.toBe(unknownEn)
      expect(zh, `${code} maps to ${textKey}, which resolves to the unknown fallback`).not.toBe(unknownZh)
    }
  })

  it('renders a bilingual text for every reachable share-action code', () => {
    // Regression (c) of the block-C gate, the clinic half: a code the
    // case page's share action can be answered with must resolve to
    // HUMAN text in both languages -- the rbac gate's denial, the
    // module's creation rate limit and the internal envelope included
    // -- never a raw key and never another language's text.
    const unknownEn = textOf(enUS, 'cases.share.errors.unknown')
    const unknownZh = textOf(zhCN, 'cases.share.errors.unknown')
    expect(unknownEn).not.toBe('')
    expect(unknownZh).not.toBe('')
    for (const [code, textKey] of Object.entries(SHARE_ACTION_ERROR_TEXT_KEYS)) {
      if (code.startsWith('client.')) {
        continue
      }
      const en = textOf(enUS, textKey)
      const zh = textOf(zhCN, textKey)
      expect(en, `${code} maps to ${textKey}, which is missing or empty in en-US`).not.toBe('')
      expect(zh, `${code} maps to ${textKey}, which is missing or empty in zh-CN`).not.toBe('')
      expect(en, `${code} maps to ${textKey}, which resolves to the unknown fallback`).not.toBe(unknownEn)
      expect(zh, `${code} maps to ${textKey}, which resolves to the unknown fallback`).not.toBe(unknownZh)
    }
  })

  it('renders a bilingual text for every reachable patient-page code', () => {
    // Regression (c) of the block-C gate, the patient half: a code the
    // share page can be answered with must resolve to HUMAN text in
    // both languages -- the outward-identical not-accessible refusal
    // (expired and revoked alike, per the module's rule 5), the rate
    // limit, the granted-but-unopenable 502 and the internal envelope
    // -- never a raw key and never another language's text.
    const unknownEn = textOf(enUS, 'shareView.errors.unknown')
    const unknownZh = textOf(zhCN, 'shareView.errors.unknown')
    expect(unknownEn).not.toBe('')
    expect(unknownZh).not.toBe('')
    for (const [code, textKey] of Object.entries(SHARE_VIEW_ERROR_TEXT_KEYS)) {
      if (code.startsWith('client.')) {
        continue
      }
      const en = textOf(enUS, textKey)
      const zh = textOf(zhCN, textKey)
      expect(en, `${code} maps to ${textKey}, which is missing or empty in en-US`).not.toBe('')
      expect(zh, `${code} maps to ${textKey}, which is missing or empty in zh-CN`).not.toBe('')
      expect(en, `${code} maps to ${textKey}, which resolves to the unknown fallback`).not.toBe(unknownEn)
      expect(zh, `${code} maps to ${textKey}, which resolves to the unknown fallback`).not.toBe(unknownZh)
    }
  })

  it('renders a bilingual text for every reachable team-surface code', () => {
    // The add-a-colleague round's own leg of the same regression: a
    // code the team surface's invite send can be answered with must
    // resolve to HUMAN text in both languages -- the rbac gate's
    // denial, go/org's create-operation refusals and the internal
    // envelope included -- never a raw key and never another
    // language's text.
    const unknownEn = textOf(enUS, 'team.invite.errors.unknown')
    const unknownZh = textOf(zhCN, 'team.invite.errors.unknown')
    expect(unknownEn).not.toBe('')
    expect(unknownZh).not.toBe('')
    for (const [code, textKey] of Object.entries(TEAM_ERROR_TEXT_KEYS)) {
      if (code.startsWith('client.')) {
        continue
      }
      const en = textOf(enUS, textKey)
      const zh = textOf(zhCN, textKey)
      expect(en, `${code} maps to ${textKey}, which is missing or empty in en-US`).not.toBe('')
      expect(zh, `${code} maps to ${textKey}, which is missing or empty in zh-CN`).not.toBe('')
      expect(en, `${code} maps to ${textKey}, which resolves to the unknown fallback`).not.toBe(unknownEn)
      expect(zh, `${code} maps to ${textKey}, which resolves to the unknown fallback`).not.toBe(unknownZh)
    }
  })

  it('whitelists every code the server can answer with (GO_PINNED is covered)', () => {
    const covered = new Set(
      Object.values(SURFACE_WHITELISTS).flatMap(nonClientCodes),
    )
    for (const code of Object.keys(GO_PINNED)) {
      expect(
        covered.has(code),
        `${code} (cited ${GO_PINNED[code]}) is whitelisted by no surface`,
      ).toBe(true)
    }
  })

  it('whitelists no code the server never emits on these surfaces (no dead entries)', () => {
    for (const [surface, codes] of Object.entries(SURFACE_WHITELISTS)) {
      for (const code of nonClientCodes(codes)) {
        expect(
          GO_PINNED[code] !== undefined ||
            WHITELISTED_BEYOND_THIS_APP[code] !== undefined,
          `${surface} whitelists ${code}, which no server sentinel cited here defines and no recorded exemption explains`,
        ).toBe(true)
      }
    }
  })

  it('keeps the beyond-this-app exemption list honest in both directions', () => {
    const whitelisted = new Set(
      Object.values(SURFACE_WHITELISTS).flatMap(nonClientCodes),
    )
    for (const [code, reasoning] of Object.entries(WHITELISTED_BEYOND_THIS_APP)) {
      // An exemption for a code no surface whitelists would be dead
      // copy of its own kind: the exempted code must actually be
      // reachable text somewhere on these surfaces.
      expect(
        whitelisted.has(code),
        `${code} is exempted but whitelisted by no surface: ${reasoning}`,
      ).toBe(true)
      // An exemption for a code the enumeration already carries is
      // redundant: once the app answers a code, it belongs in GO_PINNED
      // with its citation, not in the exemption list.
      expect(
        GO_PINNED[code],
        `${code} is exempted but already carries a GO_PINNED citation: ${reasoning}`,
      ).toBeUndefined()
    }
  })

  it('treats client.* exactly as the transport contract: the reserved trio, nothing more', () => {
    for (const [surface, codes] of Object.entries(SURFACE_WHITELISTS)) {
      const clientCodes = codes.filter((code) => code.startsWith('client.'))
      expect(
        [...clientCodes].sort(),
        `${surface} whitelists the reserved trio and no other client.* family`,
      ).toEqual([...CLIENT_RESERVED_CODES].sort())
    }
  })

  it('keeps the tenancy-ui-only codes exactly the two token-verification answers', () => {
    // Every tenancy-ui code except the two token-verification answers
    // sits inside the auth-ui family whose shared texts it copies
    // verbatim. The two exceptions are real: authn.authentication_required
    // and authn.token_invalid are the answers of authn's per-operation
    // and per-route guards -- a pre-auth sign-in surface cannot be
    // answered with either, which is why auth-ui whitelists neither and
    // tenancy-ui authors their texts instead of copying. authn.token_invalid
    // carries a GO_PINNED citation above (the composed authn.Middleware
    // 401s a presented token that fails verification on the protected
    // switch route); authn.authentication_required's reachability is
    // host-dependent, so its coverage is the recorded
    // WHITELISTED_BEYOND_THIS_APP exemption rather than an in-app
    // citation -- the no-dead-entries direction holds for both either
    // way.
    const authUi = new Set(nonClientCodes(AUTH_UI_ERROR_TEXT_CODES))
    const tenancyOnly = nonClientCodes(TENANCY_UI_ERROR_TEXT_CODES).filter(
      (code) => !authUi.has(code),
    )
    expect([...tenancyOnly].sort()).toEqual([
      'authn.authentication_required',
      'authn.token_invalid',
    ])
    for (const code of tenancyOnly) {
      expect(
        GO_PINNED[code] !== undefined ||
          WHITELISTED_BEYOND_THIS_APP[code] !== undefined,
        `${code} is reachable on tenancy-ui but carries neither a GO_PINNED citation nor a recorded exemption`,
      ).toBe(true)
    }
  })
})
