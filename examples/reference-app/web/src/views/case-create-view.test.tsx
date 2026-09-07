/**
 * CasesCreateView contract: the one-page creation flow picks the
 * patient's photos, enters the patient name and submits once -- the
 * block-A shape. The journeys drive a real client bound into the
 * runtime seam over the demo server's cases endpoints, sign in through
 * the real session operation, choose a real File through the picker's
 * hidden input, and pin the observed requests: the photo-upload leg
 * (content_base64 of the very file chosen), the create leg (patient
 * name plus the uploaded photo's object id), and the settle order that
 * makes "submits once" true -- the create button stays disabled while
 * a photo is uploading or failed, and only a fully settled queue lets
 * the single submission through.
 *
 * Refusals render their code's bilingual text from the cases error
 * map: a probe-refused photo shows on its row with Retry and Remove, a
 * refused create (whitespace patient name, an attached photo reused)
 * shows on the form's alert -- never the generic fallback, never a raw
 * key.
 */

import { fireEvent, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { RealCall } from '../test-utils/real-client.js'
import type { RealClientRig } from '../test-utils/real-client.js'
import { describe, expect, it, vi } from 'vitest'
import uiKitZhCN from '../../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { demoServer } from '../test-utils/demo-server.js'
import {
  jsonResponse,
  makeRealClientRig,
  signInWithPassword,
} from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { CasesCreateView } from './case-create-view.js'

/** A small real PNG payload standing in for a patient photograph. */
const PHOTO_BYTES = new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10, 1, 2, 3])

/** A real File the picker hands to the transport. */
function patientPhotoFile(): File {
  return new File([PHOTO_BYTES], 'patient-before.png', {
    type: 'image/png',
  })
}

/** The base64 a FileReader data URL of the photo payload yields. */
const PHOTO_BASE64 = Buffer.from(PHOTO_BYTES).toString('base64')

/** The calls of one method on one path, as observed by the rig. */
function callsOn(
  rig: RealClientRig,
  method: string,
  path: string,
): RealCall[] {
  return rig.calls.filter(
    (call) => call.method === method && call.path === path,
  )
}

/** The JSON body of the first matching call, parsed. */
function bodyOf(call: RealCall | undefined): Record<string, unknown> {
  if (call === undefined) {
    throw new Error('no matching call was observed')
  }
  return JSON.parse(call.body) as Record<string, unknown>
}

/** Renders the create page over a signed-in rig. */
async function renderCreate(rig: RealClientRig) {
  await signInWithPassword(rig)
  const onCreated = vi.fn()
  const view = renderWithAppServices(
    <CasesCreateView onCreated={onCreated} />,
    { session: rig.session, api: rig.api },
  )
  return { ...view, onCreated }
}

/** Picks one file through the picker's hidden input. */
function pickFile(
  view: ReturnType<typeof renderWithAppServices>,
  file: File,
): void {
  const input = view.container.querySelector('input[type="file"]')
  if (input === null) {
    throw new Error('create page carries no file input')
  }
  fireEvent.change(input, { target: { files: [file] } })
}

describe('CasesCreateView', () => {
  it('one submission: pick a photo, name the patient, create the case with the uploaded photo (block A journey)', async () => {
    const rig = makeRealClientRig(demoServer())
    const view = await renderCreate(rig)

    await view.findByRole('heading', { level: 1 })
    await userEvent.type(
      view.getByRole('textbox', { name: zhCN.cases.create.patientNameLabel }),
      'Anna Meyer',
    )
    pickFile(view, patientPhotoFile())

    // The pick started the upload leg: the photo's bytes (this file's
    // very base64) reach the one-shot upload op...
    await waitFor(() => {
      const uploads = callsOn(rig, 'POST', '/api/v1/cases/photos/upload')
      expect(uploads).toHaveLength(1)
      expect(bodyOf(uploads[0]).content_base64).toBe(PHOTO_BASE64)
    })
    // ...and the queue row settles succeeded before the form's submit
    // is enabled (submits once: no create fires while the photo is
    // still uploading).
    const submit = view.getByRole('button', { name: zhCN.cases.create.submit })
    await waitFor(() => expect(submit).toBeEnabled())

    await userEvent.click(submit)

    // The single create submission names the patient and the uploaded
    // photo's object id; the host's onCreated cue fired exactly once.
    await waitFor(() => {
      const creates = callsOn(rig, 'POST', '/api/v1/cases')
      expect(creates).toHaveLength(1)
      const body = bodyOf(creates[0])
      expect(body.patient_name).toBe('Anna Meyer')
      expect(body.photo_object_ids).toEqual(['obj-1'])
    })
    await waitFor(() => expect(view.onCreated).toHaveBeenCalledTimes(1))
  })

  it('holds the submission while a photo is uploading, then lets it through once (submits once)', async () => {
    // The upload leg answers only when the test settles it: the create
    // button must stay disabled (and no create may fire) while the row
    // is uploading.
    let settleUpload!: (response: Response) => void
    const heldUpload = new Promise<Response>((resolve) => {
      settleUpload = resolve
    })
    const base = demoServer()
    const rig = makeRealClientRig((call) => {
      if (call.method === 'POST' && call.path === '/api/v1/cases/photos/upload') {
        return heldUpload
      }
      return base(call)
    })
    const view = await renderCreate(rig)

    await userEvent.type(
      view.getByRole('textbox', { name: zhCN.cases.create.patientNameLabel }),
      'Ben Chen',
    )
    pickFile(view, patientPhotoFile())

    const submit = view.getByRole('button', { name: zhCN.cases.create.submit })
    await waitFor(() => expect(submit).toBeDisabled())
    // A click on the disabled submit must not fire a create: the
    // browser never lets a disabled button activate its form (user-event
    // refuses outright -- pointer-events: none -- so the event is
    // dispatched raw, which jsdom still answers with no submission).
    fireEvent.click(submit)
    expect(callsOn(rig, 'POST', '/api/v1/cases')).toHaveLength(0)

    // The upload settles; the row succeeds and the same submission now
    // goes through -- still exactly one create.
    settleUpload(jsonResponse(201, { object_id: 'obj-1' }))
    await waitFor(() => expect(submit).toBeEnabled())
    await userEvent.click(submit)
    await waitFor(() => expect(view.onCreated).toHaveBeenCalledTimes(1))
    expect(callsOn(rig, 'POST', '/api/v1/cases')).toHaveLength(1)
  })

  it('a probe-refused photo shows its bilingual row error, blocks the submit, and can be removed', async () => {
    const rig = makeRealClientRig(
      demoServer({ casesUploadRefusal: { status: 400, code: 'cases.photo_rejected' } }),
    )
    const view = await renderCreate(rig)

    await userEvent.type(
      view.getByRole('textbox', { name: zhCN.cases.create.patientNameLabel }),
      'Carl Duan',
    )
    pickFile(view, patientPhotoFile())

    const rowError = await view.findByText(zhCN.cases.errors.photoRejected)
    expect(rowError).toBeInTheDocument()
    const submit = view.getByRole('button', { name: zhCN.cases.create.submit })
    await waitFor(() => expect(submit).toBeDisabled())

    // Removing the failed row lifts the block; the form may submit a
    // case with no photos (intake-first) -- and does. (The queue's one
    // failed row makes the built-in Remove button unambiguous.)
    await userEvent.click(
      view.getByRole('button', { name: uiKitZhCN.fileUploader.actionRemove }),
    )
    await waitFor(() => expect(submit).toBeEnabled())
    await userEvent.click(submit)
    await waitFor(() => expect(view.onCreated).toHaveBeenCalledTimes(1))
    const create = bodyOf(callsOn(rig, 'POST', '/api/v1/cases')[0])
    expect(create.photo_object_ids).toBeUndefined()
  })

  it('a refused create (whitespace-only name) renders the server code text on the form', async () => {
    const rig = makeRealClientRig(demoServer())
    const view = await renderCreate(rig)

    // Whitespace passes the client's required rule (only the empty
    // string trips it) -- the server's trim-and-refuse is the real gate,
    // and its text-required answer must render, never the fallback.
    await userEvent.type(
      view.getByRole('textbox', { name: zhCN.cases.create.patientNameLabel }),
      '   ',
    )
    const submit = view.getByRole('button', { name: zhCN.cases.create.submit })
    await userEvent.click(submit)
    await waitFor(() => expect(view.getByRole('alert')).toHaveTextContent(
      zhCN.cases.errors.patientNameRequired,
    ))
    expect(view.getByRole('alert')).not.toHaveTextContent(
      zhCN.cases.errors.unknown,
    )
    expect(view.onCreated).not.toHaveBeenCalled()
  })

  it('a refused create (photo already attached to another case) renders its conflict text', async () => {
    const rig = makeRealClientRig(
      demoServer({
        casesCreateRefusal: {
          status: 409,
          code: 'cases.photo_already_attached',
        },
      }),
    )
    const view = await renderCreate(rig)

    await userEvent.type(
      view.getByRole('textbox', { name: zhCN.cases.create.patientNameLabel }),
      'Anna Meyer',
    )
    pickFile(view, patientPhotoFile())
    const submit = view.getByRole('button', { name: zhCN.cases.create.submit })
    await waitFor(() => expect(submit).toBeEnabled())
    await userEvent.click(submit)

    await waitFor(() =>
      expect(view.getByRole('alert')).toHaveTextContent(
        zhCN.cases.errors.photoAlreadyAttached,
      ),
    )
    expect(view.onCreated).not.toHaveBeenCalled()
  })
})
