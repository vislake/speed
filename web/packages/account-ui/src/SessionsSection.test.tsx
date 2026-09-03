/**
 * SessionsSection behaviour, driven over the real-client rig: the
 * section never sees a mock fetch -- the responder answers genuine
 * Response objects to a real @speed/api-client (bound through the same
 * bindRequestFn seam the generated hooks use) and every session answer
 * is the shape the server answers, assertions on what the user sees.
 *
 * The suite pins the surface contract: raw values (the user-agent
 * string, the IP, the AMR tokens) render as answered, translated text
 * never appears in their place; the current session is marked and has no
 * revoke action while every other active session has exactly one;
 * revoked sessions stay listed, greyed out, with no action; the single
 * row revoke fires DELETE /api/v1/authn/sessions/{id} with the caller's
 * bearer token and the list refetches on success, while a 404 renders
 * the session_not_found alert and changes nothing; "sign out other
 * devices" is the only double-confirmed action (ui-kit danger dialog:
 * first click arms with the ui-kit confirm-again label, second click
 * revokes) and surfaces the server's revoked_count on success; loading,
 * empty and error states render their own placeholder, the error state
 * with a retry that refetches. Every scenario ends with an axe pass.
 */

import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AuthnSession } from '@speed/api-sdk'
import { uiKitResources } from '@speed/ui-kit'
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
import { SessionsSection } from './SessionsSection.js'

const LOGIN_PATH = '/api/v1/authn/login/password'
const SESSIONS_PATH = '/api/v1/authn/sessions'
const REVOKE_OTHERS_PATH = '/api/v1/authn/sessions/revoke-others'

/** The ui-kit dialog strings the armed-confirm flow depends on, typed
 * straight from the package's own exported resources. */
const UI_KIT_ZH = uiKitResources['zh-CN'] as unknown as {
  readonly confirmDialog: {
    readonly cancelLabel: string
    readonly confirmAgainLabel: string
  }
}

const T1 = '2026-07-30T08:30:00.000Z'
const T2 = '2026-08-01T02:05:00.000Z'
const T3 = '2026-08-02T14:20:00.000Z'

function session(overrides: Partial<AuthnSession> = {}): AuthnSession {
  return {
    id: 'session-1',
    status: 'active',
    is_current: false,
    user_agent: 'Chrome/126.0.0.0 on Windows',
    ip: '203.0.113.10',
    amr: ['password'],
    created_at: T1,
    last_seen_at: T2,
    ...overrides,
  }
}

/** The formatted-time interpolation of the bundle key, computed the same
 * way the component computes it, so the assertion tracks the host's own
 * Intl output for the test language. */
function timeText(key: string, iso: string): string {
  const formatter = new Intl.DateTimeFormat('zh-CN', {
    dateStyle: 'medium',
    timeStyle: 'short',
  })
  return key.replace('{{time}}', formatter.format(new Date(iso)))
}

/** The row-end revoke label of the row whose device label reads
 * `device`, interpolated the way the component interpolates it -- each
 * row's action is named after the row itself. */
function revokeAriaOf(device: string): string {
  return zhCN.sessions.revokeAriaWithDevice.replace('{{device}}', device)
}

describe('SessionsSection', () => {
  it('render every session with raw values, the current marker, and one revoke action per non-current active row', async () => {
    const sessions = [
      session({
        id: 'current-1',
        is_current: true,
        device: 'This laptop',
        user_agent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)',
        ip: '198.51.100.4',
        amr: ['password', 'mfa:totp'],
        created_at: T1,
        last_seen_at: T3,
      }),
      session({
        id: 'other-1',
        user_agent: 'Chrome/126.0.0.0 on Windows',
        ip: '203.0.113.10',
        amr: ['social:google'],
        created_at: T2,
      }),
      session({
        id: 'old-1',
        status: 'revoked',
        user_agent: '',
        amr: [],
        ip: '203.0.113.99',
        created_at: T2,
      }),
    ]
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, {
          ...makePair(),
          principal: {
            user_id: 'user-1',
            tenant_id: 'tenant-1',
            session_id: 'current-1',
          },
        })
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        return jsonResponse(200, { sessions })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<SessionsSection />)

    // The section heading and the current-session marker. The heading is
    // present from the pending state on, so the marker assertion waits
    // for the data render before the synchronous assertions below it.
    expect(await screen.findByRole('heading', { name: zhCN.sessions.title })).toBeTruthy()
    expect(await screen.findByText(zhCN.sessions.current)).toBeTruthy()

    // The device label and the raw user-agent detail line of the current row.
    expect(screen.getByText('This laptop')).toBeTruthy()
    expect(screen.getByText('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)')).toBeTruthy()
    // The raw user-agent as the label of a row that carries no device string.
    expect(screen.getByText('Chrome/126.0.0.0 on Windows')).toBeTruthy()
    // The row with neither device nor user-agent falls back to the label.
    expect(screen.getByText(zhCN.sessions.deviceUnknown)).toBeTruthy()

    // Raw AMR tokens and raw IPs render as answered, never translated.
    expect(screen.getByText('password')).toBeTruthy()
    expect(screen.getByText('mfa:totp')).toBeTruthy()
    expect(screen.getByText('social:google')).toBeTruthy()
    expect(screen.getByText('198.51.100.4')).toBeTruthy()
    expect(screen.getByText('203.0.113.10')).toBeTruthy()

    // Times render through Intl in the current language.
    expect(screen.getByText(timeText(zhCN.sessions.signedIn, T1))).toBeTruthy()
    expect(screen.getByText(timeText(zhCN.sessions.lastSeen, T3))).toBeTruthy()

    // The rows are one real list: each session is one list item, so a
    // screen-reader user hears the row boundaries and the item count
    // instead of a flat text-and-button sequence.
    const rows = screen.getAllByRole('listitem')
    expect(rows).toHaveLength(3)
    // Each row's content lives inside its own item: the current marker
    // and the status badge in their rows, the revoke action in the
    // active non-current row -- named after that row's device label.
    expect(within(rows[0]!).getByText(zhCN.sessions.current)).toBeTruthy()
    expect(within(rows[0]!).queryByRole('button')).toBeNull()
    expect(
      within(rows[2]!).getByText(zhCN.sessions.status.revoked),
    ).toBeTruthy()
    // Exactly one revoke affordance: the active non-current row; the
    // current and the revoked rows carry none.
    const revokeButtons = screen.getAllByRole('button', {
      name: revokeAriaOf('Chrome/126.0.0.0 on Windows'),
    })
    expect(revokeButtons).toHaveLength(1)

    await expectNoAxeViolations()
  })

  it('show the loading skeleton with the header while the list is pending', async () => {
    let release: (response: Response) => void = () => undefined
    const gate = new Promise<Response>((resolve) => {
      release = resolve
    })
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        return gate
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    const { container } = renderWithProviders(<SessionsSection />)

    expect(screen.getByRole('heading', { name: zhCN.sessions.title })).toBeTruthy()
    expect(screen.getByRole('status', { name: zhCN.sessions.loading })).toBeTruthy()
    expect(container.querySelector('.MuiSkeleton-root')).not.toBeNull()

    release(jsonResponse(200, { sessions: [session()] }))
    expect(await screen.findByText('Chrome/126.0.0.0 on Windows')).toBeTruthy()
    expect(screen.queryByRole('status', { name: zhCN.sessions.loading })).toBeNull()
  })

  it('render the empty state -- no header, no actions -- when the account has no sessions', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        return jsonResponse(200, { sessions: [] })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<SessionsSection />)

    expect(await screen.findByText(zhCN.sessions.empty.title)).toBeTruthy()
    expect(screen.getByText(zhCN.sessions.empty.description)).toBeTruthy()
    expect(screen.queryByRole('heading', { name: zhCN.sessions.title })).toBeNull()
    expect(screen.queryByRole('button', { name: zhCN.sessions.revokeOthers.label })).toBeNull()

    await expectNoAxeViolations()
  })

  it('render the error state with a retry that refetches the list', async () => {
    const user = userEvent.setup()
    let listCalls = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        listCalls += 1
        if (listCalls === 1) {
          return errorResponse(500, 'client.http.500')
        }
        return jsonResponse(200, { sessions: [session()] })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<SessionsSection />)

    expect(await screen.findByText(zhCN.sessions.error.title)).toBeTruthy()
    expect(screen.getByText(zhCN.sessions.error.description)).toBeTruthy()
    expect(screen.queryByRole('heading', { name: zhCN.sessions.title })).toBeNull()

    await user.click(screen.getByRole('button', { name: zhCN.sessions.retry }))
    expect(await screen.findByText('Chrome/126.0.0.0 on Windows')).toBeTruthy()
    expect(screen.queryByText(zhCN.sessions.error.title)).toBeNull()
    expect(listCalls).toBe(2)

    await expectNoAxeViolations()
  })

  it('revoke a single session with the bearer token and refetch until the row reads revoked', async () => {
    const user = userEvent.setup()
    let otherRevoked = false
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        return jsonResponse(200, {
          sessions: [
            session({ id: 'current-1', is_current: true, device: 'This laptop' }),
            session({
              id: 'other-1',
              status: otherRevoked ? 'revoked' : 'active',
            }),
          ],
        })
      }
      if (call.method === 'DELETE' && call.path === `${SESSIONS_PATH}/other-1`) {
        otherRevoked = true
        return new Response(null, { status: 204 })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<SessionsSection />)

    const revokeButton = await screen.findByRole('button', {
      name: revokeAriaOf('Chrome/126.0.0.0 on Windows'),
    })
    await user.click(revokeButton)

    // The refetch after the successful revoke turns the row revoked and
    // no actions remain on the list.
    await waitFor(() => {
      expect(screen.getAllByText(zhCN.sessions.status.revoked)).toHaveLength(1)
    })
    expect(
      screen.queryByRole('button', {
        name: revokeAriaOf('Chrome/126.0.0.0 on Windows'),
      }),
    ).toBeNull()
    // No failure banner: a successful single revoke is silent.
    expect(screen.queryByRole('alert')).toBeNull()

    const revokeCall = rig.calls.find(
      (call) => call.method === 'DELETE' && call.path === `${SESSIONS_PATH}/other-1`,
    )
    expect(revokeCall?.authorization).toBe('Bearer access-1')
    // The refetch after the revoke really happened: GET, DELETE, GET.
    expect(rig.calls.map((call) => `${call.method} ${call.path}`)).toEqual([
      `POST ${LOGIN_PATH}`,
      `GET ${SESSIONS_PATH}`,
      `DELETE ${SESSIONS_PATH}/other-1`,
      `GET ${SESSIONS_PATH}`,
    ])

    await expectNoAxeViolations()
  })

  it('render the session_not_found alert when a single revoke answers 404 and keep the row actionable', async () => {
    const user = userEvent.setup()
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        return jsonResponse(200, {
          sessions: [session({ id: 'current-1', is_current: true }), session({ id: 'other-1' })],
        })
      }
      if (call.method === 'DELETE' && call.path === `${SESSIONS_PATH}/other-1`) {
        return errorResponse(404, 'authn.session_not_found')
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<SessionsSection />)

    const revokeButton = await screen.findByRole('button', {
      name: revokeAriaOf('Chrome/126.0.0.0 on Windows'),
    })
    await user.click(revokeButton)

    expect(await screen.findByRole('alert')).toHaveTextContent(
      zhCN.errors.authn.session_not_found,
    )
    // Nothing was revoked: the row still carries its revoke action.
    expect(
      screen.getAllByRole('button', {
        name: revokeAriaOf('Chrome/126.0.0.0 on Windows'),
      }),
    ).toHaveLength(1)
  })

  it('revoke every other session only behind the danger double-confirm dialog and surface the revoked count', async () => {
    const user = userEvent.setup()
    let othersRevoked = false
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        return jsonResponse(200, {
          sessions: [
            session({ id: 'current-1', is_current: true, device: 'This laptop' }),
            session({ id: 'other-1', status: othersRevoked ? 'revoked' : 'active' }),
            session({ id: 'other-2', status: othersRevoked ? 'revoked' : 'active' }),
          ],
        })
      }
      if (call.method === 'POST' && call.path === REVOKE_OTHERS_PATH) {
        othersRevoked = true
        return jsonResponse(200, { revoked_count: 2 })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<SessionsSection />)

    await user.click(
      await screen.findByRole('button', { name: zhCN.sessions.revokeOthers.label }),
    )

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(zhCN.sessions.revokeOthers.confirmTitle)).toBeTruthy()
    expect(
      within(dialog).getByText(zhCN.sessions.revokeOthers.confirmMessage),
    ).toBeTruthy()

    // One click arms the danger action (ui-kit's confirm-again label),
    // nothing has been revoked yet.
    await user.click(
      within(dialog).getByRole('button', { name: zhCN.sessions.revokeOthers.confirmLabel }),
    )
    expect(
      within(dialog).getByRole('button', { name: UI_KIT_ZH.confirmDialog.confirmAgainLabel }),
    ).toBeTruthy()
    expect(
      rig.calls.some((call) => call.method === 'POST' && call.path === REVOKE_OTHERS_PATH),
    ).toBe(false)

    // The second click revokes; the count surfaces and the list refetches
    // to the server's answer -- both other rows revoked, the current row
    // untouched and the section-top action gone with nothing left to revoke.
    await user.click(
      within(dialog).getByRole('button', { name: UI_KIT_ZH.confirmDialog.confirmAgainLabel }),
    )
    expect(await screen.findByText(zhCN.sessions.revokeOthers.done_other.replace('{{count}}', '2'))).toBeTruthy()
    await waitFor(() => {
      expect(screen.getAllByText(zhCN.sessions.status.revoked)).toHaveLength(2)
    })
    expect(screen.getByText('This laptop')).toBeTruthy()
    expect(
      screen.queryByRole('button', { name: zhCN.sessions.revokeOthers.label }),
    ).toBeNull()
    expect(rig.calls.at(-1)).toMatchObject({ method: 'GET', path: SESSIONS_PATH })

    await expectNoAxeViolations()
  })

  it('close the revoke-others dialog on cancel without revoking anything', async () => {
    const user = userEvent.setup()
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        return jsonResponse(200, {
          sessions: [
            session({ id: 'current-1', is_current: true }),
            session({ id: 'other-1' }),
          ],
        })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<SessionsSection />)

    await user.click(
      await screen.findByRole('button', { name: zhCN.sessions.revokeOthers.label }),
    )
    const dialog = await screen.findByRole('dialog')
    await user.click(
      within(dialog).getByRole('button', { name: UI_KIT_ZH.confirmDialog.cancelLabel }),
    )

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull()
    })
    expect(
      rig.calls.some((call) => call.method === 'POST' && call.path === REVOKE_OTHERS_PATH),
    ).toBe(false)
  })
})
