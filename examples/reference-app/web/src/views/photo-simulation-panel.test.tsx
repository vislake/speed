/**
 * PhotoSimulationPanel contract -- the block-B journey at the
 * component level: a case photo offers the documented smile option set
 * by name, a generation started from the chosen options reports its
 * honest asynchronous progress and, once it completes, the before/after
 * comparison renders the result beside the original -- both over blob
 * URLs the page owns and revokes. Journeys drive the real composed
 * surface: the panel mounted the way the case detail page mounts it,
 * through the real api-client rig over the demo responder's genuine
 * Response objects (test-utils/demo-server.ts's deterministic
 * pending -> running -> succeeded progression), never a mocked hook.
 */

import { waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { CasesCase } from '@speed/api-sdk'
import { describe, expect, it, vi } from 'vitest'
import { demoServer } from '../test-utils/demo-server.js'
import { makeRealClientRig, signInWithPassword } from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { CaseDetailView } from './case-detail-view.js'

/** The fixed demo epoch the demo server's answers carry. */
const DEMO_CREATED_AT = '2026-09-04T00:00:00Z'

/** The automatic-preview cost disclosure line as the en-US bundle
 * renders it for one generation's price. */
const AUTO_PREVIEW_COST_NOTICE =
  'The automatic first preview of a photo with no simulation yet costs 10 credits'

/** Every automatic or manual generation this responder observed: a POST
 * to the smile-simulation simulate route. */
function simulatePosts(
  calls: ReturnType<typeof makeRealClientRig>['calls'],
): number {
  return calls.filter(
    (call) =>
      call.method === 'POST' &&
      call.path.startsWith('/api/v1/smile-simulation/'),
  ).length
}

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

/** Mounts the case detail page -- the surface a clinic user opens a
 * case on -- over the real-client rig and signs the owner in. */
async function renderCaseDetail(
  options: Parameters<typeof demoServer>[0] = {},
): Promise<
  ReturnType<typeof renderWithAppServices> & {
    readonly calls: ReturnType<typeof makeRealClientRig>['calls']
    /** The whole rig, so a journey can mount the same case again over
     * the same responder. */
    readonly rig: ReturnType<typeof makeRealClientRig>
  }
> {
  const rig = makeRealClientRig(demoServer(options))
  await signInWithPassword(rig)
  const view = renderWithAppServices(
    <CaseDetailView caseId="case-1" onBack={vi.fn()} />,
    { session: rig.session, api: rig.api },
    { language: 'en-US' },
  )
  return { ...view, calls: rig.calls, rig }
}

/** The blob-URL stand-ins, installed for one test and restored after:
 * jsdom implements neither URL.createObjectURL nor revokeObjectURL, so
 * the suite substitutes counting doubles and records every blob it was
 * asked to materialize. Returns the recorded blobs and revocations. */
function installBlobURLDoubles(): {
  readonly blobs: Blob[]
  readonly revoked: string[]
  restore: () => void
} {
  const originalCreate = URL.createObjectURL
  const originalRevoke = URL.revokeObjectURL
  const blobs: Blob[] = []
  const revoked: string[] = []
  URL.createObjectURL = vi.fn<(blob: Blob) => string>((blob) => {
    blobs.push(blob)
    return `blob:sim-${blobs.length}`
  }) as typeof URL.createObjectURL
  URL.revokeObjectURL = vi.fn<(url: string) => void>((url) => {
    revoked.push(url)
  }) as typeof URL.revokeObjectURL
  return {
    blobs,
    revoked,
    restore: () => {
      URL.createObjectURL = originalCreate
      URL.revokeObjectURL = originalRevoke
    },
  }
}

describe('PhotoSimulationPanel', () => {
  it('offers the documented smile options by name and generates only from them', async () => {
    const view = await renderCaseDetail({ initialCases: [caseWithPhoto()] })

    // The style ladder, each option addressable by its own accessible
    // name (the same names the acceptance gate queries).
    const styleGroup = await view.findByRole('group', { name: 'Smile style' })
    for (const name of ['Subtle', 'Natural', 'Bright'] as const) {
      expect(
        within(styleGroup).getByRole('radio', { name }),
        `the ${name} smile style must be offerable`,
      ).toBeInTheDocument()
    }
    expect(within(styleGroup).getByRole('radio', { name: 'Natural' })).toBeChecked()

    // The shade ladder.
    const shadeGroup = view.getByRole('group', { name: 'Tooth shade' })
    for (const name of ['Natural', 'White', 'Ultra-white'] as const) {
      expect(
        within(shadeGroup).getByRole('radio', { name }),
        `the ${name} tooth shade must be offerable`,
      ).toBeInTheDocument()
    }
    expect(within(shadeGroup).getByRole('radio', { name: 'Natural' })).toBeChecked()

    // The adjustable strength: a labelled slider whose value reads
    // back, full strength by default (the service's documented default).
    expect(view.getByRole('slider', { name: 'Strength' })).toBeInTheDocument()
    expect(view.getByText('100% strength')).toBeInTheDocument()

    // The surface offers no option outside the documented vocabulary:
    // the transmitted set is exactly one style, one shade, one strength.
    await userEvent.click(within(styleGroup).getByRole('radio', { name: 'Bright' }))
    await userEvent.click(within(shadeGroup).getByRole('radio', { name: 'Ultra-white' }))
    const slider = view.getByRole('slider', { name: 'Strength' })
    slider.focus()
    await userEvent.keyboard('{ArrowLeft}{ArrowLeft}')
    await waitFor(() =>
      expect(view.getByText('90% strength')).toBeInTheDocument(),
    )
  })

  it('runs the journey: generation reports its honest progress, then the before/after comparison appears', async () => {
    const blobDoubles = installBlobURLDoubles()
    try {
      const view = await renderCaseDetail({ initialCases: [caseWithPhoto()] })

      // A freshly opened photo with no simulation attempt gets the
      // panel's one automatic default generation: the comparison
      // appears without a click -- the shape the acceptance journey
      // rides on. Once it has, a generation from a chosen option set
      // is an ordinary second one: choose a non-default option set
      // and generate, so the generation that comes back is
      // demonstrably the one chosen, with the in-flight state saying
      // so honestly along the way.
      await view.findByRole('button', { name: 'Simulate smile' })
      const comparison = await view.findByRole(
        'region',
        { name: 'Before and after' },
        { timeout: 8000 },
      )
      await waitFor(
        () => expect(within(comparison).getAllByRole('img')).toHaveLength(2),
        { timeout: 8000 },
      )
      await userEvent.click(view.getByRole('radio', { name: 'Bright' }))
      await userEvent.click(view.getByRole('radio', { name: 'Ultra-white' }))
      await userEvent.click(view.getByRole('button', { name: 'Simulate smile' }))

      // Generation is asynchronous, and the page says so honestly: a
      // status live region appears and reports the job's progression
      // (starting, then the demo responder's queued/generating states)
      // rather than a silent screen or a fake spinner.
      const status = await view.findByRole('status')
      await waitFor(() =>
        expect(status.textContent).toMatch(
          /Starting the smile simulation|The smile simulation is queued|Generating the smile simulation|hit a snag and is retrying/,
        ),
      )

      // The second result supersedes the comparison while the region
      // keeps showing a genuine pair (the original renders through its
      // own content read; the result's image is waited for as part of
      // the pair, so both halves are checked together).
      await waitFor(() =>
        expect(within(comparison).getAllByRole('img')).toHaveLength(2),
      )
      for (const image of within(comparison).getAllByRole('img')) {
        expect(image.getAttribute('src')).toMatch(/^blob:/)
      }
      expect(
        within(comparison).getByRole('img', { name: 'Patient photo 1' }),
      ).toBeInTheDocument()
      expect(
        within(comparison).getByRole('img', { name: 'Simulated smile' }),
      ).toBeInTheDocument()

      // The pair is genuinely before/after: two different payloads over
      // the wire (the demo's photo bytes vs its simulation-result
      // bytes), each materialized into a blob of the served media type.
      // The original appears twice on the page (the photo column and
      // the comparison), so its bytes arrive in two blobs; the
      // automatic preview's result and the chosen-options result each
      // arrive twice -- once for the comparison's image and once for
      // its download link (the superseded pair's blobs are revoked
      // when the comparison switches, but their materialization is
      // recorded).
      await waitFor(() => expect(blobDoubles.blobs).toHaveLength(6), {
        timeout: 8000,
      })
      const texts = await Promise.all(
        blobDoubles.blobs.map((blob) => blob.text()),
      )
      expect(texts.sort()).toEqual([
        'photo-bytes',
        'photo-bytes',
        'simulation-result-bytes',
        'simulation-result-bytes',
        'simulation-result-bytes',
        'simulation-result-bytes',
      ])
      for (const blob of blobDoubles.blobs) {
        expect(blob.type).toBe('image/png')
      }

      // The attempt history names the chosen options back, beside the
      // outcome's own status text.
      expect(view.getAllByText('Generated').length).toBeGreaterThan(0)
      // The summary appears both as the comparison's caption and as the
      // attempt row's own line.
      expect(
        view.getAllByText('Bright smile · Ultra-white teeth · 100% strength')
          .length,
      ).toBeGreaterThan(0)

      // The URLs this page created are revoked when it unmounts: a
      // session that opens and closes cases never leaks blob URLs.
      view.unmount()
      await waitFor(() =>
        expect(blobDoubles.revoked).toHaveLength(blobDoubles.blobs.length),
      )
    } finally {
      blobDoubles.restore()
    }
  })

  it('says what the completed generation cost, beside the comparison (block D)', async () => {
    // The block-D acceptance shape: where the generation happened, the
    // page must say what it cost -- a number, not just the idea of a
    // cost (a pay-per-use product that spends silently is one nobody
    // trusts). The caption reads the displayed cost from the same
    // mirrored constant the simulation-cost-lockstep suite pins to the
    // service's own charge. A freshly opened photo's first generation
    // is the panel's automatic default one (the block-C shape, and the
    // button stays disabled while it runs), so the caption is awaited
    // beside the auto-run's comparison -- the completed generation is
    // the one that must say what it cost.
    const blobDoubles = installBlobURLDoubles()
    try {
      const view = await renderCaseDetail({ initialCases: [caseWithPhoto()] })
      const comparison = await view.findByRole(
        'region',
        { name: 'Before and after' },
        { timeout: 8000 },
      )
      await waitFor(() =>
        expect(within(comparison).getAllByRole('img')).toHaveLength(2),
      )
      // The cost caption under the pair carries the figure itself --
      // the amount, in the surface language, never just the word.
      expect(
        await view.findByText('This simulation cost 10 credits'),
      ).toBeInTheDocument()
    } finally {
      blobDoubles.restore()
    }
  })

  it('connects the automatic first preview to its cost before the run spends it (the product-disclosure gate)', async () => {
    // The product-disclosure shape: a freshly opened photo with no
    // simulation attempt gets the panel's one automatic generation,
    // and that generation spends credits -- so the surface must carry
    // the price of the automatic preview BEFORE the run fires and
    // while it is in flight, never only in the after-the-fact cost
    // line under a completed comparison. (Fails before the disclosure
    // existed: the auto-run fired with no cost connected to it on the
    // surface at all.)
    const view = await renderCaseDetail({ initialCases: [caseWithPhoto()] })

    // The disclosure is on the surface from the moment the
    // enumeration answers empty -- the same answer that lets the
    // auto-run fire, which is what makes the line precede the run
    // rather than report it.
    await waitFor(
      () => expect(view.getByText(AUTO_PREVIEW_COST_NOTICE)).toBeInTheDocument(),
      { timeout: 8000 },
    )

    // The run's own simulate call is recorded with the disclosure
    // already standing, and the disclosure stays beside the in-flight
    // status region while the job runs -- the spend is never silent.
    await waitFor(
      () => expect(simulatePosts(view.calls)).toBe(1),
      { timeout: 8000 },
    )
    expect(view.getByText(AUTO_PREVIEW_COST_NOTICE)).toBeInTheDocument()
    await view.findByRole('status')
    expect(view.getByText(AUTO_PREVIEW_COST_NOTICE)).toBeInTheDocument()
  })

  it('renders no automatic-preview cost promise on a photo that already has simulations', async () => {
    // The disclosure's honesty bound: the auto-run is one-shot per
    // photo, so a photo whose enumeration shows an attempt will never
    // be auto-run again -- a line promising an automatic preview's
    // spend there would be a false promise. Once the photo's own
    // automatic preview has completed (the enumeration carries it),
    // the disclosure withdraws, and reopening the same case over the
    // same responder neither re-runs the auto-run nor shows the line.
    const view = await renderCaseDetail({ initialCases: [caseWithPhoto()] })

    // The automatic preview completes into the comparison...
    const comparison = await view.findByRole(
      'region',
      { name: 'Before and after' },
      { timeout: 8000 },
    )
    await waitFor(() =>
      expect(within(comparison).getAllByRole('img')).toHaveLength(2),
    )
    // ...and with a simulation on the record the disclosure is gone.
    expect(view.queryByText(AUTO_PREVIEW_COST_NOTICE)).toBeNull()
    expect(simulatePosts(view.calls)).toBe(1)

    // Opening the same case again is a fresh mount over the same
    // responder: the enumeration answers with the attempt on the
    // record, so the panel stays hands-off and the surface promises
    // no spend -- no disclosure line, and no second automatic
    // simulate call (the window below is the settle window in which
    // an auto-run would have fired had the photo still had none).
    view.unmount()
    const reopened = renderWithAppServices(
      <CaseDetailView caseId="case-1" onBack={vi.fn()} />,
      { session: view.rig.session, api: view.rig.api },
      { language: 'en-US' },
    )
    const again = await reopened.findByRole(
      'region',
      { name: 'Before and after' },
      { timeout: 8000 },
    )
    await waitFor(() =>
      expect(within(again).getAllByRole('img')).toHaveLength(2),
    )
    expect(reopened.queryByText(AUTO_PREVIEW_COST_NOTICE)).toBeNull()
    await new Promise((resolve) => setTimeout(resolve, 100))
    expect(simulatePosts(view.calls)).toBe(1)
  })

  it('generates again from a changed option set and keeps both attempts', async () => {
    const view = await renderCaseDetail({ initialCases: [caseWithPhoto()] })

    // The photo's automatic default generation (Natural) settles into
    // the comparison first -- the first attempt on the record.
    const simulate = await view.findByRole('button', {
      name: 'Simulate smile',
    })
    await view.findByRole(
      'region',
      { name: 'Before and after' },
      { timeout: 8000 },
    )
    await waitFor(
      () =>
        expect(
          view.getAllByText('Natural smile · Natural teeth · 100% strength')
            .length,
        ).toBeGreaterThan(0),
      { timeout: 8000 },
    )

    // A generation from a changed option set is a NEW generation: the
    // control re-enables once the automatic preview settles, and the
    // new result supersedes the comparison while both attempts stay on
    // the record.
    await userEvent.click(view.getByRole('radio', { name: 'Subtle' }))
    await userEvent.click(view.getByRole('radio', { name: 'White' }))
    await userEvent.click(simulate)
    const status = await view.findByRole('status')
    await waitFor(() =>
      expect(status.textContent).toMatch(/Generating the smile simulation|queued/),
    )
    await waitFor(
      () =>
        expect(
          view.getAllByText('Subtle smile · White teeth · 100% strength')
            .length,
        ).toBeGreaterThan(0),
      { timeout: 8000 },
    )
    expect(
      view.getAllByText('Natural smile · Natural teeth · 100% strength')
        .length,
    ).toBeGreaterThan(0)
  })

  it('renders a simulate refusal through its code text, never a raw code', async () => {
    const view = await renderCaseDetail({
      initialCases: [caseWithPhoto()],
      simulateRefusal: {
        status: 409,
        code: 'billing.insufficient_credits',
      },
    })

    await view.findByRole('button', { name: 'Simulate smile' })
    await userEvent.click(view.getByRole('button', { name: 'Simulate smile' }))

    // The refusal's code resolves to its bilingual text...
    await view.findByRole('alert')
    expect(
      view.getByText(
        'Not enough credits for one simulation. Add credits and try again.',
      ),
    ).toBeInTheDocument()
    // ...no status region opens (no job was accepted), and the control
    // stays usable for a corrected attempt.
    expect(view.queryByRole('status')).toBeNull()
    expect(view.getByRole('button', { name: 'Simulate smile' })).toBeEnabled()
  })

  it('degrades an unmapped refusal to the unknown fallback, never a raw key', async () => {
    const view = await renderCaseDetail({
      initialCases: [caseWithPhoto()],
      simulateRefusal: {
        status: 400,
        code: 'smilesim.a_future_code_this_surface_does_not_know',
      },
    })

    await view.findByRole('button', { name: 'Simulate smile' })
    await userEvent.click(view.getByRole('button', { name: 'Simulate smile' }))

    await view.findByRole('alert')
    expect(
      view.getByText('Something went wrong. Try again later.'),
    ).toBeInTheDocument()
    expect(view.queryByText(/smilesim\./)).toBeNull()
  })

  it('renders a simulation-content refusal where the result image belongs', async () => {
    const view = await renderCaseDetail({
      initialCases: [caseWithPhoto()],
      simulationContentRefusal: {
        status: 404,
        code: 'smilesim.output_not_found',
      },
    })

    // The photo's automatic generation completes into the comparison,
    // whose result-image read is refused by the route's answer.
    await view.findByRole('button', { name: 'Simulate smile' })
    const comparison = await view.findByRole(
      'region',
      { name: 'Before and after' },
      { timeout: 8000 },
    )

    // The original still renders; the missing result answers with its
    // refusal's bilingual text, never a broken image or a raw code.
    await waitFor(() =>
      expect(
        within(comparison).getByText(
          'The result image is no longer available.',
        ),
      ).toBeInTheDocument(),
    )
    expect(
      within(comparison).getByRole('img', { name: 'Patient photo 1' }),
    ).toBeInTheDocument()
  })
})
