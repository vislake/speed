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
 * revoked sessions stay listed, greyed out, with no action; a session
 * whose stored expires_at has passed is a dead session rendered
 * expired -- never a live device that stays revocable and arms "sign
 * out other devices"; the single row revoke fires
 * DELETE /api/v1/authn/sessions/{id} with the caller's bearer token and
 * the list refetches on success, while a 404 renders the
 * session_not_found alert and changes nothing; "sign out other devices"
 * is the only double-confirmed action (ui-kit danger dialog: first
 * click arms with the ui-kit confirm-again label, second click revokes)
 * and surfaces the server's revoked_count on success; loading, empty
 * and error states render their own placeholder, the error state with a
 * retry that refetches. Every scenario ends with an axe pass.
 */

import { describe, expect, it, afterEach } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import { onlineManager } from '@tanstack/react-query'
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
// The expiry fixtures are fixed far past / far future stamps: expiry is
// compared against the wall clock at render time, so a boundary-sitting
// stamp would make the scenario nondeterministic.
const PAST_EXPIRY = '2020-01-01T00:00:00.000Z'
const FUTURE_EXPIRY = '2099-01-01T00:00:00.000Z'

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
  afterEach(() => {
    // The offline scenario toggles the global online manager; every
    // other test signs in and fetches over the real transport, so the
    // device must be back online when the next test starts.
    onlineManager.setOnline(true)
  })

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

  it('never read an unresolved load as no sessions: a fetch parked offline stays on the loading branch until the network returns', async () => {
    const list = [session()]
    let wentOnline = false
    let listCalls = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        listCalls += 1
        if (!wentOnline) {
          throw new Error(
            'a list request reached the transport while the fetch was parked offline',
          )
        }
        return jsonResponse(200, { sessions: list })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    // react-query's default networkMode 'online' parks a fetch that
    // starts while the device is offline at fetchStatus 'paused':
    // isFetching -- and isLoading, its isPending-and-isFetching
    // conjunction -- are false, isError stays false and data stays
    // undefined, so a pending test derived from isLoading misses every
    // branch. The section must stay on the loading branch (isPending)
    // instead of asserting an empty list for an answer that never
    // arrived.
    onlineManager.setOnline(false)
    const { queryClient } = renderWithProviders(<SessionsSection />)

    await waitFor(() => {
      const [query] = queryClient.getQueryCache().findAll()
      expect(query?.state.fetchStatus).toBe('paused')
    })
    expect(listCalls).toBe(0)

    // Still the loading branch: the header and one loading announcement,
    // never the no-sessions or error text.
    expect(
      screen.getByRole('heading', { name: zhCN.sessions.title }),
    ).toBeTruthy()
    expect(
      screen.getByRole('status', { name: zhCN.sessions.loading }),
    ).toBeTruthy()
    expect(screen.queryByText(zhCN.sessions.empty.title)).toBeNull()
    expect(screen.queryByText(zhCN.sessions.error.title)).toBeNull()

    // The parked fetch resumes by itself once the network is back.
    wentOnline = true
    onlineManager.setOnline(true)
    expect(await screen.findByText('Chrome/126.0.0.0 on Windows')).toBeTruthy()
    expect(
      screen.queryByRole('status', { name: zhCN.sessions.loading }),
    ).toBeNull()
    expect(listCalls).toBe(1)

    await expectNoAxeViolations()
  })

  it('feedback while a retry is armed: a retry clicked offline parks on the loading branch, resumes with exactly one request, and no button exists to stack a second click', async () => {
    const user = userEvent.setup()
    let listCalls = 0
    let wentOnline = true
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        listCalls += 1
        if (listCalls === 1) {
          return errorResponse(500, 'client.http.500')
        }
        if (!wentOnline) {
          throw new Error(
            'a list request reached the transport while the device was offline',
          )
        }
        return jsonResponse(200, { sessions: [session()] })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    const { queryClient } = renderWithProviders(<SessionsSection />)

    expect(await screen.findByText(zhCN.sessions.error.title)).toBeTruthy()

    // The retry of a settled error is armed the moment it is clicked:
    // react-query puts the refetch back into the pending state (and
    // parks it at fetchStatus 'paused' while the device is offline), so
    // the retry affordance's feedback IS the loading branch -- a live
    // progress announcement, never a still-clickable button, so repeat
    // clicks cannot overlap refetches.
    onlineManager.setOnline(false)
    wentOnline = false
    await user.click(screen.getByRole('button', { name: zhCN.sessions.retry }))

    await waitFor(() => {
      const [query] = queryClient.getQueryCache().findAll()
      expect(query?.state.fetchStatus).toBe('paused')
    })
    expect(listCalls).toBe(1)
    expect(
      screen.getByRole('status', { name: zhCN.sessions.loading }),
    ).toBeTruthy()
    expect(
      screen.queryByRole('button', { name: zhCN.sessions.retry }),
    ).toBeNull()
    expect(screen.queryByText(zhCN.sessions.error.title)).toBeNull()
    expect(screen.queryByText(zhCN.sessions.empty.title)).toBeNull()

    // The parked refetch resumes by itself once the network is back,
    // with exactly one request -- the park/resume cycle never stacks a
    // duplicate.
    wentOnline = true
    onlineManager.setOnline(true)
    expect(await screen.findByText('Chrome/126.0.0.0 on Windows')).toBeTruthy()
    expect(
      screen.queryByRole('status', { name: zhCN.sessions.loading }),
    ).toBeNull()
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

  it('announce the revoke-others success into a live region that stood empty from the list phase (mount-with-text regression)', async () => {
    // PRE-FIX: the success notice's role="status" region rendered only
    // while the notice existed, so the region mounted in the same commit
    // as its text. A live region announces content changes that follow
    // its own existence, never text that mounts together with it, so the
    // revoke-others success was silent. POST-FIX: the region stands
    // mounted (empty, visually silent) for the whole list phase and the
    // notice commit fills the text into a region the screen reader
    // already knows. The same DOM node must survive the transition,
    // which is what makes the later text change an announcement rather
    // than another mount.
    const user = userEvent.setup()
    let othersRevoked = false
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        return jsonResponse(200, {
          sessions: [
            session({
              id: 'current-1',
              is_current: true,
              device: 'This laptop',
            }),
            session({
              id: 'other-1',
              status: othersRevoked ? 'revoked' : 'active',
            }),
            session({
              id: 'other-2',
              status: othersRevoked ? 'revoked' : 'active',
            }),
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

    await screen.findByRole('button', {
      name: zhCN.sessions.revokeOthers.label,
    })
    // The live region stood mounted and empty while the list was up and
    // nothing had been announced: its text is empty here, pre-fix the
    // region does not exist at all. (The dialog is still closed, so this
    // status query cannot collide with the ui-kit ConfirmDialog's own
    // arming region.)
    const status = screen.getByRole('status')
    expect(status).toHaveTextContent('')

    await user.click(
      screen.getByRole('button', { name: zhCN.sessions.revokeOthers.label }),
    )
    const dialog = await screen.findByRole('dialog')
    await user.click(
      within(dialog).getByRole('button', {
        name: zhCN.sessions.revokeOthers.confirmLabel,
      }),
    )
    await user.click(
      within(dialog).getByRole('button', {
        name: UI_KIT_ZH.confirmDialog.confirmAgainLabel,
      }),
    )
    // The success text filled the standing region -- the same node. The
    // identity is asserted through a role query, which the ui-kit
    // danger dialog's open state masks: the MUI modal stamps the app
    // tree aria-hidden while the dialog shows (and through its closing
    // transition), so the same-node check must wait for the dialog to
    // finish closing and the app tree to become queryable again.
    await waitFor(() =>
      expect(status).toHaveTextContent(
        zhCN.sessions.revokeOthers.done_other.replace('{{count}}', '2'),
      ),
    )
    await waitFor(() => expect(screen.getByRole('status')).toBe(status))

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

  it('render an expired session as expired, not as a live device: a past expires_at greys the row out of every revoke path', async () => {
    // The server answers only active/revoked (expiry is checked at use
    // time, never written back), so before the fix a session whose
    // stored expires_at had passed rendered as a live device forever:
    // it stayed individually revocable and read as a logged-in device.
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
        return jsonResponse(200, {
          sessions: [
            session({
              id: 'current-1',
              is_current: true,
              device: 'This laptop',
              expires_at: FUTURE_EXPIRY,
            }),
            session({
              id: 'live-1',
              device: 'A live phone',
              expires_at: FUTURE_EXPIRY,
            }),
            session({
              id: 'dead-1',
              device: 'An old tablet',
              // Still status 'active': only the stored expiry gives it
              // away.
              expires_at: PAST_EXPIRY,
            }),
          ],
        })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<SessionsSection />)

    // The expired row renders its own status badge, never as a live
    // device...
    expect(await screen.findByText('An old tablet')).toBeTruthy()
    expect(
      screen.getAllByText(zhCN.sessions.status.expired),
    ).toHaveLength(1)
    expect(
      screen.queryByText(zhCN.sessions.status.revoked),
    ).toBeNull()
    // ...and carries no revoke affordance, while the live row keeps its
    // own -- each action named after the row it belongs to.
    expect(
      screen.queryByRole('button', { name: revokeAriaOf('An old tablet') }),
    ).toBeNull()
    expect(
      screen.getAllByRole('button', { name: revokeAriaOf('A live phone') }),
    ).toHaveLength(1)
    // The current-session rule is unchanged: the current row stays
    // marked and unrevocable.
    expect(screen.getByText(zhCN.sessions.current)).toBeTruthy()
    expect(
      screen.queryByRole('button', { name: revokeAriaOf('This laptop') }),
    ).toBeNull()

    await expectNoAxeViolations()
  })

  it('not arm "sign out other devices" for an expired session: only live other sessions count', async () => {
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
        return jsonResponse(200, {
          sessions: [
            session({ id: 'current-1', is_current: true }),
            session({ id: 'dead-1', device: 'An old tablet', expires_at: PAST_EXPIRY }),
          ],
        })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<SessionsSection />)

    expect(await screen.findByText(zhCN.sessions.status.expired)).toBeTruthy()
    // The only other session is dead: the bulk action has nothing to
    // sign out and stays hidden -- before the fix the expired row
    // counted as a live other and armed the action forever.
    expect(
      screen.queryByRole('button', { name: zhCN.sessions.revokeOthers.label }),
    ).toBeNull()

    await expectNoAxeViolations()
  })

  it('end the loading state when a settled answer omits the optional sessions key: the error state renders with aria-busy ended, and its retry converges once the answer carries the list', async () => {
    const user = userEvent.setup()
    let listCalls = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === SESSIONS_PATH) {
        listCalls += 1
        if (listCalls === 1) {
          // AuthnListSessionsResponse marks `.sessions` optional, so a
          // 200 whose body omits the key is type-legal and react-query
          // settles it as a successful answer. Before the fix the
          // section read this settled shape as still loading and held
          // the aria-busy skeleton forever, with no exit.
          return jsonResponse(200, {})
        }
        return jsonResponse(200, { sessions: [session()] })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    })
    await signInWithPassword(rig)
    renderWithProviders(<SessionsSection />)

    // The finite state: the error state -- whose copy claims no account
    // content -- never the loading skeleton. The loading announcement
    // and its aria-busy are gone, no fabricated "no sessions" claim
    // takes the list's place, and the header stays hidden so the error
    // title keeps the heading level.
    expect(await screen.findByText(zhCN.sessions.error.title)).toBeTruthy()
    expect(screen.getByText(zhCN.sessions.error.description)).toBeTruthy()
    expect(
      screen.queryByRole('status', { name: zhCN.sessions.loading }),
    ).toBeNull()
    expect(screen.queryByText(zhCN.sessions.empty.title)).toBeNull()
    expect(
      screen.queryByRole('heading', { name: zhCN.sessions.title }),
    ).toBeNull()

    // The error state's retry is the exit: a refetch whose answer
    // carries the key converges onto the list.
    await user.click(screen.getByRole('button', { name: zhCN.sessions.retry }))
    expect(await screen.findByText('Chrome/126.0.0.0 on Windows')).toBeTruthy()
    expect(screen.queryByText(zhCN.sessions.error.title)).toBeNull()
    expect(listCalls).toBe(2)

    await expectNoAxeViolations()
  })
})
