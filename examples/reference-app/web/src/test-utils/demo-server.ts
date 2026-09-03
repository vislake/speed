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
 * body, which is why RealCall carries it) and POST /api/v1/authn/logout
 * (204). Anything else fails the test loudly: an unpinned request means
 * the journey under test reached an endpoint the demo does not serve.
 */

import type { PublicConfigResponse } from '@speed/api-client'
import { jsonResponse, makePair } from './real-client.js'
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

/** Builds the shared demo responder for the given options. */
export function demoServer(options: DemoServerOptions = {}): RealResponder {
  const {
    publicConfig = EMPTY_PUBLIC_CONFIG,
    tenantId = 'tenant-acme',
    userId = 'user-1',
    sessionId = 'session-1',
  } = options
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
          created_at: '2026-09-04T00:00:00Z',
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
      default:
        throw new Error(`demo-server: unexpected request: ${key}`)
    }
  }
}
