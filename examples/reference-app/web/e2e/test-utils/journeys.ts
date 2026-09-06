/**
 * The shared journey steps and the user-visible text the specs assert on.
 *
 * Every string below is quoted from an en-US bundle that actually ships
 * (each package's own src/locales/en-US.json under web/packages, and this
 * app's own src/locales/en-US.json), and the specs assert on these rather than on
 * test ids: the tier's whole point is what a person reading the page
 * sees, so a rename that changes the rendered copy is supposed to fail
 * here and be re-read by a human, not silently pass because a hidden
 * attribute survived.
 */
import { expect, type Page } from '@playwright/test'
import type { DemoAccount } from './accounts.js'

/** auth-ui's sign-in surface (its own en-US bundle). */
export const SIGN_IN_TEXT = {
  identifierLabel: 'Email or phone number',
  passwordLabel: 'Password',
  submit: 'Sign in',
  registerPrompt: 'No account yet?',
  registerAction: 'Register',
  passwordTab: 'Password',
  smsTab: 'SMS code',
  displayNameLabel: 'Display name (optional)',
} as const

/** auth-ui's error bundle: the codes a sign-in or register attempt can answer with. */
export const AUTH_ERROR_TEXT = {
  invalidCredentials: 'Email, phone number or password is incorrect.',
  passwordTooShort: 'The password is too short.',
  accountLocked: 'The account is locked. Please try again later.',
  rateLimited: 'Too many attempts. Please try again later.',
  emailAlreadyRegistered: 'This email is already registered.',
  tenantMembershipRequired: 'Your account is not a member of this organization.',
  /**
   * The whitelist's own fallback. No assertion should ever WANT this
   * text: it is what a code outside the reachable-error whitelist -- or,
   * before f23079d, every real backend answer -- degrades to. The specs
   * assert its ABSENCE next to each specific error, which is what makes
   * them a regression gate for the envelope-contract defect rather than
   * a check that some error appeared.
   */
  genericFallback: 'Something went wrong. Please try again later.',
} as const

/** auth-ui's session-ended screen and sign-out control. */
export const SESSION_TEXT = {
  signOut: 'Sign out',
  endedTitle: 'Session ended',
  endedDescription: 'Your sign-in is no longer valid. Sign in again to continue.',
  signInAgain: 'Sign in again',
} as const

/** This app's own namespace: the frame, the surfaces and the notes copy. */
export const APP_TEXT = {
  navHome: 'Home',
  navNotes: 'Notes',
  navAccount: 'Account',
  registerHeading: 'Create an account',
  registerSubmit: 'Create account',
  registerSuccess: 'Account created. Sign in with the credentials you just registered.',
  registerBackToSignIn: 'Back to sign in',
  notesHeading: 'Notes',
  notesTextLabel: 'Text',
  notesCreateSubmit: 'Create note',
  notesEmptyTitle: 'No notes yet',
  notesPermissionDenied:
    'You do not have permission to create notes. If you believe this is a mistake, contact an administrator.',
  accountHeading: 'Account',
  tenantAcme: 'Acme Dental',
  tenantGlobex: 'Globex Dental',
} as const

/** react-hook-form's required-field message, as ui-kit renders it. */
export const REQUIRED_FIELD_TEXT = 'This field is required.'

/**
 * Opens the app at its sign-in surface. The suite's browser context
 * carries the en-US locale (playwright.config.ts), so the app's own
 * language negotiation settles there with no URL parameter -- this
 * navigation is the one a first-time visitor makes.
 */
export async function visitSignIn(page: Page): Promise<void> {
  await page.goto('/')
  await expect(page.getByRole('button', { name: SIGN_IN_TEXT.submit })).toBeVisible()
}

/**
 * Fills and submits the password sign-in form. Does not assert the
 * outcome: callers assert either the frame (success) or the error banner
 * (refusal), and a helper that assumed success could not serve both.
 */
export async function submitPasswordSignIn(
  page: Page,
  identifier: string,
  password: string,
): Promise<void> {
  await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(identifier)
  await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill(password)
  await page.getByRole('button', { name: SIGN_IN_TEXT.submit }).click()
}

/**
 * Signs in as a seeded demo account and waits for the authenticated
 * frame: the nav the AppShell renders is the observable proof that the
 * three-branch view machine (@speed/product-shell) settled on the
 * signed-in branch, rather than on the sign-in surface or the
 * session-ended screen.
 */
export async function signInAs(page: Page, account: DemoAccount): Promise<void> {
  await visitSignIn(page)
  await submitPasswordSignIn(page, account.email, account.password)
  await expect(page.getByRole('link', { name: APP_TEXT.navNotes })).toBeVisible()
}

/**
 * The tenant display names this host configures, in its own
 * `reference-app` bundle. Which of them an account lands in when it signs
 * in with no tenant named is NOT fixed: authn resolves the account's
 * first tenant from the host's MembershipReader, and this host's demo
 * seeding grants tenants while iterating a Go map, whose order is
 * randomized per boot and per account. So a spec must never assume a
 * tenant -- it reads the one the frame actually landed in and switches
 * deliberately when it needs the other. (Reported as a finding: a
 * returning member of a multi-location practice should land somewhere
 * predictable, which is a host-side ordering decision, not an authn one.)
 */
export const TENANT_NAMES = [APP_TEXT.tenantAcme, APP_TEXT.tenantGlobex] as const

/** The tenant the frame is currently scoped to, read from the switcher. */
export async function readCurrentTenant(page: Page): Promise<string> {
  for (const name of TENANT_NAMES) {
    if ((await page.getByRole('button', { name }).count()) > 0) {
      return name
    }
  }
  throw new Error('e2e: the frame shows no known tenant in its switcher')
}

/** The other configured tenant, for a spec that needs to cross the boundary. */
export function otherTenant(current: string): string {
  const other = TENANT_NAMES.find((name) => name !== current)
  if (other === undefined) {
    throw new Error(`e2e: no counterpart tenant for ${current}`)
  }
  return other
}

/**
 * Switches the frame to another tenant through the header's switcher
 * (@speed/tenancy-ui's TenantSwitcher, which renders a MUI Menu -- its
 * entries carry the menuitem role, not option).
 */
export async function switchTenant(page: Page, target: string): Promise<void> {
  const current = await readCurrentTenant(page)
  if (current === target) {
    return
  }
  await page.getByRole('button', { name: current }).click()
  await page.getByRole('menuitem', { name: target }).click()
  await expect(page.getByRole('button', { name: target })).toBeVisible()
}

/** Navigates the signed-in frame to one of the app's hash-routed surfaces. */
export async function openSurface(
  page: Page,
  name: typeof APP_TEXT.navHome | typeof APP_TEXT.navNotes | typeof APP_TEXT.navAccount,
): Promise<void> {
  await page.getByRole('link', { name }).click()
}

/**
 * Asserts the page shows exactly the specific error text, and NOT the
 * whitelist's generic fallback. Both halves matter: the first says the
 * server's answer arrived, the second says it arrived as a code the UI
 * could resolve -- the property the envelope-contract defect broke while
 * every mocked suite still passed.
 */
export async function expectSpecificError(page: Page, text: string): Promise<void> {
  await expect(page.getByRole('alert')).toContainText(text)
  await expect(page.getByRole('alert')).not.toContainText(AUTH_ERROR_TEXT.genericFallback)
}
