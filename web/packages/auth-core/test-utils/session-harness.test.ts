/**
 * session-harness.test.ts -- the own-property dispatch-gate regression
 * for the harness in session-harness.ts (the gate landed in commit
 * dddd294d).
 *
 * The harness reads its handler off the caller-supplied script at the
 * key it derives from the request (`${method} ${path}`), and that read
 * is own-property-gated: Object.hasOwn decides between the scripted
 * handler and the missing-handler error an unknown key produces. The
 * rest of the package's suites only ever script legitimate keys, so
 * they pin the gate's own-property branch while nothing drives its
 * other branch -- a key that exists only on the script's prototype
 * chain must take the missing-handler path, never dispatch onto an
 * inherited callable. This file drives exactly that branch: the
 * request's method is 'constructor', the best-known prototype-chain
 * name, and the script is built over a prototype that carries a
 * callable under the precise key the request derives. On the
 * pre-gate code the bare script[key] read would have resolved that
 * callable through the prototype and dispatched it, so the rejection
 * asserted here is the gate's own observable behaviour.
 *
 * Lives next to the harness it tests. Each package that drives
 * sessions carries its own copy of the harness by the convention
 * session-harness.ts records, so this regression is duplicated with
 * its copy and the files stay in lockstep the same way.
 */

import { describe, expect, it } from 'vitest'
import type { HttpMethod } from '@speed/api-client'
import { speedRequest } from '@speed/api-sdk/runtime'
import { makeHarness } from './session-harness'

describe('session-harness script dispatch', () => {
  it('refuses a request whose method names a prototype-chain function', async () => {
    // The harness looks its handler up at `${method} ${path}`, so the
    // request below derives the key
    // 'constructor /api/v1/authn/login/password'. A script built over
    // a prototype that carries a callable under exactly that derived
    // key must not be dispatched onto: the request answers with the
    // same missing-handler error an unknown key produces, and the
    // inherited callable never runs. The method value lies outside the
    // spec's HttpMethod vocabulary by design -- a prototype-chain
    // method name is the adversarial input the gate exists for.
    const inheritedRuns: string[] = []
    makeHarness(
      Object.create({
        'constructor /api/v1/authn/login/password': () => {
          inheritedRuns.push('dispatched')
          return undefined
        },
      }),
    )
    let error: unknown
    try {
      await speedRequest({
        url: '/api/v1/authn/login/password',
        method: 'constructor' as HttpMethod,
      })
    } catch (caught) {
      error = caught
    }
    expect(inheritedRuns).toEqual([])
    expect(error).toBeInstanceOf(Error)
    expect((error as Error).message).toBe(
      'no scripted handler for constructor /api/v1/authn/login/password',
    )
  })
})
