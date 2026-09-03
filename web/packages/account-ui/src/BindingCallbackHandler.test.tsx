/**
 * BindingCallbackHandler behaviour, driven over the real-client rig: the
 * handler posts the (code, state) pair it was given to the callback
 * endpoint of the provider named in its props -- a plain generated call
 * (the binding adds an identity to the caller's account, it signs nobody
 * in) -- exactly once per pair, guarded against StrictMode's double
 * effect invocation. The suite pins the shape dispatch: a binding-shaped
 * answer (no tokens) invalidates the identities list -- the probe
 * observes its refetch -- and fires onBound once; a login-shaped answer
 * (tokens present, the exchange turned into a sign-in) renders the
 * dedicated signed-elsewhere panel and fires nothing; a failed exchange
 * renders its code text under a retry that re-runs the same pair until
 * it lands. Every scenario ends with an axe pass.
 */

import { StrictMode } from 'react'
import { describe, expect, it } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useAuthnListIdentities } from '@speed/api-sdk'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import {
  errorResponse,
  jsonResponse,
  makeRealClientRig,
} from '../test-utils/real-client.js'
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import { BindingCallbackHandler } from './BindingCallbackHandler.js'

const CALLBACK_PATH = '/api/v1/authn/social/google/callback'
const IDENTITIES_PATH = '/api/v1/authn/identities'

/** Renders the identities list length so a test can observe the
 * handler's invalidate (a refetch lands as one more GET on the rig).
 * -1 while the list has not loaded. */
function IdentitiesProbe() {
  const { data } = useAuthnListIdentities()
  return <div>{data?.identities ? data.identities.length : -1}</div>
}

describe('BindingCallbackHandler', () => {
  it('exchange the pair exactly once under StrictMode and fire onBound once on a binding-shaped answer', async () => {
    // The exchange is gated so the probe's initial identities fetch
    // settles first: an invalidate that lands on an in-flight identical
    // fetch merges into it (react-query dedupe), which would make the
    // refetch unobservable.
    let releaseExchange: () => void = () => undefined
    const exchangeGate = new Promise<void>((resolve) => {
      releaseExchange = resolve
    })
    let callbackCount = 0
    let identitiesCount = 0
    makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === CALLBACK_PATH) {
        callbackCount += 1
        await exchangeGate
        return jsonResponse(200, {
          bound: true,
          identity: { id: 'google-1', provider: 'google' },
        })
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        identitiesCount += 1
        return jsonResponse(200, { identities: [{ id: 'google-1' }] })
      }
      return errorResponse(500, 'internal')
    })
    let boundCount = 0
    renderWithProviders(
      <StrictMode>
        <BindingCallbackHandler
          provider="google"
          code="code-1"
          state="state-1"
          onBound={() => {
            boundCount += 1
          }}
        />
        <IdentitiesProbe />
      </StrictMode>,
    )

    // StrictMode's double effect invocation starts one exchange.
    await waitFor(() => expect(callbackCount).toBe(1))
    // The probe's list settled; the exchange still sits on the gate.
    expect(await screen.findByText('1')).toBeTruthy()
    const loadsBeforeInvalidate = identitiesCount
    releaseExchange()
    await waitFor(() =>
      expect(identitiesCount).toBe(loadsBeforeInvalidate + 1),
    )
    expect(boundCount).toBe(1)
    // The pending notice stays up for the host to react.
    expect(screen.getByRole('status')).toBeTruthy()
    expect(screen.queryByRole('alert')).toBeNull()

    await expectNoAxeViolations()
  })

  it('fire onBound once and let the identities list refetch on a binding-shaped answer', async () => {
    let releaseExchange: () => void = () => undefined
    const exchangeGate = new Promise<void>((resolve) => {
      releaseExchange = resolve
    })
    let identitiesCount = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === CALLBACK_PATH) {
        await exchangeGate
        return jsonResponse(200, {
          identity: { id: 'google-1', provider: 'google' },
        })
      }
      if (call.method === 'GET' && call.path === IDENTITIES_PATH) {
        identitiesCount += 1
        return jsonResponse(200, { identities: [{ id: 'google-1' }] })
      }
      return errorResponse(500, 'internal')
    })
    let boundCount = 0
    renderWithProviders(
      <>
        <BindingCallbackHandler
          provider="google"
          code="code-1"
          state="state-1"
          onBound={() => {
            boundCount += 1
          }}
        />
        <IdentitiesProbe />
      </>,
    )

    expect(await screen.findByText('1')).toBeTruthy()
    const loadsBeforeInvalidate = identitiesCount
    releaseExchange()
    await waitFor(() =>
      expect(identitiesCount).toBe(loadsBeforeInvalidate + 1),
    )
    expect(boundCount).toBe(1)
    // One exchange, no retry surface.
    expect(
      rig.calls.filter((call) => call.method === 'POST' && call.path === CALLBACK_PATH),
    ).toHaveLength(1)
    expect(screen.queryByRole('button')).toBeNull()

    await expectNoAxeViolations()
  })

  it('render the signed-elsewhere panel for a login-shaped answer and fire nothing', async () => {
    makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === CALLBACK_PATH) {
        return jsonResponse(200, {
          user: { id: 'other-user', email: 'other@example.test' },
          tokens: {
            access_token: 'access-other',
            refresh_token: 'refresh-other',
            principal: {
              user_id: 'other-user',
              tenant_id: 'tenant-1',
              session_id: 'session-other',
            },
          },
          created: false,
        })
      }
      return errorResponse(500, 'internal')
    })
    let boundCount = 0
    renderWithProviders(
      <BindingCallbackHandler
        provider="google"
        code="code-1"
        state="state-1"
        onBound={() => {
          boundCount += 1
        }}
      />,
    )

    expect(
      await screen.findByText(zhCN.bindingCallback.signedInElsewhere.title),
    ).toBeTruthy()
    expect(
      screen.getByText(zhCN.bindingCallback.signedInElsewhere.description),
    ).toBeTruthy()
    expect(boundCount).toBe(0)
    // No retry: the code is consumed, the panel is terminal.
    expect(screen.queryByRole('button')).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()

    await expectNoAxeViolations()
  })

  it('render the code text of a failed exchange and retry the same pair', async () => {
    let attempts = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === CALLBACK_PATH) {
        attempts += 1
        if (attempts === 1) {
          // The external identity's verified email already belongs to
          // another account whose auto-link conditions were not met.
          return errorResponse(409, 'authn.identity_requires_binding')
        }
        return jsonResponse(200, { bound: true })
      }
      return errorResponse(500, 'internal')
    })
    let boundCount = 0
    renderWithProviders(
      <BindingCallbackHandler
        provider="google"
        code="code-1"
        state="state-1"
        onBound={() => {
          boundCount += 1
        }}
      />,
    )

    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toBe(zhCN.errors.authn.identity_requires_binding)
    expect(boundCount).toBe(0)

    await userEvent.click(
      screen.getByRole('button', { name: zhCN.bindingCallback.retry }),
    )
    await waitFor(() => expect(boundCount).toBe(1))
    expect(attempts).toBe(2)
    // The same pair went out both times.
    expect(
      rig.calls.filter((call) => call.method === 'POST' && call.path === CALLBACK_PATH),
    ).toHaveLength(2)
    expect(screen.queryByRole('alert')).toBeNull()

    await expectNoAxeViolations()
  })
})
