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
})
