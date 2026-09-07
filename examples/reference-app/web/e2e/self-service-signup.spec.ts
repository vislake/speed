/**
 * That a practice which signs itself up can actually get in.
 *
 * The sign-in surface offers "No account yet? Register", the form works,
 * and the product then says "Account created. Sign in with the
 * credentials you just registered." Doing exactly that is answered with
 * "Your account is not a member of this organization", and the screen
 * offers nothing further: no way to create a practice, no way to ask for
 * an invitation, no explanation. The product invites a person in, tells
 * them what to do next, and then refuses them for doing it.
 *
 * That is worse than a missing feature. A missing feature is understood;
 * this reads as a broken product, and every prospect who takes the
 * offered path leaves with that impression.
 *
 * The gate below encodes the direction chosen for it: registration is
 * self-service, and registering creates the practice its registrant owns.
 * A person completes the journey the surface offers -- register, sign in,
 * work -- without an administrator anywhere in it. The two halves are
 * asserted separately on purpose: that they get IN (a tenant, a frame),
 * and that they get in as someone who can DO something (the owner of a
 * new practice, not a spectator in someone else's).
 *
 * Invitation remains a second entrance, gated by
 * org-invitation-sign-in.spec.ts. Nothing here says it should not exist;
 * this says it should not be the only way in while the surface advertises
 * another.
 */
import { expect, test } from '@playwright/test'
import {
  APP_TEXT,
  SIGN_IN_TEXT,
  TENANT_NAMES,
  readCurrentTenant,
  submitPasswordSignIn,
  visitSignIn,
} from './test-utils/journeys.js'

/** A password that satisfies authn's real policy (12 characters minimum). */
const SIGNUP_PASSWORD = 'e2e-new-clinic-2026'

test.describe('a practice signing itself up', () => {
  test('registers, signs in, and lands in its own practice', async ({ page }) => {
    const email = `e2e-clinic-${Date.now()}@example.com`

    // The path the surface itself offers a first-time visitor.
    await visitSignIn(page)
    await page.getByRole('button', { name: SIGN_IN_TEXT.registerAction }).click()
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(email)
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill(SIGNUP_PASSWORD)
    await page
      .getByRole('textbox', { name: SIGN_IN_TEXT.displayNameLabel })
      .fill('E2E Dental Clinic')
    await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()
    await expect(page.getByRole('status')).toContainText(APP_TEXT.registerSuccess)

    // Doing exactly what the product just told them to do.
    await page.getByRole('button', { name: APP_TEXT.registerBackToSignIn }).click()
    await submitPasswordSignIn(page, email, SIGNUP_PASSWORD)

    // In: a frame, and a practice of their own. The tenant is NOT one of
    // the demo practices -- a new registrant landing in someone else's
    // clinic would be a far worse defect than being locked out.
    await expect(
      page.getByRole('button', { name: APP_TEXT.navNotes }).or(
        page.getByRole('link', { name: APP_TEXT.navNotes }),
      ),
      'a practice that signs itself up must reach the product',
    ).toBeVisible()
    const tenant = await readCurrentTenant(page).catch(() => '')
    expect(
      TENANT_NAMES as readonly string[],
      'a new registrant must not land inside one of the demo practices',
    ).not.toContain(tenant)
  })

  test('can do the work an owner does, not merely look at it', async ({ page }) => {
    const email = `e2e-clinic-owner-${Date.now()}@example.com`

    await visitSignIn(page)
    await page.getByRole('button', { name: SIGN_IN_TEXT.registerAction }).click()
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(email)
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill(SIGNUP_PASSWORD)
    await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()
    // Wait for the register verdict before leaving the register surface.
    // The surface's "Back to sign in" control exists from the moment the
    // register form shows, so clicking it while the request is still in
    // flight races the register's own verdict: when the request then
    // settles, the host's onRegistered flips the view to the
    // created-account panel and the sign-in form never appears. Leg one
    // waits for the same panel -- the product tells a registrant to sign
    // in with the credentials they just registered, so the gate takes
    // that turn in the same order.
    await expect(page.getByRole('status')).toContainText(APP_TEXT.registerSuccess)
    await page.getByRole('button', { name: APP_TEXT.registerBackToSignIn }).click()
    await submitPasswordSignIn(page, email, SIGNUP_PASSWORD)

    // Owning the practice means being able to write in it. The notes
    // surface stands in for that here because it is the write path this
    // app has today; when the case surface lands (block A), this is the
    // assertion that moves to it.
    await page.getByRole('link', { name: APP_TEXT.navNotes }).click()
    const text = `first note in a self-registered practice ${Date.now()}`
    await page.getByRole('textbox', { name: APP_TEXT.notesTextLabel }).fill(text)
    await page.getByRole('button', { name: APP_TEXT.notesCreateSubmit }).click()

    await expect(
      page.getByRole('row', { name: new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')) }),
      'the owner of a new practice must be able to write in it',
    ).toBeVisible()
  })
})
