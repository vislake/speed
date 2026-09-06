/**
 * The registration journey: creating an account from the sign-in surface,
 * and what the form says when the server's password policy refuses.
 *
 * The too-short-password assertion is this suite's second, independent
 * regression gate for the envelope-contract defect (f23079d), and the
 * cheaper of the two: it reaches a specific server error without
 * spending a failed sign-in, so it costs no lockout window
 * (password-sign-in.spec.ts's header explains why that matters). The
 * server answers authn.password_too_short with a min_length parameter;
 * the surface is expected to render the whitelisted message for that
 * code, never the whitelist's generic fallback.
 *
 * Registration deliberately does NOT sign the new account in
 * (@speed/auth-ui's RegisterForm: register is not a session operation),
 * and this app renders the created account as a success panel that sends
 * the visitor back to the sign-in surface. A freshly registered account
 * also has no organization membership yet, so it cannot sign in until
 * one exists -- which is the invitation journey, not this one
 * (org-invitation-sign-in.spec.ts).
 */
import { expect, test } from '@playwright/test'
import {
  APP_TEXT,
  AUTH_ERROR_TEXT,
  REQUIRED_FIELD_TEXT,
  SIGN_IN_TEXT,
  expectSpecificError,
  visitSignIn,
} from './test-utils/journeys.js'

/** A password that satisfies authn's real policy (12 characters minimum). */
const VALID_PASSWORD = 'e2e-registration-2026'

/** Opens the register surface from the sign-in one, the way a visitor does. */
async function visitRegister(page: import('@playwright/test').Page): Promise<void> {
  await visitSignIn(page)
  await page.getByRole('button', { name: SIGN_IN_TEXT.registerAction }).click()
  await expect(page.getByRole('button', { name: APP_TEXT.registerSubmit })).toBeVisible()
}

test('a too-short password renders the specific policy message, not the generic fallback', async ({
  page,
}) => {
  await visitRegister(page)
  await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(uniqueEmail())
  await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill('too-short')
  await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()

  await expectSpecificError(page, AUTH_ERROR_TEXT.passwordTooShort)
})

test('an empty submit is refused by the form itself, before any request', async ({ page }) => {
  await visitRegister(page)

  // The two required fields answer for themselves; nothing reaches the
  // server, so no whole-attempt banner appears beside them.
  await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()
  await expect(page.getByText(REQUIRED_FIELD_TEXT)).toHaveCount(2)
  await expect(page.getByRole('alert')).toHaveCount(0)
})

test('a valid registration reports the created account and offers sign-in', async ({ page }) => {
  await visitRegister(page)
  await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(uniqueEmail())
  await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill(VALID_PASSWORD)
  await page
    .getByRole('textbox', { name: SIGN_IN_TEXT.displayNameLabel })
    .fill('E2E Registration')
  await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()

  // The app's own success panel, not a session: the created account is
  // handed to the host's onRegistered callback, which renders this and
  // routes back to sign-in rather than pretending register signed anyone
  // in.
  await expect(page.getByRole('status')).toContainText(APP_TEXT.registerSuccess)
  await expect(page.getByRole('button', { name: APP_TEXT.registerBackToSignIn })).toBeVisible()
})

test('registering an address twice is refused with the specific conflict message', async ({
  page,
}) => {
  const email = uniqueEmail()

  await visitRegister(page)
  await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(email)
  await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill(VALID_PASSWORD)
  await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()
  await expect(page.getByRole('status')).toContainText(APP_TEXT.registerSuccess)

  await visitRegister(page)
  await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(email)
  await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill(VALID_PASSWORD)
  await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()

  await expectSpecificError(page, AUTH_ERROR_TEXT.emailAlreadyRegistered)
})

/**
 * A fresh address per attempt. The suite's database is new on every run
 * (playwright.config.ts), but a single run registers several accounts and
 * a retry re-runs a test that may already have created its address, so
 * uniqueness comes from the clock and a counter rather than from the
 * database being empty.
 */
let registrationCounter = 0
function uniqueEmail(): string {
  registrationCounter += 1
  return `e2e-register-${Date.now()}-${registrationCounter}@example.com`
}
