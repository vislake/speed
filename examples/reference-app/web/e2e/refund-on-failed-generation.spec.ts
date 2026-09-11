/**
 * That a generation which fails gives the credits back.
 *
 * This is the most expensive wrong path in a pay-per-use product, and
 * the reason is what it does NOT look like: nobody complains about a
 * silent failure to refund. A practice whose simulation failed sees an
 * error, tries again, and never audits the ledger -- so the charge for
 * work that was never delivered sits there until someone reconciles,
 * which for a small clinic may be never. Charging for a delivered
 * simulation is the business; charging for a failed one is taking money
 * for nothing, and it accumulates quietly.
 *
 * The refusal is real: the fake vendor can be armed to refuse
 * (FAKE_IMAGE_FAIL=1 in e2e/test-utils/fake-image-provider.mjs), and the
 * ledger the refund restores has a surface to read (go/billing's
 * credits HTTP surface and the app's credits view) -- so the claim this
 * gate makes is checkable rather than argued.
 *
 * WHY IT ASSERTS THE LEDGER ROW AND NOT THE BALANCE
 *
 * A balance that returns to its old number is weaker evidence than it
 * looks: it is equally consistent with the charge never having been
 * taken, and a product that forgot to charge would pass a gate written
 * that way while being wrong in the other direction. The ledger says
 * what actually happened -- a deduction, then its refund, each its own
 * row -- and go/billing exposes those rows rather than only the total,
 * which is what makes the stronger question askable.
 *
 * THE VENDOR IS THE ONLY THING FAKED
 *
 * Everything downstream of the refusal is real: go/ai-gateway parses the
 * answer, the job fails, the business module's compensation runs, and
 * the credits ledger records it. This suite does not simulate a refund;
 * it makes a generation fail and then asks the product what it did about
 * the money.
 */
import { expect, test } from '@playwright/test'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { REFUND_API_PORT, REFUSING_IMAGE_PORT } from '../playwright.config.js'
import { bootImageProvider, bootServer, routeApiTo } from './test-utils/servers.js'
import { DEMO_OWNER } from './test-utils/accounts.js'
import {
  expectSignedIn,
  openSurface,
  submitPasswordSignIn,
  visitSignIn,
} from './test-utils/journeys.js'
import { PATIENT_PHOTO } from './test-utils/cases.js'

/** The app's own credits copy (its en-US bundle). */
const CREDITS_TEXT = {
  nav: 'Credits',
  /** The ledger row a refunded simulation renders as. */
  refundedRow: 'Simulation refunded',
  /** The row a charge renders as while the job is still running. */
  pendingRow: 'Simulation (in progress)',
  /**
   * How the panel says a generation is over and will not be retried
   * (the app's smilesim.status.deadLetter copy). It renders as a row in
   * the attempt list, not as an alert.
   */
  failedAttempt: 'This generation failed and cannot be retried.',
} as const


/**
 * This spec's own database and server. The run's shared server cannot be
 * the one under test: a vendor armed to refuse would fail the generation
 * gate too, and that gate is what proves generation works.
 */
const databasePath = join(
  tmpdir(),
  `reference-app-e2e-refund-${Date.now()}-${process.pid}.db`,
)

// @budget: verified, and out of the default run only because it boots
// two extra processes of its own and spends a sign-in.
test(
  'a generation that fails gives the credits back, and says so in the ledger',
  { tag: '@budget' },
  async ({ page }) => {
    // A REFUSING vendor of this spec's own, and a server pointed at it:
    // FAKE_IMAGE_FAIL is the fake provider process's switch, so the
    // server must be given the vendor's address explicitly
    // (APP_AI_GATEWAY_IMAGE_BASE_URL) or it would reach for a real
    // vendor and every generation would fail for that reason instead.
    const vendor = await bootImageProvider({ port: REFUSING_IMAGE_PORT, refuse: true })
    const server = await bootServer({
      port: REFUND_API_PORT,
      databasePath,
      env: {
        APP_AI_GATEWAY_IMAGE_BASE_URL: `http://127.0.0.1:${REFUSING_IMAGE_PORT}`,
        APP_AI_GATEWAY_IMAGE_API_KEY: 'e2e-image-key',
      },
    })

    try {
      await routeApiTo(page, REFUND_API_PORT)

      await visitSignIn(page)
      await submitPasswordSignIn(page, DEMO_OWNER.email, DEMO_OWNER.password)
      // expectSignedIn identifies the frame by the sign-out control,
      // which exists below the md breakpoint where the navigation is
      // behind the menu button and no nav link is in the DOM.
      await expectSignedIn(page)

      // A case with a photo, then a generation that will fail.
      // Probe: prove we are signed in and on the cases surface before
      // driving anything, so a silent skip cannot masquerade as a pass.
      await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible()
      await openSurface(page, /cases|patients/i)
      await expect(page.getByRole('heading', { level: 1 })).toContainText(/cases/i)
      await page.getByRole('button', { name: /new case|create case|add case/i }).click()
      await page
        .getByRole('textbox', { name: /case name|patient|reference/i })
        .fill(`E2E refund ${Date.now()}`)
      const chooser = page.waitForEvent('filechooser')
      await page.getByRole('button', { name: /add photo|upload photo|choose file|upload/i }).click()
      await (await chooser).setFiles(PATIENT_PHOTO)
      await page.getByRole('button', { name: /create|save|confirm/i }).click()

      await page.getByRole('listitem').filter({ hasText: /E2E refund/ }).first().click()
      // NOT clicked. The panel generates once on its own for a photo
      // with no simulation on it (photo-simulation-panel.tsx's automatic
      // default preview, one auto-run per mount), so a click would add a
      // SECOND generation and this gate would be about two charges
      // rather than one refund. The auto-run is the honest single
      // subject here: it is a real generation, against a vendor that
      // refuses, and the money it takes has to come back.

      // THE PERSON IS TOLD. A charge taken for work that then failed is
      // bad; a charge taken for work that failed silently is worse,
      // because nobody knows to look.
      await expect(
        page.getByText(CREDITS_TEXT.failedAttempt).first(),
        'the generation failed and the surface never said so, so a practice is left waiting for a simulation that is not coming',
      ).toBeVisible({ timeout: 120_000 })

      // EXACTLY ONE generation happened, so the ledger below is about
      // one charge: the auto-run's own job, whose failure this gate
      // follows to the refund.

      // THE MONEY CAME BACK, as a row that says what happened.
      //
      // Polled: the refund is the job's failure compensation, which runs
      // after the vendor's refusal travels back through the queue -- the
      // server's own timing, not the browser's. A refund that arrives
      // eventually is correct; one that never arrives is the defect.
      await openSurface(page, CREDITS_TEXT.nav)
      // On the credits surface, asserted rather than assumed: a ledger
      // read from whatever page came before would not be the ledger.
      await expect(
        page.getByRole('heading', { level: 1 }),
        'the gate never reached the credits surface, so whatever it read next was not the ledger',
      ).toContainText(/credits/i)
      const ledger = page.getByRole('main')
      await expect
        .poll(async () => await ledger.innerText(), { timeout: 60_000 })
        .toContain(CREDITS_TEXT.refundedRow)

      // And nothing is left sitting as a live charge for it. A pending
      // row that never resolves is the same defect wearing a different
      // face: the credits are reserved, unusable, and nobody was told.
      // Polled to settle: the refund and the pending row clearing are
      // the same compensation, and the ledger is read through a fetch --
      // a pending row that is still there for a moment is the queue
      // working, while one that never clears is credits reserved
      // against work that will never be delivered.
      //
      // The window is deliberately generous because of how a late
      // failure is settled: the panel polls a job only while it is
      // mounted, so a generation whose failure lands after the person
      // navigates away is settled by the boot-time reconcile sweep
      // instead, on the sweep's own cadence -- the timeout below is
      // sized to outlast it. Demanding a shorter interval would assert a
      // product expectation (a faster sweep, or a completion signal that
      // needs no observer) that is nowhere stated, rather than checking
      // the mechanism that exists.
      //
      // What it still catches is the thing that matters: credits
      // reserved against a failed generation that are NEVER released.
      await expect
        .poll(async () => await ledger.getByText(CREDITS_TEXT.pendingRow).count(), {
          timeout: 400_000,
          intervals: [5_000],
        })
        .toBe(0)
    } finally {
      await server.stop()
      await vendor.stop()
    }
  },
)
