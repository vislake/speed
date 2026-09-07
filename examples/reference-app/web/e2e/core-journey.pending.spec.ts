/**
 * The acceptance gates for the product's own reason to exist: a dental
 * practice uploading a patient's photo, generating a smile simulation,
 * comparing it with the original, sharing it with the patient, and seeing
 * what it cost.
 *
 * These are written BEFORE the surfaces they check, which is why they are
 * tagged @pending and left out of the default run (playwright.config.ts's
 * grepInvert). Run them deliberately:
 *
 *   pnpm test:e2e:pending
 *
 * Every one of them fails today, and that is their present value: an
 * acceptance review found that a signed-in practice can reach nothing but
 * a notes scratchpad and an account page, while the backends for all four
 * blocks below are real and tested (go/storage's three-step upload,
 * internal/cases, internal/smilesim's async job, go/sharing's tokens,
 * go/billing's credit ledger). The gap is assembly, not capability, and
 * these gates are what turn "assembled" into something checkable rather
 * than arguable.
 *
 * WHAT THESE ASSERT, AND WHAT THEY DELIBERATELY DO NOT
 *
 * They assert what a person must be able to ACCOMPLISH, by clicking:
 * reach the surface, do the thing, see the result. They do not prescribe
 * layout, wording, component choice or route shape. Where a locator names
 * a control, the name is this suite's expectation of an accessible name,
 * not a design instruction -- the UI round is free to name it otherwise,
 * in which case UI_NAMES below is the one place to reconcile, and the
 * reconciliation is a conversation about what a control should be called,
 * which is a conversation worth having in the open rather than a hidden
 * test-id.
 *
 * The blocks are ordered the way they will be delivered (A first: it is
 * the journey's entrance), and each is independently runnable, so a block
 * can be accepted the day it lands instead of waiting for the whole.
 */
import { expect, test, type Page } from '@playwright/test'
import { DEMO_OWNER, DEMO_READER } from './test-utils/accounts.js'
import { signInAs } from './test-utils/journeys.js'

/**
 * The accessible names the gates look for. Expectations, not decrees:
 * when the UI lands with different names, this block is what changes,
 * and nothing else in the file should need to.
 */
const UI_NAMES = {
  /** Block A: the nav entry that leads to the practice's cases. */
  navCases: /cases|patients/i,
  /** Block A: the control that starts a new case. */
  newCase: /new case|create case|add case/i,
  /** Block A: the field naming the case (a patient reference). */
  caseNameField: /case name|patient|reference/i,
  /** Block A: the control that attaches a photo to the open case. */
  addPhoto: /add photo|upload photo|choose file|upload/i,
  /** Block B: the control that starts a simulation from the open photo. */
  simulate: /simulate|generate/i,
  /** Block B: the region showing the original and the result together. */
  comparison: /before.*after|comparison/i,
  /** Block C: the control that mints a patient-facing link. */
  share: /share|link/i,
  /** Block D: where the cost of one generation is shown. */
  cost: /credit|cost|usage/i,
} as const

/** A patient reference unique to one run, so a rerun never collides. */
function caseName(): string {
  return `E2E patient ${Date.now()}`
}

/** A small, valid PNG standing in for a patient photograph. */
const PATIENT_PHOTO = {
  name: 'patient-before.png',
  mimeType: 'image/png',
  // A 1x1 opaque pixel: the smallest thing the server's own probe will
  // still accept as a real PNG.
  buffer: Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
    'base64',
  ),
}

test.describe('the core journey', { tag: '@pending' }, () => {
  test('block A: a practice opens a case with the patient photo in one step', async ({ page }) => {
    await signInAs(page, DEMO_OWNER)

    // Reachable at all: the journey's entrance must be in the frame's own
    // navigation, not behind a URL only its author knows.
    const casesEntry = page.getByRole('link', { name: UI_NAMES.navCases })
    await expect(casesEntry, 'a practice must be able to reach its cases from the nav').toBeVisible()
    await casesEntry.click()

    // One step, not two. The backend takes a case's photos as storage
    // object ids it already holds (cases.CreateCaseInput.PhotoObjectIDs),
    // so the surface is expected to pick the file, upload it and create
    // the case from one submission -- which is also the shape the work
    // actually has: a receptionist photographs the patient and opens the
    // case, rather than opening an empty case and coming back later.
    await page.getByRole('button', { name: UI_NAMES.newCase }).click()
    const name = caseName()
    await page.getByRole('textbox', { name: UI_NAMES.caseNameField }).fill(name)

    const chooser = page.waitForEvent('filechooser')
    await page.getByRole('button', { name: UI_NAMES.addPhoto }).click()
    await (await chooser).setFiles(PATIENT_PHOTO)

    await page.getByRole('button', { name: /create|save|confirm/i }).click()

    // The case exists, is findable, and carries the photo -- which is
    // what go/storage's three-step protocol exists to make true.
    await expect(page.getByText(name), 'the case must be findable after creation').toBeVisible({
      timeout: 30_000,
    })
    await page.getByText(name).click()
    await expect(
      page.getByRole('img', { name: /photo|patient|before/i }).first(),
      'the photo submitted with the case must appear on it',
    ).toBeVisible({ timeout: 30_000 })
  })

  test('block A: a case one colleague opened is visible to another', async ({ page }) => {
    // The property that makes this a practice's tool rather than a
    // personal notebook, and the one the backend's own list query got
    // wrong: it listed by creator, so a dentist could not see the case
    // the receptionist had just opened for them, and a returning patient
    // met a colleague who could not find their last case -- one patient,
    // two charts, a split record.
    //
    // Tenant isolation is what protects another practice's cases (proven
    // separately in authorization.spec.ts); inside one practice, the
    // people who treat a patient together must see the same case.
    const name = caseName()

    await signInAs(page, DEMO_OWNER)
    await page.getByRole('link', { name: UI_NAMES.navCases }).click()
    await page.getByRole('button', { name: UI_NAMES.newCase }).click()
    await page.getByRole('textbox', { name: UI_NAMES.caseNameField }).fill(name)
    const chooser = page.waitForEvent('filechooser')
    await page.getByRole('button', { name: UI_NAMES.addPhoto }).click()
    await (await chooser).setFiles(PATIENT_PHOTO)
    await page.getByRole('button', { name: /create|save|confirm/i }).click()
    await expect(page.getByText(name)).toBeVisible({ timeout: 30_000 })

    // A colleague in the same practice, signing in on the same machine
    // the way a shift change happens at a front desk.
    await signInAs(page, DEMO_READER)
    await page.getByRole('link', { name: UI_NAMES.navCases }).click()
    await expect(
      page.getByText(name),
      'a colleague in the same practice cannot see the case, so the patient gets a second chart',
    ).toBeVisible({ timeout: 30_000 })
  })

  test('block B: the practice chooses the smile it wants, not a hardcoded one', async ({
    page,
  }) => {
    // "Multiple smile/tooth shade options" is in the product brief in so
    // many words, and the backend already carries the whole vocabulary:
    // three smile styles (subtle / natural / bright), three tooth shades
    // (natural / white / ultra-white) and an adjustable strength, each
    // with its own validation answer (smilesim.unsupported_smile_style,
    // unsupported_tooth_shade, strength_out_of_range). A surface that
    // generates from hardcoded defaults would leave a dentist unable to
    // do the thing the product is sold on -- offering a patient a choice
    // -- while the capability sat unused underneath.
    //
    // This gate was missing from the first draft of these blocks, which
    // asserted only that one simulation appeared. That omission would
    // have let block B ship "working" and still miss a brief requirement.
    await signInAs(page, DEMO_OWNER)
    await openCaseWithPhoto(page)

    // The choices a person can actually make, by name rather than by
    // count: a dentist tells a patient "let's try the natural one", so
    // the words matter more than the number of buttons.
    for (const style of ['subtle', 'natural', 'bright'] as const) {
      await expect(
        page.getByRole('group', { name: /style|smile/i }).getByText(new RegExp(style, 'i')),
        `the ${style} smile style must be offerable to a patient`,
      ).toBeVisible()
    }
    for (const shade of ['natural', 'white', 'ultra'] as const) {
      await expect(
        page.getByRole('group', { name: /shade|colour|color/i }).getByText(new RegExp(shade, 'i')),
        `the ${shade} tooth shade must be offerable to a patient`,
      ).toBeVisible()
    }
  })

  test('block B: a simulation is generated and shown beside the original', async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await openCaseWithPhoto(page)

    await page.getByRole('button', { name: UI_NAMES.simulate }).click()

    // Generation is asynchronous by design (internal/smilesim enqueues a
    // job), so the person must be told it is happening rather than left
    // looking at a frozen screen.
    await expect(
      page.getByRole('status'),
      'a generation in flight must say so',
    ).toBeVisible()

    // The result, and the original, visible together: a dentist shows the
    // patient the difference, which is the product's entire proposition.
    const comparison = page.getByRole('region', { name: UI_NAMES.comparison })
    await expect(comparison, 'the result must be shown against the original').toBeVisible({
      timeout: 120_000,
    })
    await expect(comparison.getByRole('img')).toHaveCount(2)
  })

  test('block C: the result becomes a link a patient can open', async ({ page, context }) => {
    await signInAs(page, DEMO_OWNER)
    await openCaseWithSimulation(page)

    await page.getByRole('button', { name: UI_NAMES.share }).click()

    // The link is handed to the practice in a form they can actually send
    // -- readable on screen, not only in a clipboard a test cannot read.
    const link = page.getByRole('textbox', { name: UI_NAMES.share })
    await expect(link, 'the practice must be able to see and copy the link').toBeVisible()
    const url = await link.inputValue()
    expect(url, 'the share control must produce a URL').toMatch(/^https?:\/\//)

    // The patient's side: a different browser context, no session, no
    // account -- and the simulation is there.
    const patient = await context.browser()?.newContext()
    if (patient === undefined) {
      throw new Error('e2e: could not open a second browser context for the patient')
    }
    try {
      const patientPage = await patient.newPage()
      await patientPage.goto(url)
      await expect(
        patientPage.getByRole('img').first(),
        'a patient opening the link must see the simulation',
      ).toBeVisible()
    } finally {
      await patient.close()
    }
  })

  test('block D: the practice sees what a generation costs and what is left', async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await openCaseWithSimulation(page)

    // What this generation cost, where the generation happened: a
    // pay-per-use product that spends silently is one nobody trusts.
    await expect(
      page.getByText(UI_NAMES.cost).first(),
      'a generation must say what it cost',
    ).toBeVisible()

    // And the standing balance, somewhere a person can check it.
    await page.getByRole('link', { name: /account|billing|usage/i }).click()
    await expect(
      page.getByText(/balance|remaining|credits/i).first(),
      'the practice must be able to see its remaining credits',
    ).toBeVisible()
  })
})

/**
 * Opens a case that already has a photo on it, creating one when the run
 * has none. Written as a helper because blocks B, C and D all start from
 * that state; its shape will firm up when block A's surface lands, which
 * is the point at which guessing stops.
 */
async function openCaseWithPhoto(page: Page): Promise<void> {
  await page.getByRole('link', { name: UI_NAMES.navCases }).click()
  const firstCase = page.getByRole('listitem').first()
  await expect(
    firstCase,
    'block B starts from a case with a photo, which block A is what creates',
  ).toBeVisible()
  await firstCase.click()
}

/** Opens a case whose simulation has already been generated. */
async function openCaseWithSimulation(page: Page): Promise<void> {
  await openCaseWithPhoto(page)
  await expect(
    page.getByRole('region', { name: UI_NAMES.comparison }),
    'blocks C and D start from a generated simulation, which block B is what produces',
  ).toBeVisible()
}
