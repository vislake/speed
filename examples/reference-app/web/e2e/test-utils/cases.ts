/**
 * Opening a case, attaching the patient's photograph, and getting to a
 * generated simulation: the steps every gate about the product's own
 * work has to take before it can ask its question.
 *
 * The steps and the fixture live here, shared by the gates that drive
 * the case journey, rather than copied between spec files --
 * PATIENT_PHOTO included.
 *
 * The accessible names below are this suite's EXPECTATIONS, not design
 * instructions. When a surface names a control differently, this file
 * is what changes and nothing else should need to -- and the
 * reconciliation is a conversation about what a control should be
 * called, which is worth having in the open rather than buried in a
 * test id.
 */
import { expect, type Locator, type Page } from '@playwright/test'
import { openSurface } from './journeys.js'

/** The accessible names the case-journey gates look for. */
export const CASE_UI = {
  /** The nav entry that leads to the practice's cases. */
  navCases: /cases|patients/i,
  /** The control that starts a new case. */
  newCase: /new case|create case|add case/i,
  /** The field naming the case (a patient reference). */
  caseNameField: /case name|patient|reference/i,
  /** The control that attaches a photo to the open case. */
  addPhoto: /add photo|upload photo|choose file|upload/i,
  /** The control that starts a simulation from the open photo. */
  simulate: /simulate|generate/i,
  /** The region showing the original and the result together. */
  comparison: /before.*after|comparison/i,
  /** The control that mints a patient-facing link. */
  share: /share|link/i,
  /** Where the cost of one generation is shown. */
  cost: /credit|cost|usage/i,
  /**
   * The nav entry leading to the standing balance.
   *
   * The real name, not a pattern that might reach it: a pattern like
   * `/account|billing|usage/i` would reach "Account" instead -- the
   * entry is called "Credits" -- and a surface that carries digits
   * could satisfy a balance check while being the wrong page entirely.
   * A pattern loose enough to reach the thing it wants is loose enough
   * to reach something else.
   */
  navCredits: 'Credits',
} as const

/** A patient reference unique to one run, so a rerun never collides. */
export function caseName(): string {
  return `E2E patient ${Date.now()}`
}

/** A small, valid PNG standing in for a patient photograph. */
export const PATIENT_PHOTO = {
  name: 'patient-before.png',
  mimeType: 'image/png',
  // A 1x1 opaque pixel: the smallest thing the server's own probe will
  // still accept as a real PNG.
  buffer: Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
    'base64',
  ),
}

/** Creates one case with the photograph attached, from the cases surface. */
export async function createCaseWithPhoto(page: Page, name: string): Promise<void> {
  await page.getByRole('button', { name: CASE_UI.newCase }).click()
  await page.getByRole('textbox', { name: CASE_UI.caseNameField }).fill(name)

  const chooser = page.waitForEvent('filechooser')
  await page.getByRole('button', { name: CASE_UI.addPhoto }).click()
  await (await chooser).setFiles(PATIENT_PHOTO)

  await page.getByRole('button', { name: /create|save|confirm/i }).click()
  await expect(page.getByText(name), 'the case must be findable after creation').toBeVisible({
    timeout: 30_000,
  })
}

/**
 * Leaves the browser on a case that has a photograph on it.
 *
 * CREATES the case, rather than hoping one is there: against a fresh
 * database -- which every local run gets, deliberately -- there is no
 * row to open, so a helper that only opened an existing row would fail
 * every gate that starts from this state. Creating unconditionally is
 * the honest shape: it makes each gate independent of what earlier
 * gates left behind, and of the order they ran in. It costs no sign-in.
 */
export async function openCaseWithPhoto(page: Page): Promise<void> {
  await openSurface(page, CASE_UI.navCases)
  const name = caseName()
  await createCaseWithPhoto(page, name)
  await page.getByText(name).click()
  await expect(
    page.getByRole('img', { name: /photo|patient|before/i }).first(),
    'the case just created does not show the photo submitted with it',
  ).toBeVisible({ timeout: 30_000 })
}

/** Leaves the browser on a case whose simulation has been generated. */
export async function openCaseWithSimulation(page: Page): Promise<void> {
  await openCaseWithPhoto(page)
  await expect(
    page.getByRole('region', { name: CASE_UI.comparison }),
    'this gate starts from a generated simulation, which block B is what produces',
  ).toBeVisible()
}

/** The two images a comparison must show: both loaded, and not the same one. */
export async function expectBeforeAndAfter(images: Locator, what: string): Promise<void> {
  await expect(images, `${what} does not show two images`).toHaveCount(2)

  // Polled, not sampled once: the count reaching two says the elements
  // are THERE, not that the browser has finished with them -- an <img>
  // exists the moment it is rendered and decodes some milliseconds
  // later, so a point-in-time read of the decode state would fail on
  // whichever engine decoded slowest, naming a broken image while the
  // requests all answered. A false red is not a cheap failure: it can
  // hide a real regression, because once a gate is known to flake, its
  // red stops being read.
  const settled = async (): Promise<readonly { source: string; decoded: boolean }[]> =>
    await images.evaluateAll((nodes) =>
      nodes.map((node) => {
        const image = node as HTMLImageElement
        return {
          source: image.currentSrc || image.src,
          decoded: image.complete && image.naturalWidth > 0,
        }
      }),
    )

  await expect
    .poll(async () => (await settled()).every((image) => image.decoded), { timeout: 15_000 })
    .toBe(true)

  const shown = await settled()

  // Kept as an assertion rather than folded into the poll: a poll that
  // times out says only "never became true", while this names the source
  // that never loaded -- and a broken frame where a smile should be is
  // exactly what a patient would report.
  const broken = shown.filter((image) => !image.decoded).map((image) => image.source)
  expect(
    broken,
    `${what} shows an image that never loaded (${broken.join(' , ')}), which is a broken frame where a smile should be`,
  ).toEqual([])

  const sources = shown.map((image) => image.source)
  expect(
    new Set(sources).size,
    `${what} shows the same image twice (${sources.join(' , ')}), so nothing about the simulation is on screen`,
  ).toBe(2)
}
