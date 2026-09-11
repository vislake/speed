/**
 * SimulationShareAction contract -- the share journey at the
 * component level, driven the same way the simulation panel suite
 * drives its own: the case detail page mounted over the real api-client rig
 * answering from the demo responder's genuine Response objects. A
 * completed simulation's comparison carries the share action; clicking
 * it mints the BEFORE/AFTER pair -- one share per half of the
 * comparison the patient page renders, the original photo and the
 * simulation's output object -- through the owner-facing route and puts
 * the patient link (a real, copyable, absolute URL carrying both
 * halves' tokens) in a read-only textbox with the expiry the server
 * resolved beside it. A mint where either half is refused shows that
 * refusal's bilingual text and revokes the half that did mint -- a
 * failed share action leaves no live, unhanded-out link behind -- while
 * refusals resolve through the share action's reachable-error whitelist
 * to human text, including the rbac gate's denial a practice member
 * without sharing:create genuinely draws and the module's creation
 * rate limit.
 */

import { waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { CasesCase } from '../app-api/index.js'
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

/** The share-action posts the rig recorded. */
function shareMints(
  view: Awaited<ReturnType<typeof renderCaseDetailForShare>>,
): ReturnType<typeof makeRealClientRig>['calls'] {
  return view.calls.filter(
    (call) =>
      call.method === 'POST' && call.path === '/api/v1/sharing/shares',
  )
}

/** The compensation revokes the rig recorded. */
function shareRevokes(
  view: Awaited<ReturnType<typeof renderCaseDetailForShare>>,
): ReturnType<typeof makeRealClientRig>['calls'] {
  return view.calls.filter(
    (call) =>
      call.method === 'POST' &&
      call.path.startsWith('/api/v1/sharing/shares/') &&
      call.path.endsWith('/revoke'),
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
      expect(
        view.queryByRole('button', { name: 'Share with patient' }),
      ).not.toBeInTheDocument()

      // The photo's first generation (the panel's automatic default
      // preview) completes into the comparison.
      const comparison = await view.findByRole(
        'region',
        { name: 'Before and after' },
        { timeout: 8000 },
      )

      // The comparison's share action mints the PAIR -- one share for
      // the original photo and one for the newest succeeded output --
      // through the owner-facing route: two POSTs carrying the bearer
      // and each half's object id.
      await userEvent.click(
        within(comparison).getByRole('button', { name: 'Share with patient' }),
      )

      const link = await view.findByRole('textbox', { name: 'Share link' })
      const expected = new URL(
        '/#/share/share-token-1/share-token-2',
        window.location.href,
      ).href
      expect(
        link,
        'the share control must produce an absolute, copyable URL carrying both halves',
      ).toHaveValue(expected)

      // The expiry the server resolved is shown beside the link -- with
      // the date actually substituted, never the literal "{date}" a
      // single-braced bundle value renders (i18next interpolates
      // {{name}}, so cases.share.expiresOn must carry double braces in
      // both languages).
      const expiry = view.getByText(/^Link expires /)
      expect(
        expiry.textContent,
        'the expiry line renders the placeholder itself -- cases.share.expiresOn uses single braces and i18next does not interpolate them',
      ).not.toContain('{date}')
      expect(
        expiry.textContent,
        'the expiry line carries no real date, so it tells a practice nothing about how long the link stays usable',
      ).toMatch(/\d/)

      const mints = shareMints(view)
      expect(mints).toHaveLength(2)
      expect(mints[0]?.authorization).toMatch(/^Bearer /)
      expect(JSON.parse(mints[0]?.body ?? '{}')).toEqual({
        resourceRef: 'photo-1',
      })
      expect(JSON.parse(mints[1]?.body ?? '{}')).toEqual({
        resourceRef: 'sim-out-job-1',
      })
      // The share action's own compensation never fires on the happy
      // path: nothing was refused, so nothing is revoked.
      expect(shareRevokes(view)).toHaveLength(0)
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
      const comparison = await view.findByRole(
        'region',
        { name: 'Before and after' },
        { timeout: 8000 },
      )

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
      // Both halves of the pair drew the same gate; nothing to
      // compensate.
      expect(shareMints(view)).toHaveLength(2)
      expect(shareRevokes(view)).toHaveLength(0)
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
      const comparison = await view.findByRole(
        'region',
        { name: 'Before and after' },
        { timeout: 8000 },
      )
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
      // Both halves refused; nothing minted, nothing to revoke.
      expect(shareRevokes(view)).toHaveLength(0)

      // Retry mints again (and is refused again by the same answer):
      // a refusal is never a dead end.
      const actionAgain = view.getByRole('button', {
        name: 'Share with patient',
      })
      await userEvent.click(actionAgain)
      await waitFor(() => expect(shareMints(view)).toHaveLength(4))
      expect(shareRevokes(view)).toHaveLength(0)
    } finally {
      blobDouble.restore()
    }
  })

  it('revokes the half of a pair that did mint when the other half is refused', async () => {
    // The half-refused pair: the before half mints, the after half is
    // refused by the create rate limit. All-or-nothing means no link --
    // the refusal's own text is shown -- and the minted half is revoked
    // again on the spot, so a failed share action leaves no live,
    // unhanded-out link behind.
    const blobDouble = installBlobURLDouble()
    try {
      const view = await renderCaseDetailForShare({
        initialCases: [caseWithPhoto()],
        sharesCreateSecondRefusal: {
          status: 429,
          code: 'sharing.rate_limited',
        },
      })
      const comparison = await view.findByRole(
        'region',
        { name: 'Before and after' },
        { timeout: 8000 },
      )
      await userEvent.click(
        within(comparison).getByRole('button', { name: 'Share with patient' }),
      )

      const refusal = await view.findByRole('alert')
      expect(refusal.textContent).toBe(
        'Too many share links were created just now. Wait a moment and try again.',
      )
      expect(
        view.queryByRole('textbox', { name: 'Share link' }),
      ).not.toBeInTheDocument()

      // The before half (the run's first create) minted as share-1 and
      // was revoked again as the compensation leg.
      const revokes = shareRevokes(view)
      expect(revokes).toHaveLength(1)
      expect(revokes[0]?.path).toBe(
        '/api/v1/sharing/shares/share-1/revoke',
      )
      expect(revokes[0]?.authorization).toMatch(/^Bearer /)

      // Retryable: the next attempt mints a fresh pair (the
      // second-create refusal scripts only the run's second create).
      await userEvent.click(
        view.getByRole('button', { name: 'Share with patient' }),
      )
      const link = await view.findByRole('textbox', { name: 'Share link' })
      const expected = new URL(
        '/#/share/share-token-2/share-token-3',
        window.location.href,
      ).href
      expect(link).toHaveValue(expected)
    } finally {
      blobDouble.restore()
    }
  })
})
