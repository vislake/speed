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
 * report for a successful action. A deliberate sign-out returning to
 * the sign-in surface, with the session-ended copy reserved for a
 * genuinely dead session, is an open product decision the machine
 * cannot take alone -- it cannot tell a deliberate sign-out from a
 * session that died mid-use; until that decision is made, the
 * session-ended branch is what both land on, and this spec holds it.
 *
 * The account here is the single-tenant one, not the owner. Sign-in is
 * rate-limited per account (go/authn's ratelimit.go), and this file
 * signs in more than any other, so it spends an account the write-path
 * specs do not need. Spreading sign-ins across the three seeded accounts
 * is how the suite lives inside a real server's real limits instead of
 * papering over them with retries.
 */
import { expect, test } from '@playwright/test'
import { DEMO_ACME_ONLY } from './test-utils/accounts.js'
import {
  SESSION_TEXT,
  SIGN_IN_TEXT,
  expectSignedIn,
  expectSignedOut,
  signInAs,
  submitPasswordSignIn,
} from './test-utils/journeys.js'

test('signing out ends the session and signing in again reaches the frame', async ({ page }) => {
  await signInAs(page, DEMO_ACME_ONLY)

  await page.getByRole('button', { name: SESSION_TEXT.signOut }).click()

  // Current behaviour: the session-ended branch, with its own action
  // back to the sign-in surface. See this file's header.
  // The screen's own heading, not any text saying so: a text match for
  // 'Session ended' would also hit the live-region announcement that
  // renders for a screen reader -- a context change that ends a session
  // has to be announced, not only drawn. Naming the heading says "the
  // session-ended screen is showing" and stays true however many places
  // also say it in words.
  await expect(page.getByRole('heading', { name: SESSION_TEXT.endedTitle })).toBeVisible()
  await expect(page.getByRole('button', { name: SESSION_TEXT.signInAgain })).toBeVisible()
  // The frame is gone. Asserted through the sign-out control, not a nav
  // link: a nav link is absent on a narrow viewport whether or not
  // anyone is signed in, so a nav-based check would pass on the iPad
  // project for a reason that has nothing to do with signing out.
  await expectSignedOut(page)

  await page.getByRole('button', { name: SESSION_TEXT.signInAgain }).click()
  await expect(page.getByRole('button', { name: SIGN_IN_TEXT.submit })).toBeVisible()

  await submitPasswordSignIn(page, DEMO_ACME_ONLY.email, DEMO_ACME_ONLY.password)
  await expectSignedIn(page)
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
  await expectSignedOut(page)
})
