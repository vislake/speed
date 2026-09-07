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
  SHELL_TEXT,
  SIGN_IN_TEXT,
  expectSignedIn,
  openSurface,
  readSettledText,
  submitPasswordSignIn,
  visitSignIn,
} from './test-utils/journeys.js'

import { PATIENT_PHOTO } from './test-utils/cases.js'

/** A password that satisfies authn's real policy (12 characters minimum). */
const SIGNUP_PASSWORD = 'e2e-new-practice-2026'

/**
 * The home surface's own copy, quoted from this app's en-US bundle
 * (home.emptyDescription). The zh-CN half says the same thing, and the
 * defect is in both.
 */
const HOME_TEXT = {
  askAnAdministrator: 'Ask an administrator to enable a feature for this clinic.',
} as const

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

/**
 * That the first screen a new practice sees does not send it looking for
 * somebody who does not exist.
 *
 * Found by walking the journey as a person, which is the only way it
 * could have been: a freshly self-registered practice signs in and its
 * home surface says "No features are enabled yet" and "Ask an
 * administrator to enable a feature for this clinic" -- while the
 * navigation beside it offers Cases, Credits and Notes, and every one of
 * them works. The practice registered itself, so it IS the
 * administrator. There is nobody to ask.
 *
 * The whole journey was then completed from that same account, by
 * clicking, with nothing enabled by anyone: a case opened with a
 * photograph, the smile and shade chosen, a simulation generated and
 * shown beside the original, a patient link minted, the cost disclosed
 * as ten credits, and the ledger showing the deduction against a
 * thousand-credit seed. So the sentence is not a warning about a real
 * limitation -- it is false, and it is the first thing the product says.
 *
 * WHY NO EXISTING GATE CAUGHT IT
 *
 * home-is-self-consistent.spec.ts asks the neighbouring question -- does
 * the home surface promise cards it has none of -- and asks it of a
 * SEEDED demo account, which is granted the feature flags a real
 * registrant is not. That is the wrong-population lesson for the third
 * time in this suite (the clinic-naming gate, then the four core-journey
 * gates, now this), and the shape is always the same: the question was
 * right and the population was not. This gate asks it of the population
 * that actually meets the screen.
 *
 * WHAT IT DELIBERATELY DOES NOT PRESCRIBE
 *
 * Not what the home surface should say instead. Listing what the
 * practice can do, describing the trial it was given, or saying nothing
 * at all would each pass. What fails is telling a self-registered owner
 * that nothing works and to go ask an administrator, while the product
 * works and there is no administrator.
 */
test(
  'the first screen a new practice sees does not tell it to ask an administrator',
  { tag: '@pending' },
  async ({ page }) => {
    const email = `e2e-first-screen-${Date.now()}@example.com`

    await visitSignIn(page)
    await page.getByRole('button', { name: SIGN_IN_TEXT.registerAction }).click()
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(email)
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill(SIGNUP_PASSWORD)
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.displayNameLabel }).fill('Daybreak Dental')
    await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()
    await expect(page.getByRole('status')).toContainText(APP_TEXT.registerSuccess)

    await page.getByRole('button', { name: APP_TEXT.registerBackToSignIn }).click()
    await submitPasswordSignIn(page, email, SIGNUP_PASSWORD)
    await expectSignedIn(page)

    // The home surface is where a sign-in lands, so this is read without
    // navigating anywhere: it is the first thing the practice is told.
    const home = await readSettledText(page.getByRole('main'))

    expect(
      home,
      `a practice that just registered itself is told to ask an administrator to enable features -- it registered itself, so there is nobody to ask, and the navigation beside this message offers Cases, Credits and Notes, all of which work. Home said: ${home}`,
    ).not.toContain(HOME_TEXT.askAnAdministrator)

    // And the positive half, so the gate cannot be satisfied by a home
    // surface that says nothing at all while still being useless: the
    // practice must be able to see, from this screen, that it can work.
    // Any of the nav entries naming a capability satisfies it -- the
    // assertion is about the frame the practice lands in, not about a
    // card layout nobody has committed to.
    await expect(
      page.getByRole('link', { name: /cases|patients/i }).or(page.getByRole('button', { name: SHELL_TEXT.openNav })).first(),
      'the frame offers no way to reach the practice\'s own work',
    ).toBeVisible()
  },
)
