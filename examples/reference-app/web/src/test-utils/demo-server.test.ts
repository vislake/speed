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
 *
 * The second describe pins the billing ledger the same responder
 * serves (creditLedgerState): the balance movements of an accepted
 * simulation job's reservation across its terminal outcomes, driven
 * through the same real-call journey -- sign-in, simulate, the
 * deterministic job-status polls, the billing reads under the issued
 * bearer.
 */

import { describe, expect, it } from 'vitest'
import type { RealCall } from './real-client.js'
import {
  DEMO_CREDIT_SEED_GRANT,
  DEMO_SIMULATION_CREDIT_COST,
  demoServer,
} from './demo-server.js'

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

describe('demo-server credit ledger', () => {
  /** Signs in as the owner account and returns the bearer the
   * responder's own login answer issued (the same journey shape the
   * bearer suites above use). */
  async function signInBearer(
    respond: ReturnType<typeof demoServer>,
  ): Promise<string> {
    const login = await respond({
      method: 'POST',
      path: '/api/v1/authn/login/password',
      query: '',
      authorization: null,
      body: JSON.stringify({ identifier: 'owner@example.test' }),
    })
    const pair = (await login.json()) as { readonly access_token: string }
    return `Bearer ${pair.access_token}`
  }

  /** Accepts one simulation under the bearer, returning its job id. */
  async function acceptSimulation(
    respond: ReturnType<typeof demoServer>,
    bearer: string,
  ): Promise<string> {
    const simulate = await respond({
      method: 'POST',
      path: '/api/v1/smile-simulation/simulate',
      query: '',
      authorization: bearer,
      body: JSON.stringify({
        photo_object_id: 'photo-1',
        options: {
          smile_style: 'natural',
          tooth_shade: 'natural',
          strength: 1,
        },
      }),
    })
    const answer = (await simulate.json()) as { readonly job_id: string }
    return answer.job_id
  }

  /** Advances one job by one status read: the responder's deterministic
   * progression runs pending -> running on the first read and reaches
   * its terminal outcome (succeeded, or dead_letter under the
   * simulateJobOutcome option) on the second. */
  async function pollJob(
    respond: ReturnType<typeof demoServer>,
    bearer: string,
    jobId: string,
  ): Promise<string> {
    const poll = await respond({
      method: 'GET',
      path: `/api/v1/smile-simulation/jobs/${jobId}`,
      query: '',
      authorization: bearer,
      body: '',
    })
    const answer = (await poll.json()) as { readonly status: string }
    return answer.status
  }

  /** The balance read's own answer, narrowed to the two numbers the
   * billing fragment names (the answer also carries updatedAt, which
   * the ledger regressions do not assert). */
  async function readBalance(
    respond: ReturnType<typeof demoServer>,
    bearer: string,
  ): Promise<{ available: number; reserved: number }> {
    const balance = await respond({
      method: 'GET',
      path: '/api/v1/billing/credits/balance',
      query: '',
      authorization: bearer,
      body: '',
    })
    const body = (await balance.json()) as {
      available: number
      reserved: number
    }
    return { available: body.available, reserved: body.reserved }
  }

  it("holds a dead_letter job's reservation out of available and releases it back at the refund (regression a)", async () => {
    // The full failed-generation journey against one responder: the
    // balance must move the way the real server's two-phase credit
    // lifecycle moves it (go/billing's PreDeduct takes the reservation
    // out of Available into Reserved when the job opens, and Refund
    // puts it back when the job dies -- smilesim's settleCredit runs
    // the refund for a dead_letter). A mirror whose available never
    // leaves the seed while a reservation is live states the same ten
    // credits twice, and its refunded row releases nothing a balance
    // read could observe -- the shape that left the refund gate with no
    // charge-taken to verify the return of.
    const respond = demoServer({ simulateJobOutcome: 'dead_letter' })
    const bearer = await signInBearer(respond)

    // A freshly booted tenant: available at the boot-time grant,
    // nothing reserved.
    expect(await readBalance(respond, bearer)).toEqual({
      available: DEMO_CREDIT_SEED_GRANT,
      reserved: 0,
    })

    // An accepted job is a live reservation: its amount sits in the
    // reserved bucket and out of available.
    const jobId = await acceptSimulation(respond, bearer)
    expect(await readBalance(respond, bearer)).toEqual({
      available: DEMO_CREDIT_SEED_GRANT - DEMO_SIMULATION_CREDIT_COST,
      reserved: DEMO_SIMULATION_CREDIT_COST,
    })

    // The job dies: the refund releases the reservation back to
    // available -- the balance is back at the full seed with nothing
    // reserved, the answer the real server's Refund leaves for a
    // generation that failed after its reservation opened.
    await pollJob(respond, bearer, jobId)
    expect(await pollJob(respond, bearer, jobId)).toBe('dead_letter')
    expect(await readBalance(respond, bearer)).toEqual({
      available: DEMO_CREDIT_SEED_GRANT,
      reserved: 0,
    })
  })

  it('reads a succeeded job as a permanent spend, confirmed behaviour unchanged (regression b)', async () => {
    // A succeeded generation's reservation became the permanent spend:
    // available reads the seed minus its cost, nothing reserved -- the
    // consumption every journey that scripts a succeeded row relies on.
    const respond = demoServer()
    const bearer = await signInBearer(respond)
    const jobId = await acceptSimulation(respond, bearer)
    await pollJob(respond, bearer, jobId)
    expect(await pollJob(respond, bearer, jobId)).toBe('succeeded')
    expect(await readBalance(respond, bearer)).toEqual({
      available: DEMO_CREDIT_SEED_GRANT - DEMO_SIMULATION_CREDIT_COST,
      reserved: 0,
    })
  })

  it('reads a still-running job as a live reservation (regression c)', async () => {
    // A generation that has not reached its terminal outcome keeps its
    // reservation open: the reserved bucket carries its amount while
    // the job runs, and available is that amount lighter -- never a
    // balance that states the reserved credits twice.
    const respond = demoServer()
    const bearer = await signInBearer(respond)
    const jobId = await acceptSimulation(respond, bearer)
    expect(await pollJob(respond, bearer, jobId)).toBe('running')
    expect(await readBalance(respond, bearer)).toEqual({
      available: DEMO_CREDIT_SEED_GRANT - DEMO_SIMULATION_CREDIT_COST,
      reserved: DEMO_SIMULATION_CREDIT_COST,
    })
  })
})

describe('demo-server preferences pair', () => {
  const PREFERENCES_PATH = '/api/v1/authn/me/preferences'

  /** One preferences call as the fetch stand-in records it. */
  function preferencesCall(method: string, body: string = ''): RealCall {
    return {
      method,
      path: PREFERENCES_PATH,
      query: '',
      authorization: 'Bearer access-1',
      body,
    }
  }

  it('serves the not-chosen state and merges PATCHes statefully, clearing on the empty string', async () => {
    const respond = demoServer()

    // The fresh account: both fields absent (the wire spelling of
    // "not chosen", mirroring the real handler's str() projection).
    const initial = await respond(preferencesCall('GET'))
    expect(initial.status).toBe(200)
    expect(await initial.json()).toEqual({})

    // A partial PATCH replaces its field alone and answers the pair as
    // stored.
    const withLocale = await respond(
      preferencesCall('PATCH', JSON.stringify({ locale: 'en-US' })),
    )
    expect(await withLocale.json()).toEqual({ locale: 'en-US' })

    const withZone = await respond(
      preferencesCall('PATCH', JSON.stringify({ timezone: 'Asia/Tokyo' })),
    )
    expect(await withZone.json()).toEqual({
      locale: 'en-US',
      timezone: 'Asia/Tokyo',
    })

    // Stateful across calls: a later GET serves what the PATCHes stored.
    const readBack = await respond(preferencesCall('GET'))
    expect(await readBack.json()).toEqual({
      locale: 'en-US',
      timezone: 'Asia/Tokyo',
    })

    // The empty string clears its field, the partial-update contract's
    // clearing value.
    const cleared = await respond(
      preferencesCall('PATCH', JSON.stringify({ locale: '' })),
    )
    expect(await cleared.json()).toEqual({ timezone: 'Asia/Tokyo' })
  })
})
