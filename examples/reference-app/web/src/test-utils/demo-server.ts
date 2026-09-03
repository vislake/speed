/**
 * demo-server.ts -- the scripted demo backend the app's surface
 * journeys share: one responder that answers the endpoints a real
 * reference-app server answers, with the demo's own facts baked in as
 * defaults and every answer overridable per suite.
 *
 * The demo facts this script mirrors (the server seeds them, never an
 * app choice): two tenants whose ids are tenant-acme and tenant-globex
 * (their display names are host copy -- no roster endpoint exists, so
 * names live in the app namespace), a membership store granting every
 * demo user membership in both, a Public config carrying brand.site_name
 * plus the dependency-resolved feature list for the request's tenant,
 * and an authn surface that issues token pairs on login and tenant
 * switch. The endpoints: GET /api/config/public (pre-auth, the config
 * fetch usePublicConfig drives), POST /api/v1/authn/register (201, the
 * created AuthnUser), POST /api/v1/authn/login/password (200, a token
 * pair whose principal lands in the configured default tenant),
 * POST /api/v1/authn/tenant/switch (200, a pair whose principal is in
 * the tenant the body asks for -- the one answer that reads the request
 * body, which is why RealCall carries it), POST /api/v1/authn/logout
 * (204), the account surface's endpoints -- GET
 * /api/v1/authn/sessions (200, the current session list),
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
 * carries) -- plus GET /api/v1/notes (200, the current note list) and
 * POST /api/v1/notes (201, the created note, appended to the list so a
 * refetch after a create really shows it). The notes answers mirror the
 * real handler's refusals: a create whose trimmed text is empty answers
 * 400 notes.text_required, one over the 4000-character limit answers
 * 400 notes.text_too_long, and the deny switches below answer the
 * read/write permission refusals the rbac gate over the notes module
 * produces (403 rbac.permission_denied) -- the journeys gate and error
 * surfaces are driven by genuine refusals, never stubbed locally.
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
import { errorResponse, jsonResponse, makePair } from './real-client.js'
import type { RealCall, RealResponder } from './real-client.js'

/** The empty Public answer: no brand, no enabled features. */
const EMPTY_PUBLIC_CONFIG: PublicConfigResponse = {
  config: {},
  features: [],
}

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
  /** The GET /api/v1/notes list as first served. Stateful from there:
   * a create appends the note the next list answer carries. Default
   * [] -- the real handler's list is never a null answer. */
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

/** A token-issuing answer (200) for a principal in the given tenant. */
function pairAnswer(options: DemoServerOptions, tenantId: string): Response {
  const { userId = 'user-1', sessionId = 'session-1' } = options
  return jsonResponse(
    200,
    makePair({
      principal: { user_id: userId, tenant_id: tenantId, session_id: sessionId },
    }),
  )
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
  // The notes list is stateful per responder instance (a create appends
  // the note later list answers carry) -- each test's rig gets its own
  // server, so the state never leaks across journeys.
  const notes: NotesNote[] = [...initialNotes]
  let nextNoteId = 1
  return (call) => {
    const key = `${call.method} ${call.path}`
    switch (key) {
      case 'GET /api/config/public':
        return jsonResponse(200, publicConfig)
      case 'POST /api/v1/authn/login/password':
        return pairAnswer({ userId, sessionId }, tenantId)
      case 'POST /api/v1/authn/register': {
        const body = bodyObject(call)
        const email = typeof body.email === 'string' ? body.email : undefined
        return jsonResponse(201, {
          id: 'user-9',
          ...(email !== undefined ? { email } : {}),
          created_at: DEMO_NOTE_CREATED_AT,
        })
      }
      case 'POST /api/v1/authn/tenant/switch': {
        const body = bodyObject(call)
        const requested =
          typeof body.tenant_id === 'string' ? body.tenant_id : tenantId
        return pairAnswer({ userId, sessionId }, requested)
      }
      case 'POST /api/v1/authn/logout':
        return new Response(null, { status: 204 })
      case 'GET /api/v1/notes':
        if (denyNotesRead) {
          return errorResponse(403, NOTES_DENIED_CODE)
        }
        return jsonResponse(200, { notes })
      case 'POST /api/v1/notes': {
        if (denyNotesWrite) {
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
        notes.push(note)
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
      // invalidation really shows the row.
      const identity: AuthnIdentity = {
        id: `social-${callbackPathMatch[1]}`,
        provider: callbackPathMatch[1],
        email: 'owner@example.test',
        created_at: DEMO_NOTE_CREATED_AT,
      }
      identities.push(identity)
      return jsonResponse(200, { bound: true, identity })
    }
    throw new Error(`demo-server: unexpected request: ${key}`)
  }
}
