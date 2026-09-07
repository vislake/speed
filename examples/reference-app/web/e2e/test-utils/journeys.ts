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

/** layout-kit's AppShell chrome (its own en-US bundle). */
export const SHELL_TEXT = {
  openNav: 'Open navigation menu',
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

  // Wait for whichever settles first: the frame, or a refusal. Waiting
  // only for the frame turned every refused sign-in into "Sign out is not
  // visible after 10s", which says nothing about why -- and against a
  // real deployment the why is almost always mundane: the seeded accounts
  // were registered with whatever APP_DEMO_USERS_PASSWORD that deployment
  // was given, while this suite defaults to its own local one. Racing the
  // two outcomes is what lets the failure name its own cause.
  //
  // The frame is identified by the sign-out control rather than a nav
  // link, because below the md breakpoint AppShell collapses its
  // navigation behind the menu button and no nav link is in the DOM until
  // a person opens the drawer -- which quietly made this helper
  // desktop-only until a spec that resizes to a phone found out.
  const frame = page.getByRole('button', { name: SESSION_TEXT.signOut })
  const refusal = page
    .getByRole('alert')
    .filter({ hasText: AUTH_ERROR_TEXT.invalidCredentials })
  // The budget refusal is raced too, and named on its own, because it
  // is the one failure a caller can do nothing about by looking at the
  // product: go/authn allows five sign-ins per account and twenty per
  // IP per minute, and the second is a pool the whole suite shares. A
  // run that crosses it fails on whichever journey was unlucky, and
  // without this branch the report says only that some control never
  // appeared -- which reads like a defect and is not one.
  const rateLimited = page
    .getByRole('alert')
    .filter({ hasText: AUTH_ERROR_TEXT.rateLimited })
  await Promise.race([
    frame.waitFor({ state: 'visible' }).catch(() => undefined),
    refusal.waitFor({ state: 'visible' }).catch(() => undefined),
    rateLimited.waitFor({ state: 'visible' }).catch(() => undefined),
  ])

  if (await rateLimited.isVisible().catch(() => false)) {
    throw new Error(
      `e2e: ${account.email} was rate-limited at sign-in. This is the suite's own login budget, not a product defect: go/authn allows five sign-ins per account and twenty per IP per minute, shared by every gate in the run. Ask for one block rather than a whole tier (see e2e/README.md).`,
    )
  }

  if (await refusal.isVisible()) {
    const hint =
      process.env.E2E_BASE_URL !== undefined && process.env.E2E_DEMO_PASSWORD === undefined
        ? ` Deployment mode needs E2E_DEMO_PASSWORD set to ${process.env.E2E_BASE_URL}'s own APP_DEMO_USERS_PASSWORD; the suite default only matches a server this config started.`
        : ''
    throw new Error(`e2e: ${account.email} was refused at sign-in.${hint}`)
  }

  await expect(frame).toBeVisible()
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
  const label = await readTenantLabel(page)
  if ((TENANT_NAMES as readonly string[]).includes(label)) {
    return label
  }
  // Naming what it actually saw, because the previous form could not:
  // it probed for each demo name in turn and threw "no known tenant"
  // either way, so a frame scoped to a self-service clinic and a frame
  // showing no switcher at all produced the same message.
  throw new Error(
    `e2e: the frame's switcher shows "${label}", which is not one of the demo practices`,
  )
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
 * The label the tenant switcher is currently showing, WHATEVER it says.
 *
 * The counterpart to readCurrentTenant, and the difference is the point:
 * readCurrentTenant only recognises the two demo practices, so it cannot
 * describe a frame scoped to any other tenant -- and self-service
 * registration creates exactly that, a clinic of the registrant's own
 * whose trigger shows a tenant id no helper knows by name.
 *
 * It exists because the alternative was being used and was unsound:
 * `TENANT_NAMES.not.toContain(await readCurrentTenant(page).catch(() => ''))`
 * passes whenever readCurrentTenant THROWS -- an unloaded frame, an
 * unrendered switcher, a bug in the helper itself -- so it cannot tell
 * "landed in its own clinic" from "shows no tenant at all", and the
 * direction it fails in is the one where it says yes. An assertion about
 * where a person landed needs to name where they landed.
 *
 * Read from the chrome's own buttons rather than by an accessible name,
 * because the name is the answer being looked for. The chrome's other
 * controls are known and skipped; tenancy-ui's own no-tenant label is
 * returned as itself rather than treated as absence, since "signed in
 * with no organization" is a real state and worth being able to assert.
 *
 * textContent, NOT innerText, and this is the mirror image of the choice
 * the session-address gate had to make in the other direction. innerText
 * returns text as RENDERED, and MUI's Button applies
 * `text-transform: uppercase` -- so the switcher reads back "ACME
 * DENTAL" while the tenant is named "Acme Dental", and every comparison
 * against a configured name fails. textContent is the DOM's own text,
 * which is what the accessible name is computed from and what a screen
 * reader announces, so it is the identity. (Its own hazard --
 * concatenating adjacent elements with no separator -- does not apply to
 * one button's label, which is why the two gates land on opposite
 * answers: ask "what does this render as" and innerText is right; ask
 * "which tenant is this" and only the untransformed text is.)
 */
export async function readTenantLabel(page: Page): Promise<string> {
  const chromeControls = [SESSION_TEXT.signOut, SHELL_TEXT.openNav] as const
  const buttons = page.locator('header button')
  const count = await buttons.count()
  for (let index = 0; index < count; index += 1) {
    const label = ((await buttons.nth(index).textContent()) ?? '').replace(/\s+/g, ' ').trim()
    if (label === '' || chromeControls.some((control) => label === control)) {
      continue
    }
    return label
  }
  throw new Error('e2e: the frame shows no tenant switcher in its chrome')
}

/**
 * Asserts the frame is scoped to a tenant, and that it is NOT one of the
 * demo organizations.
 *
 * Both halves, in that order, and the first is what the assertion this
 * replaces was missing: a person has to have landed SOMEWHERE before
 * "not there" means anything. Used by the invitation journey to establish
 * that an invitee is outside the inviting organization before accepting
 * -- the control that makes the acceptance afterwards prove something.
 */
export async function expectOutsideDemoOrganizations(page: Page): Promise<void> {
  const landed = await readTenantLabel(page)
  expect(landed, 'the frame names no tenant at all, so it cannot be said where this person landed').not.toBe(
    '',
  )
  expect(
    TENANT_NAMES as readonly string[],
    `the frame is scoped to ${landed}, one of the demo organizations, so nothing later can prove an invitation opened it`,
  ).not.toContain(landed)
}

/**
 * The clinics this person can work in, read from the switcher's own menu.
 *
 * The list, not the landing. Which tenant a sign-in with no tenant named
 * lands in is NOT fixed -- authn takes the first the host's membership
 * answer returns, and that order is not guaranteed -- so an assertion
 * about where someone landed is an assertion about ordering. Since
 * self-service registration gives every registrant a clinic of their
 * own, anyone who was also invited somewhere has two, and "did the
 * invitation work" cannot be answered by looking at which one the frame
 * opened on.
 *
 * Leaves the menu closed, so a caller can keep driving the page.
 */
export async function readTenantOptions(page: Page): Promise<string[]> {
  const current = await readTenantLabel(page)
  await page.getByRole('button', { name: current }).click()
  const options = await page.getByRole('menuitem').allTextContents()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('menuitem')).toHaveCount(0)
  return options.map((option) => option.replace(/\s+/g, ' ').trim()).filter((o) => o !== '')
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

/**
 * Asserts a person is signed in, by the one control that is present on
 * every screen size when they are: the sign-out button.
 *
 * NOT a nav link, which is what four specs used and what made them
 * desktop-only -- below the md breakpoint AppShell collapses the
 * navigation behind the menu button, so no nav link is in the DOM at
 * all. The failures read as "element(s) not found" on the iPad project
 * while the person was signed in perfectly well.
 */
export async function expectSignedIn(page: Page): Promise<void> {
  await expect(page.getByRole('button', { name: SESSION_TEXT.signOut })).toBeVisible()
}

/**
 * Asserts a person is NOT signed in.
 *
 * This one matters more than its twin above, because the assertion it
 * replaces was not merely desktop-only -- it was VACUOUS on a phone or
 * a tablet. "The frame is gone" was written as "no nav link is on the
 * page", and below the md breakpoint no nav link is on the page whether
 * someone is signed in or not. So the check passed on the iPad project
 * for the wrong reason, and would have kept passing if signing out had
 * stopped working entirely.
 *
 * That is the same failure this suite already learned once: an indirect
 * measure standing in for the real property fails silently in BOTH
 * directions, and the direction that costs you is the one where it says
 * yes. The sign-out button is the real property -- it exists when there
 * is a session to end and not otherwise, on every screen size.
 */
export async function expectSignedOut(page: Page): Promise<void> {
  await expect(page.getByRole('button', { name: SESSION_TEXT.signOut })).toHaveCount(0)
}

/**
 * Asserts the frame is showing a named surface, identified by that
 * surface's own top-level heading.
 *
 * `level: 1` is what makes this unambiguous, and the ambiguity is not
 * hypothetical: an accessible-name match is a SUBSTRING match, so
 * `{ name: 'Account' }` alone also matched account-ui's own
 * "Linked social accounts" section heading and failed on strict mode --
 * a gate reporting a locator problem where a person reading it would
 * expect a product problem. A surface has exactly one h1, and it is the
 * surface's own title, so that is what a "we are on this surface"
 * assertion should name.
 */
export async function expectOnSurface(page: Page, heading: string): Promise<void> {
  await expect(page.getByRole('heading', { name: heading, level: 1 })).toBeVisible()
}

/**
 * Navigates the signed-in frame to one of the app's hash-routed
 * surfaces, the way a person on that screen size would.
 *
 * Below the md breakpoint AppShell collapses its navigation behind the
 * menu button and no nav link is in the DOM until someone opens the
 * drawer -- so a helper that clicks the link directly is desktop-only,
 * and silently so: it timed out on the iPad project with "locator.click:
 * Test timeout", which reads like a broken product rather than a helper
 * that does not know how to walk this screen. That matters more here
 * than in most products, because the iPad IS the device a dentist shows
 * a patient their simulation on. `signInAs` above had the same bug and
 * the same symptom; this is the other half of it.
 *
 * Opening the drawer first is what a person does, not a workaround: on a
 * narrow screen the menu button is the navigation.
 */
export async function openSurface(page: Page, name: string | RegExp): Promise<void> {
  const link = page.getByRole('link', { name })
  const menu = page.getByRole('button', { name: SHELL_TEXT.openNav })

  // Wait for whichever entrance THIS viewport offers, rather than asking
  // whether one is there right now.
  //
  // `isVisible()` is a point-in-time question with no waiting in it, and
  // asking it first made this helper race the frame's own render: a
  // journey that signs in and navigates immediately -- no signInAs to
  // settle the frame first -- found neither the link nor the menu and
  // failed with "no way to reach Notes" while both were about to appear.
  // The direct `.click()` this helper replaced never had that problem,
  // because a click auto-waits; the fix is to keep the waiting, not to
  // sleep. `.or()` waits for either and settles as soon as one is
  // visible, so a wide viewport does not pay for the narrow one's menu.
  await expect(
    link.or(menu).first(),
    `no way to reach ${String(name)}: the frame offers neither that nav entry nor the menu button that would hold it`,
  ).toBeVisible()

  if (await link.isVisible()) {
    await link.click()
    return
  }

  await menu.click()
  await link.waitFor({ state: 'visible' })
  await link.click()

  // Wait for the drawer to finish closing, not just for the click.
  //
  // The temporary drawer is a modal, and while it is open -- including
  // through its closing animation -- everything behind it is excluded
  // from the accessibility tree. The controls are all still in the DOM,
  // so this does not look like a hidden element: it looks like the app
  // bar's buttons losing their accessible NAMES, because a name is not
  // computed for an element that is not in the tree. `readCurrentTenant`
  // then reported "the frame shows no known tenant in its switcher"
  // while a screenshot showed the switcher plainly, on the iPad project
  // only -- a gate accusing the product of losing its tenant switcher
  // when the truth was that this helper returned half a step early.
  await link.waitFor({ state: 'hidden' })
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
