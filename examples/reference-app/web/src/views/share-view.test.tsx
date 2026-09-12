/**
 * ShareView contract -- the patient's share surface at the component
 * level: the page one share link opens renders the shared BEFORE/AFTER
 * pair straight from the public access route for anyone holding the
 * link -- no session attached, no sign-in, no app frame -- each half
 * under its own share token, and a refused link (expired, revoked,
 * over the route's rate budget, a resource storage can no longer open,
 * a transport failure) resolves through the patient page's
 * reachable-error whitelist to one honest human message in the page's
 * own language, never a broken frame and never a crash.
 *
 * The browser-decode half of the acceptance shape (naturalWidth over a
 * real answer) cannot run in jsdom -- images never load here -- so it
 * is pinned at the two layers that can: the wire journey in the Go
 * suite (flowtests/share_journey_flow_test.go decodes the access
 * route's real bytes for both halves) and the e2e gate that opens the
 * real page in a real browser (web/e2e/core-journey.spec.ts).
 * What this suite pins is the page's own contract: two image elements,
 * one per half of the pair, each consuming its own half's access-route
 * answer directly, and every refusal the route can answer rendering
 * its coded, bilingual text.
 */

import { fireEvent, waitFor } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import type { RealCall } from '../test-utils/real-client.js'
import type { RealResponder } from '../test-utils/real-client.js'
import { demoServer } from '../test-utils/demo-server.js'
import { makeRealClientRig } from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { ShareView } from './share-view.js'

const BEFORE_TOKEN = 'patient-before-token'
const AFTER_TOKEN = 'patient-after-token'
const SHARE_ACCESS_PATH = '/api/v1/sharing/access'

/** Mounts the patient page over the real-client rig -- the anonymous
 * visitor's own shape: no session is ever attached. */
function renderShareView(
  options: Parameters<typeof demoServer>[0] = {},
): ReturnType<typeof renderWithAppServices> & {
  readonly calls: ReturnType<typeof makeRealClientRig>['calls']
} {
  const rig = makeRealClientRig(demoServer(options))
  const view = renderWithAppServices(
    <ShareView beforeToken={BEFORE_TOKEN} afterToken={AFTER_TOKEN} />,
    { session: rig.session, api: rig.api },
    { attach: false, language: 'en-US' },
  )
  return { ...view, calls: rig.calls }
}

/** One half of the pair's image element, by its accessible name in the
 * active language. */
function imageOf(
  view: ReturnType<typeof renderWithAppServices>,
  name: string,
): HTMLElement {
  const image = view.getByRole('img', { name })
  if (!(image instanceof HTMLImageElement)) {
    throw new Error('share page image is not an HTMLImageElement')
  }
  return image
}

/** The access requests the page made: the image loads themselves are
 * browser fetches jsdom never performs, so the observable legs are the
 * diagnosis probes, each carrying no bearer (a patient holds none). */
function accessGets(view: ReturnType<typeof renderShareView>): RealCall[] {
  return view.calls.filter((call) => call.path === SHARE_ACCESS_PATH)
}

describe('ShareView', () => {
  it('renders the shared before/after pair from the public access route, no session attached', () => {
    // The page a share link opens is the patient's, not the clinic's:
    // heading and two images -- the original photo and the simulated
    // smile -- each sourced from its own half's access-route answer,
    // the real bytes the browser decodes, never a signed-in surface's
    // blob.
    const view = renderShareView()
    expect(
      view.getByRole('heading', { name: 'Your smile preview' }),
    ).toBeInTheDocument()
    const before = imageOf(view, 'Original photo')
    expect(before.getAttribute('src')).toBe(
      `${window.location.origin}${SHARE_ACCESS_PATH}?token=${BEFORE_TOKEN}`,
    )
    const after = imageOf(view, 'Simulated smile')
    expect(after.getAttribute('src')).toBe(
      `${window.location.origin}${SHARE_ACCESS_PATH}?token=${AFTER_TOKEN}`,
    )
    // The two halves load from different URLs: two shares, two tokens
    // -- the shape the browser must fetch two distinct objects from.
    expect(before.getAttribute('src')).not.toBe(after.getAttribute('src'))
    // No request was made under a session: the visitor's page performs
    // no API call at all until an image load fails (and even that probe
    // is credential-less, below) -- the happy path is two browser image
    // loads and nothing else.
    expect(view.calls).toHaveLength(0)
  })

  it('refuses an expired or revoked link honestly: one human message, no image, no crash', async () => {
    // sharing.not_accessible is the module's single outward answer for
    // every refusal of a recognized token -- expired, revoked,
    // view-exhausted and password-refused are indistinguishable by
    // design -- so the page renders its one honest text for either
    // half that refuses.
    const view = renderShareView({
      shareAccessRefusal: { status: 404, code: 'sharing.not_accessible' },
    })
    fireEvent.error(imageOf(view, 'Original photo'))

    const refusal = await view.findByRole('alert')
    expect(refusal.textContent).toBe(
      'This link is no longer available. It may have expired, or the clinic may have stopped sharing it. Ask the clinic for a fresh link.',
    )
    expect(view.queryByRole('img')).not.toBeInTheDocument()
    // The diagnosis asked the refusing half's route itself,
    // credential-less.
    const probes = accessGets(view)
    expect(probes).toHaveLength(1)
    expect(probes[0]?.authorization).toBeNull()
    expect(probes[0]?.query).toBe(`?token=${BEFORE_TOKEN}`)
  })

  it('refuses the whole pair when the after half is gone', async () => {
    // The refusal is per half, the page is one pair: whichever half
    // fails first names the message, and the other half's image is
    // taken down with it -- a half-open link is not a deliverable.
    const view = renderShareView({
      shareAccessRefusal: { status: 404, code: 'sharing.not_accessible' },
    })
    fireEvent.error(imageOf(view, 'Simulated smile'))

    const refusal = await view.findByRole('alert')
    expect(refusal.textContent).toBe(
      'This link is no longer available. It may have expired, or the clinic may have stopped sharing it. Ask the clinic for a fresh link.',
    )
    expect(view.queryByRole('img')).not.toBeInTheDocument()
    const probes = accessGets(view)
    expect(probes).toHaveLength(1)
    expect(probes[0]?.query).toBe(`?token=${AFTER_TOKEN}`)
  })

  it('renders every refusal the access route can answer through its own code text', async () => {
    const cases: ReadonlyArray<{
      readonly status: number
      readonly code: string
      readonly text: string
    }> = [
      {
        status: 429,
        code: 'sharing.rate_limited',
        text: 'Too many visits in a short time. Wait a moment and try again.',
      },
      {
        status: 502,
        code: 'sharing.resource_unavailable',
        text: "This link's image could not be loaded. Ask the clinic to check that the simulation is still available.",
      },
      {
        status: 500,
        code: 'sharing.internal_error',
        text: 'Something went wrong on the server. Try again later.',
      },
      {
        // A code no whitelist entry covers resolves to the surface's
        // unknown fallback -- never a raw key on the patient's page.
        status: 404,
        code: 'sharing.some_future_refusal',
        text: 'This preview could not be shown. Try again later.',
      },
    ]
    for (const scenario of cases) {
      const view = renderShareView({
        shareAccessRefusal: { status: scenario.status, code: scenario.code },
      })
      fireEvent.error(imageOf(view, 'Original photo'))
      const refusal = await view.findByRole('alert')
      expect(
        refusal.textContent,
        `${scenario.code} must render its own text`,
      ).toBe(scenario.text)
      view.unmount()
    }
  })

  it('retries once when a half is live but its image load failed transiently', async () => {
    // A live share answers the diagnosis with real bytes, which the
    // request function refuses as client.protocol -- the page's retry
    // signal: the image is remounted for one more attempt before any
    // message is shown. A second failure of a live share is the
    // transport message, not an infinite loop.
    const view = renderShareView()
    fireEvent.error(imageOf(view, 'Original photo'))

    // The retry: a fresh image element (same source) replaces the
    // failed one, and the other half never moved.
    await waitFor(() => expect(view.getAllByRole('img')).toHaveLength(2))
    fireEvent.error(imageOf(view, 'Original photo'))

    const refusal = await view.findByRole('alert')
    expect(refusal.textContent).toBe(
      'The image could not be loaded. Check your connection and try again.',
    )
    expect(accessGets(view)).toHaveLength(2)
  })

  it('renders the transport message when the diagnosis itself cannot reach the server', async () => {
    const responder = demoServer()
    const failingResponder: RealResponder = (call) =>
      call.path === SHARE_ACCESS_PATH
        ? Promise.reject(new TypeError('network down'))
        : responder(call)
    const rig = makeRealClientRig(failingResponder)
    const view = renderWithAppServices(
      <ShareView beforeToken={BEFORE_TOKEN} afterToken={AFTER_TOKEN} />,
      { session: rig.session, api: rig.api },
      { attach: false, language: 'en-US' },
    )
    fireEvent.error(imageOf(view, 'Original photo'))

    const refusal = await view.findByRole('alert')
    expect(refusal.textContent).toBe(
      'The image could not be loaded. Check your connection and try again.',
    )
  })

  it('speaks the page language when it refuses', async () => {
    // The zh-CN leg: the refusal text comes from the page's own bundle
    // in the active language (asserted through the imported fixture --
    // no inline CJK in this file), the same resource the codes-alignment
    // suite pins key-for-key.
    const rig = makeRealClientRig(
      demoServer({
        shareAccessRefusal: { status: 404, code: 'sharing.not_accessible' },
      }),
    )
    const view = renderWithAppServices(
      <ShareView beforeToken={BEFORE_TOKEN} afterToken={AFTER_TOKEN} />,
      { session: rig.session, api: rig.api },
      { attach: false, language: 'zh-CN' },
    )
    fireEvent.error(imageOf(view, zhCN.shareView.beforeImageAlt))
    const refusal = await view.findByRole('alert')
    expect(refusal.textContent).toBe(zhCN.shareView.errors.notAccessible)
  })
})
