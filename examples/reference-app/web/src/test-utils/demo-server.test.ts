/**
 * The demo-server bearer contract: a principal-requiring endpoint
 * (notes read/write, tenant switch, the MFA surface) answers only a
 * token this responder instance itself issued -- every token a journey
 * can hold was issued by its own sign-in answer, so an absent or
 * unknown bearer is a harness bug and fails loudly, never answered as
 * a default principal (which would mask an app regression into
 * fetching protected data without a token).
 *
 * The suite pins both halves: the served journey shape (a sign-in
 * through the server's own login answer, then a protected read under
 * the issued token) and the guard (each refused bearer names what was
 * wrong). Calling the responder throws synchronously, so each refused
 * case goes through a promise-wrapping helper -- the shape a fetch
 * stand-in would surface either way.
 */

import { describe, expect, it } from 'vitest'
import type { RealCall } from './real-client.js'
import { demoServer } from './demo-server.js'

/** The protected read every signed-in notes journey starts with. */
const NOTE_LIST_CALL: RealCall = {
  method: 'GET',
  path: '/api/v1/notes',
  query: '',
  authorization: 'Bearer access-1',
  body: '',
}

describe('demo-server bearer principal', () => {
  it('serves a protected read under a token its own login answer issued', async () => {
    const respond = demoServer()
    const login = await respond({
      method: 'POST',
      path: '/api/v1/authn/login/password',
      query: '',
      authorization: null,
      body: JSON.stringify({ identifier: 'owner@example.test' }),
    })
    expect(login.status).toBe(200)
    const pair = (await login.json()) as {
      readonly access_token: string
    }
    const list = await respond({
      ...NOTE_LIST_CALL,
      authorization: `Bearer ${pair.access_token}`,
    })
    expect(list.status).toBe(200)
    const body = (await list.json()) as { readonly notes: unknown }
    expect(Array.isArray(body.notes)).toBe(true)
  })

  it('fails loudly on a principal-requiring endpoint reached with no bearer token', async () => {
    const respond = demoServer()
    const answering = Promise.resolve().then(() =>
      respond({ ...NOTE_LIST_CALL, authorization: null }),
    )
    await expect(answering).rejects.toThrow(/no bearer token/)
  })

  it('fails loudly on a principal-requiring endpoint reached with an unknown bearer token', async () => {
    const respond = demoServer()
    const answering = Promise.resolve().then(() =>
      respond({
        method: 'POST',
        path: '/api/v1/authn/tenant/switch',
        query: '',
        authorization: 'Bearer access-99',
        body: JSON.stringify({ tenant_id: 'tenant-globex' }),
      }),
    )
    await expect(answering).rejects.toThrow(/unknown bearer token/)
  })

  it('fails loudly on a read-denied notes list reached with no bearer token (the read gate never answers an anonymous request)', async () => {
    // The denyNotesRead switch scripts the rbac read gate's 403 -- but a
    // gate answer is still an authorization decision about a caller, so
    // it must never short-circuit the bearer check: an anonymous request
    // to a protected endpoint is a harness bug whether the gate would
    // have denied or served the caller, and answering it 403 would mask
    // an app regression into fetching protected data without a token
    // (the shape this test pins regressed once: the read-denial branch
    // returned before principalOf, silently answering anonymous reads).
    const respond = demoServer({ denyNotesRead: true })
    const answering = Promise.resolve().then(() =>
      respond({ ...NOTE_LIST_CALL, authorization: null }),
    )
    await expect(answering).rejects.toThrow(/no bearer token/)
  })

  it('answers the read-denied refusal to a bearer its own login issued', async () => {
    const respond = demoServer({ denyNotesRead: true })
    const login = await respond({
      method: 'POST',
      path: '/api/v1/authn/login/password',
      query: '',
      authorization: null,
      body: JSON.stringify({ identifier: 'owner@example.test' }),
    })
    const pair = (await login.json()) as {
      readonly access_token: string
    }
    const list = await respond({
      ...NOTE_LIST_CALL,
      authorization: `Bearer ${pair.access_token}`,
    })
    expect(list.status).toBe(403)
    const body = (await list.json()) as { readonly code: string }
    expect(body.code).toBe('rbac.permission_denied')
  })
})
