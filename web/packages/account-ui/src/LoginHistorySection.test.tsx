/**
 * LoginHistorySection behaviour, driven over the real-client rig: a real
 * @speed/api-client answers genuine Responses from the responder, and
 * assertions read what the user sees.
 *
 * The suite pins the surface contract: the one GET carries the frozen
 * page size (limit=20, no pagination machinery); known methods render
 * their bilingual labels while a method outside the known set -- and a
 * failure_reason token outside the known set, or an absent one --
 * render their generic labels, never a raw value and never the API's
 * message field; success renders its own label, every failure maps to
 * its reason's text; times render through Intl in the surface language,
 * never hand-formatted; loading, empty and error states render their
 * own placeholder, the error state with a retry that refetches. Every
 * scenario ends with an axe pass.
 */

import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AuthnLoginAttempt } from '@speed/api-sdk'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import {
  errorResponse,
  jsonResponse,
  makePair,
  makeRealClientRig,
  signInWithPassword,
} from '../test-utils/real-client.js'
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import { LoginHistorySection } from './LoginHistorySection.js'

const LOGIN_PATH = '/api/v1/authn/login/password'
const HISTORY_PATH = '/api/v1/authn/login-history'

const T1 = '2026-07-30T08:30:00.000Z'
const T2 = '2026-08-01T02:05:00.000Z'
const T3 = '2026-08-02T14:20:00.000Z'

function attempt(overrides: Partial<AuthnLoginAttempt> = {}): AuthnLoginAttempt {
  return {
    id: 'attempt-1',
    method: 'password',
    result: 'success',
    ip: '203.0.113.10',
    created_at: T1,
    ...overrides,
  }
}

/** The formatted-time string the component renders, computed the same
 * way the component computes it (Intl in the test language). */
function plainTime(iso: string): string {
  return new Intl.DateTimeFormat('zh-CN', {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(iso))
}

describe('LoginHistorySection', () => {
  it('render method, outcome and time per attempt, mapping known values and falling back for unknown ones', async () => {
    const attempts = [
      attempt({
        id: 'a-1',
        method: 'password',
        result: 'success',
        ip: '203.0.113.10',
        created_at: T1,
      }),
      attempt({
        id: 'a-2',
        method: 'sms',
        result: 'failure',
        failure_reason: 'bad_code',
        ip: '203.0.113.11',
        created_at: T2,
      }),
      attempt({
        id: 'a-3',
        method: 'social',
        result: 'success',
        ip: '203.0.113.12',
        created_at: T3,
      }),
      attempt({
        id: 'a-4',
        method: 'oidc',
        result: 'failure',
        failure_reason: 'no_membership',
        ip: '203.0.113.13',
        created_at: '2026-07-28T11:15:00.000Z',
      }),
      // A future channel and a future reason: both must fall back to the
      // generic labels -- never the raw method/reason value.
      attempt({
        id: 'a-5',
        method: 'magic',
        result: 'failure',
        failure_reason: 'future_reason',
        ip: '203.0.113.14',
        created_at: '2026-07-27T16:40:00.000Z',
      }),
    ]
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === HISTORY_PATH) {
        return jsonResponse(200, { attempts })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<LoginHistorySection />)

    expect(
      await screen.findByRole('heading', { name: zhCN.history.title }),
    ).toBeTruthy()

    // Known methods map to their labels, one row each. The heading is
    // present from the pending state on, so the first row assertion waits
    // for the data render before the synchronous assertions below it.
    expect(await screen.findByText(zhCN.history.method.password)).toBeTruthy()
    expect(screen.getByText(zhCN.history.method.sms)).toBeTruthy()
    expect(screen.getByText(zhCN.history.method.social)).toBeTruthy()
    expect(screen.getByText(zhCN.history.method.oidc)).toBeTruthy()
    // The method outside the known set renders the generic label.
    expect(screen.getByText(zhCN.history.method.other)).toBeTruthy()

    // Success renders its own label; known failure reasons map to their
    // text; the unknown reason renders the generic failure label.
    expect(screen.getAllByText(zhCN.history.result.success)).toHaveLength(2)
    expect(screen.getByText(zhCN.history.reason.bad_code)).toBeTruthy()
    expect(screen.getByText(zhCN.history.reason.no_membership)).toBeTruthy()
    expect(screen.getByText(zhCN.history.reason.other)).toBeTruthy()

    // Raw values never surface -- not the method, not the reason token.
    expect(screen.queryByText('magic')).toBeNull()
    expect(screen.queryByText('future_reason')).toBeNull()

    // Raw IPs and Intl-formatted times render as answered.
    expect(screen.getByText('203.0.113.10')).toBeTruthy()
    expect(screen.getByText('203.0.113.14')).toBeTruthy()
    expect(screen.getByText(plainTime(T1))).toBeTruthy()
    expect(screen.getByText(plainTime(T3))).toBeTruthy()

    // The one list request carried the frozen page size.
    const historyCall = rig.calls.find(
      (call) => call.method === 'GET' && call.path === HISTORY_PATH,
    )
    expect(historyCall?.query).toContain('limit=20')

    await expectNoAxeViolations()
  })

  it('show the loading skeleton with the header while the history is pending', async () => {
    let release: (response: Response) => void = () => undefined
    const gate = new Promise<Response>((resolve) => {
      release = resolve
    })
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === HISTORY_PATH) {
        return gate
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    const { container } = renderWithProviders(<LoginHistorySection />)

    expect(screen.getByRole('heading', { name: zhCN.history.title })).toBeTruthy()
    expect(screen.getByRole('status', { name: zhCN.history.loading })).toBeTruthy()
    expect(container.querySelector('.MuiSkeleton-root')).not.toBeNull()

    release(jsonResponse(200, { attempts: [attempt()] }))
    expect(await screen.findByText(zhCN.history.method.password)).toBeTruthy()
    expect(screen.queryByRole('status', { name: zhCN.history.loading })).toBeNull()
  })

  it('render the empty state without the header when there is no history', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === HISTORY_PATH) {
        return jsonResponse(200, { attempts: [] })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<LoginHistorySection />)

    expect(await screen.findByText(zhCN.history.empty.title)).toBeTruthy()
    expect(screen.getByText(zhCN.history.empty.description)).toBeTruthy()
    expect(screen.queryByRole('heading', { name: zhCN.history.title })).toBeNull()

    await expectNoAxeViolations()
  })

  it('render the error state with a retry that refetches the history', async () => {
    const user = userEvent.setup()
    let historyCalls = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === HISTORY_PATH) {
        historyCalls += 1
        if (historyCalls === 1) {
          return errorResponse(500, 'client.http.500')
        }
        return jsonResponse(200, {
          attempts: [attempt({ method: 'sms', result: 'success' })],
        })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<LoginHistorySection />)

    expect(await screen.findByText(zhCN.history.error.title)).toBeTruthy()
    expect(screen.getByText(zhCN.history.error.description)).toBeTruthy()
    expect(screen.queryByRole('heading', { name: zhCN.history.title })).toBeNull()

    await user.click(screen.getByRole('button', { name: zhCN.history.retry }))
    expect(await screen.findByText(zhCN.history.method.sms)).toBeTruthy()
    expect(screen.queryByText(zhCN.history.error.title)).toBeNull()
    expect(historyCalls).toBe(2)

    await expectNoAxeViolations()
  })
})
