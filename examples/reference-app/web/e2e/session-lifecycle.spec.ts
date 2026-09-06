/**
 * The session lifecycle at the surface: signing out, what the app shows
 * afterwards, signing back in, and what a reload does.
 *
 * The three-branch view machine (@speed/product-shell's ProductShell) is
 * what this spec observes: anonymous-and-never-in-the-app shows the
 * sign-in surface, authenticated shows the frame, and
 * anonymous-after-the-app-was-reached shows the session-ended screen.
 * The last branch is what a sign-out lands on today -- deliberately, per
 * that component's own doc comment, because the machine cannot tell a
 * deliberate sign-out from a session that died mid-use.
 *
 * That is worth stating plainly, because this spec PINS current
 * behaviour rather than endorsing it: telling someone who just chose to
 * sign out that their "sign-in is no longer valid" reads as a failure
 * report for a successful action, and it has been raised as a product
 * decision to revisit. If that decision lands, this spec is the place
 * the change surfaces -- a deliberate sign-out would then be expected to
 * return to the sign-in surface, and the session-ended copy to belong to
 * a genuinely dead session alone.
 *
 * The account here is the single-tenant one, not the owner. Sign-in is
 * rate-limited per account (go/authn's ratelimit.go: five per account
 * per minute), and this file signs in more than any other, so it spends
 * an account the write-path specs do not need. Spreading sign-ins across
 * the three seeded accounts is how the suite lives inside a real
 * server's real limits instead of papering over them with retries.
 */
import { expect, test } from '@playwright/test'
import { DEMO_ACME_ONLY } from './test-utils/accounts.js'
import {
  APP_TEXT,
  SESSION_TEXT,
  SIGN_IN_TEXT,
  signInAs,
  submitPasswordSignIn,
} from './test-utils/journeys.js'

test('signing out ends the session and signing in again reaches the frame', async ({ page }) => {
  await signInAs(page, DEMO_ACME_ONLY)

  await page.getByRole('button', { name: SESSION_TEXT.signOut }).click()

  // Current behaviour: the session-ended branch, with its own action
  // back to the sign-in surface. See this file's header.
  await expect(page.getByText(SESSION_TEXT.endedTitle)).toBeVisible()
  await expect(page.getByRole('button', { name: SESSION_TEXT.signInAgain })).toBeVisible()
  // The frame is gone: no nav, nothing tenant-scoped left on screen.
  await expect(page.getByRole('link', { name: APP_TEXT.navNotes })).toHaveCount(0)

  await page.getByRole('button', { name: SESSION_TEXT.signInAgain }).click()
  await expect(page.getByRole('button', { name: SIGN_IN_TEXT.submit })).toBeVisible()

  await submitPasswordSignIn(page, DEMO_ACME_ONLY.email, DEMO_ACME_ONLY.password)
  await expect(page.getByRole('link', { name: APP_TEXT.navNotes })).toBeVisible()
})

test('a reload after signing in starts anonymous', async ({ page }) => {
  await signInAs(page, DEMO_ACME_ONLY)

  await page.reload()

  // The access token lives in memory only (@speed/api-client's memory
  // store) and the refresh token in the session closure, so a reload
  // genuinely starts over -- nothing was written to storage for it to
  // pick up. A visitor who reloads is a fresh visitor, which is why this
  // lands on the sign-in surface rather than the session-ended screen.
  await expect(page.getByRole('button', { name: SIGN_IN_TEXT.submit })).toBeVisible()
  await expect(page.getByRole('link', { name: APP_TEXT.navNotes })).toHaveCount(0)
})
