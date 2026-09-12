/**
 * That a practice which signs itself up can actually get in.
 *
 * Registration is self-service: registering creates the practice its
 * registrant owns, and the journey the surface offers -- register, sign
 * in, work -- has no administrator anywhere in it. The journey the
 * advertised copy describes must end in work, never in a refusal the
 * screen offers no way past. The two halves are asserted separately on
 * purpose: that they get IN (a tenant, a frame), and that they get in as
 * someone who can DO something (the owner of a new practice, not a
 * spectator in someone else's).
 *
 * Invitation remains a second entrance, gated by
 * org-invitation-sign-in.spec.ts. Nothing here says it should not exist;
 * this says it should not be the only way in while the surface advertises
 * another.
 */
import { expect, test } from '@playwright/test'
import {
  APP_TEXT,
  expectOutsideDemoOrganizations,
  expectSignedIn,
  openSurface,
  registerThroughUi,
  submitPasswordSignIn,
} from './test-utils/journeys.js'

/** A password that satisfies authn's real policy (12 characters minimum). */
const SIGNUP_PASSWORD = 'e2e-new-clinic-2026'

// @budget, not untagged: these two tests are VERIFIED -- registration
// provisions the registrant's own clinic and the sign-in reaches it --
// and they are out of the default run only because their sign-in
// attempts would push that tier against go/authn's per-account and
// per-IP ceilings, which the default run already spends.
// `pnpm test:e2e:budget` gives them a fresh budget of their own; see
// e2e/README.md.
test.describe('a practice signing itself up', { tag: '@budget' }, () => {
  test('registers, signs in, and lands in its own practice', async ({ page }) => {
    const email = `e2e-clinic-${Date.now()}@example.com`

    // The path the surface itself offers a first-time visitor.
    await registerThroughUi(page, {
      email,
      password: SIGNUP_PASSWORD,
      displayName: 'E2E Dental Clinic',
    })

    // Doing exactly what the product just told them to do.
    await page.getByRole('button', { name: APP_TEXT.registerBackToSignIn }).click()
    await submitPasswordSignIn(page, email, SIGNUP_PASSWORD)

    // In: a frame -- asserted by the sign-out control, the one control
    // present at every screen size, since a nav link is not in the DOM
    // at all below the md breakpoint -- and in a practice of their own.
    // The two halves are asserted in order: a tenant is named at all,
    // and it is not one of the demo practices. An assertion of only the
    // second would pass whenever the read THREW (an unloaded frame, an
    // unrendered switcher, a bug in the helper), so it could not tell
    // "landed in its own clinic" from "shows no tenant at all" -- and a
    // new registrant landing in someone else's clinic would be a far
    // worse defect than being locked out.
    await expectSignedIn(page)
    await expectOutsideDemoOrganizations(page)
  })

  test('can do the work an owner does, not merely look at it', async ({ page }) => {
    const email = `e2e-clinic-owner-${Date.now()}@example.com`

    // Leg one takes the turn the product asks of a registrant: sign in
    // with the credentials just registered. registerThroughUi returns on
    // the created-account panel, which is the verdict the back-to-sign-in
    // click must not race (the helper's own doc tells that story).
    await registerThroughUi(page, { email, password: SIGNUP_PASSWORD })
    await page.getByRole('button', { name: APP_TEXT.registerBackToSignIn }).click()
    await submitPasswordSignIn(page, email, SIGNUP_PASSWORD)

    // Owning the practice means being able to write in it. The notes
    // surface carries the assertion here as this app's tenant-scoped
    // write path; the case write path is the core-journey gates in
    // core-journey.spec.ts.
    await openSurface(page, APP_TEXT.navNotes)
    const text = `first note in a self-registered practice ${Date.now()}`
    await page.getByRole('textbox', { name: APP_TEXT.notesTextLabel }).fill(text)
    await page.getByRole('button', { name: APP_TEXT.notesCreateSubmit }).click()

    await expect(
      page.getByRole('row', { name: new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')) }),
      'the owner of a new practice must be able to write in it',
    ).toBeVisible()
  })
})
