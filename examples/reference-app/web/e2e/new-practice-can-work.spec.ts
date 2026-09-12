/**
 * That a practice which just signed itself up can actually use the
 * product it signed up for.
 *
 * WHY THIS EXISTS SEPARATELY FROM THE CORE JOURNEY GATES
 *
 * Every gate in core-journey.pending.spec.ts signs in as a SEEDED demo
 * account. Boot-time seeding gives the demo tenants their entitlement
 * (internal/app/demo/demo_entitlements.go) and their credits
 * (internal/app/demo/demo_credits.go);
 * a clinic created at run time by self-service registration receives the
 * same subscription and credit seed through its own path
 * (internal/app/self_service.go). A gate that only ever signs in as a
 * seeded account cannot see whether that grant is real -- whether a
 * practice that registers, opens a case, attaches the photograph, picks
 * the smile it wants and presses the button gets past the plan and
 * credit refusals to an actual simulation, instead of stopping at a
 * refusal it cannot act on.
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
  expectSignedIn,
  openSurface,
  registerThroughUi,
  submitPasswordSignIn,
} from './test-utils/journeys.js'

import { PATIENT_PHOTO } from './test-utils/cases.js'

/** A password that satisfies authn's real policy (12 characters minimum). */
const SIGNUP_PASSWORD = 'e2e-new-practice-2026'


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

// @budget: this test registers and signs in, which the default tier's
// sign-in budget cannot absorb -- not because anything about it is
// unverified.
test(
  'a practice that just signed itself up can generate its first simulation',
  { tag: '@budget' },
  async ({ page }) => {
    const email = `e2e-new-practice-${Date.now()}@example.com`

    // The whole journey the product offers a first-time visitor, in one
    // sitting, the way somebody evaluating it would.
    await registerThroughUi(page, {
      email,
      password: SIGNUP_PASSWORD,
      displayName: 'Sunrise Dental',
    })

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
