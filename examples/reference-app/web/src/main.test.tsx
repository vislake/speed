/**
 * main.test.tsx -- exercises evictTenantQueriesOnSessionEnd in
 * isolation, over a real AuthSession (createAuthSession, driven
 * through the real-client rig's own scripted responder) and a real
 * QueryClient, without mounting bootstrapReferenceApp's own DOM tree:
 * the wiring is pure session-and-cache plumbing, so nothing here needs
 * React, i18n, or the app's own views.
 *
 * The cross-account leak this pins is reference-app-web.md P1-1's
 * root cause -- nothing evicted a tenant's cached queries on
 * sign-out/session-death, only a tenant switch did -- and the
 * notes-view suite's own gate test covers the ternary-ordering half of
 * the same finding; the full end-to-end regression (a signed-out
 * account's cached notes never reaching a different, read-denied
 * account signing into the same tenant) lives in app-journey.test.tsx.
 */

import { QueryClient } from '@tanstack/react-query'
import { describe, expect, it } from 'vitest'
import { evictTenantQueriesOnSessionEnd } from './main.js'
import { jsonResponse, makeRealClientRig } from './test-utils/real-client.js'
import type { RealResponder } from './test-utils/real-client.js'

const TENANT_ID = 'tenant-acme'

/** A minimal responder: password login answers a token pair in
 * TENANT_ID, logout answers 204. Nothing else is reachable from this
 * suite's calls. */
const respond: RealResponder = (call) => {
  if (call.method === 'POST' && call.path === '/api/v1/authn/login/password') {
    return jsonResponse(200, {
      access_token: 'access-1',
      refresh_token: 'refresh-1',
      principal: {
        user_id: 'user-1',
        tenant_id: TENANT_ID,
        session_id: 'session-1',
      },
    })
  }
  if (call.method === 'POST' && call.path === '/api/v1/authn/logout') {
    return new Response(null, { status: 204 })
  }
  throw new Error(`unexpected request: ${call.method} ${call.path}`)
}

/** A note-shaped cache entry under the app's own tenant-namespaced
 * query-key convention (['tenant', tenantId, ...]); the exact suffix
 * does not matter to the eviction, which removes the whole prefix. */
function seedNotesCache(queryClient: QueryClient): void {
  queryClient.setQueryData(
    ['tenant', TENANT_ID, '/api/v1/notes', {}],
    { notes: [{ id: 'note-1', text: 'cached from the departing session' }] },
  )
}

describe('evictTenantQueriesOnSessionEnd', () => {
  it('evicts the tenant a sign-out leaves behind', async () => {
    const rig = makeRealClientRig(respond)
    const queryClient = new QueryClient()
    evictTenantQueriesOnSessionEnd(rig.session, queryClient)

    await rig.session.loginWithPassword({
      identifier: 'owner@example.test',
      password: 'correct-horse-battery-staple',
    })
    seedNotesCache(queryClient)
    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeDefined()

    await rig.session.logout()

    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeUndefined()
  })

  it('evicts the tenant a session death (a refused silent refresh) leaves behind', async () => {
    // refresh() never rejects for a refused token -- it resolves false
    // and signs the session out locally (session.ts's own contract),
    // the same authenticated -> anonymous transition a manual sign-out
    // produces, so the eviction must fire on this path too.
    const rig = makeRealClientRig((call) => {
      if (call.method === 'POST' && call.path === '/api/v1/authn/token/refresh') {
        return new Response(
          JSON.stringify({ code: 'authn.token_invalid', traceId: 'trace-1' }),
          { status: 401, headers: { 'content-type': 'application/json' } },
        )
      }
      return respond(call)
    })
    const queryClient = new QueryClient()
    evictTenantQueriesOnSessionEnd(rig.session, queryClient)

    await rig.session.loginWithPassword({
      identifier: 'owner@example.test',
      password: 'correct-horse-battery-staple',
    })
    seedNotesCache(queryClient)

    expect(await rig.session.refresh()).toBe(false)

    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeUndefined()
  })

  it('does nothing while the session was never authenticated', async () => {
    const rig = makeRealClientRig(respond)
    const queryClient = new QueryClient()
    evictTenantQueriesOnSessionEnd(rig.session, queryClient)
    seedNotesCache(queryClient)

    // A snapshot notification with no prior authenticated state (none
    // fires here since nothing logs in) must not evict anything --
    // asserted by the cache surviving to this point unexamined by any
    // transition at all.
    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeDefined()
  })

  it('leaves the cache alone across a still-authenticated transition (a tenant switch)', async () => {
    // A tenant switch keeps state === 'authenticated' throughout (only
    // the principal's tenant_id changes), so this eviction -- which
    // fires strictly on the authenticated -> anonymous edge -- must
    // not also fire here; user-menu.tsx's own tenant-switch eviction
    // owns that transition, and double-eviction is harmless but would
    // mask this function targeting the wrong edge if it fired.
    const rig = makeRealClientRig((call) => {
      if (call.method === 'POST' && call.path === '/api/v1/authn/tenant/switch') {
        return jsonResponse(200, {
          access_token: 'access-2',
          principal: {
            user_id: 'user-1',
            tenant_id: 'tenant-globex',
            session_id: 'session-1',
          },
        })
      }
      return respond(call)
    })
    const queryClient = new QueryClient()
    evictTenantQueriesOnSessionEnd(rig.session, queryClient)

    await rig.session.loginWithPassword({
      identifier: 'owner@example.test',
      password: 'correct-horse-battery-staple',
    })
    seedNotesCache(queryClient)

    await rig.session.switchTenant('tenant-globex')

    expect(
      queryClient.getQueryData(['tenant', TENANT_ID, '/api/v1/notes', {}]),
    ).toBeDefined()
  })
})
