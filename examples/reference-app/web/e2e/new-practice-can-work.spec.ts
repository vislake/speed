/**
 * That a practice which just signed itself up can actually use the
 * product it signed up for.
 *
 * WHY THIS EXISTS SEPARATELY FROM THE CORE JOURNEY GATES
 *
 * Every gate in core-journey.pending.spec.ts signs in as a SEEDED demo
 * account, and the seeds are granted things a real registrant is not:
 * boot-time seeding gives an entitlement to each of cfg.HostTenants
 * (cmd/server/demo_entitlements.go) and credits to the same set
 * (demo_credits.go). A clinic created at run time by self-service
 * registration is in neither list -- self_service.go does not mention
 * billing at all -- so those gates are structurally blind to whether a
 * new practice can do anything.
 *
 * They were all green while a browser walk-through of the same journey
 * stopped dead at "This clinic's plan does not include smile
 * simulation." A practice registers, opens a case, attaches the
 * photograph, picks the smile it wants, presses the button, and is told
 * its plan does not include the one thing the product is for. That is
 * the self-service dead end in its second form: the first refused them
 * at the door, this one lets them in and refuses them at the work.
 *
 * The lesson is about the population a gate asks its question of, which
 * this suite has now had twice: the clinic-naming gate asked "can a
 * person tell which clinic they are in" only of clinics configured
 * before the server booted, and these ask "can a practice work" only of
 * practices that were handed an entitlement. Both questions were right.
 * Both populations were wrong.
 *
 * WHAT IT DELIBERATELY DOES NOT PRESCRIBE
 *
 * How a new practice comes to be able to work is a product decision --
 * a trial entitlement at signup, a free allowance, a plan chosen during
 * registration, an explicit "start your trial" step. This gate asserts
 * only that the journey the product offers does not end in a refusal
 * the person cannot act on. A surface that offered a way to fix it
 * (choose a plan, start a trial, ask an owner) would be a different
 * product decision and would need a different gate; being told "your
 * plan does not include this" with nothing to do about it is not one.
 */
import { expect, test } from '@playwright/test'
import {
  APP_TEXT,
  SIGN_IN_TEXT,
  expectSignedIn,
  openSurface,
  submitPasswordSignIn,
  visitSignIn,
} from './test-utils/journeys.js'

/** A password that satisfies authn's real policy (12 characters minimum). */
const SIGNUP_PASSWORD = 'e2e-new-practice-2026'

/** A small, valid PNG standing in for a patient photograph. */
const PATIENT_PHOTO = {
  name: 'patient-before.png',
  mimeType: 'image/png',
  buffer: Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
    'base64',
  ),
}

/**
 * The refusals a new practice must not be left holding. Quoted from the
 * app's own en-US bundle: an entitlement refusal names the plan, a
 * credit refusal names the balance. Either one, reached by following the
 * product's own instructions with no way forward offered, is the defect.
 */
const DEAD_END_TEXT = [
  "This clinic's plan does not include smile simulation.",
  'This clinic has no credits left for a smile simulation.',
] as const

// @budget now: the defect this gate found is closed. Self-service
// registration grants the new clinic the same subscription and credit
// seed the demo tenants get (1b6f9a2), so the journey the product offers
// no longer ends in a refusal the person cannot act on. It stays out of
// the default run because it registers and signs in, which the sign-in
// budget cannot absorb -- not because anything about it is unverified.
test(
  'a practice that just signed itself up can generate its first simulation',
  { tag: '@budget' },
  async ({ page }) => {
    const email = `e2e-new-practice-${Date.now()}@example.com`

    // The whole journey the product offers a first-time visitor, in one
    // sitting, the way somebody evaluating it would.
    await visitSignIn(page)
    await page.getByRole('button', { name: SIGN_IN_TEXT.registerAction }).click()
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(email)
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill(SIGNUP_PASSWORD)
    await page
      .getByRole('textbox', { name: SIGN_IN_TEXT.displayNameLabel })
      .fill('Sunrise Dental')
    await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()
    await expect(page.getByRole('status')).toContainText(APP_TEXT.registerSuccess)

    await page.getByRole('button', { name: APP_TEXT.registerBackToSignIn }).click()
    await submitPasswordSignIn(page, email, SIGNUP_PASSWORD)
    await expectSignedIn(page)

    // A patient, a photograph, and the smile this practice wants to
    // show them.
    await openSurface(page, /cases|patients/i)
    await page.getByRole('button', { name: /new case|create case|add case/i }).click()
    await page
      .getByRole('textbox', { name: /case name|patient|reference/i })
      .fill(`E2E first patient ${Date.now()}`)
    const chooser = page.waitForEvent('filechooser')
    await page.getByRole('button', { name: /add photo|upload photo|choose file|upload/i }).click()
    await (await chooser).setFiles(PATIENT_PHOTO)
    await page.getByRole('button', { name: /create|save|confirm/i }).click()

    await page.getByRole('listitem').filter({ hasText: /E2E first patient/ }).first().click()
    await page.getByRole('button', { name: /simulate|generate/i }).click()

    // NOT a refusal it cannot act on.
    //
    // Asserted before the success, and separately, so the failure names
    // the actual answer rather than reporting that a comparison region
    // never appeared -- which is true of a refusal too, and says nothing
    // about why.
    for (const refusal of DEAD_END_TEXT) {
      await expect(
        page.getByText(refusal),
        `a practice that just registered followed the product's own instructions and was told "${refusal}", with nothing on the screen to do about it -- the self-service dead end in its second form`,
      ).toHaveCount(0)
    }

    // And the simulation it came for.
    await expect(
      page.getByRole('region', { name: /before.*after|comparison/i }),
      'the first simulation a new practice asks for never appears',
    ).toBeVisible({ timeout: 120_000 })
  },
)
