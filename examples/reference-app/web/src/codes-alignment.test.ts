/**
 * codes-alignment.test.ts -- the reference-app shell's reachable-error
 * alignment suite: every server-emittable error code the shell's six
 * surfaces can be answered with (the auth-ui sign-in/session family, the
 * account-ui signed-in family, the tenancy-ui switch family, the notes
 * create surface, the cases surface and the smile-simulation surface the
 * block-B round added) is rendered through a reachable-error whitelist,
 * and this suite pins the whitelists to the server codes themselves.
 *
 * The server side of the comparison is GO_PINNED below: a hand-maintained
 * enumeration of the codes the Go side of this app can answer with on
 * these six surfaces, each entry carrying the source citation of the
 * sentinel that defines it (go/authn/errors.go for the authn codes, the
 * go/rbac and notes-module sentinels for the others). The shell side is
 * derived, never copied: the auth-ui / account-ui / tenancy-ui
 * whitelists are deep-imported from the packages' own error-text modules
 * (never from package entry points -- each package's whitelist is its
 * own, and this app-level suite reads them where they live), and the
 * notes whitelist from this app's own notes-view. The two directions are
 * asserted separately so a drift names its side:
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
 * The GO_PINNED enumeration is deliberately hand-maintained -- a
 * machine-extracted server-side code census (walking the Go sources and
 * the generated spec for every answerable code of every route this app
 * mounts) is DEFERRED: no such extractor ships in this round, the app's
 * four surfaces are small and their reachable codes are enumerated here
 * with citations for audit, and the suite's two directions keep the
 * manual list honest -- a code the server gains and the whitelists do
 * not cover, or a whitelist entry with no citation, both fail here.
 */

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { ERROR_TEXT_CODES as AUTH_UI_ERROR_TEXT_CODES } from '../../../../web/packages/auth-ui/src/internal/error-text.js'
import { ERROR_TEXT_CODES as ACCOUNT_UI_ERROR_TEXT_CODES } from '../../../../web/packages/account-ui/src/internal/error-text.js'
import { ERROR_TEXT_CODES as TENANCY_UI_ERROR_TEXT_CODES } from '../../../../web/packages/tenancy-ui/src/internal/error-text.js'
import { CASES_ERROR_TEXT_KEYS } from './cases-errors.js'
import { SMILE_SIM_ERROR_TEXT_KEYS } from './smile-sim-errors.js'
import { NOTE_ERROR_TEXT_KEYS } from './views/notes-view.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

/**
 * The server-emittable codes of this app's four surfaces, each cited to
 * the source of the sentinel that defines it. The authn sentinels all
 * live in go/authn/errors.go; rbac.permission_denied is go/rbac's
 * ErrPermissionDenied, and the notes codes are the notes module
 * handler's own sentinels.
 *
 * Each citation names the file, the sentinel's current line AND the
 * sentinel identifier that defines the code -- the identifier is the
 * stable half an audit greps for when a Go edit moves the line, which
 * is how the reference-app-web.md P2-1 drift (a sentinel inserted above
 * the authn block shifted 25 of 30 line citations and stayed silent
 * until an audit re-measured them) became visible at all: a citation
 * carrying only a line number cannot be checked without re-opening the
 * Go file, one carrying the identifier can. Since that audit, the
 * suite's own "every citation sits at its declared line" check below
 * has re-measured mechanically: a Go edit that moves a sentinel now
 * fails this suite with the code, the citation and the file named
 * (the P3-rnweb-3 re-pin of the 22 authn citations an intervening
 * round's sentinel insertions had shifted).
 */
const GO_PINNED: Readonly<Record<string, string>> = {
  // go/authn/errors.go -- the authn module's error sentinels, in current
  // file order (re-measured this round: a Go edit inserted a block above
  // ErrPasswordTooWeak since the last audit, shifting every sentinel
  // from there to ErrSessionNotFound by +10 to +17 lines; the
  // token-verification answers entered the enumeration with this
  // re-measurement, and composed-stack verification then kept
  // authn.token_invalid -- authn.authentication_required's citation
  // moved to WHITELISTED_BEYOND_THIS_APP below, see its entry). A later
  // rescan round's sentinel additions between ErrTenantMembershipUnavailable
  // and ErrOAuthStateInvalid (the revocation-check and token-verification
  // sentinels among them) shifted every citation from ErrOAuthStateInvalid
  // to ErrSessionNotFound by exactly +12 lines; re-measured here against
  // the current declaration sites).
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
  'authn.rate_limited': 'go/authn/errors.go:254 (ErrRateLimited)',
  'authn.account_locked': 'go/authn/errors.go:262 (ErrAccountLocked)',
  'authn.channel_disabled': 'go/authn/errors.go:272 (ErrChannelDisabled)',
  'authn.verification_code_invalid': 'go/authn/errors.go:280 (ErrVerificationCodeInvalid)',
  'authn.mfa_not_enrolled': 'go/authn/errors.go:297 (ErrMFANotEnrolled)',
  'authn.mfa_already_enrolled': 'go/authn/errors.go:301 (ErrMFAAlreadyEnrolled)',
  'authn.mfa_invalid_code': 'go/authn/errors.go:307 (ErrMFAInvalidCode)',
  'authn.step_up_required': 'go/authn/errors.go:311 (ErrStepUpRequired)',
  'authn.session_not_found': 'go/authn/errors.go:320 (ErrSessionNotFound)',
  // go/rbac/errors.go -- the permission-denied sentinel the notes route's
  // rbac gate answers with.
  'rbac.permission_denied': 'go/rbac/errors.go:56 (ErrPermissionDenied)',
  // examples/reference-app/internal/notes/handler.go -- the notes module
  // handler's own sentinels.
  'notes.text_required': 'examples/reference-app/internal/notes/handler.go:31 (ErrTextRequired)',
  // The two declarations below sit after the maxRequestBodyBytes constant
  // block the request-body-cap round added above ErrTextTooLong; the cited
  // lines are the current declaration sites, kept in step with the Go
  // source (the deep-check below fails any drift).
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
  'jobs.job_not_found': 'go/jobs/job.go:186 (ErrJobNotFound)',
  // go/billing/errors.go -- the credit-reservation refusal a simulate
  // answers when the tenant's balance cannot cover one generation.
  'billing.insufficient_credits': 'go/billing/errors.go:70 (ErrInsufficientCredits)',
  // go/ai-gateway/errors.go -- the entitlement-gate refusal a simulate
  // answers for a tenant whose subscription lacks the image model.
  'aigateway.entitlement_denied': 'go/ai-gateway/errors.go:42 (ErrEntitlementDenied)',
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

/** The six whitelists by surface, for failure messages that name the
 * list a drift was found in. */
const SURFACE_WHITELISTS: Readonly<Record<string, readonly string[]>> = {
  'auth-ui sign-in/session': AUTH_UI_ERROR_TEXT_CODES,
  'account-ui signed-in family': ACCOUNT_UI_ERROR_TEXT_CODES,
  'tenancy-ui tenant switch': TENANCY_UI_ERROR_TEXT_CODES,
  'notes create surface': Object.keys(NOTE_ERROR_TEXT_KEYS),
  'cases surface': Object.keys(CASES_ERROR_TEXT_KEYS),
  'smile-simulation surface': Object.keys(SMILE_SIM_ERROR_TEXT_KEYS),
}

/** A citation's path, line and sentinel identifier. */
interface SentinelCitation {
  readonly path: string
  readonly line: number
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
  return { path, line: Number(lineText), identifier }
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
  it('keeps every GO_PINNED citation at the line that declares its sentinel', () => {
    // The machine half of the identifier-citation discipline (see the
    // GO_PINNED doc comment): each cited line must actually declare
    // the cited sentinel, so a Go edit that moves a sentinel -- the
    // drift reference-app-web.md P2-1 recorded -- fails this suite
    // instead of waiting for the next manual audit. The files are
    // read relative to this test file (the app lives at
    // examples/reference-app/web, four levels under the repository
    // root, and the citations are repository-root-relative paths).
    for (const [code, citation] of Object.entries(GO_PINNED)) {
      const { path, line, identifier } = parseCitation(code, citation)
      const source = readFileSync(`${repoRootPath()}${path}`, 'utf8')
      const cited = source.split('\n')[line - 1]
      expect(
        cited,
        `${code} is cited at ${path}:${line} (${identifier}), but that line does not declare the sentinel`,
      ).toMatch(new RegExp(`^\\s*(?:var\\s+)?${identifier}\\s*=`))
    }
  })

  it('keeps the hand-maintained enumeration at its audited size', () => {
    // 34 authn sentinels (the 33 of the previous audit plus this
    // round's authn.token_invalid -- the second middleware-era
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
    // go/ai-gateway). The size guard makes a GO_PINNED edit (in either
    // direction) fail loudly here rather than silently through the
    // subset assertions below.
    expect(Object.keys(GO_PINNED)).toHaveLength(67)
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
    const textOf = (bundle: Record<string, unknown>, key: string): string => {
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
