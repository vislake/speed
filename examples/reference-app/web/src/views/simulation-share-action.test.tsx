/**
 * SimulationShareAction contract -- the block-C journey at the
 * component level, driven the same way the block-B panel suite drives
 * its own: the case detail page mounted over the real api-client rig
 * answering from the demo responder's genuine Response objects. A
 * completed simulation's comparison carries the share action; clicking
 * it mints a share for the simulation's OUTPUT object through the
 * owner-facing route and puts the patient link -- a real, copyable,
 * absolute URL -- in a read-only textbox with the expiry the server
 * resolved beside it. Refusals resolve through the share action's
 * reachable-error whitelist to human text, including the rbac gate's
 * denial a practice member without sharing:create genuinely draws and
 * the module's creation rate limit.
 */

import { waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { CasesCase } from '@speed/api-sdk'
import { describe, expect, it, vi } from 'vitest'
import { DEMO_READER_IDENTIFIER } from '../test-utils/demo-server.js'
import { demoServer } from '../test-utils/demo-server.js'
import { makeRealClientRig, signInWithPassword } from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { CaseDetailView } from './case-detail-view.js'

const DEMO_CREATED_AT = '2026-09-04T00:00:00Z'

/** A case with one attached photo, as the demo server serves it. */
function caseWithPhoto(): CasesCase {
  return {
    id: 'case-1',
    patient_name: 'Anna Meyer',
    patient_ref: 'CH-1001',
    creator_user_id: 'user-1',
    created_at: DEMO_CREATED_AT,
    photos: [{ object_id: 'photo-1' }],
  }
}

/** The blob-URL stand-in (jsdom implements neither createObjectURL nor
 * revokeObjectURL): the page's image reads need it, exactly as the
 * panel suite's copy of this double provides it there. */
function installBlobURLDouble(): { restore: () => void } {
  const originalCreate = URL.createObjectURL
  const originalRevoke = URL.revokeObjectURL
  URL.createObjectURL = vi.fn<typeof URL.createObjectURL>(
    () => 'blob:share-journey',
  )
  URL.revokeObjectURL = vi.fn<typeof URL.revokeObjectURL>()
  return {
    restore: () => {
      URL.createObjectURL = originalCreate
      URL.revokeObjectURL = originalRevoke
    },
  }
}

/** Mounts the case detail page -- the surface the share action lives
 * on -- over the real-client rig and signs the given account in. */
async function renderCaseDetailForShare(
  options: Parameters<typeof demoServer>[0] = {},
  identifier: string = 'owner@example.test',
): Promise<
  ReturnType<typeof renderWithAppServices> & {
    readonly calls: ReturnType<typeof makeRealClientRig>['calls']
  }
> {
  const rig = makeRealClientRig(demoServer(options))
  if (identifier === DEMO_READER_IDENTIFIER) {
    await rig.session.loginWithPassword({
      identifier,
      password: 'correct-horse-battery-staple',
    })
  } else {
    await signInWithPassword(rig)
  }
  const view = renderWithAppServices(
    <CaseDetailView caseId="case-1" onBack={vi.fn()} />,
    { session: rig.session, api: rig.api },
    { language: 'en-US' },
  )
  return { ...view, calls: rig.calls }
}

/** Drives one generation to completion on the rendered case page: the
 * panel's simulate button through the job's deterministic progression
 * to the succeeded comparison -- the state the share action requires. */
async function completeOneSimulation(view: Awaited<ReturnType<typeof renderCaseDetailForShare>>): Promise<void> {
  await view.findByRole('button', { name: 'Simulate smile' })
  await userEvent.click(view.getByRole('button', { name: 'Simulate smile' }))
  await view.findByRole(
    'region',
    { name: 'Before and after' },
    { timeout: 8000 },
  )
}

describe('SimulationShareAction', () => {
  it('runs the journey: a completed simulation is shared as a visible, copyable patient link', async () => {
    const blobDouble = installBlobURLDouble()
    try {
      const view = await renderCaseDetailForShare({
        initialCases: [caseWithPhoto()],
      })

      // No share control exists before a simulation completes.
      await view.findByRole('button', { name: 'Simulate smile' })
      expect(
        view.queryByRole('button', { name: 'Share with patient' }),
      ).not.toBeInTheDocument()

      await completeOneSimulation(view)

      // The comparison's share action mints the link for the newest
      // succeeded simulation's OUTPUT object: one POST through the
      // owner-facing route, carrying the bearer and the output id.
      const comparison = view.getByRole('region', { name: 'Before and after' })
      await userEvent.click(
        within(comparison).getByRole('button', { name: 'Share with patient' }),
      )

      const link = await view.findByRole('textbox', { name: 'Share link' })
      const expected = new URL('/#/share/share-token-1', window.location.href)
        .href
      expect(
        link,
        'the share control must produce an absolute, copyable URL',
      ).toHaveValue(expected)

      // The expiry the server resolved is shown beside the link.
      expect(view.getByText(/^Link expires /)).toBeInTheDocument()

      const mints = view.calls.filter(
        (call) =>
          call.method === 'POST' &&
          call.path === '/api/v1/sharing/shares',
      )
      expect(mints).toHaveLength(1)
      const mint = mints[0]
      if (mint === undefined) {
        throw new Error('the share mint call was never recorded')
      }
      expect(mint.authorization).toMatch(/^Bearer /)
      expect(JSON.parse(mint.body)).toEqual({
        resourceRef: 'sim-out-job-1',
      })
    } finally {
      blobDouble.restore()
    }
  })

  it('refuses a colleague without sharing:create through the rbac gate text, never a raw code', async () => {
    // The read-only member's grant asymmetry, mirrored from the Go
    // suite: its cases read is served, its share create is refused by
    // the rbac gate -- the exact answers a practice member whose role
    // holds no sharing:create draws from the real route.
    const blobDouble = installBlobURLDouble()
    try {
      const view = await renderCaseDetailForShare(
        { initialCases: [caseWithPhoto()], reader: true },
        DEMO_READER_IDENTIFIER,
      )
      await completeOneSimulation(view)

      const comparison = view.getByRole('region', { name: 'Before and after' })
      await userEvent.click(
        within(comparison).getByRole('button', { name: 'Share with patient' }),
      )

      const refusal = await view.findByRole('alert')
      expect(refusal.textContent).toBe(
        "You don't have permission to share results with patients. Ask an owner of your clinic.",
      )
      expect(
        view.queryByRole('textbox', { name: 'Share link' }),
      ).not.toBeInTheDocument()
      // The action stays retryable after a refusal: the control is
      // still there for the colleague who gains the permission.
      expect(
        view.getByRole('button', { name: 'Share with patient' }),
      ).toBeEnabled()
    } finally {
      blobDouble.restore()
    }
  })

  it('renders a rate-limited mint through its code text and stays retryable', async () => {
    const blobDouble = installBlobURLDouble()
    try {
      const view = await renderCaseDetailForShare({
        initialCases: [caseWithPhoto()],
        sharesCreateRefusal: { status: 429, code: 'sharing.rate_limited' },
      })
      await completeOneSimulation(view)

      const comparison = view.getByRole('region', { name: 'Before and after' })
      const action = within(comparison).getByRole('button', {
        name: 'Share with patient',
      })
      await userEvent.click(action)

      const refusal = await view.findByRole('alert')
      await waitFor(() =>
        expect(refusal.textContent).toBe(
          'Too many share links were created just now. Wait a moment and try again.',
        ),
      )

      // Retry mints again (and is refused again by the same answer):
      // a refusal is never a dead end.
      const actionAgain = view.getByRole('button', {
        name: 'Share with patient',
      })
      await userEvent.click(actionAgain)
      await waitFor(() =>
        expect(
          view.calls.filter(
            (call) =>
              call.method === 'POST' && call.path === '/api/v1/sharing/shares',
          ),
        ).toHaveLength(2),
      )
    } finally {
      blobDouble.restore()
    }
  })
})
