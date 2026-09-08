/**
 * NotesView contract: the notes surface gates on the list read it
 * drives and renders what the server answers. The read is the real
 * permission fetch -- the demo server's deny switches answer the rbac
 * gate's 403 the way the real server does, so the gate's denied branch
 * (the ui-kit no-permission empty state, form and list absent) and the
 * create refusals are driven by genuine refusals, never stubbed
 * locally. The journeys drive a real client bound into the runtime
 * seam, sign in through the real session operation (the surface reads
 * the current tenant from the auth-core hooks, so an anonymous render
 * can never leave pending), and pin the observed requests -- bearer
 * header, body, and the invalidating refetch after a create.
 *
 * The gate's three statuses each earn a check: the pending spinner
 * stands in until a held read answers (layout-kit's own copy), the
 * served list flips the gate open, a refused read flips it shut. The
 * create path is probed against the server's authority: whitespace-only
 * text is sent (the client's required rule only catches the empty
 * string, exactly as the real handler's trim does not) and the
 * text-required refusal renders, an over-long client-side rule is not
 * exercised here, and a refused create keeps the draft on the form with
 * the permission text -- the write gate is answered by the mutation,
 * never pre-empted. Empty submits never reach the network.
 *
 * Built-in strings are asserted through the bundles they render from --
 * the app's own zh-CN/en-US fixtures and the ui-kit and layout-kit
 * package fixtures (relative imports, the product-shell precedent) --
 * never inline: the CJK scan treats test files as English text like
 * everything else. Served note text is server data, not copy, and
 * travels in the journey verbatim. Created-at cells are asserted
 * through the same Intl formatting the view uses, so a machine's time
 * zone never enters the expectation.
 */

import { act, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { RequestFn } from '@speed/api-client'
import type { NotesNote } from '@speed/api-sdk'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { describe, expect, it } from 'vitest'
import layoutKitZhCN from '../../../../../web/packages/layout-kit/src/locales/zh-CN.json' with { type: 'json' }
import uiKitZhCN from '../../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { createAppQueryClient } from '../main.js'
import { demoServer } from '../test-utils/demo-server.js'
import type { RealClientRig } from '../test-utils/real-client.js'
import {
  errorResponse,
  makeRealClientRig,
  signInWithPassword,
} from '../test-utils/real-client.js'
import type { RenderWithProvidersOptions } from '../test-utils/render.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { clearNotesDraft } from './notes-draft.js'
import { NotesView } from './notes-view.js'

/** A transport whose every call rejects with a raw, code-less error --
 * the shape a bug-shaped transport throw (or an error thrown before
 * the api-client could normalize it) arrives in: no envelope code, no
 * client.* code, nothing apiErrorCodeOf can read. The bound-seam
 * replacement is how these tests drive the generated hook's queryFn
 * through a genuine codeless rejection. */
const UNNORMALIZED_TRANSPORT_FAILURE: RequestFn = () =>
  Promise.reject(
    new Error('[notes-view.test] an un-normalized transport failure'),
  )

/** The fixed demo epoch the demo server's note answers carry. */
const DEMO_CREATED_AT = '2026-09-04T00:00:00Z'

/** The served note texts travel as their own constants (a note's text
 * is optional on the spec type, but a served note always carries one). */
const NOTE_ONE_TEXT = 'First note'
const NOTE_TWO_TEXT = 'Second note'
const NEW_NOTE_TEXT = 'Written in the browser'
const DENIED_NOTE_TEXT = 'A note that must not land'
/** The half-typed text of the draft journeys: written, left behind by a
 * surface switch, and restored by the remount. */
const ROUND_TRIP_NOTE_TEXT = 'Half-typed before navigating away'

const NOTE_ONE: NotesNote = {
  id: 'note-1',
  text: NOTE_ONE_TEXT,
  created_at: DEMO_CREATED_AT,
}
const NOTE_TWO: NotesNote = {
  id: 'note-2',
  text: NOTE_TWO_TEXT,
  created_at: DEMO_CREATED_AT,
}

/** The created-at cell text the view's own Intl formatting produces. */
function createdAtText(language: string, value: string): string {
  return new Intl.DateTimeFormat(language, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(value))
}

/** Renders the notes surface over a signed-in rig (the surface reads
 * the current tenant from the auth-core hooks). */
function renderNotes(
  rig: RealClientRig,
  options: RenderWithProvidersOptions = {},
) {
  return renderWithAppServices(
    <NotesView />,
    { session: rig.session, api: rig.api },
    options,
  )
}

function notesGets(rig: RealClientRig): number {
  return rig.calls.filter(
    (call) => call.method === 'GET' && call.path === '/api/v1/notes',
  ).length
}

function noteCreates(rig: RealClientRig): number {
  return rig.calls.filter(
    (call) => call.method === 'POST' && call.path === '/api/v1/notes',
  ).length
}

describe('NotesView', () => {
  it('names the clinic being worked in at page-title level, under the heading', async () => {
    // The acceptance shape for the notes surface (current-clinic-is-
    // visible): a person writing a patient record must see which
    // clinic the record will land in from where they are writing --
    // the clinic's display name inside the main content, under the
    // page heading -- never only in the chrome's tenant switcher.
    const rig = makeRealClientRig(demoServer({ initialNotes: [NOTE_ONE] }))
    await signInWithPassword(rig)
    const view = renderNotes(rig)
    await view.findByRole('heading', { level: 1 })
    expect(
      await view.findByText(
        zhCN.clinic.currentClinic.replace('{{name}}', zhCN.tenants.acme),
      ),
    ).toBeInTheDocument()
  })

  it('gates on the read: the pending spinner stands in until the list answers, then the rows render', async () => {
    let release: (() => void) | undefined
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    const server = demoServer({ initialNotes: [NOTE_ONE] })
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/notes') {
        await gate
      }
      return server(call)
    })
    await signInWithPassword(rig)
    const view = renderNotes(rig)

    // No read answer yet: the guard's pending state -- its own spinner,
    // layout-kit copy -- stands in for the whole surface.
    expect(
      await view.findByRole('progressbar', {
        name: layoutKitZhCN.routeGuard.pending,
      }),
    ).toBeInTheDocument()
    expect(view.queryByText(NOTE_ONE_TEXT)).not.toBeInTheDocument()

    await act(async () => {
      release?.()
    })
    expect(await view.findByText(NOTE_ONE_TEXT)).toBeInTheDocument()
    expect(
      view.queryByRole('progressbar', { name: layoutKitZhCN.routeGuard.pending }),
    ).not.toBeInTheDocument()
  })

  it('renders the served notes with Intl-formatted created times over one pinned read', async () => {
    const rig = makeRealClientRig(
      demoServer({ initialNotes: [NOTE_ONE, NOTE_TWO] }),
    )
    await signInWithPassword(rig)
    const view = renderNotes(rig)

    expect(await view.findByText(NOTE_ONE_TEXT)).toBeInTheDocument()
    expect(view.getByText(NOTE_TWO_TEXT)).toBeInTheDocument()
    // The created cells speak the surface language through Intl -- the
    // same formatter the view uses, so the machine's zone never enters.
    const expected = createdAtText(view.i18n.language, DEMO_CREATED_AT)
    expect(view.getAllByText(expected)).toHaveLength(2)
    expect(view.getByText(zhCN.notes.createdColumn)).toBeInTheDocument()

    // The list is one authenticated read: no refetch, the bearer token
    // the sign-in planted.
    expect(notesGets(rig)).toBe(1)
    const listCall = rig.calls.find(
      (call) => call.method === 'GET' && call.path === '/api/v1/notes',
    )
    expect(listCall?.authorization).toBe('Bearer access-1')
  })

  it('creates a note through the real client: the POST carries the text, the invalidated list shows the served row', async () => {
    // A fresh form: the draft store is module-scoped, so an earlier
    // journey's typed-but-uncreated text must not seed this one.
    clearNotesDraft()
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderNotes(rig)
    const user = userEvent.setup()

    // The empty demo list renders the list's empty state.
    expect(
      await view.findByText(zhCN.notes.list.emptyTitle),
    ).toBeInTheDocument()

    const input = view.getByLabelText(zhCN.notes.create.textLabel)
    await user.type(input, NEW_NOTE_TEXT)
    await user.click(
      view.getByRole('button', { name: zhCN.notes.create.submit }),
    )

    // The create reached the server with exactly the typed text, and
    // the form turned itself over only after the served answer.
    await waitFor(() => expect(noteCreates(rig)).toBe(1))
    const createCall = rig.calls.find(
      (call) => call.method === 'POST' && call.path === '/api/v1/notes',
    )
    expect(JSON.parse(createCall?.body ?? '{}')).toEqual({
      text: NEW_NOTE_TEXT,
    })

    // The create invalidated the namespaced list key: one refetch, and
    // the row the server served is what the list now shows -- the
    // served answer, never a client-assembled one.
    await waitFor(() => expect(notesGets(rig)).toBe(2))
    expect(
      await view.findByText(NEW_NOTE_TEXT),
    ).toBeInTheDocument()
    expect(view.queryByText(zhCN.notes.list.emptyTitle)).not.toBeInTheDocument()
    expect(input).toHaveValue('')
  })

  it('a refused create keeps the draft on the form and renders the permission text', async () => {
    // The list read answers fine; the write is refused with the rbac
    // gate's 403 -- the surface stays up, the refusal renders where
    // the form can answer it. A fresh form: see the create test.
    clearNotesDraft()
    const rig = makeRealClientRig(demoServer({ denyNotesWrite: true }))
    await signInWithPassword(rig)
    const view = renderNotes(rig)
    const user = userEvent.setup()
    await view.findByText(zhCN.notes.list.emptyTitle)

    const input = view.getByLabelText(zhCN.notes.create.textLabel)
    await user.type(input, DENIED_NOTE_TEXT)
    await user.click(
      view.getByRole('button', { name: zhCN.notes.create.submit }),
    )

    const alert = await view.findByRole('alert')
    expect(alert).toHaveTextContent(zhCN.notes.errors.permissionDenied)
    // The draft survived the refusal, ready to resubmit.
    expect(input).toHaveValue(DENIED_NOTE_TEXT)
    // The refusal came back before any refetch: still the one read.
    expect(noteCreates(rig)).toBe(1)
    expect(notesGets(rig)).toBe(1)
  })

  it('renders the surface\'s client text when a create fails at the transport (client.network)', async () => {
    // The offline-save acceptance shape at the unit tier: a create
    // whose request dies at the transport (the api-client normalizes
    // the rejection to client.network -- here a responder that throws
    // a raw TypeError, the same failure the e2e gate cuts with
    // route.abort) must render the surface's own notes.errors.client
    // text, never the generic fallback -- the code-mapping defect the
    // gate exists to keep closed (the browser proof is the
    // offline-save gate; this pins the mapping where the default CI
    // can run it). A fresh form: the draft store is module-scoped
    // (notes-draft.ts), and this journey's own half-typed text is the
    // assertion's whole point -- nothing from an earlier test may have
    // seeded the field before it types (see the create test).
    clearNotesDraft()
    const server = demoServer()
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'POST' && call.path === '/api/v1/notes') {
        throw new TypeError('network down')
      }
      return server(call)
    })
    await signInWithPassword(rig)
    const view = renderNotes(rig)
    const user = userEvent.setup()
    await view.findByText(zhCN.notes.list.emptyTitle)

    const input = view.getByLabelText(zhCN.notes.create.textLabel)
    await user.type(input, NOTE_ONE_TEXT)
    await user.click(
      view.getByRole('button', { name: zhCN.notes.create.submit }),
    )

    const alert = await view.findByRole('alert')
    expect(alert).toHaveTextContent(zhCN.notes.errors.client)
    expect(alert).not.toHaveTextContent(zhCN.notes.errors.unknown)
    // The draft survived the transport failure, ready to resubmit
    // once the network is back.
    expect(input).toHaveValue(NOTE_ONE_TEXT)
  })

  it('fails closed on a refetch 403 even though the previous read is still cached (reference-app-web.md P1-1)', async () => {
    // A fresh form: this journey mounts the surface with the read gate
    // open and asserts the served row by find-by-text -- a leftover
    // draft holding the same text as the row would match twice (see
    // the create test).
    clearNotesDraft()
    // The read succeeds once (the row lands in the query's cache),
    // then the same query is refetched and refused -- tanstack query
    // v5 keeps the prior successful `data` through a failed refetch,
    // so the gate must fail closed on the error alone rather than on
    // whether `data` happens to be defined, or the stale row would
    // keep rendering past a permission revoked mid-session (or a
    // different account signing into this tenant, the app-journey
    // regression covering that shape end to end).
    let denyRead = false
    const server = demoServer({ initialNotes: [NOTE_ONE] })
    const rig = makeRealClientRig(async (call) => {
      if (call.method === 'GET' && call.path === '/api/v1/notes' && denyRead) {
        return errorResponse(403, 'rbac.permission_denied')
      }
      return server(call)
    })
    await signInWithPassword(rig)
    const view = renderNotes(rig)

    // The first read serves the row and opens the gate.
    expect(await view.findByText(NOTE_ONE_TEXT)).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.noPermission.title),
    ).not.toBeInTheDocument()

    // Access is revoked and the same query is forced to refetch --
    // never a fresh mount, so whatever the query already cached is
    // exactly what a real revoked-mid-session read would still hold.
    denyRead = true
    await act(async () => {
      await view.queryClient.refetchQueries()
    })

    // The gate converges to denied and the stale row is gone -- never
    // left on screen because `data` was still defined.
    expect(
      await view.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(view.queryByText(NOTE_ONE_TEXT)).not.toBeInTheDocument()
  })

  it('a 5xx read failure renders the read-error state, never the no-permission gate (reference-app-web.md P2-2)', async () => {
    // Only the rbac read gate's own refusal is an authorization fact.
    // A read that fails for any other reason -- a 5xx here, a transport
    // answer in general -- is not a permission problem, and wearing the
    // no-permission empty state would tell a user whose server is down
    // that they are forbidden from looking (and an administrator whose
    // grant is misconfigured that the service is healthy).
    const server = demoServer()
    const rig = makeRealClientRig((call) => {
      if (call.method === 'GET' && call.path === '/api/v1/notes') {
        return errorResponse(500, 'notes.internal_error')
      }
      return server(call)
    })
    await signInWithPassword(rig)
    const view = renderNotes(rig)

    // The ui-kit error empty state stands in for the whole surface: its
    // own icon and copy, distinct from the no-permission suit.
    expect(
      await view.findByText(uiKitZhCN.emptyState.error.title),
    ).toBeInTheDocument()
    expect(
      view.getByText(uiKitZhCN.emptyState.error.description),
    ).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.noPermission.title),
    ).not.toBeInTheDocument()
    expect(
      view.queryByLabelText(zhCN.notes.create.textLabel),
    ).not.toBeInTheDocument()
  })

  it('a refused read denies the gate: the no-permission empty state, no form', async () => {
    // The read refusal is the permission answer: the gate falls closed
    // on the rbac 403, and the denied branch is layout-kit's default --
    // ui-kit's no-permission empty state. No form exists to probe a
    // write with.
    const rig = makeRealClientRig(demoServer({ denyNotesRead: true }))
    await signInWithPassword(rig)
    const view = renderNotes(rig)

    expect(
      await view.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(
      view.getByText(uiKitZhCN.emptyState.noPermission.description),
    ).toBeInTheDocument()
    expect(
      view.queryByLabelText(zhCN.notes.create.textLabel),
    ).not.toBeInTheDocument()
    expect(
      view.queryByRole('button', { name: zhCN.notes.create.submit }),
    ).not.toBeInTheDocument()
  })

  it('a codeless refusal after a served read renders the read-error state, never the stale rows (reference-app-web.md P2-rnweb-1)', async () => {
    // A fresh form: this journey mounts the surface with the read gate
    // open and asserts the served row by find-by-text -- a leftover
    // draft holding the same text as the row would match twice (see
    // the create test).
    clearNotesDraft()
    // The read serves the row once and opens the gate; then the same
    // query is refetched into a refusal that carries NO code -- a raw
    // transport error the api-client never normalized. The failed
    // refetch leaves the earlier `data` in the cache (tanstack v5
    // keeps it), so classifying the failure by its code alone would
    // read "no code, data defined" and keep the stale row under an
    // 'allowed' gate. The error state itself must be the
    // classification: it renders the read-error suit.
    const rig = makeRealClientRig(demoServer({ initialNotes: [NOTE_ONE] }))
    await signInWithPassword(rig)
    const view = renderNotes(rig)

    expect(await view.findByText(NOTE_ONE_TEXT)).toBeInTheDocument()

    // Replace the bound transport with the codeless-failing one, then
    // force the same query to refetch (never a fresh mount, so what
    // the query already cached is exactly what a real mid-session
    // failure would still hold).
    bindRequestFn(UNNORMALIZED_TRANSPORT_FAILURE)
    try {
      await act(async () => {
        await view.queryClient.refetchQueries()
      })

      expect(
        await view.findByText(uiKitZhCN.emptyState.error.title),
      ).toBeInTheDocument()
      expect(view.queryByText(NOTE_ONE_TEXT)).not.toBeInTheDocument()
      expect(
        view.queryByText(uiKitZhCN.emptyState.noPermission.title),
      ).not.toBeInTheDocument()
    } finally {
      // The seam is last-bind-wins and shared across this file's
      // suites: restore the rig's own client whatever happened.
      bindRequestFn(rig.api)
    }
  })

  it('a codeless refusal with no data ever served renders the read-error state, never a permanent pending (reference-app-web.md P2-rnweb-1)', async () => {
    // The failure arrives before any read ever served data -- the
    // shape a host whose transport is broken from the start produces.
    // A gate that only knew coded failures had nothing to classify and
    // parked at 'pending' forever (no answer, no error branch); the
    // error state itself is the failure, whatever it carries.
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    bindRequestFn(UNNORMALIZED_TRANSPORT_FAILURE)
    try {
      const view = renderNotes(rig)

      expect(
        await view.findByText(uiKitZhCN.emptyState.error.title),
      ).toBeInTheDocument()
      expect(
        view.queryByRole('progressbar', {
          name: layoutKitZhCN.routeGuard.pending,
        }),
      ).not.toBeInTheDocument()
      expect(
        view.queryByText(uiKitZhCN.emptyState.noPermission.title),
      ).not.toBeInTheDocument()
    } finally {
      bindRequestFn(rig.api)
    }
  })

  it('surfaces a refused read on the first response under the app bootstrap\'s own query-client policy (reference-app-web.md P2-rnweb-2)', async () => {
    // The production QueryClient (createAppQueryClient) retries
    // nothing: transient retries belong to the api-client transport
    // (its frozen policy already retries the transient classes on
    // idempotent methods before an answer surfaces), and a definitive
    // refusal like the rbac gate's 403 must never burn a doomed
    // multi-second react-query retry window before the gate sees it.
    // The gate answer must land on the FIRST response.
    const rig = makeRealClientRig(demoServer({ denyNotesRead: true }))
    await signInWithPassword(rig)
    const view = renderNotes(rig, { queryClient: createAppQueryClient() })

    expect(
      await view.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    // Exactly one read left the page: the refusal surfaced on the
    // first response, no retries behind it.
    expect(notesGets(rig)).toBe(1)
  })

  it('sends whitespace-only text and renders the server\'s text-required refusal', async () => {
    // A fresh form: the whitespace-only text is the whole point, so
    // nothing may have seeded the field before this journey types.
    clearNotesDraft()
    // The client's required rule only catches the empty string, exactly
    // as the real handler's trim does not -- so whitespace-only text
    // travels, and the server's authority answers with text_required.
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderNotes(rig)
    const user = userEvent.setup()
    await view.findByText(zhCN.notes.list.emptyTitle)

    const input = view.getByLabelText(zhCN.notes.create.textLabel)
    await user.type(input, '   ')
    await user.click(
      view.getByRole('button', { name: zhCN.notes.create.submit }),
    )

    const alert = await view.findByRole('alert')
    expect(alert).toHaveTextContent(zhCN.notes.errors.textRequired)
    expect(noteCreates(rig)).toBe(1)
    expect(input).toHaveValue('   ')
  })

  it('refuses an empty submit client-side with the field\'s required text and no request', async () => {
    // An empty form is the subject: no earlier journey's draft may
    // seed the field.
    clearNotesDraft()
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderNotes(rig)
    const user = userEvent.setup()
    await view.findByText(zhCN.notes.list.emptyTitle)

    await user.click(
      view.getByRole('button', { name: zhCN.notes.create.submit }),
    )

    // The injected required rule answers with ui-kit's own copy.
    expect(
      await view.findByText(uiKitZhCN.form.required),
    ).toBeInTheDocument()
    expect(noteCreates(rig)).toBe(0)
  })

  it('speaks the active language over the served state', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderNotes(rig, { language: 'en-US' })

    expect(await view.findByText(enUS.notes.heading)).toBeInTheDocument()
    expect(
      await view.findByText(enUS.notes.list.emptyTitle),
    ).toBeInTheDocument()
    expect(
      view.getByRole('button', { name: enUS.notes.create.submit }),
    ).toBeInTheDocument()
  })

  it('restores a half-typed draft when the surface remounts (the hash round trip back)', async () => {
    // The store is module-scoped (notes-draft.ts): a journey that
    // typed and left text behind in an earlier test of this file must
    // not leak into this one, so each draft journey starts from a
    // cleared store.
    clearNotesDraft()
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderNotes(rig)
    const user = userEvent.setup()
    await view.findByText(zhCN.notes.list.emptyTitle)

    const input = view.getByLabelText(zhCN.notes.create.textLabel)
    await user.type(input, ROUND_TRIP_NOTE_TEXT)

    // The surface unmounts -- what a hash navigation to another
    // surface does to this view -- and remounts.
    view.unmount()
    const remounted = renderNotes(rig)
    const restored = await remounted.findByLabelText(
      zhCN.notes.create.textLabel,
    )

    // The half-typed text is where the clinician left it: the page
    // never reloaded, and neither should the draft.
    expect(restored).toHaveValue(ROUND_TRIP_NOTE_TEXT)
  })

  it('a successful create consumes the stored draft: a later remount starts from an empty field', async () => {
    clearNotesDraft()
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderNotes(rig)
    const user = userEvent.setup()
    await view.findByText(zhCN.notes.list.emptyTitle)

    const input = view.getByLabelText(zhCN.notes.create.textLabel)
    await user.type(input, NEW_NOTE_TEXT)
    await user.click(
      view.getByRole('button', { name: zhCN.notes.create.submit }),
    )
    await waitFor(() => expect(input).toHaveValue(''))

    // The draft became a note: nothing may remain for the next mount
    // to restore -- the same note would otherwise be created twice.
    view.unmount()
    const remounted = renderNotes(rig)
    const fresh = await remounted.findByLabelText(
      zhCN.notes.create.textLabel,
    )
    expect(fresh).toHaveValue('')
  })
})
