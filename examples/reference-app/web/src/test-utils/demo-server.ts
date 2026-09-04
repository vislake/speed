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
 * tenant-globex the real server refuses (authn.tenant_membership_
 * required) -- a Public config carrying brand.site_name plus the
 * dependency-resolved feature list for the request's tenant, and an
 * authn surface that issues tokens on login and tenant switch.
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
 * reader's list as served, demo_users_test.go:135-153), and a note
 * create from that principal answers the 403 the write gate gives a
 * caller without notes:write (rbac.permission_denied, asserted at
 * server_test.go:335). An account registration answers 201 and records
 * the identifier, and a later sign-in of a recorded identifier answers
 * the membership refusal of a registered-but-unseeded account -- 403
 * authn.tenant_membership_required, in the browser's own shape: the
 * sign-in body names no tenant_id (the login form has no tenant
 * field), the shape the composed stack pins at demo_users_test.go:297
 * -- an account the register route created, granted nowhere, drawing
 * the no-membership-anywhere form of the code; :162 and :233 are its
 * named-tenant siblings. Registering twice
 * answers the same 409 authn.email_already_registered a real handler
 * answers (go/authn/errors.go's ErrEmailAlreadyRegistered).
 *
 * The endpoints: GET /api/config/public (pre-auth, the config fetch
 * usePublicConfig drives), POST /api/v1/authn/register (201, the
 * created AuthnUser), POST /api/v1/authn/login/password (200, a token
 * pair whose principal lands in the configured default tenant),
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
 * shows it). The notes answers mirror the real handler's refusals: a
 * create whose trimmed text is empty answers 400 notes.text_required
 * (internal/notes/handler.go), one over the 4000-character limit
 * answers 400 notes.text_too_long, and the deny switches below answer
 * the read/write permission refusals the rbac gate over the notes
 * module produces (403 rbac.permission_denied) -- the journeys gate and
 * error surfaces are driven by genuine refusals, never stubbed locally.
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
 * read-only member (its list served, demo_users_test.go:135-153, its
 * create refused, server_test.go:335). The read-denied refusal of a
 * caller without notes:read is the denyNotesRead switch's answer, a
 * shape no seeded account carries.
 *
 * Anything else fails the test loudly: an unpinned request means the
 * journey under test reached an endpoint the demo does not serve.
 */

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

/** The code a sign-in of a registered-but-unseeded account answers with
 * (go/authn/errors.go's ErrTenantMembershipRequired; the browser-shaped
 * refusal -- a sign-in body with no tenant_id -- the composed stack
 * pins at demo_users_test.go:297, whose named-tenant siblings sit at
 * :162 and :233). */
export const MEMBERSHIP_REQUIRED_CODE = 'authn.tenant_membership_required'

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
}

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

/** The permission code the notes module's rbac gate answers a caller
 * without the requested grant with (ErrPermissionDenied). */
const NOTES_DENIED_CODE = 'rbac.permission_denied'

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
const IDENTITY_PATH = /^\/api\/v1\/authn\/identities\/([^/]+)$/
const SOCIAL_CALLBACK_PATH = /^\/api\/v1\/authn\/social\/([^/]+)\/callback$/

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
  // The accounts a register answered. A later sign-in of a recorded
  // identifier answers the membership refusal of a registered-but-
  // unseeded account (registration alone grants no membership).
  const registeredEmails = new Set<string>()
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
      case 'POST /api/v1/authn/login/password': {
        const body = bodyObject(call)
        const identifier =
          typeof body.identifier === 'string' ? body.identifier : undefined
        // A registered account has no seeded membership: its sign-in
        // answers the refusal the Go suite pins in the browser's own
        // shape -- no tenant_id in the body -- at demo_users_test.go:297
        // (an account the register route created, granted nowhere); the
        // named-tenant form of the same refusal sits at :162 and :233.
        if (identifier !== undefined && registeredEmails.has(identifier)) {
          return errorResponse(403, MEMBERSHIP_REQUIRED_CODE)
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
          // Registration records the account; a second register of the
          // same email answers the real handler's uniqueness refusal.
          if (registeredEmails.has(email)) {
            return errorResponse(409, 'authn.email_already_registered')
          }
          registeredEmails.add(email)
        }
        return jsonResponse(201, {
          id: 'user-9',
          ...(email !== undefined ? { email } : {}),
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
        if (denyNotesRead) {
          return errorResponse(403, NOTES_DENIED_CODE)
        }
        return jsonResponse(200, { notes: notesOf(principalOf(call).tenant_id) })
      }
      case 'POST /api/v1/notes': {
        const principal = principalOf(call)
        // The write gate: the global deny switch a suite scripts, and --
        // behind the reader option -- the reader's own grant asymmetry
        // (its list is served, its create refused, the way the Go
        // demo-users suite pins the read-only member).
        if (denyNotesWrite || (reader && principal.user_id === DEMO_READER_USER_ID)) {
          return errorResponse(403, NOTES_DENIED_CODE)
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
      default:
        break
    }
    // The parameterized paths cannot ride the exact-key switch; each
    // matcher guards its own method and shape, and an unpinned request
    // still fails loudly at the end.
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
