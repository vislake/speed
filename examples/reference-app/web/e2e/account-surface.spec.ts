/**
 * The signed-in account surface: the sessions a member holds, the
 * sign-in attempts recorded against their account, the social identities
 * they can bind, and the two-step verification they can set up.
 *
 * Every section on this page reads real server state through the
 * generated react-query hooks (@speed/account-ui is the first package to
 * render those into a component tree), so this spec is where a broken
 * read shows up as an empty or stuck section rather than as a green
 * component test against a scripted list. It asserts the sections
 * render with the member's own data -- the current session marked as
 * such, the sign-in that just happened present in the history -- and
 * stops at reading: revoking sessions, unbinding identities and
 * enrolling a factor each mutate state other specs depend on, so driving
 * them here would couple this spec to their order, and the browser suite
 * exercises no account mutation.
 *
 * One sign-in covers every section, as the read-only member: sign-in is
 * rate-limited per account (the limits live in go/authn's ratelimit.go),
 * so the suite spends one sign-in per spec file where it can and spreads
 * the files across the three seeded accounts. The account surface needs
 * no write permission at all, which makes the reader the honest account
 * for it.
 */
import { expect, test } from '@playwright/test'
import { DEMO_READER } from './test-utils/accounts.js'
import { APP_TEXT, openSurface, signInAs } from './test-utils/journeys.js'

/** @speed/account-ui's own en-US bundle: the section headings and marks. */
const ACCOUNT_TEXT = {
  sessionsHeading: 'Sessions',
  currentSessionMark: 'Current session',
  historyHeading: 'Sign-in history',
  historySucceeded: 'Success',
  bindingsHeading: 'Linked social accounts',
  mfaHeading: 'Two-step verification',
  authenticatorHeading: 'Authenticator app',
  recoveryCodesHeading: 'Recovery codes',
} as const

test('the account surface shows the member their own sessions and history', async ({ page }) => {
  await signInAs(page, DEMO_READER)
  await openSurface(page, APP_TEXT.navAccount)

  await expect(page.getByRole('heading', { name: APP_TEXT.accountHeading, level: 1 })).toBeVisible()

  // Sessions: the one this browser is holding, marked current by the
  // server's own is_current flag rather than by anything the page
  // guessed.
  await expect(page.getByRole('heading', { name: ACCOUNT_TEXT.sessionsHeading })).toBeVisible()
  await expect(page.getByText(ACCOUNT_TEXT.currentSessionMark)).toBeVisible()

  // History: the sign-in that just happened, recorded as a successful
  // password attempt.
  await expect(page.getByRole('heading', { name: ACCOUNT_TEXT.historyHeading })).toBeVisible()
  await expect(page.getByText(ACCOUNT_TEXT.historySucceeded).first()).toBeVisible()

  // The bindings section lists the providers this host configures, as
  // add-a-binding actions; the demo configures no real OAuth client, so
  // the buttons exist and their exchange is not driven here.
  await expect(page.getByRole('heading', { name: ACCOUNT_TEXT.bindingsHeading })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Google' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'GitHub' })).toBeVisible()

  // MFA: the section never claims a factor is enabled or disabled (the
  // spec ships no factor-status operation), it offers the two actions it
  // can actually drive.
  await expect(page.getByRole('heading', { name: ACCOUNT_TEXT.mfaHeading })).toBeVisible()
  await expect(page.getByRole('heading', { name: ACCOUNT_TEXT.authenticatorHeading })).toBeVisible()
  await expect(page.getByRole('heading', { name: ACCOUNT_TEXT.recoveryCodesHeading })).toBeVisible()
})
