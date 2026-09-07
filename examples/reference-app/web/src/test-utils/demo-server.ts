/**
 * demo-server.ts -- the scripted demo backend the app's surface
 * journeys share: one responder that answers the endpoints a real
 * reference-app server answers, with the demo's own facts baked in as
 * defaults and every answer overridable per suite.
 *
 * The demo facts this script mirrors (the server seeds them, never an
 * app choice): two tenants whose ids are tenant-acme and tenant-globex
 * (their display names are host copy -- no roster endpoint exists, so
 * names live in the app namespace), membership grants matching the
 * seed's per-account model -- the accounts a journey can sign in (the
 * owner, and the reader behind its option) can reach both demo
 * tenants, the counterpart of the every-tenant grants the seed's
 * demo-owner and demo-reader get, while the seed's third account
 * (demo-acme-only@example.com, a tenant-acme member alone) has no web
 * counterpart: no journey drives a member whose switch into
 * tenant-globex the real server would refuse -- a Public config
 * carrying brand.site_name plus the dependency-resolved feature list
 * for the request's tenant, and an authn surface that issues tokens on
 * login and tenant switch.
 *
 * Two demo accounts model the seeded accounts a journey can drive,
 * whose answers the server's own tests pin
 * (cmd/server/demo_users_test.go): the owner account
 * (DEMO_OWNER_IDENTIFIER, every suite's default principal -- the web
 * rig's own account, never a seed mirror: the Go seed's
 * demo-owner@example.com has no web counterpart, because no journey
 * reads it) and -- behind the `reader` option -- the read-only member
 * (DEMO_READER_IDENTIFIER, the web mirror of the demo-reader@example.com
 * seed): its sign-in answers a principal of its own user and session,
 * the notes list serves it like any member's (the Go suite pins a
 * reader's list as served, demo_users_test.go:143-153), and a note
 * create from that principal answers the 403 the write gate gives a
 * caller without notes:write (rbac.permission_denied, asserted at
 * server_test.go:443-445).
 *
 * Registration answers 201 and PROVISIONS the account's own clinic --
 * the web mirror of the composed stack's self-service signup
 * (cmd/server/self_service.go, whose journey regression
 * self_service_test.go pins): the registered account is allocated its
 * own tenant (an id derived from the account, never one of the demo
 * tenants) and a later sign-in of the same identifier succeeds into
 * that clinic, exactly like any member's sign-in -- the browser shape,
 * a body naming no tenant_id (the login form has no tenant field),
 * which the composed stack answers the same way. The old
 * registered-but-unseeded dead end -- registration answering 201 and a
 * later sign-in answering 403 authn.tenant_membership_required -- no
 * longer exists on the real server and no longer exists here. A
 * clinic's NAME mirrors the real host's naming decision
 * (cmd/server/self_service.go's clinicRootNameFor): the display name a
 * register body carries names the provisioned clinic (the later
 * /api/reference-app/clinic-name answer serves it), and a registration
 * that named none falls back to REGISTERED_CLINIC_DEFAULT_NAME -- the
 * fixture's en-US mirror of the real catalog default (the zh-CN value
 * is a CJK literal no test file here may carry). The
 * named-tenant refusal keeps its shape for what it always meant: the
 * real server refuses a sign-in naming a tenant the account holds no
 * membership in (authn.tenant_membership_required, the shape pinned at
 * demo_users_test.go:170-181 by the acme-only account asking for
 * tenant-globex) -- a refusal no journey drives, since the switch and
 * sign-in surfaces a browser reaches never ask for a tenant it was not
 * granted. Registering twice answers the same 409
 * authn.email_already_registered a real handler answers
 * (go/authn/errors.go's ErrEmailAlreadyRegistered).
 *
 * The endpoints: GET /api/config/public (pre-auth, the config fetch
 * usePublicConfig drives), POST /api/v1/authn/register (201, the
 * created AuthnUser; the answer provisions the account's own clinic --
 * see the registration paragraph above), POST /api/v1/authn/login/password
 * (200, a token pair whose principal lands in the configured default
 * tenant -- or, for an account a register created, in the clinic that
 * registration provisioned),
 * POST /api/v1/authn/tenant/switch (200, an access-only answer whose
 * principal is in the tenant the body asks for -- the one answer that
 * reads the request body, which is why RealCall carries it; the spec
 * rotates no refresh token on a switch, so none is issued),
 * POST /api/v1/authn/logout (204), the account surface's endpoints --
 * GET /api/v1/authn/sessions (200, the current session list),
 * DELETE /api/v1/authn/sessions/{id} (204, the row marked revoked for
 * later list answers), POST /api/v1/authn/sessions/revoke-others (200,
 * every non-current active row marked revoked, the answered
 * revoked_count what the notice renders), GET /api/v1/authn/login-history
 * (200, the attempt list; a limit query is not part of a match key),
 * GET /api/v1/authn/identities (200, the bound identity list),
 * DELETE /api/v1/authn/identities/{id} (204, or the unbind refusal the
 * refuseUnbindIdentityId option scripts -- 409 authn.last_login_method),
 * and POST /api/v1/authn/social/{provider}/callback (200, a
 * binding-shaped answer whose identity the next identities answer
 * carries) -- plus GET /api/v1/notes (200, the note list of the bearer
 * principal's tenant) and POST /api/v1/notes (201, the created note,
 * appended to that tenant's list so a refetch after a create really
 * shows it), and the cases surface's five operations mirroring the
 * real fragment's clinic-wide shape: GET /api/v1/cases (200, the case
 * list of the bearer principal's tenant), POST /api/v1/cases (201, the
 * created case with its photos echoed, appended to that tenant's list),
 * POST /api/v1/cases/photos/upload (201 {object_id}, the one-shot
 * upload answer), GET /api/v1/cases/{caseId} (200, one case) and GET
 * /api/v1/cases/{caseId}/photos/{photoObjectID}/content (200, the
 * photo's bytes). The notes answers mirror the real handler's
 * refusals: a create whose trimmed text is empty answers 400
 * notes.text_required (internal/notes/handler.go), one over the
 * 4000-character limit answers 400 notes.text_too_long, and the deny
 * switches below answer the read/write permission refusals the rbac
 * gate over the notes module produces (403 rbac.permission_denied) --
 * while the cases answers mirror the real refusals through their own
 * switches (denyCasesList, casesCreateRefusal, casesUploadRefusal,
 * casesPhotoContentRefusal) -- the journeys gate and error surfaces are
 * driven by genuine refusals, never stubbed locally. The app's own
 * tenant-identity answer, GET /api/reference-app/clinic-name (200,
 * {name}), mirrors the host route cmd/server/clinic_name.go mounts:
 * the org root name of the bearer principal's tenant, served for the
 * off-roster clinics the demo roster's host copy cannot name (see the
 * clinic-naming paragraph above). The smile-simulation and sharing
 * surfaces ride the same shape: the simulation endpoints under
 * /api/v1/smile-simulation answer the block-B read legs (the
 * job-status, per-photo enumeration and content reads over the
 * deterministic job ledger, plus the simulate 202), and the block-C
 * sharing endpoints answer the clinic's mint and the patient's open --
 * POST /api/v1/sharing/shares (201, a share echoing the body's
 * resourceRef plus its once-returned bearer token; the reader-shaped
 * principal's mint answers the rbac gate's 403) and GET
 * /api/v1/sharing/access (200, the shared simulation's raw bytes --
 * genuinely public, no bearer resolved -- or the scripted refusal
 * through the shareAccessRefusal switch).
 *
 * The multi-factor surface mirrors the authn handler's step-up
 * machine (the same states its own tests pin): POST
 * /api/v1/authn/mfa/totp/enroll answers 200 {secret, provisioning_uri}
 * while no factor is active, and 403 authn.step_up_required -- the
 * discover-by-acting signal that an active factor exists -- to a caller
 * whose access token carries no fresh second-factor proof; POST
 * /api/v1/authn/mfa/step-up verifies DEMO_MFA_CONFIRM_CODE and answers
 * an access-only token elevated for the principal (a wrong code answers
 * 400 authn.mfa_invalid_code; the elevation lives only in that token,
 * per errors.go's ErrStepUpRequired contract); and POST
 * /api/v1/authn/mfa/totp/confirm makes the factor active (a second
 * session's confirm while one is pending answers 409
 * authn.mfa_already_enrolled to an unelevated caller), answering the
 * show-once recovery codes -- DEMO_MFA_RECOVERY_CODES on the first
 * setup, DEMO_MFA_REPLACEMENT_RECOVERY_CODES when an elevated caller
 * confirms over an active factor, both exported so a journey can pin
 * the rendered rows verbatim.
 *
 * One deliberate scope limit, recorded rather than half-built: the
 * account-domain state (the session list, the bound identities, the
 * active factor) belongs to the owner story -- the demo seeds no
 * reader-shaped account state, because a reader journey into the
 * account surface would answer the owner's rows. The `reader`
 * option's own day (the app-journey suite scripts it) therefore
 * stays on notes, the exact surface where the seed's grant asymmetry
 * lives: the list served like any member's, a create refused with
 * the rbac write gate's 403 -- the answers the Go suite pins for the
 * read-only member (its list served, demo_users_test.go:143-153, its
 * create refused, server_test.go:443-445). The read-denied refusal of a
 * caller without notes:read is the denyNotesRead switch's answer, a
 * shape no seeded account carries.
 *
 * Anything else fails the test loudly: an unpinned request means the
 * journey under test reached an endpoint the demo does not serve.
 */

import type { CasesCase } from '@speed/api-sdk'
import type { NotesNote } from '@speed/api-sdk'
import type {
  AuthnIdentity,
  AuthnLoginAttempt,
  AuthnSession,
} from '@speed/api-sdk'
import type { PublicConfigResponse } from '@speed/api-client'
import { errorResponse, jsonResponse } from './real-client.js'
import type { RealCall, RealResponder } from './real-client.js'

/** The empty Public answer: no brand, no enabled features. */
const EMPTY_PUBLIC_CONFIG: PublicConfigResponse = {
  config: {},
  features: [],
}

/** The owner account every suite signs in as (the current session the
 * default session rows ride on, the email bound by a default exchange).
 * The web rig's own account; the Go seed's demo-owner@example.com has no
 * web counterpart because nothing in a web journey reads it. */
export const DEMO_OWNER_IDENTIFIER = 'owner@example.test'

/** The read-only member account the `reader` option scripts -- the web
 * mirror of the demo-reader@example.com seed the Go server tests sign in
 * (cmd/server/demo_users_test.go): same identifier, same asymmetry --
 * the notes list served, a create refused. */
export const DEMO_READER_IDENTIFIER = 'demo-reader@example.com'
/** The reader-shaped principal's user id (the demo seed's reader row). */
export const DEMO_READER_USER_ID = 'user-2'
/** The reader-shaped principal's session id -- its own session, never
 * the owner's. */
export const DEMO_READER_SESSION_ID = 'session-4'
/** The user id a register answer allocates for the FIRST account it
 * creates (registered user ids count up from user-9, past the seeded
 * rows user-1..user-8, mirroring how the fixture numbers registered
 * sessions from session-9). */
export const FIRST_REGISTERED_USER_ID = 'user-9'
/** The clinic tenant the FIRST registered account's registration
 * provisions: 'tenant-' + its user id, the web mirror of the composed
 * stack's deterministic tenant derivation (self_service.go's
 * clinicTenantOf). A journey that signs the first registered account in
 * asserts the frame names this tenant. */
export const FIRST_REGISTERED_CLINIC_TENANT_ID = `tenant-${FIRST_REGISTERED_USER_ID}`


/** The TOTP secret every enroll answer serves -- scripted once so the
 * journeys can pin the wizard's rendered secret verbatim. */
export const DEMO_MFA_SECRET = 'JBSWY3DPEHPK3PXP'
/** The provisioning URI every enroll answer serves beside the secret. */
export const DEMO_MFA_PROVISIONING_URI =
  'otpauth://totp/Smile%20Simulation%20Platform:owner@example.test' +
  `?secret=${DEMO_MFA_SECRET}&issuer=Smile%20Simulation%20Platform`
/** The one code the step-up and confirm handlers accept (a scripted
 * stand-in for the TOTP value a real authenticator would produce). */
export const DEMO_MFA_CONFIRM_CODE = '123456'
/** The ten recovery codes the first confirm answers -- shown exactly
 * once by the account surface, scripted once here so a journey can pin
 * rendered rows verbatim. */
export const DEMO_MFA_RECOVERY_CODES: readonly string[] = [
  'maple-falcon-1842',
  'amber-velvet-6390',
  'coral-meadow-2715',
  'ember-silver-9046',
  'fjord-cactus-5173',
  'glow-willow-7284',
  'harbor-brick-3501',
  'iris-comet-4967',
  'jade-lantern-8052',
  'kite-orchard-2638',
]
/** The set a confirm over an active factor (the elevated replacement
 * path) answers -- a fresh set, as the replacing server would issue,
 * scripted once here so a journey can pin rendered rows verbatim the
 * way it pins DEMO_MFA_RECOVERY_CODES' on the first setup. */
export const DEMO_MFA_REPLACEMENT_RECOVERY_CODES: readonly string[] = [
  'lumen-delta-1495',
  'meadow-sparrow-2830',
  'nimbus-raven-3764',
  'onyx-fern-4521',
  'pearl-grove-5187',
  'quill-harbor-6043',
  'raven-iris-7592',
  'saffron-lotus-8260',
  'tundra-beacon-9314',
  'umber-dune-0876',
]

export interface DemoServerOptions {
  /**
   * The GET /api/config/public answer; defaults to an empty config and
   * feature list. Suites whose surfaces render server-driven content
   * (the brand, the home feature cards) script their answer here.
   */
  readonly publicConfig?: PublicConfigResponse
  /** The tenant a fresh password sign-in lands in; default
   * 'tenant-acme', the first tenant of the demo roster. */
  readonly tenantId?: string
  /** The user id of a token-issuing answer's principal; default
   * 'user-1'. */
  readonly userId?: string
  /** The session id of a token-issuing answer's principal; default
   * 'session-1'. */
  readonly sessionId?: string
  /**
   * Scripts the demo's read-only member account: a sign-in with
   * DEMO_READER_IDENTIFIER answers a principal of DEMO_READER_USER_ID
   * and DEMO_READER_SESSION_ID in the configured default tenant, the
   * notes list serves it like any member's, and a note create from
   * that principal answers the rbac write gate's 403 -- the grant
   * asymmetry the Go demo-users suite pins (list served, create
   * refused). Default false: every account is owner-shaped.
   */
  readonly reader?: boolean
  /** The GET /api/v1/notes list of the default tenant as first served.
   * Stateful from there: a create appends the note the next list
   * answer of that tenant carries. Default [] -- the real handler's
   * list is never a null answer. */
  readonly initialNotes?: readonly NotesNote[]
  /** Refuses the notes list with the rbac read gate's 403 (the answer a
   * caller without notes:read gets); default false. */
  readonly denyNotesRead?: boolean
  /** Refuses a note create with the rbac write gate's 403 (the answer a
   * caller without notes:write gets); default false. */
  readonly denyNotesWrite?: boolean
  /** The GET /api/v1/authn/sessions list as first served; defaults to
   * three active demo rows -- the current session on the option's own
   * session id (device 'Demo laptop') plus two others. Stateful from
   * there: a revoke or revoke-others marks rows revoked for later list
   * answers. */
  readonly initialSessions?: readonly AuthnSession[]
  /** The GET /api/v1/authn/login-history list as first served; defaults
   * to one successful password sign-in and one bad-password failure
   * (method and reason tokens both on the section's known lists). */
  readonly initialLoginAttempts?: readonly AuthnLoginAttempt[]
  /** The GET /api/v1/authn/identities list as first served; default []
   * -- the demo account starts with nothing bound, every demo channel
   * offered by the add area. Stateful from there: a binding exchange
   * appends its identity for later list answers. */
  readonly initialIdentities?: readonly AuthnIdentity[]
  /** The id of a bound identity whose unbind the server refuses with
   * the 409 authn.last_login_method a real account whose last sign-in
   * method is that binding answers; default undefined -- every unbind
   * succeeds. */
  readonly refuseUnbindIdentityId?: string
  /** The authorize_url a GET of the social-authorize endpoint answers
   * with (the add area's session request, whose redirect_uri query the
   * app's channels carry). Defaults to a benign https destination;
   * the account-view protocol-guard journeys script a hostile scheme
   * here to prove the host refuses it. */
  readonly socialAuthorizeUrl?: string
  /** The GET /api/v1/cases list of the default tenant as first served.
   * Stateful from there: a create appends the case later list answers
   * of the same tenant carry (id 'case-N', the request echoed), the
   * clinic-wide shape the real handler serves. Default [] -- the real
   * handler's list is never a null answer. */
  readonly initialCases?: readonly CasesCase[]
  /** Answers every GET /api/v1/cases with the 500 internal-error
   * envelope, the load-failure a surface renders its error state for;
   * default false. */
  readonly denyCasesList?: boolean
  /** Refuses every POST /api/v1/cases with this coded answer -- a
   * suite scripts the create refusals its surface must render (the
   * photo-already-attached conflict, the over-long name); default
   * undefined -- every create succeeds. */
  readonly casesCreateRefusal?: {
    readonly status: number
    readonly code: string
  }
  /** Refuses every POST /api/v1/cases/photos/upload with this coded
   * answer -- a suite scripts the refusals its queue must render (the
   * probe's photo_rejected, say); default undefined -- every upload
   * succeeds. */
  readonly casesUploadRefusal?: {
    readonly status: number
    readonly code: string
  }
  /** Refuses every photo-content read (GET /api/v1/cases/{caseId}/
   * photos/{photoObjectID}/content) with this coded answer; default
   * undefined -- every attached photo's content is served. */
  readonly casesPhotoContentRefusal?: {
    readonly status: number
    readonly code: string
  }
  /** The photo-content answer every attached photo serves (the media
   * type the probe assigned and the stored bytes); default the demo
   * payload above. */
  readonly casesPhotoContent?: {
    readonly media_type: string
    readonly content_base64: string
  }
  /** Refuses every POST /api/v1/smile-simulation/simulate with this
   * coded answer -- a suite scripts the refusals its surface must
   * render (the credit reservation's billing.insufficient_credits, the
   * entitlement gate's aigateway.entitlement_denied); default
   * undefined -- every simulate succeeds. */
  readonly simulateRefusal?: {
    readonly status: number
    readonly code: string
  }
  /** Refuses every simulation-content read (GET /api/v1/smile-simulation/
   * photos/{photoObjectID}/simulations/{jobID}/content) with this coded
   * answer; default undefined -- a succeeded simulation's content is
   * served. */
  readonly simulationContentRefusal?: {
    readonly status: number
    readonly code: string
  }
  /** The simulation-content answer a succeeded simulation serves (the
   * media type the probe assigned and the stored bytes); default the
   * demo payload below, distinct from the photo payload so a journey
   * can tell before from after by content. */
  readonly simulationContent?: {
    readonly media_type: string
    readonly content_base64: string
  }
  /** The name the GET /api/reference-app/clinic-name answer serves for
   * a current tenant that is neither a demo tenant nor a clinic a
   * register answer provisioned (a suite that signs a principal
   * straight into a clinic-shaped tenant without the register turn
   * scripts this); default REGISTERED_CLINIC_DEFAULT_NAME. */
  readonly clinicName?: string
  /** Refuses every POST /api/v1/sharing/shares with this coded answer
   * -- a suite scripts the create refusals its surface must render (the
   * module's 429 sharing.rate_limited); default undefined -- every
   * create succeeds (a create from the reader-shaped principal answers
   * the rbac gate's 403 rbac.permission_denied behind the `reader`
   * option, the grant asymmetry the Go suite pins for sharing:create).
   */
  readonly sharesCreateRefusal?: {
    readonly status: number
    readonly code: string
  }
  /** Refuses the SECOND POST /api/v1/sharing/shares of the run with
   * this coded answer -- a suite scripts the share action's
   * half-refused-pair shape (one half of the before/after pair mints,
   * the other is refused, and the minted half must be revoked again);
   * default undefined -- every create succeeds (the first create of a
   * run still succeeds under this option, and so does every create
   * after the second). */
  readonly sharesCreateSecondRefusal?: {
    readonly status: number
    readonly code: string
  }
  /** Refuses every GET /api/v1/sharing/access with this coded answer --
   * a suite scripts the refusals the patient page must render (the
   * expired/revoked 404 sharing.not_accessible, the rate limit's 429,
   * the 502 sharing.resource_unavailable); default undefined -- the
   * access route serves the shared simulation's bytes. The route is
   * genuinely public: its answer never resolves a bearer. */
  readonly shareAccessRefusal?: {
    readonly status: number
    readonly code: string
  }
}

/** The name the fixture gives a registered account's clinic when its
 * registration named none: the fixture's en-US mirror of the real
 * host's fallback (the org catalog's default workspace name rendered in
 * the platform-default zh-CN locale -- a CJK literal no test file here
 * may carry, so the mirror is the en-US value). The register turn of
 * the journeys types a display name when the clinic's own name is what
 * the journey asserts; the e2e tier drives the real server, whose root
 * naming comes from the real catalog. */
export const REGISTERED_CLINIC_DEFAULT_NAME = 'Workspace'

/** What an issued access token stands for: the principal it belongs to
 * and whether it carries a fresh second-factor proof (the elevation a
 * step-up settles, living only in that token's lifetime). */
interface IssuedPrincipal {
  readonly user_id: string
  readonly tenant_id: string
  readonly session_id: string
  readonly elevated: boolean
}

/** The body of a request whose payload the answer depends on, or {} for
 * a body-less request or one that is not JSON (neither endpoint here
 * has one: the register and switch bodies are client-produced JSON). */
function bodyObject(call: RealCall): Record<string, unknown> {
  if (call.body === '') {
    return {}
  }
  try {
    const parsed: unknown = JSON.parse(call.body)
    return typeof parsed === 'object' && parsed !== null
      ? (parsed as Record<string, unknown>)
      : {}
  } catch {
    return {}
  }
}

/** The permission code an rbac gate answers a caller without the
 * requested grant with (ErrPermissionDenied) -- the notes module's
 * write gate and the sharing module's create gate alike. */
const RBAC_PERMISSION_DENIED_CODE = 'rbac.permission_denied'

/** The effective options of a simulate body: the documented defaults
 * (natural smile, natural shade, full strength -- the same defaults
 * internal/smilesim's DefaultSimulationOptions declares) with each
 * explicitly-present dimension overridden. The real service applies the
 * same defaulting, so an options-less body generates like the real
 * server's no-options call and the echoed options always carry the
 * effective set. */
function effectiveSimulationOptions(body: Record<string, unknown>): {
  smile_style: string
  tooth_shade: string
  strength: number
} {
  const requested =
    typeof body.options === 'object' && body.options !== null
      ? (body.options as Record<string, unknown>)
      : {}
  const smileStyle =
    typeof requested.smile_style === 'string'
      ? requested.smile_style
      : 'natural'
  const toothShade =
    typeof requested.tooth_shade === 'string'
      ? requested.tooth_shade
      : 'natural'
  const strength =
    typeof requested.strength === 'number' ? requested.strength : 1
  return { smile_style: smileStyle, tooth_shade: toothShade, strength }
}

/** The created_at every demo note answer carries -- the same fixed demo
 * epoch the register answer uses, so journeys can pin rendered times. */
const DEMO_NOTE_CREATED_AT = '2026-09-04T00:00:00Z'

/** The notes text limit the real handler enforces (its
 * notes.text_too_long answer names it as the 'limit' param). */
const NOTE_TEXT_LIMIT = 4000

/** The parameterized account paths: the delete-by-id routes plus the
 * social callback. Each matcher guards its own method and shape; the
 * revoke-others route is exact-keyed in the switch, so its path never
 * falls through to these. */
const SESSION_PATH = /^\/api\/v1\/authn\/sessions\/([^/]+)$/
/** The share-revoke path (POST /api/v1/sharing/shares/{shareId}/revoke,
 * sharing.PathShares with the module's /revoke suffix): the share
 * action's compensation leg, revoking the half of a failed pair that
 * did mint. */
const SHARE_REVOKE_PATH = /^\/api\/v1\/sharing\/shares\/([^/]+)\/revoke$/
const IDENTITY_PATH = /^\/api\/v1\/authn\/identities\/([^/]+)$/
const SOCIAL_CALLBACK_PATH = /^\/api\/v1\/authn\/social\/([^/]+)\/callback$/
const SOCIAL_AUTHORIZE_PATH = /^\/api\/v1\/authn\/social\/([^/]+)\/authorize$/

/** The authorize URL the demo answers with unless the option scripts
 * another one -- a benign https destination, so a journey that ever
 * drives the add area's authorize click without scripting its own
 * answer goes somewhere harmless instead of nowhere. */
const DEFAULT_SOCIAL_AUTHORIZE_URL =
  'https://sso.example.test/authorize?channel=demo'

/** The parameterized cases paths: one case's detail and one attached
 * photo's content (the block-A surface's read legs). The photo-upload
 * route is exact-keyed in the switch, so its path never falls through
 * to these. */
const CASE_PATH = /^\/api\/v1\/cases\/([^/]+)$/
const CASE_PHOTO_CONTENT_PATH =
  /^\/api\/v1\/cases\/([^/]+)\/photos\/([^/]+)\/content$/

/** The parameterized smile-simulation paths (the block-B surface's read
 * legs). The simulate route is exact-keyed in the switch, so its path
 * never falls through to these; the job-status route is exact-keyed
 * with one parameter; the enumeration path's $ anchor keeps the content
 * path (which carries a deeper suffix) from matching it. */
const SIMULATION_JOB_PATH = /^\/api\/v1\/smile-simulation\/jobs\/([^/]+)$/
const SIMULATION_LIST_PATH =
  /^\/api\/v1\/smile-simulation\/photos\/([^/]+)\/simulations$/
const SIMULATION_CONTENT_PATH =
  /^\/api\/v1\/smile-simulation\/photos\/([^/]+)\/simulations\/([^/]+)\/content$/

/** The created_at every demo case answer carries -- the same fixed demo
 * epoch the notes answers use, so journeys can pin rendered times. */
const DEMO_CASE_CREATED_AT = DEMO_NOTE_CREATED_AT

/** The expiry every minted demo share answers with: the real server's
 * forced default is 30 days from creation (go/sharing's
 * defaultShareExpiry), and the demo's clock stands at the fixed epoch
 * above. */
const DEMO_SHARE_EXPIRES_AT = '2026-10-04T00:00:00Z'

/** The raw bytes the access route serves for a granted share -- the
 * decoded form of the simulation-result payload above, since the
 * shared object IS the simulation's output. Not a decodable image:
 * jsdom loads no images, and the api-client's refusal to parse these
 * bytes as JSON is exactly the patient page's live-share retry signal.
 */
const DEMO_SHARE_CONTENT_BYTES = 'simulation-result-bytes'

/** The photo bytes every demo photo-content answer serves unless a
 * suite scripts its own: the base64 of the ASCII payload "photo-bytes"
 * -- not a decodable image (the demo never renders it), but a stable,
 * assertable payload. */
const DEMO_PHOTO_CONTENT_BASE64 = 'cGhvdG8tYnl0ZXM='

/** The simulation-result bytes every simulation-content answer serves
 * unless a suite scripts its own: the base64 of the ASCII payload
 * "simulation-result-bytes", deliberately distinct from the photo
 * payload above so a journey can prove the before/after pair shows two
 * different images. */
const DEMO_SIMULATION_CONTENT_BASE64 = 'c2ltdWxhdGlvbi1yZXN1bHQtYnl0ZXM='

/** The demo's three sessions: the current one on the rig's own session
 * id (the same row every token-issuing answer names) plus two active
 * others -- the rows the revoke journeys act on. The device strings and
 * fixed epochs are served data, scripted here once so the journeys can
 * pin rendered rows verbatim. */
function defaultSessions(sessionId: string): AuthnSession[] {
  return [
    {
      id: sessionId,
      status: 'active',
      is_current: true,
      device: 'Demo laptop',
      ip: '198.51.100.4',
      amr: ['password'],
      created_at: '2026-08-27T08:00:00.000Z',
      last_seen_at: '2026-09-03T22:15:00.000Z',
    },
    {
      id: 'session-2',
      status: 'active',
      is_current: false,
      device: 'Windows desktop',
      ip: '198.51.100.52',
      amr: ['password'],
      created_at: '2026-08-24T11:30:00.000Z',
      last_seen_at: '2026-09-03T18:40:00.000Z',
    },
    {
      id: 'session-3',
      status: 'active',
      is_current: false,
      device: 'iPad Safari',
      ip: '198.51.100.77',
      amr: ['password'],
      created_at: '2026-08-19T09:00:00.000Z',
      last_seen_at: '2026-09-02T07:05:00.000Z',
    },
  ]
}

/** The demo's login history: one successful password sign-in and one
 * bad-password failure, method and reason tokens both on the login
 * history section's known lists (so both render their bundle text). */
function defaultLoginAttempts(): AuthnLoginAttempt[] {
  return [
    {
      method: 'password',
      result: 'success',
      ip: '198.51.100.4',
      created_at: '2026-09-03T22:15:00.000Z',
    },
    {
      method: 'password',
      result: 'failure',
      failure_reason: 'bad_password',
      ip: '198.51.100.200',
      created_at: '2026-09-02T06:45:00.000Z',
    },
  ]
}

/** Builds the shared demo responder for the given options. */
export function demoServer(options: DemoServerOptions = {}): RealResponder {
  const {
    publicConfig = EMPTY_PUBLIC_CONFIG,
    tenantId = 'tenant-acme',
    userId = 'user-1',
    sessionId = 'session-1',
    reader = false,
    initialNotes = [],
    denyNotesRead = false,
    denyNotesWrite = false,
    initialSessions,
    initialLoginAttempts,
    initialIdentities = [],
    refuseUnbindIdentityId,
    socialAuthorizeUrl = DEFAULT_SOCIAL_AUTHORIZE_URL,
    initialCases = [],
    denyCasesList = false,
    casesCreateRefusal,
    casesUploadRefusal,
    casesPhotoContentRefusal,
    casesPhotoContent = {
      media_type: 'image/png',
      content_base64: DEMO_PHOTO_CONTENT_BASE64,
    },
    simulateRefusal,
    simulationContentRefusal,
    simulationContent = {
      media_type: 'image/png',
      content_base64: DEMO_SIMULATION_CONTENT_BASE64,
    },
    clinicName,
    sharesCreateRefusal,
    sharesCreateSecondRefusal,
    shareAccessRefusal,
  } = options
  // The account state is stateful per responder instance (a revoke
  // marks a row for later list answers, an exchange appends a bound
  // identity) -- each test's rig gets its own server, so the state
  // never leaks across journeys. The default sessions ride on the
  // option's own session id, so the current row is always the session
  // the token-issuing answers named.
  const sessions: AuthnSession[] = [
    ...(initialSessions ?? defaultSessions(sessionId)),
  ]
  const loginAttempts: AuthnLoginAttempt[] = [
    ...(initialLoginAttempts ?? defaultLoginAttempts()),
  ]
  const identities: AuthnIdentity[] = [...initialIdentities]
  // The notes lists are stateful per responder instance and keyed by
  // tenant: a create appends the note later list answers of the same
  // tenant carry -- a switch to another tenant answers that tenant's
  // own list, the way the real handler's tenant-scoped repository does.
  const notesByTenant = new Map<string, NotesNote[]>([
    [tenantId, [...initialNotes]],
  ])
  const notesOf = (tenant: string): NotesNote[] => {
    const list = notesByTenant.get(tenant)
    if (list === undefined) {
      const fresh: NotesNote[] = []
      notesByTenant.set(tenant, fresh)
      return fresh
    }
    return list
  }
  let nextNoteId = 1
  // The case lists are stateful per responder instance and keyed by
  // tenant exactly like the notes lists: a create appends the case
  // later list answers of the same tenant carry -- the clinic-wide
  // shape the real handler's tenant-scoped repository serves.
  const casesByTenant = new Map<string, CasesCase[]>([
    [tenantId, [...initialCases]],
  ])
  const casesOf = (tenant: string): CasesCase[] => {
    const list = casesByTenant.get(tenant)
    if (list === undefined) {
      const fresh: CasesCase[] = []
      casesByTenant.set(tenant, fresh)
      return fresh
    }
    return list
  }
  let nextCaseId = 1
  // The object ids the demo's photo-upload answers hand out, counting
  // per responder instance like the note and case ids.
  let nextObjectId = 1
  // The smile-simulation ledger: every job this responder accepted,
  // keyed by its job id and remembered in creation order. Each job's
  // live status advances deterministically per job-status read --
  // pending (no read yet), running (one read), succeeded (two or more)
  // -- so a journey that polls the status route observes an honest
  // queued -> generating -> generated progression that always
  // terminates, and the enumeration and content answers read the same
  // ledger the status route advances.
  const simulationJobs = new Map<
    string,
    {
      photo_object_id: string
      options: { smile_style: string; tooth_shade: string; strength: number }
      reads: number
      created_at: string
    }
  >()
  const simulationJobOrder: string[] = []
  let nextSimulationJobId = 1
  // The share-ledger counters: ids and bearer tokens count up per
  // responder instance like the note, case and object ids -- each mint
  // answers a distinct token, the once-returned credential the share
  // link carries.
  let nextShareId = 1
  let nextShareToken = 1
  // The create call count, for the half-refused-pair switch (the
  // second create of a run is the one a suite can script to fail).
  let shareCreateCount = 0

  /** The live status of one accepted job under this responder's
   * deterministic progression (see the ledger comment above). */
  function simulationStatusOf(jobID: string): string {
    const job = simulationJobs.get(jobID)
    if (job === undefined) {
      return ''
    }
    if (job.reads >= 2) {
      return 'succeeded'
    }
    return job.reads === 1 ? 'running' : 'pending'
  }
  // The accounts a register answered, mapped to the clinic their
  // registration provisioned (the web mirror of the composed stack's
  // self-service signup, cmd/server/self_service.go): the account's own
  // user id and the own tenant -- an id derived from the account, never
  // one of the demo tenants -- a later sign-in lands in. Registration
  // alone now grants a sign-in-able membership, exactly like the real
  // server's journey regression (self_service_test.go) pins.
  const registeredAccounts = new Map<
    string,
    { user_id: string; tenant_id: string; clinic_name: string }
  >()
  // The clinic tenants registered above, mapped to the name their
  // registration gave the clinic: the display name the register body
  // carried (the name a practice types at signup), or the fixture's
  // default when it carried none -- the mirror of the real
  // provisioner's root naming (self_service.go's clinicRootNameFor,
  // which reads the registrant's authn display name back and falls back
  // to the catalog default).
  const clinicNamesByTenant = new Map<string, string>()
  // The id counters for registered accounts: their user ids start after
  // the seeded rows (the demo accounts own user-1..user-8) and their
  // sessions after the demo roster's (session-1..session-4), so nothing
  // a journey pins about a seeded principal can collide with a
  // registered one. The clinic tenant id is derived from the account's
  // user id ('tenant-' + user_id), the same deterministic-derivation
  // shape the real server uses (clinicTenantOf).
  let nextRegisteredUserNumber = 9
  let nextRegisteredSessionNumber = 9
  // Every issued access token, mapped to the principal it stands for.
  // Token strings count up per responder instance ('access-1',
  // 'access-2', ...) so journeys can pin the first issue and read the
  // store for the rest -- the numbering mirrors a server whose issues
  // increment, never a fixed vocabulary.
  const issued = new Map<string, IssuedPrincipal>()
  let accessCounter = 0
  let refreshCounter = 0
  // The multi-factor state: no active factor until a confirm succeeds,
  // a factor once active gates every enroll behind a step-up.
  let factorActive = false

  /** The bearer principal of a request, resolved from the access token
   * the request carried. Every token a journey can hold was issued by
   * this responder instance -- the app's suites sign in through the
   * server's own login answer, never a hand-scripted one -- so an
   * absent or unknown bearer on a principal-requiring endpoint is a
   * harness bug: fail the test loudly rather than answer as a default
   * principal, which would mask an app regression into fetching
   * protected data without a token. */
  function principalOf(call: RealCall): IssuedPrincipal {
    const authorization = call.authorization
    const prefix = 'Bearer '
    const token =
      authorization !== null && authorization.startsWith(prefix)
        ? authorization.slice(prefix.length)
        : null
    const principal = token !== null ? issued.get(token) : undefined
    if (principal === undefined) {
      const bearer =
        token === null ? 'no bearer token' : 'an unknown bearer token'
      throw new Error(
        `demo-server: ${call.method} ${call.path} reached a principal-requiring endpoint with ${bearer}`,
      )
    }
    return principal
  }

  /** Issues an access token for the principal, numbering it with the
   * instance's next count. */
  function issueAccess(
    principal: Omit<IssuedPrincipal, 'elevated'>,
    elevated: boolean,
  ): string {
    accessCounter += 1
    const token = `access-${accessCounter}`
    issued.set(token, { ...principal, elevated })
    return token
  }

  /** Issues the next refresh token. */
  function issueRefreshToken(): string {
    refreshCounter += 1
    return `refresh-${refreshCounter}`
  }

  return (call) => {
    const key = `${call.method} ${call.path}`
    switch (key) {
      case 'GET /api/config/public':
        return jsonResponse(200, publicConfig)
      case 'GET /api/reference-app/clinic-name': {
        // The app's own tenant-identity answer (cmd/server/clinic_name.go
        // mounts the real route): the org root name of the tenant the
        // bearer principal is scoped to. A clinic a register answer
        // provisioned answers the name its registration carried; a
        // principal scripted straight into another off-roster tenant
        // answers the clinicName option; the demo tenants (whose names
        // are host copy the app renders without this request) are
        // answered with the fixture's default -- no journey asks.
        const principal = principalOf(call)
        const registeredName = clinicNamesByTenant.get(principal.tenant_id)
        if (registeredName !== undefined) {
          return jsonResponse(200, { name: registeredName })
        }
        if (clinicName !== undefined) {
          return jsonResponse(200, { name: clinicName })
        }
        return jsonResponse(200, { name: REGISTERED_CLINIC_DEFAULT_NAME })
      }
      case 'POST /api/v1/authn/login/password': {
        const body = bodyObject(call)
        const identifier =
          typeof body.identifier === 'string' ? body.identifier : undefined
        // A registered account signs into the clinic its registration
        // provisioned -- its own principal (user, session and tenant all
        // its own), the answer the composed stack's self-service journey
        // regression pins in the browser's own shape -- no tenant_id in
        // the body (self_service_test.go): the account a register
        // created can sign in and act in its clinic like any member. The
        // old registered-but-unseeded refusal shape is gone with the
        // product decision that removed it.
        const registered = registeredAccounts.get(identifier ?? '')
        if (registered !== undefined) {
          const principal = {
            user_id: registered.user_id,
            tenant_id: registered.tenant_id,
            session_id: `session-${nextRegisteredSessionNumber}`,
          }
          nextRegisteredSessionNumber += 1
          return jsonResponse(200, {
            access_token: issueAccess(principal, false),
            refresh_token: issueRefreshToken(),
            principal,
          })
        }
        // The reader option's member: its own principal, the web mirror
        // of the demo seed's reader row.
        const principal =
          reader && identifier === DEMO_READER_IDENTIFIER
            ? {
                user_id: DEMO_READER_USER_ID,
                tenant_id: tenantId,
                session_id: DEMO_READER_SESSION_ID,
              }
            : { user_id: userId, tenant_id: tenantId, session_id: sessionId }
        return jsonResponse(200, {
          access_token: issueAccess(principal, false),
          refresh_token: issueRefreshToken(),
          principal,
        })
      }
      case 'POST /api/v1/authn/register': {
        const body = bodyObject(call)
        const email = typeof body.email === 'string' ? body.email : undefined
        if (email !== undefined) {
          // A second register of the same email answers the real
          // handler's uniqueness refusal.
          if (registeredAccounts.has(email)) {
            return errorResponse(409, 'authn.email_already_registered')
          }
          // Registration provisions the account's clinic -- its own user
          // id and its own tenant, derived from that id the way the real
          // server derives it (self_service.go's clinicTenantOf) -- so
          // the 201 the register form shows answers for an account a
          // later sign-in can actually enter.
          const user_id = `user-${nextRegisteredUserNumber}`
          nextRegisteredUserNumber += 1
          // The clinic's name is the display name the registration
          // carried (the real host reads it back from the authn user
          // row at provisioning time), falling back to the fixture's
          // default when the body named none.
          const rawName =
            typeof body.display_name === 'string'
              ? body.display_name
              : typeof body.displayName === 'string'
                ? body.displayName
                : ''
          const clinic_name =
            rawName.trim().length > 0 ? rawName.trim() : REGISTERED_CLINIC_DEFAULT_NAME
          const tenant_id = `tenant-${user_id}`
          registeredAccounts.set(email, {
            user_id,
            tenant_id,
            clinic_name,
          })
          clinicNamesByTenant.set(tenant_id, clinic_name)
          return jsonResponse(201, {
            id: user_id,
            email,
            created_at: DEMO_NOTE_CREATED_AT,
          })
        }
        // A register that named no email (the identifier was a phone) is
        // recorded under nothing a sign-in can name, so it stays the
        // pre-self-service fixture's unrecorded 201 answer -- no journey
        // drives a phone registration.
        return jsonResponse(201, {
          id: 'user-9',
          created_at: DEMO_NOTE_CREATED_AT,
        })
      }
      case 'POST /api/v1/authn/tenant/switch': {
        const principal = principalOf(call)
        const body = bodyObject(call)
        const requested =
          typeof body.tenant_id === 'string'
            ? body.tenant_id
            : principal.tenant_id
        // The switch settles an access-only answer: the spec rotates no
        // refresh token on a tenant switch, and the session parses the
        // answer as access-only -- the issued token names the requested
        // tenant, and nothing here carries the caller's elevation over
        // (a fresh token stands for what it stands for).
        const switched = {
          user_id: principal.user_id,
          tenant_id: requested,
          session_id: principal.session_id,
        }
        return jsonResponse(200, {
          access_token: issueAccess(switched, false),
          principal: switched,
        })
      }
      case 'POST /api/v1/authn/logout':
        return new Response(null, { status: 204 })
      case 'GET /api/v1/notes': {
        const principal = principalOf(call)
        // The read gate: the deny switch scripts the rbac read gate's
        // 403 (the answer a caller without notes:read gets). The bearer
        // is resolved BEFORE the gate answers, mirroring the write gate
        // below: a read-denial is still an authorization decision about
        // a caller, so an anonymous request must fail loudly as the
        // harness bug it is, never be answered 403 -- answering it would
        // mask an app regression into fetching protected data without a
        // token.
        if (denyNotesRead) {
          return errorResponse(403, RBAC_PERMISSION_DENIED_CODE)
        }
        return jsonResponse(200, { notes: notesOf(principal.tenant_id) })
      }
      case 'POST /api/v1/notes': {
        const principal = principalOf(call)
        // The write gate: the global deny switch a suite scripts, and --
        // behind the reader option -- the reader's own grant asymmetry
        // (its list is served, its create refused, the way the Go
        // demo-users suite pins the read-only member).
        if (denyNotesWrite || (reader && principal.user_id === DEMO_READER_USER_ID)) {
          return errorResponse(403, RBAC_PERMISSION_DENIED_CODE)
        }
        // The real handler trims and validates: an empty trimmed text is
        // refused before anything is stored, so the client's required
        // rule cannot be the whole story (whitespace-only text passes it).
        const body = bodyObject(call)
        const raw = typeof body.text === 'string' ? body.text : ''
        const text = raw.trim()
        if (text === '') {
          return errorResponse(400, 'notes.text_required')
        }
        if (text.length > NOTE_TEXT_LIMIT) {
          return errorResponse(400, 'notes.text_too_long')
        }
        const note: NotesNote = {
          id: `note-${nextNoteId}`,
          text,
          created_at: DEMO_NOTE_CREATED_AT,
        }
        nextNoteId += 1
        notesOf(principal.tenant_id).push(note)
        return jsonResponse(201, note)
      }
      case 'GET /api/v1/cases': {
        // The cases surface has no read gate (any member of the tenant
        // may read the clinic's cases), so the list is served like any
        // member's; the deny switch scripts the load failure a surface
        // renders its error state for, and the bearer is resolved
        // before it answers, exactly like the notes gates.
        const principal = principalOf(call)
        if (denyCasesList) {
          return errorResponse(500, 'cases.internal_error')
        }
        return jsonResponse(200, { cases: casesOf(principal.tenant_id) })
      }
      case 'POST /api/v1/cases': {
        const principal = principalOf(call)
        if (casesCreateRefusal !== undefined) {
          return errorResponse(casesCreateRefusal.status, casesCreateRefusal.code)
        }
        // The real service trims and validates: an empty trimmed
        // patient name is refused before anything is stored, so the
        // client's required rule cannot be the whole story
        // (whitespace-only text passes it).
        const body = bodyObject(call)
        const raw = typeof body.patient_name === 'string' ? body.patient_name : ''
        const patientName = raw.trim()
        if (patientName === '') {
          return errorResponse(400, 'cases.patient_name_required')
        }
        const photoObjectIds =
          Array.isArray(body.photo_object_ids)
            ? body.photo_object_ids.filter(
                (entry): entry is string => typeof entry === 'string',
              )
            : []
        const created: CasesCase = {
          id: `case-${nextCaseId}`,
          patient_name: patientName,
          patient_ref: '',
          creator_user_id: principal.user_id,
          created_at: DEMO_CASE_CREATED_AT,
          photos: photoObjectIds.map((object_id) => ({ object_id })),
        }
        nextCaseId += 1
        casesOf(principal.tenant_id).push(created)
        return jsonResponse(201, created)
      }
      case 'POST /api/v1/cases/photos/upload': {
        // The bearer is resolved (and an anonymous upload fails loudly
        // as the harness bug it is) even though the answer carries no
        // principal: an upload is tenant-member work like every other
        // cases route.
        principalOf(call)
        if (casesUploadRefusal !== undefined) {
          return errorResponse(casesUploadRefusal.status, casesUploadRefusal.code)
        }
        const body = bodyObject(call)
        const contentBase64 =
          typeof body.content_base64 === 'string'
            ? body.content_base64.trim()
            : ''
        if (contentBase64 === '') {
          return errorResponse(400, 'cases.photo_content_required')
        }
        const objectId = `obj-${nextObjectId}`
        nextObjectId += 1
        return jsonResponse(201, { object_id: objectId })
      }
      case 'POST /api/v1/smile-simulation/simulate': {
        // The bearer is resolved (and an anonymous simulate fails loudly
        // as the harness bug it is) even though the answer carries no
        // principal: starting a simulation is tenant-member work like
        // every other smilesim route.
        principalOf(call)
        if (simulateRefusal !== undefined) {
          return errorResponse(simulateRefusal.status, simulateRefusal.code)
        }
        const body = bodyObject(call)
        const photoObjectID =
          typeof body.photo_object_id === 'string'
            ? body.photo_object_id.trim()
            : ''
        if (photoObjectID === '') {
          return errorResponse(400, 'smilesim.photo_object_id_required')
        }
        const jobID = `job-${nextSimulationJobId}`
        nextSimulationJobId += 1
        simulationJobs.set(jobID, {
          photo_object_id: photoObjectID,
          options: effectiveSimulationOptions(body),
          reads: 0,
          created_at: DEMO_CASE_CREATED_AT,
        })
        simulationJobOrder.push(jobID)
        return jsonResponse(202, { job_id: jobID })
      }
      case 'GET /api/v1/authn/sessions':
        return jsonResponse(200, { sessions })
      case 'GET /api/v1/authn/login-history':
        return jsonResponse(200, { attempts: loginAttempts })
      case 'GET /api/v1/authn/identities':
        return jsonResponse(200, { identities })
      case 'POST /api/v1/authn/sessions/revoke-others': {
        // Marks every non-current active row revoked, mirroring the
        // real handler's semantics; the answered count is what the
        // section's notice renders. Exact-keyed here so its path never
        // falls through to the session delete matcher below.
        let revokedCount = 0
        for (const candidate of sessions) {
          if (!candidate.is_current && candidate.status === 'active') {
            candidate.status = 'revoked'
            revokedCount += 1
          }
        }
        return jsonResponse(200, { revoked_count: revokedCount })
      }
      case 'POST /api/v1/authn/mfa/totp/enroll': {
        const principal = principalOf(call)
        // An active factor gates every enroll behind a fresh second-
        // factor proof: the 403 is the discover-by-acting signal the
        // account surface reads as "a factor exists, prove yourself
        // before replacing it".
        if (factorActive && !principal.elevated) {
          return errorResponse(403, 'authn.step_up_required')
        }
        return jsonResponse(200, {
          secret: DEMO_MFA_SECRET,
          provisioning_uri: DEMO_MFA_PROVISIONING_URI,
        })
      }
      case 'POST /api/v1/authn/mfa/step-up': {
        const principal = principalOf(call)
        const body = bodyObject(call)
        const code = typeof body.code === 'string' ? body.code : undefined
        if (code !== DEMO_MFA_CONFIRM_CODE) {
          return errorResponse(400, 'authn.mfa_invalid_code')
        }
        // A successful verification settles an access-only token whose
        // elevation stands for the fresh proof; it lives only in this
        // token's lifetime, exactly as the authn handler's contract
        // records (never persisted, never outliving the token).
        const verified = {
          user_id: principal.user_id,
          tenant_id: principal.tenant_id,
          session_id: principal.session_id,
        }
        return jsonResponse(200, {
          access_token: issueAccess(verified, true),
          principal: verified,
        })
      }
      case 'POST /api/v1/authn/mfa/totp/confirm': {
        const principal = principalOf(call)
        const body = bodyObject(call)
        const code = typeof body.code === 'string' ? body.code : undefined
        // An active factor answers an unelevated confirm with the
        // already-enrolled conflict: the pending setup is someone
        // else's now, and only an elevated caller (the replacement path
        // through the step-up) can confirm over it.
        if (factorActive && !principal.elevated) {
          return errorResponse(409, 'authn.mfa_already_enrolled')
        }
        if (code !== DEMO_MFA_CONFIRM_CODE) {
          return errorResponse(400, 'authn.mfa_invalid_code')
        }
        const answered = factorActive
          ? DEMO_MFA_REPLACEMENT_RECOVERY_CODES
          : DEMO_MFA_RECOVERY_CODES
        factorActive = true
        return jsonResponse(200, { recovery_codes: [...answered] })
      }
      case 'POST /api/v1/sharing/shares': {
        const principal = principalOf(call)
        // The create gate mirrors the rbac gate the real owner-facing
        // route sits behind (sharing.PathShares, sharingPermissionFor
        // selecting sharing:create for POST): the global refusal switch
        // a suite scripts, and -- behind the reader option -- the
        // reader's own grant asymmetry (its share create answers the
        // rbac gate's 403, exactly like its note create answers the
        // notes write gate's).
        if (sharesCreateRefusal !== undefined) {
          return errorResponse(
            sharesCreateRefusal.status,
            sharesCreateRefusal.code,
          )
        }
        if (
          sharesCreateSecondRefusal !== undefined &&
          shareCreateCount === 1
        ) {
          // The run's second create (its first is answered above by
          // the counter not matching yet): the half-refused pair.
          shareCreateCount += 1
          return errorResponse(
            sharesCreateSecondRefusal.status,
            sharesCreateSecondRefusal.code,
          )
        }
        shareCreateCount += 1
        if (reader && principal.user_id === DEMO_READER_USER_ID) {
          return errorResponse(403, RBAC_PERMISSION_DENIED_CODE)
        }
        const body = bodyObject(call)
        const resourceRef =
          typeof body.resourceRef === 'string' ? body.resourceRef.trim() : ''
        if (resourceRef === '') {
          return errorResponse(400, 'sharing.resource_ref_required')
        }
        // The minted share and its once-returned bearer token, echoing
        // the owner-facing route's 201 shape; the expiry mirrors the
        // real server's forced default (30 days from the demo epoch).
        const token = `share-token-${nextShareToken}`
        nextShareToken += 1
        const share = {
          id: `share-${nextShareId}`,
          resourceRef,
          expiresAt: DEMO_SHARE_EXPIRES_AT,
          viewCount: 0,
          passwordProtected: false,
          sensitive: false,
          createdAt: DEMO_CASE_CREATED_AT,
        }
        nextShareId += 1
        return jsonResponse(201, { share, token })
      }
      case 'GET /api/v1/sharing/access': {
        // The one genuinely public route: no bearer is resolved -- a
        // patient holds no session, and the real route answers from the
        // share token alone (go/sharing's Service.AccessPublic). The
        // refusal switch scripts the answers the patient page renders;
        // a granted answer is the shared simulation's raw bytes, the
        // same payload the simulation-content answers carry.
        if (shareAccessRefusal !== undefined) {
          return errorResponse(shareAccessRefusal.status, shareAccessRefusal.code)
        }
        return new Response(DEMO_SHARE_CONTENT_BYTES, {
          status: 200,
          headers: {
            'Content-Type': 'image/png',
            'Cache-Control': 'no-store',
          },
        })
      }
      default:
        break
    }
    // The parameterized paths cannot ride the exact-key switch; each
    // matcher guards its own method and shape, and an unpinned request
    // still fails loudly at the end.
    const shareRevokeMatch = SHARE_REVOKE_PATH.exec(call.path)
    if (call.method === 'POST' && shareRevokeMatch !== null) {
      // The compensation leg of the share action: an owner revoking a
      // share it just minted answers 200 with the revoked share, as the
      // real owner-facing route answers (the spec's revoke operation).
      principalOf(call)
      return jsonResponse(200, {})
    }
    const sessionPathMatch = SESSION_PATH.exec(call.path)
    if (call.method === 'DELETE' && sessionPathMatch !== null) {
      const session = sessions.find(
        (candidate) => candidate.id === sessionPathMatch[1],
      )
      if (session === undefined) {
        return errorResponse(404, 'authn.session_not_found')
      }
      session.status = 'revoked'
      return new Response(null, { status: 204 })
    }
    const identityPathMatch = IDENTITY_PATH.exec(call.path)
    if (call.method === 'DELETE' && identityPathMatch !== null) {
      const id = identityPathMatch[1]
      if (id === refuseUnbindIdentityId) {
        return errorResponse(409, 'authn.last_login_method')
      }
      const identityIndex = identities.findIndex(
        (candidate) => candidate.id === id,
      )
      if (identityIndex === -1) {
        return errorResponse(404, 'authn.identity_not_found')
      }
      identities.splice(identityIndex, 1)
      return new Response(null, { status: 204 })
    }
    const authorizePathMatch = SOCIAL_AUTHORIZE_PATH.exec(call.path)
    if (call.method === 'GET' && authorizePathMatch !== null) {
      // The add area's authorize request: the session operation a
      // channel click drives. The answer carries whatever authorize_url
      // the option scripts -- the host's protocol guard is the thing
      // under test when a journey answers a non-http(s) scheme here,
      // exactly the shape a misconfigured or compromised provider
      // answer would arrive in.
      principalOf(call)
      return jsonResponse(200, { authorize_url: socialAuthorizeUrl })
    }
    const casePathMatch = CASE_PATH.exec(call.path)
    if (call.method === 'GET' && casePathMatch !== null) {
      const principal = principalOf(call)
      const record = casesOf(principal.tenant_id).find(
        (candidate) => candidate.id === casePathMatch[1],
      )
      if (record === undefined) {
        return errorResponse(404, 'cases.not_found')
      }
      return jsonResponse(200, record)
    }
    const photoContentPathMatch = CASE_PHOTO_CONTENT_PATH.exec(call.path)
    if (call.method === 'GET' && photoContentPathMatch !== null) {
      const principal = principalOf(call)
      if (casesPhotoContentRefusal !== undefined) {
        return errorResponse(
          casesPhotoContentRefusal.status,
          casesPhotoContentRefusal.code,
        )
      }
      const record = casesOf(principal.tenant_id).find(
        (candidate) => candidate.id === photoContentPathMatch[1],
      )
      const attached =
        record?.photos.some(
          (photo) => photo.object_id === photoContentPathMatch[2],
        ) ?? false
      if (!attached) {
        return errorResponse(404, 'cases.photo_not_found')
      }
      return jsonResponse(200, casesPhotoContent)
    }
    const simulationJobMatch = SIMULATION_JOB_PATH.exec(call.path)
    if (call.method === 'GET' && simulationJobMatch !== null) {
      // The job-status poll, and the read that advances the job's
      // deterministic progression (see the ledger comment above): a
      // journey that polls observes pending -> running -> succeeded.
      const jobID = simulationJobMatch[1]
      if (jobID === undefined) {
        // Unreachable: the matcher above guarantees the group; kept as a
        // guard so the ledger lookups below can name a definite string.
        throw new Error(`demo-server: ${call.path} matched without a job id`)
      }
      const job = simulationJobs.get(jobID)
      if (job === undefined) {
        return errorResponse(404, 'jobs.job_not_found')
      }
      job.reads += 1
      const status = simulationStatusOf(jobID)
      const answer: Record<string, unknown> = {
        status,
        options: job.options,
      }
      if (status === 'succeeded') {
        answer.output_object_id = `sim-out-${jobID}`
      }
      return jsonResponse(200, answer)
    }
    const simulationListMatch = SIMULATION_LIST_PATH.exec(call.path)
    if (call.method === 'GET' && simulationListMatch !== null) {
      const principal = principalOf(call)
      const photoObjectID = simulationListMatch[1]
      if (photoObjectID === undefined) {
        // Unreachable, same shape as the job-status guard above.
        throw new Error(`demo-server: ${call.path} matched without a photo id`)
      }
      void principal
      const rows = [...simulationJobOrder]
        .reverse()
        .filter((jobID) => {
          const job = simulationJobs.get(jobID)
          return job?.photo_object_id === photoObjectID
        })
        .map((jobID) => {
          const job = simulationJobs.get(jobID)
          if (job === undefined) {
            throw new Error(`demo-server: ledger row ${jobID} missing`)
          }
          const status = simulationStatusOf(jobID)
          const row: Record<string, unknown> = {
            job_id: jobID,
            photo_object_id: job.photo_object_id,
            options: job.options,
            status,
            created_at: job.created_at,
          }
          if (status === 'succeeded') {
            row.output_object_id = `sim-out-${jobID}`
          }
          return row
        })
      return jsonResponse(200, { simulations: rows })
    }
    const simulationContentMatch = SIMULATION_CONTENT_PATH.exec(call.path)
    if (call.method === 'GET' && simulationContentMatch !== null) {
      principalOf(call)
      const photoObjectID = simulationContentMatch[1]
      const jobID = simulationContentMatch[2]
      if (photoObjectID === undefined || jobID === undefined) {
        // Unreachable, same shape as the job-status guard above.
        throw new Error(`demo-server: ${call.path} matched without its ids`)
      }
      const job = simulationJobs.get(jobID)
      if (job === undefined || job.photo_object_id !== photoObjectID) {
        return errorResponse(404, 'smilesim.simulation_not_found')
      }
      if (simulationStatusOf(jobID) !== 'succeeded') {
        return errorResponse(404, 'smilesim.output_not_ready')
      }
      if (simulationContentRefusal !== undefined) {
        return errorResponse(
          simulationContentRefusal.status,
          simulationContentRefusal.code,
        )
      }
      return jsonResponse(200, simulationContent)
    }
    const callbackPathMatch = SOCIAL_CALLBACK_PATH.exec(call.path)
    if (call.method === 'POST' && callbackPathMatch !== null) {
      // A binding-shaped answer (no tokens): the exchange bound the
      // account, and the bound identity joins the list the next
      // identities answer carries -- the refetch after the handler's
      // invalidation really shows the row. The identity is the owner's
      // (the account-domain story the header records).
      const identity: AuthnIdentity = {
        id: `social-${callbackPathMatch[1]}`,
        provider: callbackPathMatch[1],
        email: DEMO_OWNER_IDENTIFIER,
        created_at: DEMO_NOTE_CREATED_AT,
      }
      identities.push(identity)
      return jsonResponse(200, { bound: true, identity })
    }
    throw new Error(`demo-server: unexpected request: ${key}`)
  }
}
