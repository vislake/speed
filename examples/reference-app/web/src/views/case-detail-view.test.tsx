/**
 * CaseDetailView contract: one case renders what the server answers --
 * its patient record and its photos, the photo bytes fetched through
 * the generated photo-content operation and rendered from a blob URL
 * the page owns and revokes.
 *
 * jsdom implements neither URL.createObjectURL nor revokeObjectURL, so
 * the suite installs counting stand-ins (the app's real browser calls
 * the real ones); the assertions pin the property the blob-URL pattern
 * exists for: the img's src is an object URL created from a Blob of
 * exactly the served bytes and media type, and the URL is revoked when
 * the photo leaves the page. A photo whose read fails renders its
 * code's bilingual text (photo_not_found when the object is gone),
 * never a broken image; a case that cannot be found renders the error
 * state behind a working back control.
 */

import { waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { CasesCase } from '@speed/api-sdk'
import { describe, expect, it, vi } from 'vitest'
import uiKitZhCN from '../../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { demoServer } from '../test-utils/demo-server.js'
import { makeRealClientRig, signInWithPassword } from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { CaseDetailView } from './case-detail-view.js'

/** The fixed demo epoch the demo server's case answers carry. */
const DEMO_CREATED_AT = '2026-09-04T00:00:00Z'

/** A case with one attached photo, as the demo server serves it. */
function caseWithPhoto(photoObjectID: string): CasesCase {
  return {
    id: 'case-1',
    patient_name: 'Anna Meyer',
    patient_ref: 'CH-1001',
    creator_user_id: 'user-1',
    created_at: DEMO_CREATED_AT,
    photos: [{ object_id: photoObjectID }],
  }
}

/** The demo content payload decoded: 'photo-bytes' */
const DEMO_PHOTO_BYTES = 'photo-bytes'

describe('CaseDetailView', () => {
  it('renders the case with its photo visible: a blob URL over the served bytes, revoked on cleanup', async () => {
    // The blob-URL stand-ins, counting what the page creates and
    // revokes (jsdom has neither; the real browser calls the real
    // pair). The originals are restored afterwards.
    const originalCreateObjectURL = URL.createObjectURL
    const originalRevokeObjectURL = URL.revokeObjectURL
    const createObjectURL = vi.fn<(blob: Blob) => string>(() => 'blob:case-photo-1')
    const revokeObjectURL = vi.fn<(url: string) => void>()
    URL.createObjectURL = createObjectURL as typeof URL.createObjectURL
    URL.revokeObjectURL = revokeObjectURL as typeof URL.revokeObjectURL

    try {
      const rig = makeRealClientRig(
        demoServer({ initialCases: [caseWithPhoto('photo-1')] }),
      )
      await signInWithPassword(rig)
      const view = renderWithAppServices(
        <CaseDetailView caseId="case-1" onBack={vi.fn()} />,
        { session: rig.session, api: rig.api },
      )

      // The photo is visible on the case: an image whose accessible
      // name names the patient photo, sourced from the object URL.
      const image = await view.findByRole('img', {
        name: zhCN.cases.detail.photoAlt.replace('{{index}}', '1'),
      })
      expect(image).toHaveAttribute('src', 'blob:case-photo-1')

      // The URL was created from a Blob of exactly the served bytes and
      // the media type the server's probe assigned.
      await waitFor(() => expect(createObjectURL).toHaveBeenCalledTimes(1))
      const firstCall = createObjectURL.mock.calls[0]
      if (firstCall === undefined) {
        throw new Error('createObjectURL was never called')
      }
      const blob = firstCall[0]
      expect(blob.type).toBe('image/png')
      expect(await blob.text()).toBe(DEMO_PHOTO_BYTES)

      // The patient record renders alongside the photo.
      expect(view.getByText('Anna Meyer')).toBeInTheDocument()

      // The URL the page created is revoked when the photo leaves the
      // page: a session that opens and closes cases never leaks blob
      // URLs.
      view.unmount()
      await waitFor(() =>
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:case-photo-1'),
      )
    } finally {
      URL.createObjectURL = originalCreateObjectURL
      URL.revokeObjectURL = originalRevokeObjectURL
    }
  })

  it('a photo whose content read fails renders its code text, never a broken image', async () => {
    const rig = makeRealClientRig(
      demoServer({
        initialCases: [caseWithPhoto('photo-1')],
        casesPhotoContentRefusal: {
          status: 404,
          code: 'cases.photo_not_found',
        },
      }),
    )
    await signInWithPassword(rig)
    const view = renderWithAppServices(
      <CaseDetailView caseId="case-1" onBack={vi.fn()} />,
      { session: rig.session, api: rig.api },
    )

    await view.findByText('Anna Meyer')
    await waitFor(() =>
      expect(
        view.getByText(zhCN.cases.errors.photoNotFound),
      ).toBeInTheDocument(),
    )
    expect(view.queryByRole('img')).not.toBeInTheDocument()
  })

  it('a case the tenant cannot read renders the read-error state behind a working back control', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const onBack = vi.fn()
    const view = renderWithAppServices(
      <CaseDetailView caseId="case-missing" onBack={onBack} />,
      { session: rig.session, api: rig.api },
    )

    // The unknown case answers cases.not_found: the error empty state.
    await view.findByText(uiKitZhCN.emptyState.error.title)
    // The back control stays reachable (a dead-end page is not a
    // working page) and reports its cue upward.
    await userEvent.click(
      view.getByRole('button', { name: zhCN.cases.detail.backToCases }),
    )
    expect(onBack).toHaveBeenCalledTimes(1)
  })
})
