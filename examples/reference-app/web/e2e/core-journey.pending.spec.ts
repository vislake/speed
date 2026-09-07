/**
 * The acceptance gates for the product's own reason to exist: a dental
 * practice uploading a patient's photo, generating a smile simulation,
 * comparing it with the original, sharing it with the patient, and seeing
 * what it cost.
 *
 * These are tagged @pending and left out of the default run
 * (playwright.config.ts's grepInvert). Block A's two tests now PASS --
 * the block-A surface shipped -- and are run deliberately together with
 * the rest:
 *
 *   pnpm test:e2e:pending
 *
 * Block B's two tests now PASS too (the block-B surface shipped: the
 * case photo carries the option pickers, the async generation with its
 * honest status, and the before/after comparison; the run's own
 * throwaway OpenAI-compatible images provider -- fake-image-provider.mjs
 * under e2e/test-utils, wired in playwright.config.ts -- is what lets a
 * freshly booted server complete the generation deterministically). To
 * run the passing A and B gates while C and D are still open:
 *
 *   pnpm exec playwright test --grep "block A|block B"
 *
 * (A must ride along with B: each run boots a fresh server and a fresh
 * database, and block B's helpers open a case that block A's own tests
 * create earlier in the same run.)
 *
 * The gates stay out of the default run for a structural reason, not an
 * implementation one: go/authn's per-IP login budget (limitLoginByIP,
 * 20 attempts per minute) is the standing ceiling the default suite is
 * sized against -- the pre-block-A default run already sits at roughly
 * 19 login attempts, and the core-journey gates' sign-ins push the
 * composed default run over the budget mid-suite (the org-invitation
 * and password-sign-in specs start answering authn.rate_limited). A
 * block whose gate rides the same demo accounts and the same per-IP
 * budget cannot join the default run until the suite's budget question
 * is answered (a per-suite rate allowance, dedicated demo accounts, or
 * fewer sign-ins elsewhere) -- recorded here as the reason these four
 * gates stay in the deliberate selection. Blocks C and D fail today,
 * and that is their present value: an
 * acceptance review found that a signed-in practice can reach nothing but
 * a notes scratchpad and an account page, while the backends for the
 * blocks below are real and tested (go/storage's three-step upload,
 * internal/cases, internal/smilesim's async job, go/sharing's tokens,
 * go/billing's credit ledger). The gap was assembly, not capability, and
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
import {
  openSurface,
  readCurrentTenant,
  signInAs,
  switchTenant,
} from './test-utils/journeys.js'

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
  /**
   * Block D: the nav entry leading to the standing balance.
   *
   * The real name, not a pattern that might reach it. This was
   * `/account|billing|usage/i`, which matches NONE of them -- the entry
   * is called "Credits" -- and instead matched "Account", whose surface
   * happens to carry digits, so the gate passed on chromium by landing
   * on the wrong page entirely. On webkit and the iPad project the same
   * mistake failed, which is the only reason it was found. A pattern
   * loose enough to reach the thing it wants is loose enough to reach
   * something else.
   */
  navCredits: 'Credits',
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
    // navigation, not behind a URL only its author knows. Reached through
    // openSurface rather than by clicking a nav link directly, because a
    // link is only VISIBLE on a wide screen -- below the md breakpoint
    // AppShell collapses the navigation behind the menu button, and a
    // dentist reaching this surface on the iPad they show patients does
    // it through that menu. Asserting the link's visibility instead made
    // this gate accuse the product of having no entrance on the one
    // device the product is most often held in.
    await openSurface(page, UI_NAMES.navCases)

    // One step, not two. The backend takes a case's photos as storage
    // object ids it already holds (cases.CreateCaseInput.PhotoObjectIDs),
    // so the surface is expected to pick the file, upload it and create
    // the case from one submission -- which is also the shape the work
    // actually has: a receptionist photographs the patient and opens the
    // case, rather than opening an empty case and coming back later.
    const name = caseName()
    await createCaseWithPhoto(page, name)

    // The case exists, is findable, and carries the photo -- which is
    // what go/storage's three-step protocol exists to make true.
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
    await openSurface(page, UI_NAMES.navCases)
    await page.getByRole('button', { name: UI_NAMES.newCase }).click()
    await page.getByRole('textbox', { name: UI_NAMES.caseNameField }).fill(name)
    const chooser = page.waitForEvent('filechooser')
    await page.getByRole('button', { name: UI_NAMES.addPhoto }).click()
    await (await chooser).setFiles(PATIENT_PHOTO)
    await page.getByRole('button', { name: /create|save|confirm/i }).click()
    await expect(page.getByText(name)).toBeVisible({ timeout: 30_000 })
    // The clinic the case was opened in: the demo seeds no fixed
    // sign-in landing tenant (an account's first tenant comes from a Go
    // map's iteration order, randomized per boot -- journeys.ts's
    // documented finding), so the colleague's visit is aimed at the
    // clinic where the case actually lives, the same shift-change
    // switch a front desk makes.
    const clinic = await readCurrentTenant(page)

    // A colleague in the same practice, signing in on the same machine
    // the way a shift change happens at a front desk.
    await signInAs(page, DEMO_READER)
    await switchTenant(page, clinic)
    await openSurface(page, UI_NAMES.navCases)
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

    // The choices a person can actually make, option by option: a
    // dentist tells a patient "let's try the natural one", so the words
    // matter more than the number of controls. Each option is queried
    // by its own accessible name (a radio's label) rather than by bare
    // text -- a locator reconciliation, not a design instruction: with
    // the documented vocabulary as visible copy, the word "white" is a
    // substring of the "ultra-white" option's label, so a bare text
    // query for it matches two options at once and Playwright's
    // strictness fails the gate no matter what any UI labels its
    // options. The option name is the label's text, exactly as a person
    // sees it.
    const styleGroup = page.getByRole('group', { name: /style|smile/i })
    for (const style of ['subtle', 'natural', 'bright'] as const) {
      await expect(
        styleGroup.getByRole('radio', { name: new RegExp(`^${style}$`, 'i') }),
        `the ${style} smile style must be offerable to a patient`,
      ).toBeVisible()
    }
    const shadeGroup = page.getByRole('group', { name: /shade|colour|color/i })
    for (const shade of ['natural', 'white', 'ultra-white'] as const) {
      await expect(
        shadeGroup.getByRole('radio', { name: new RegExp(`^${shade}$`, 'i') }),
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
    // looking at a frozen screen. Scoped to the work area and required
    // to SAY something: the chrome carries live regions of its own (the
    // tenant switcher's announcement), so an unscoped, text-free check
    // could be satisfied by a region that has nothing to do with this
    // generation.
    const announcement = page.getByRole('main').getByRole('status').first()
    await expect(announcement, 'a generation in flight must say so').toBeVisible()
    await expect(
      announcement,
      'the in-flight notice is empty, so it announces nothing',
    ).not.toHaveText('')

    // The result, and the original, visible together: a dentist shows the
    // patient the difference, which is the product's entire proposition.
    const comparison = page.getByRole('region', { name: UI_NAMES.comparison })
    await expect(comparison, 'the result must be shown against the original').toBeVisible({
      timeout: 120_000,
    })
    await expectBeforeAndAfter(comparison.getByRole('img'), "the practice's comparison")
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
      // The SIMULATION, not merely an image. An unscoped first-image
      // check is satisfied by a logo, a placeholder or an error
      // illustration -- so a patient page that failed to load the
      // simulation at all could pass it. The image has to be one the
      // browser actually decoded, which a broken or missing source is
      // not.
      // A PAGE with the PAIR on it, not a file and not the result alone.
      //
      // go/sharing's public route answers the resource's raw bytes with
      // its own MIME type (handler.go's io.Copy over the resolved
      // content), so handing a patient that URL directly opens a bare
      // image in their browser: no practice name, no explanation, and no
      // BEFORE. This gate passed that shape until it was looked at --
      // a browser given image bytes builds a document around an <img>,
      // so an image-role check and a decoded-width check both hold.
      // Found by reading the gate as though it had already passed and
      // asking what it would have let through, which is the only defence
      // available for a gate written before its surface exists.
      //
      // Both requirements below were settled as product decisions rather
      // than assumed here: "before/after comparison" is a core
      // requirement and it has to hold on the PATIENT's side -- the
      // result alone is not a delivery, and narrowing the comparison to
      // something only the clinic sees was considered and rejected. So
      // the patient's landing is a side-by-side page.
      //
      // Text first, because it is the cheapest way to tell a page from a
      // file: a bare image document contains no text at all. What the
      // page should SAY -- the practice's name, the patient's, an
      // explanation -- is still a product decision this does not make.
      await expect(
        patientPage.locator('body'),
        'the patient received a bare file rather than a page: nothing on it says whose smile this is',
      ).not.toHaveText('')

      await expectBeforeAndAfter(patientPage.getByRole('img'), "the patient's page")

      // AGAIN, because a patient opens the link more than once: from the
      // message when it arrives, then later to show someone at home.
      //
      // Worth asserting rather than assuming now that the pair costs two
      // token reads per visit. go/sharing settles a granted view only
      // after the content has actually been served (ee20d37 -- before
      // that, an unwired resolver or an interrupted stream permanently
      // spent a limited share nobody saw), and this app mints its
      // patient links with no view cap at all, so a second visit is
      // supposed to work. This gate is what would notice if a cap
      // appeared later: with one, the pair's two reads would spend it on
      // the first visit and the patient's second look would be a
      // refusal.
      await patientPage.reload()
      await expectBeforeAndAfter(
        patientPage.getByRole('img'),
        "the patient's page on a second visit",
      )
    } finally {
      await patient.close()
    }
  })

  test('block D: the practice sees what a generation costs and what is left', async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await openCaseWithSimulation(page)

    // What this generation cost, where the generation happened: a
    // pay-per-use product that spends silently is one nobody trusts.
    //
    // A NUMBER, not the vocabulary. The first draft of this asserted
    // only that text matching /credit|cost|usage/i appeared somewhere on
    // the page, which a nav entry called "Usage" satisfies on its own --
    // so a surface that said the word "credits" and no amount would have
    // passed a gate whose whole subject is how much was spent. A cost
    // that does not say how much is not a cost.
    const cost = page.getByRole('main').getByText(UI_NAMES.cost).first()
    await expect(cost, 'a generation must say what it cost').toBeVisible()
    await expect(
      cost,
      'the surface names the idea of a cost but never an amount, so a practice still cannot tell what this generation spent',
    ).toContainText(/\d/)
    // NOT zero, and this closes a hole found by reading go/billing's own
    // fragment before the surface existed: a tenant with no ledger rows
    // answers an all-zero balance by design (a zero state rather than a
    // missing-resource refusal, which is the right answer). So "a digit
    // appears" is satisfied by "0", and a surface that charged nothing
    // -- or displayed the charge wrongly -- would have passed a gate
    // whose subject is what this generation spent. A generation that
    // costs nothing is not a pay-per-use product; either the charge did
    // not happen or the number is wrong, and both are worth failing on.
    await expect(
      cost,
      'this generation is reported as costing zero, so either nothing was charged for it or the figure shown is wrong -- a pay-per-use product cannot say both that it charges and that this was free',
    ).toContainText(/[1-9]/)

    // And the standing balance, somewhere a person can check it.
    // Reached through openSurface: below the md breakpoint the
    // navigation is behind the menu button, and clicking the link
    // directly is the desktop-only mistake this suite has now made six
    // times.
    await openSurface(page, UI_NAMES.navCredits)
    // Polled to settle, because the balance is a fetch and the heading
    // renders before it answers.
    //
    // The fourth time this suite has read a point-in-time sample as if it
    // were a settled state -- after an assertion, a wait, and a helper.
    // Here it passed on chromium and failed on webkit and the iPad
    // project, purely on how fast each got the answer: the snapshot at
    // failure was the whole surface reduced to `heading "Credits"`, with
    // the figure still in flight. A gate that resolves on engine speed
    // reports nothing about the product.
    const main = page.getByRole('main')
    await expect
      .poll(async () => await main.innerText(), { timeout: 15_000 })
      .toMatch(/\d/)

    // The line that carries the FIGURE, not the label above it.
    //
    // `getByText(/balance|remaining|credits/i).first()` picked the
    // standalone "Balance" label, which never contains a number by
    // design -- the amount lives in its own line ("N credits
    // available"). So the assertion could not pass on this surface at
    // all, and the reason chromium went green earlier was that the
    // navigation was landing on the Account page instead. Neither
    // engine's answer was about the product.
    //
    // Matched on a number next to the word rather than on either alone:
    // a label with no figure fails, and a figure belonging to something
    // else is not accepted just for being a digit somewhere on the page.
    const balance = main.getByText(/\d[\d,.]*\s*(credit|credits)/i).first()
    await expect(
      balance,
      'the practice must be able to see its remaining credits: no line on this surface states an amount of credits',
    ).toBeVisible()
  })
})

/**
 * Opens a case that already has a photo on it, creating one when the run
 * has none. Written as a helper because blocks B, C and D all start from
 * that state. Its targeting firmed up when block A's surface landed: the
 * page's FIRST listitem is the frame navigation's own Home entry, not a
 * case row (the nav list precedes the main landmark in the DOM and both
 * use the listitem role), so the helper scopes to the main landmark --
 * the cases list -- before taking its first row. It deliberately opens
 * whatever the newest case is rather than creating one, mirroring a
 * receptionist continuing yesterday's work.
 */
async function openCaseWithPhoto(page: Page): Promise<void> {
  // CREATES the case, rather than hoping one is there.
  //
  // This helper's own doc comment promised to create one "when the run
  // has none" and never did: it clicked the first row and asserted it
  // was visible. Against a fresh database -- which every local run gets,
  // deliberately -- there is no row, so every block-B gate failed with
  // "block B starts from a case with a photo, which block A is what
  // creates" the moment block B's surface actually landed. The helper
  // was describing an intention, and the note left in it ("its shape
  // will firm up when block A's surface lands, which is the point at
  // which guessing stops") came due exactly there.
  //
  // Creating unconditionally rather than only-if-empty is the honest
  // shape: it makes each gate independent of what earlier gates left
  // behind, and of the order they ran in. It costs no sign-in.
  await openSurface(page, UI_NAMES.navCases)
  const name = caseName()
  await createCaseWithPhoto(page, name)
  await page.getByText(name).click()
  await expect(
    page.getByRole('img', { name: /photo|patient|before/i }).first(),
    'the case just created does not show the photo submitted with it',
  ).toBeVisible({ timeout: 30_000 })
}

/**
 * Asserts a region shows a genuine before/after pair: two images, both
 * decoded by the browser, and NOT the same image twice.
 *
 * One implementation, shared by the clinic's own comparison (block B)
 * and the patient's page (block C), because the two must hold the same
 * property and a change to what counts as a pair must not leave one
 * side checking something the other stopped checking.
 *
 * Each half of it has caught something. Counting alone passes when a
 * surface renders the ORIGINAL twice -- the product's whole proposition
 * rendered as a no-op, and it looks right in a screenshot. Requiring the
 * images to have decoded catches a broken source, which is what a
 * patient would actually report: a broken frame where their new smile
 * should be.
 *
 * Compared by source rather than by pixels: two storage objects are two
 * URLs, the cheapest honest difference. A surface rendering one object
 * twice fails; one rendering the same IMAGE from two different objects
 * is not what this is about.
 */
async function expectBeforeAndAfter(
  images: import('@playwright/test').Locator,
  what: string,
): Promise<void> {
  await expect(images, `${what} does not show two images`).toHaveCount(2)

  // Polled, not sampled once, and this cost a round's worth of false red.
  //
  // The first version read the decode state immediately after the count
  // reached two -- one `evaluateAll` and a hard assertion. But the count
  // reaching two says the elements are THERE, not that the browser has
  // finished with them: an <img> exists the moment it is rendered and
  // decodes some milliseconds later. So the check raced the decode and
  // failed on about one engine execution in six, naming a broken image
  // while the traces showed both requests answering 200 image/png in
  // ~35ms. The app was never at fault; the gate was reading a
  // point-in-time sample as if it were a settled state -- the same
  // mistake `isVisible()` made in openSurface, in a different costume.
  //
  // A false red is not a cheap failure. It would have gone on hitting
  // every later block's gate, and worse, it can hide a real regression:
  // once a gate is known to flake, its red stops being read.
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

/**
 * Opens one case with one patient photo, from the cases surface, the way
 * a receptionist does: name the patient, attach the photograph, submit
 * once. Leaves the browser on the cases list with the new case in it.
 *
 * The one implementation of that sequence, shared by block A's own
 * journey and by the blocks that start from its result -- so a change to
 * how a case is opened cannot leave the later blocks driving a shape the
 * product no longer has.
 */
async function createCaseWithPhoto(page: Page, name: string): Promise<void> {
  await page.getByRole('button', { name: UI_NAMES.newCase }).click()
  await page.getByRole('textbox', { name: UI_NAMES.caseNameField }).fill(name)

  const chooser = page.waitForEvent('filechooser')
  await page.getByRole('button', { name: UI_NAMES.addPhoto }).click()
  await (await chooser).setFiles(PATIENT_PHOTO)

  await page.getByRole('button', { name: /create|save|confirm/i }).click()
  await expect(page.getByText(name), 'the case must be findable after creation').toBeVisible({
    timeout: 30_000,
  })
}

/** Opens a case whose simulation has already been generated. */
async function openCaseWithSimulation(page: Page): Promise<void> {
  await openCaseWithPhoto(page)
  await expect(
    page.getByRole('region', { name: UI_NAMES.comparison }),
    'blocks C and D start from a generated simulation, which block B is what produces',
  ).toBeVisible()
}
