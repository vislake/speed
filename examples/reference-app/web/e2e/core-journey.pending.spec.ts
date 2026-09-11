/**
 * The acceptance gates for the product's own reason to exist: a dental
 * practice uploading a patient's photo, generating a smile simulation,
 * comparing it with the original, sharing it with the patient, and seeing
 * what it cost.
 *
 * The gates pass in one run, on each engine, with the run's own
 * throwaway OpenAI-compatible images provider standing in for the vendor
 * (fake-image-provider.mjs under e2e/test-utils, wired in
 * playwright.config.ts) so a freshly booted server completes the
 * generation deterministically:
 *
 *   pnpm test:e2e:budget
 *
 * The suite paces its sign-ins inside go/authn's per-account login
 * limits (test-utils/journeys.ts's payTheLoginBudget), which is why the
 * whole file can be asked for at once rather than block by block. The
 * tag is @budget: together the gates spend the demo-owner account's
 * whole login allowance, so in the default tier they would be waits
 * rather than sign-ins.
 *
 * The gates must run together: each run boots a fresh server and a
 * fresh database, and a later gate's helpers open a case an earlier
 * gate's own tests create in the same run.
 *
 * WHAT THESE ASSERT, AND WHAT THEY DELIBERATELY DO NOT
 *
 * They assert what a person must be able to ACCOMPLISH, by clicking:
 * reach the surface, do the thing, see the result. They do not prescribe
 * layout, wording, component choice or route shape. Where a locator names
 * a control, the name is this suite's expectation of an accessible name,
 * not a design instruction -- naming a control is a conversation about
 * what it should be called, worth having in the open rather than buried
 * in a test id, and test-utils/cases.ts's CASE_UI is the one place to
 * reconcile when a name changes.
 *
 * The gates are ordered along the journey (the entrance gate first),
 * and each is independently runnable.
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER, DEMO_READER } from './test-utils/accounts.js'
import {
  openSurface,
  readCurrentTenant,
  signInAs,
  switchTenant,
} from './test-utils/journeys.js'
import {
  CASE_UI,
  PATIENT_PHOTO,
  caseName,
  createCaseWithPhoto,
  expectBeforeAndAfter,
  openCaseWithPhoto,
  openCaseWithSimulation,
} from './test-utils/cases.js'

test.describe('the core journey', { tag: '@budget' }, () => {
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
    await openSurface(page, CASE_UI.navCases)

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
    // personal notebook: a receptionist opens cases and a dentist runs
    // the simulations, so the people who treat a patient together must
    // see the same case -- one patient, one chart. Tenant isolation is
    // what protects another practice's cases (proven separately in
    // authorization.spec.ts); inside one practice the case is shared.
    const name = caseName()

    await signInAs(page, DEMO_OWNER)
    await openSurface(page, CASE_UI.navCases)
    await page.getByRole('button', { name: CASE_UI.newCase }).click()
    await page.getByRole('textbox', { name: CASE_UI.caseNameField }).fill(name)
    const chooser = page.waitForEvent('filechooser')
    await page.getByRole('button', { name: CASE_UI.addPhoto }).click()
    await (await chooser).setFiles(PATIENT_PHOTO)
    await page.getByRole('button', { name: /create|save|confirm/i }).click()
    await expect(page.getByText(name)).toBeVisible({ timeout: 30_000 })
    // The clinic the case was opened in, read from the frame rather
    // than assumed (an account's landing tenant is the first row of its
    // own membership enumeration, an app-seeded fact rather than a
    // contract): the colleague's visit is aimed at the clinic where the
    // case actually lives, the same shift-change switch a front desk
    // makes.
    const clinic = await readCurrentTenant(page)

    // A colleague in the same practice, signing in on the same machine
    // the way a shift change happens at a front desk.
    await signInAs(page, DEMO_READER)
    await switchTenant(page, clinic)
    await openSurface(page, CASE_UI.navCases)
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

    await page.getByRole('button', { name: CASE_UI.simulate }).click()

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
    const comparison = page.getByRole('region', { name: CASE_UI.comparison })
    await expect(comparison, 'the result must be shown against the original').toBeVisible({
      timeout: 120_000,
    })
    await expectBeforeAndAfter(comparison.getByRole('img'), "the practice's comparison")
  })

  test('block C: the result becomes a link a patient can open', async ({ page, context }) => {
    await signInAs(page, DEMO_OWNER)
    await openCaseWithSimulation(page)

    await page.getByRole('button', { name: CASE_UI.share }).click()

    // The link is handed to the practice in a form they can actually send
    // -- readable on screen, not only in a clipboard a test cannot read.
    const link = page.getByRole('textbox', { name: CASE_UI.share })
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
      //
      // A PAGE with the PAIR on it, not a file and not the result
      // alone: "before/after comparison" is a core requirement and it
      // has to hold on the PATIENT's side -- the result alone is not a
      // delivery -- so the patient's landing is a side-by-side page.
      // This is also what tells a page from a file: go/sharing's public
      // route answers the requested resource's raw bytes with its own
      // MIME type (handler.go's io.Copy over the resolved content), and
      // a browser given image bytes builds a document around an <img> --
      // no text at all, no practice name, no explanation, no BEFORE.
      //
      // Text first, because it is the cheapest way to tell a page from a
      // file: a bare image document contains no text at all. What the
      // page should SAY -- the practice's name, the patient's, an
      // explanation -- is still a product decision this gate does not
      // make.
      await expect(
        patientPage.locator('body'),
        'the patient received a bare file rather than a page: nothing on it says whose smile this is',
      ).not.toHaveText('')

      await expectBeforeAndAfter(patientPage.getByRole('img'), "the patient's page")

      // AGAIN, because a patient opens the link more than once: from the
      // message when it arrives, then later to show someone at home.
      // This app mints its patient links with no view cap (share-api.ts
      // sends only resourceRef), so a second visit is supposed to work
      // and does; go/sharing counts a served view against a grant's cap
      // only when the grant carries one, so the repeated-open journey
      // stays pinned against that accounting rather than assumed.
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
    // A NUMBER, not the vocabulary: a surface that said the word
    // "credits" and no amount would pass nothing a practice can check --
    // a cost that does not say how much is not a cost.
    const cost = page.getByRole('main').getByText(CASE_UI.cost).first()
    await expect(cost, 'a generation must say what it cost').toBeVisible()
    await expect(
      cost,
      'the surface names the idea of a cost but never an amount, so a practice still cannot tell what this generation spent',
    ).toContainText(/\d/)
    // NOT zero: a tenant with no ledger rows answers an all-zero balance
    // by design (a zero state rather than a missing-resource refusal,
    // which is the right answer for an empty ledger), so "a digit
    // appears" is satisfied by "0". This generation did spend -- either
    // the charge did not happen or the figure is wrong, and both are
    // worth failing on: a generation that costs nothing is not a
    // pay-per-use product.
    await expect(
      cost,
      'this generation is reported as costing zero, so either nothing was charged for it or the figure shown is wrong -- a pay-per-use product cannot say both that it charges and that this was free',
    ).toContainText(/[1-9]/)

    // And the standing balance, somewhere a person can check it.
    // Reached through openSurface rather than by clicking the link
    // directly: below the md breakpoint the navigation is behind the
    // menu button, and a direct click works only on wide screens.
    await openSurface(page, CASE_UI.navCredits)
    // Polled to settle, because the balance is a fetch and the heading
    // renders before the answer arrives: a point-in-time read would
    // resolve on how fast the engine got the answer, reporting nothing
    // about the product.
    const main = page.getByRole('main')
    await expect
      .poll(async () => await main.innerText(), { timeout: 15_000 })
      .toMatch(/\d/)

    // The line that carries the FIGURE, not the label above it: the
    // standalone "Balance" label never contains a number by design --
    // the amount lives in its own line ("N credits available"). Matched
    // on a number next to the word rather than on either alone: a label
    // with no figure fails, and a figure belonging to something else is
    // not accepted just for being a digit somewhere on the page.
    const balance = main.getByText(/\d[\d,.]*\s*(credit|credits)/i).first()
    await expect(
      balance,
      'the practice must be able to see its remaining credits: no line on this surface states an amount of credits',
    ).toBeVisible()
  })
})

/**
 * That opening a patient's photo does not spend money without saying so.
 *
 * The panel gives a photo with no simulation on it one automatic
 * generation the moment it can act (photo-simulation-panel.tsx's
 * autoPreviewStartedRef), a deliberate product decision: the case page
 * would otherwise open on nothing but pickers and a blank promise, so a
 * practice's first look at a new patient photo is the comparison the
 * auto-run produces. This gate does not argue with that.
 *
 * What it holds is the disclosure. A generation costs credits -- the
 * product says so itself, in the line it renders AFTERWARDS ("This
 * simulation cost N credits") -- so a dentist who opens a case to look
 * at a photograph has spent money before touching a control, and learns
 * the price only once it is gone. On a pay-per-use product that is the
 * difference between a feature and a surprise on the invoice: nobody
 * disputes the charge for a simulation they asked for, and nobody
 * expects one for a page they opened.
 *
 * The automatic run's credit cost is disclosed ahead of it, under the
 * Smile simulation heading before the option pickers. The bar the gate
 * holds is that SOMETHING on the surface, before or as the automatic
 * run happens, connects it to a cost -- a line saying the preview uses
 * a credit, a balance shown beside the panel, a first-visit note, or an
 * explicit "generate the first preview" button that makes the spend a
 * choice would each pass. Silence does not.
 */
// @budget rather than untagged: it opens a case and attaches a photo,
// which the default tier's sign-in budget cannot absorb.
test(
  'opening a case does not spend a credit without telling anyone',
  { tag: '@budget' },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)

    const name = caseName()
    await openSurface(page, CASE_UI.navCases)
    await createCaseWithPhoto(page, name)
    await page.getByText(name).click()

    // Wait for the case page itself: opening a case is a network round
    // trip, and reading too early samples the cases list, which no
    // honest surface would have a cost word on.
    await expect(
      page.getByRole('img', { name: /photo|patient|before/i }).first(),
      'the case never opened, so nothing here is about what it discloses',
    ).toBeVisible({ timeout: 30_000 })

    // The work area once the photo is open and before the automatic run
    // has produced anything: whatever that run is about to spend, this is
    // everything the person was told about it.
    const workArea = await page.getByRole('main').innerText()

    expect(
      workArea,
      'opening a case starts a generation that spends credits, and nothing on the surface mentions a cost, a balance or a choice -- so a dentist who opened a patient photograph to look at it finds out what it cost only after the money is gone',
    ).toMatch(/credit|cost|balance|charge/i)
  },
)

/**
 * That nothing on the share panel is still a placeholder.
 *
 * i18next interpolates {{name}} with DOUBLE braces; a single-braced
 * token such as "{date}" is never substituted and reaches the screen
 * verbatim, in both zh-CN and en-US. The panel says when the patient's
 * link expires -- the one thing that sentence exists to say -- so a
 * literal "{date}" there tells a practice nothing about how long the
 * patient has.
 *
 * Asserted as a CLASS rather than as this one string: any single-braced
 * token surviving into rendered text is the same defect wearing a
 * different name, and this is the cheapest place to notice the next one.
 *
 * @budget rather than untagged: it signs in and generates.
 */
test(
  'the share panel shows a date, not a placeholder',
  { tag: '@budget' },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await openCaseWithSimulation(page)
    await page.getByRole('button', { name: CASE_UI.share }).click()
    await expect(page.getByRole('textbox', { name: CASE_UI.share })).toBeVisible()

    const panel = await page.getByRole('main').innerText()
    const placeholders = panel.match(/\{[a-zA-Z_][\w.]*\}/g) ?? []
    expect(
      placeholders,
      `the share panel shows un-substituted placeholders (${placeholders.join(' , ')}) -- i18next interpolates {{name}}, so a single-braced token reaches the screen verbatim`,
    ).toEqual([])
  },
)
