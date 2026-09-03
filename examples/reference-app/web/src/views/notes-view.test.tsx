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
import type { NotesNote } from '@speed/api-sdk'
import { describe, expect, it } from 'vitest'
import layoutKitZhCN from '../../../../../web/packages/layout-kit/src/locales/zh-CN.json' with { type: 'json' }
import uiKitZhCN from '../../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { demoServer } from '../test-utils/demo-server.js'
import type { RealClientRig } from '../test-utils/real-client.js'
import { makeRealClientRig, signInWithPassword } from '../test-utils/real-client.js'
import type { RenderWithProvidersOptions } from '../test-utils/render.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { NotesView } from './notes-view.js'

/** The fixed demo epoch the demo server's note answers carry. */
const DEMO_CREATED_AT = '2026-09-04T00:00:00Z'

/** The served note texts travel as their own constants (a note's text
 * is optional on the spec type, but a served note always carries one). */
const NOTE_ONE_TEXT = 'First note'
const NOTE_TWO_TEXT = 'Second note'
const NEW_NOTE_TEXT = 'Written in the browser'
const DENIED_NOTE_TEXT = 'A note that must not land'

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
    // the form can answer it.
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

  it('sends whitespace-only text and renders the server\'s text-required refusal', async () => {
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
})
