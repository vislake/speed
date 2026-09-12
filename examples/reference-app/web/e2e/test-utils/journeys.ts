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
import { expect, test, type Locator, type Page } from '@playwright/test'
import { readFileSync, writeFileSync } from 'node:fs'
import { LOGIN_LEDGER_PATH } from '../../playwright.config.js'
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
   * text: it is what a code outside the reachable-error whitelist
   * degrades to, and if a real backend answer's code never arrived the
   * fallback is what would stand in for it. The specs assert its ABSENCE
   * next to each specific error, which is what pins the envelope
   * contract rather than merely checking that some error appeared.
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
  // Armed before the first navigation, so a crash that lands anywhere in
  // the journey is already being watched for (see watchNetworkProcess).
  watchNetworkProcess(page)
  await page.goto('/')
  await expect(page.getByRole('button', { name: SIGN_IN_TEXT.submit })).toBeVisible()
}

/**
 * Opens the register surface from the sign-in one, the way a first-time
 * visitor does: the sign-in page's own create-account entrance, then the
 * register form's submit control as proof the form arrived. Gates whose
 * subject is a refusal the form itself makes (an empty submit, a
 * duplicate address on the second attempt) open the form this way and
 * then drive it their own way.
 */
export async function openRegisterForm(page: Page): Promise<void> {
  await visitSignIn(page)
  await page.getByRole('button', { name: SIGN_IN_TEXT.registerAction }).click()
  await expect(
    page.getByRole('button', { name: APP_TEXT.registerSubmit }),
  ).toBeVisible()
}

/**
 * Registers an account through the register form -- the path the
 * surface itself offers a first-time visitor -- and returns once the
 * created-account panel is up. The panel is the register verdict the
 * host's onRegistered callback renders (register is not a session
 * operation; the panel sends the visitor back to sign-in), and waiting
 * for it is load-bearing rather than polite: the "Back to sign in"
 * control exists from the moment the form shows, so a caller that
 * clicks it while the request is still in flight races the verdict --
 * when the request settles, the panel replaces the sign-in form the
 * click was aiming at.
 *
 * The display name is filled only when one is given: the field is
 * optional, and the gates that omit it exercise the name-less
 * registration the self-service provisioning falls back on.
 */
export async function registerThroughUi(
  page: Page,
  registration: {
    readonly email: string
    readonly password: string
    readonly displayName?: string
  },
): Promise<void> {
  await openRegisterForm(page)
  await page
    .getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel })
    .fill(registration.email)
  await page
    .getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel })
    .fill(registration.password)
  if (registration.displayName !== undefined) {
    await page
      .getByRole('textbox', { name: SIGN_IN_TEXT.displayNameLabel })
      .fill(registration.displayName)
  }
  await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()
  await expect(page.getByRole('status')).toContainText(APP_TEXT.registerSuccess)
}

/**
 * go/authn's two limits on sign-in, as this suite has to live with them:
 * five attempts per account per minute (limitLoginByAccount) and twenty
 * per IP per minute (limitLoginByIP), the second a pool every gate in
 * the run shares. Both read from go/authn/ratelimit.go.
 */
const LOGIN_BUDGET = {
  perAccount: 5,
  perIp: 20,
  windowMs: 60_000,
} as const

/**
 * How long after this ledger records an attempt the server counts it.
 *
 * The ledger's record is written when the pacing decides; go/authn's
 * counter moves when the guarded request arrives, and the form fill, the
 * button click and the request's flight sit between the two -- measured
 * at roughly a third of a second on an idle machine, longer under load.
 * A second is the bound the model works with; a larger value is only
 * more conservative (see wouldAllow).
 */
const SUBMISSION_SKEW_MS = 1_000

/** When each account attempted, and when this IP did, across the run. */
interface LoginLedger {
  readonly byAccount: Record<string, number[]>
  readonly byIp: number[]
}

/**
 * Reads the run's ledger. A missing or unreadable file is an empty
 * ledger, never a failure: the worst it costs is one refusal that names
 * itself, and a helper that could fail a gate over its own bookkeeping
 * would be a worse instrument than the problem it solves.
 */
function readLedger(): LoginLedger {
  try {
    const parsed: unknown = JSON.parse(readFileSync(LOGIN_LEDGER_PATH, 'utf8'))
    if (typeof parsed === 'object' && parsed !== null && 'byIp' in parsed) {
      return parsed as LoginLedger
    }
  } catch {
    // A first sign-in, or a file another worker is mid-write on.
  }
  return { byAccount: {}, byIp: [] }
}

/** Writes it back. Safe to lose: see readLedger. */
function writeLedger(ledger: LoginLedger): void {
  try {
    writeFileSync(LOGIN_LEDGER_PATH, JSON.stringify(ledger))
  } catch {
    // Same reasoning as readLedger: bookkeeping never fails a gate.
  }
}

/**
 * Whether go/ratelimit would allow one more hit on `stamps` at `at`.
 *
 * A faithful replica of slidingWindowLimiter.Allow: any approximation
 * that drifts from the server's own decision costs a run, so the
 * replica is exact. It is a sliding-window COUNTER over two fixed
 * windows, not a rolling log:
 *
 *   weighted = thisWindow + previousWindow * (1 - elapsedFraction)
 *   allowed  = weighted <= Rate
 *
 * Three consequences an approximation gets wrong:
 *
 *   - The windows are aligned to the epoch, not to the first attempt.
 *     "Sixty seconds since the oldest" is not the boundary; where the
 *     attempts sat inside their window is.
 *   - Only the current and immediately previous window are read at all.
 *     An attempt two windows back counts for exactly nothing.
 *   - The hit is counted BEFORE the decision, so a REFUSED attempt
 *     increments the counter too. Refusals make the next attempt worse,
 *     which is why the ledger records every submission and not just the
 *     ones that worked.
 *
 * THE SKEW ENVELOPE, and why the decision is not a single instant
 *
 * The ledger's stamp is not the instant the server counts: the pacing
 * decides, then someone fills the form and clicks it, and the request
 * arrives SUBMISSION_SKEW_MS later. The server's windows are aligned to
 * the epoch, so an attempt recorded within that skew of a boundary is
 * counted by the server in the window the ledger filed as the NEXT one
 * -- and near a boundary the previous window still weighs almost fully,
 * so one attempt filed into the neighbouring window moves the weighted
 * sum by about a whole unit. That is the size of this suite's margins:
 * the ledger's view of that attempt has already decayed (says "fits")
 * while the server counts it at full weight and refuses.
 *
 * So the model does not pretend to know the instant: an earlier attempt
 * counts in EVERY window the skew could have placed it in, and the
 * attempt being decided is weighed at every instant the skew could date
 * it to -- its decision instant, and just past the next boundary when
 * that boundary falls inside the skew. Both directions only ever make
 * the ledger's counts an upper bound on the server's, so "fits" here
 * cannot be a verdict the server refuses: the cost of the envelope is
 * waiting, never a 429.
 */
function wouldAllow(stamps: readonly number[], rate: number, at: number): boolean {
  const per = LOGIN_BUDGET.windowMs
  const inWindow = (which: number): number =>
    stamps.filter((stamp) => {
      const earliest = Math.floor(stamp / per)
      const latest = Math.floor((stamp + SUBMISSION_SKEW_MS) / per)
      return which >= earliest && which <= latest
    }).length

  const weightedAt = (instant: number): number => {
    const index = Math.floor(instant / per)
    const elapsedFraction = (instant % per) / per
    // +1 for the hit being weighed: Allow increments first, then decides.
    return inWindow(index) + 1 + inWindow(index - 1) * (1 - elapsedFraction)
  }

  // The instants the server could count this attempt at: now, and -- when
  // the skew reaches across the next boundary -- a moment into the window
  // after it, where the attempt is weighed against that window's counts.
  const nextBoundary = (Math.floor(at / per) + 1) * per
  const instants =
    nextBoundary - at <= SUBMISSION_SKEW_MS ? [at, nextBoundary + 1] : [at]
  return instants.every((instant) => weightedAt(instant) <= rate)
}

/**
 * Waits until this sign-in is inside go/authn's budget, then records it.
 *
 * THIS IS NOT A RETRY LOOP, and the difference is the whole reason it is
 * shaped this way. It waits BEFORE an attempt so the attempt is legal; it
 * never re-submits one the server refused. A helper that retried past a
 * refusal would destroy this suite's ability to tell a real regression
 * from its own impatience, which is also why the budget refusal below is
 * still thrown rather than waited out.
 *
 * WHY IT IS NEEDED AT ALL
 *
 * The @budget tier signs demo-owner in several times within a single
 * sixty-second window against the five-per-minute limit, and a tier that
 * only passed on the runs slow enough to spread the attempts out would
 * be reporting its own timing, not the product -- so pacing is built in
 * rather than left to run speed.
 *
 * WHAT IT CANNOT SEE
 *
 * The ledger is this RUN's own -- a file, because a Playwright worker
 * serves one project and restarts at every engine boundary, so an
 * in-memory one would reset several times per run while the server
 * counted once (playwright.config.ts's LOGIN_LEDGER_PATH note). It
 * matches the server's view only because a local run boots a server of
 * its own whose rate limiter is an in-memory KVStore nothing else talks
 * to. Against a long-lived deployment (E2E_BASE_URL) another client's
 * sign-ins are invisible here, so the pacing reduces self-inflicted
 * refusals rather than guaranteeing none -- which is why signInAs still
 * names the refusal when it comes.
 *
 * Read-modify-write with no lock, which is sound only because this
 * config runs workers: 1 and fullyParallel: false: one attempt is in
 * flight at a time. A parallel run would need a real lock, and would
 * blow the per-IP budget long before the ledger's races mattered.
 *
 * Exported so EVERY sign-in can spend from it, the API-driven one
 * included (test-utils/invitations.ts's signInThroughApi): an attempt
 * the server counts is an attempt this ledger must count, whichever
 * surface it goes through, or the pacing is computed against a budget
 * the server does not agree with.
 */
export async function payTheLoginBudget(identifier: string): Promise<void> {
  /** How often to re-ask. Local arithmetic only -- it costs no attempt. */
  const step = 1_000
  /**
   * When to stop waiting and just try. Two windows is longer than any
   * legitimate wait can be (a window's own contribution is gone after
   * two), so reaching this means the ledger's model and the server's
   * state have diverged -- in which case the honest move is to make the
   * attempt and let the refusal name itself, not to wait forever.
   */
  const giveUpAt = Date.now() + 2 * LOGIN_BUDGET.windowMs

  for (;;) {
    const ledger = readLedger()
    const own = ledger.byAccount[identifier] ?? []
    const now = Date.now()
    const fits =
      wouldAllow(own, LOGIN_BUDGET.perAccount, now) &&
      wouldAllow(ledger.byIp, LOGIN_BUDGET.perIp, now)

    if (fits || now >= giveUpAt) {
      writeLedger({
        byAccount: { ...ledger.byAccount, [identifier]: [...own, now] },
        byIp: [...ledger.byIp, now],
      })
      return
    }

    // The test is given exactly the time the wait costs, rather than the
    // whole suite being given a longer timeout: a genuinely hung gate
    // should still report at its own deadline instead of minutes later.
    const info = test.info()
    info.setTimeout(info.timeout + step)
    await new Promise((resolve) => setTimeout(resolve, step))
  }
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
  await payTheLoginBudget(identifier)
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
  await awaitSignInAnswer(page, account)
}

/**
 * Waits for the submitted sign-in attempt to answer, and names the
 * answer when it is not the signed-in frame.
 *
 * The attempt is split in two -- submitPasswordSignIn sends it, this
 * reads the answer -- so harness-failure-guards.spec.ts can drive the
 * branches below against a page it controls, with no server and no
 * login budget. Four answers settle the wait: the frame (success), the
 * invalid-credentials refusal, the rate-limit refusal, and the generic
 * fallback the surface renders for any code it cannot name. Racing the
 * outcome (rather than waiting on the frame alone) is what lets the
 * failure name its own cause -- against a real deployment the why is
 * almost always mundane (the seeded accounts were registered with
 * whatever APP_DEMO_USERS_PASSWORD that deployment was given, while
 * this suite defaults to its own local one), and a bare 60-second
 * timeout names nothing.
 *
 * The fallback is raced because it is an ANSWER, not noise: the moment
 * it renders, the attempt is over, so a wait that ignored it would sit
 * until the whole test budget was spent and report a timeout -- beside
 * the fixture teardown's "Target page, context or browser has been
 * closed", which reads like a browser event and is not one. What the
 * answer actually was (typically a 5xx -- the server folding an
 * unmapped internal failure into its error envelope) is in the trace's
 * network log for this request, and the cause behind it in the run's
 * server output.
 *
 * The frame is identified by the sign-out control rather than a nav
 * link, because below the md breakpoint AppShell collapses its
 * navigation behind the menu button and no nav link is in the DOM
 * until a person opens the drawer -- a nav-based check would work only
 * on wide screens.
 */
export async function awaitSignInAnswer(page: Page, account: DemoAccount): Promise<void> {
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
  const unnameable = page
    .getByRole('alert')
    .filter({ hasText: AUTH_ERROR_TEXT.genericFallback })
  await Promise.race([
    frame.waitFor({ state: 'visible' }).catch(() => undefined),
    refusal.waitFor({ state: 'visible' }).catch(() => undefined),
    rateLimited.waitFor({ state: 'visible' }).catch(() => undefined),
    unnameable.waitFor({ state: 'visible' }).catch(() => undefined),
  ])

  // The isVisible calls below all swallow a closed page or context for
  // the same reason: that state is the fixture teardown that follows a
  // test which has already run out its budget (or a browser that died
  // mid-wait), and it must not surface as this helper's own error --
  // the frame expectation at the bottom is what reports it.
  if (await rateLimited.isVisible().catch(() => false)) {
    throw new Error(
      `e2e: ${account.email} was rate-limited at sign-in. This is the suite's own login budget, not a product defect: go/authn allows five sign-ins per account and twenty per IP per minute, shared by every gate in the run. Ask for one block rather than a whole tier (see e2e/README.md).`,
    )
  }

  if (await refusal.isVisible().catch(() => false)) {
    const hint =
      process.env.E2E_BASE_URL !== undefined && process.env.E2E_DEMO_PASSWORD === undefined
        ? ` Deployment mode needs E2E_DEMO_PASSWORD set to ${process.env.E2E_BASE_URL}'s own APP_DEMO_USERS_PASSWORD; the suite default only matches a server this config started.`
        : ''
    throw new Error(`e2e: ${account.email} was refused at sign-in.${hint}`)
  }

  if (await unnameable.isVisible().catch(() => false)) {
    throw new Error(
      `e2e: the sign-in attempt for ${account.email} was answered with an error the surface cannot name: ` +
        `the form shows the whitelist's generic fallback ("${AUTH_ERROR_TEXT.genericFallback}"), which is what a ` +
        `response outside the client's mapped error codes degrades to -- typically a 5xx. The trace's network log ` +
        `for this POST carries the response's status and code, and the run's server output the cause behind them.`,
    )
  }

  await expect(frame).toBeVisible()
}

/**
 * The tenant display names this host configures, in its own
 * `reference-app` bundle. Which tenant an account lands in when it signs
 * in with no tenant named is the first row of its membership
 * enumeration, which org orders by tenant id (go/org's
 * MemberService.TenantsOf); which of those rows comes first is a fact
 * about the account's own memberships and the app's seeding, not a
 * contract a spec should assume. So a spec never names a tenant -- it
 * reads the one the frame actually landed in and switches deliberately
 * when it needs the other.
 */
export const TENANT_NAMES = [APP_TEXT.tenantAcme, APP_TEXT.tenantGlobex] as const

/** The tenant the frame is currently scoped to, read from the switcher. */
export async function readCurrentTenant(page: Page): Promise<string> {
  const label = await readTenantLabel(page)
  if ((TENANT_NAMES as readonly string[]).includes(label)) {
    return label
  }
  // Naming what it actually saw: a frame scoped to a self-service
  // clinic and a frame showing no switcher at all must not produce the
  // same message.
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
 * whose switcher shows a tenant id no helper knows by name. An
 * assertion about where a person landed needs to name where they
 * landed: an expression of the form "not one of TENANT_NAMES" would
 * pass whenever readCurrentTenant THROWS -- an unloaded frame, an
 * unrendered switcher, a bug in the helper itself -- so it cannot tell
 * "landed in its own clinic" from "shows no tenant at all".
 *
 * Read from the chrome's own buttons rather than by an accessible name,
 * because the name is the answer being looked for. The chrome's other
 * controls are known and skipped; tenancy-ui's own no-tenant label is
 * returned as itself rather than treated as absence, since "signed in
 * with no organization" is a real state and worth being able to assert.
 *
 * textContent, NOT innerText: innerText returns text as RENDERED, and
 * MUI's Button applies `text-transform: uppercase` -- so the switcher
 * reads back "ACME DENTAL" while the tenant is named "Acme Dental",
 * and every comparison against a configured name fails. textContent is
 * the DOM's own text, which is what the accessible name is computed
 * from and what a screen reader announces, so it is the identity. (Its
 * own hazard -- concatenating adjacent elements with no separator --
 * does not apply to one button's label: ask "what does this render as"
 * and innerText is right; ask "which tenant is this" and only the
 * untransformed text is.)
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
 * The list, not the landing: the frame opens on the membership answer's
 * first row (ordered by tenant id), so "did the invitation work" cannot
 * be answered by looking at which tenant the frame opened on. Since
 * self-service registration gives every registrant a clinic of their
 * own, anyone who was also invited somewhere has two, and the question
 * is whether the inviting organization is among them.
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
 * The text a surface shows once it has stopped changing.
 *
 * A point-in-time sample of a surface that is still settling -- a query
 * that has not answered, a navigation in flight, an announcement a beat
 * behind its trigger -- is an answer that is not about the product, and
 * a bare `await locator.innerText()` is one call and always available,
 * so it is what gets written. This makes waiting the equally short
 * option.
 *
 * "Settled" is two identical reads a beat apart, which is enough for
 * those shapes. It is not a guarantee against a surface that changes
 * forever -- nothing is -- and it deliberately does not assert anything
 * about the content, so a caller's own assertion still says what the
 * text must contain.
 */
export async function readSettledText(
  locator: import('@playwright/test').Locator,
  timeout = 15_000,
): Promise<string> {
  const deadline = Date.now() + timeout
  let previous = await locator.innerText().catch(() => '')
  while (Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 250))
    const current = await locator.innerText().catch(() => '')
    if (current === previous && current.trim() !== '') {
      return current
    }
    previous = current
  }
  return previous
}

/**
 * Asserts a person is signed in, by the one control that is present on
 * every screen size when they are: the sign-out button.
 *
 * NOT a nav link: below the md breakpoint AppShell collapses the
 * navigation behind the menu button, so no nav link is in the DOM at
 * all, and a nav-based check would fail on the iPad project while the
 * person is signed in perfectly well.
 */
export async function expectSignedIn(page: Page): Promise<void> {
  await expect(page.getByRole('button', { name: SESSION_TEXT.signOut })).toBeVisible()
}

/**
 * Asserts a person is NOT signed in.
 *
 * "The frame is gone" must not be written as "no nav link is on the
 * page": below the md breakpoint no nav link is on the page whether
 * someone is signed in or not, so that check would pass on the iPad
 * project for the wrong reason, and keep passing if signing out stopped
 * working entirely. An indirect measure standing in for the real
 * property fails silently in BOTH directions, and the direction that
 * costs you is the one where it says yes. The sign-out button is the
 * real property -- it exists when there is a session to end and not
 * otherwise, on every screen size.
 */
export async function expectSignedOut(page: Page): Promise<void> {
  await expect(page.getByRole('button', { name: SESSION_TEXT.signOut })).toHaveCount(0)
}

/**
 * The sign-in surface, as the one control that is on it: the identifier
 * field. The observable form of "this page has no session", so a wait
 * that expects the signed-in frame can tell a slow surface from a lost
 * session.
 */
function signInSurface(page: Page): Locator {
  return page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel })
}

/**
 * The browser's own report that its network process died.
 *
 * WebKit prints exactly this line to the page console when its network
 * process crashes -- a failure inside the browser, not the page -- and
 * every request in flight dies with it; when the vite client's HMR
 * socket is among them, the client then reloads the document. The suite
 * cannot prevent the crash; it refuses to misreport it, which is what
 * this signature is collected for.
 */
const NETWORK_PROCESS_CRASH = /Network process crashed/

/** One page's crash watch: settles on the first report of the crash. */
interface CrashWatch {
  readonly crashed: Promise<void>
}

/**
 * The watch per page. A WeakMap so the entry follows the page's own
 * lifetime, and a single install per page: a second console listener
 * would never resolve, the first one owning the report.
 */
const crashWatches = new WeakMap<Page, CrashWatch>()

/**
 * Arms a page's crash watch, returning it. Idempotent, so any helper
 * that needs the signal can call it without coordinating.
 */
function watchNetworkProcess(page: Page): CrashWatch {
  const existing = crashWatches.get(page)
  if (existing !== undefined) {
    return existing
  }
  let report: () => void = () => undefined
  const crashed = new Promise<void>((resolve) => {
    report = resolve
  })
  page.on('console', (message) => {
    if (NETWORK_PROCESS_CRASH.test(message.text())) {
      report()
    }
  })
  const watch: CrashWatch = { crashed }
  crashWatches.set(page, watch)
  return watch
}

/**
 * The race and the reporting the `expect…WhileSignedIn` helpers share:
 * the caller's own assertion (however long its timeout), the app's
 * return to the sign-in surface, and the browser's own crash report --
 * whichever settles first, with the crash and the loss named as what
 * they are.
 *
 * WHY IT EXISTS
 *
 * This product keeps the access token in memory and the refresh token in
 * a closure, never in storage (@speed/api-client's store, @speed/auth-core's
 * session), so a full document load is the one event that discards a
 * session -- by design, and this suite works within it. The suite drives
 * a vite DEV server, whose own client reloads the page when its HMR
 * socket is lost (@vite/client's "[vite] server connection lost" branch
 * pings and then calls location.reload unconditionally), and a browser
 * network-process crash brings that loss about. When a reload lands
 * mid-journey the app comes back anonymous, the element a journey is
 * waiting for can never appear, and without these branches the failure
 * spends its whole timeout and reports that some control was missing --
 * which reads like a product defect and is not one. (A session the
 * SERVER ended is a different state: it renders the "Session ended"
 * screen, not this one.)
 *
 * The crash branch is the same event at the source and without the
 * reload's help: the console line is the browser's own, and the page
 * can also just stop answering -- requests failing until the wait's
 * timeout -- without any reload landing at all. The session-lost branch
 * is therefore an EDGE (the sign-in surface must REAPPEAR); the crash
 * branch is the level beneath it. A surface already on screen when the
 * wait begins is a sign-in still in flight -- its request unanswered,
 * its frame not yet rendered -- and a wait that fired on bare
 * visibility would report that ordinary slow sign-in as a dead session.
 *
 * What these branches deliberately do NOT do is weaken the assertion:
 * the wait's own condition must still settle within the same timeout,
 * for the same reason. The race only decides WHY a failure reports what
 * it does, and reports it at the moment the loss is observable rather
 * than a timeout later.
 */
async function whileSignedIn(
  page: Page,
  message: string,
  assertion: Promise<'settled' | { error: unknown }>,
): Promise<void> {
  const crash = watchNetworkProcess(page)
  // Two steps, and the first one is what makes the signal an edge
  // rather than a level. From whatever state the page is in, the
  // surface must be GONE first: that settles at once when the frame is
  // signed in (there is no such surface), and waits out a sign-in still
  // on screen instead of firing on it. Only a surface that then becomes
  // visible again -- the frame that was up has been replaced by the
  // sign-in form -- is the loss.
  const signIn = signInSurface(page)
  const sessionLost = signIn
    .waitFor({ state: 'hidden', timeout: 0 })
    .then(() => signIn.waitFor({ state: 'visible', timeout: 0 }))
    .then(
      () => 'session-lost' as const,
      // The page or context went away while waiting -- in either step:
      // that is this test's own end, not a session loss, and the
      // assertion above is the one that must report it.
      () => 'unobservable' as const,
    )

  const outcome = await Promise.race([
    crash.crashed.then(() => 'network-process-crashed' as const),
    sessionLost,
    assertion,
  ])
  if (outcome === 'network-process-crashed') {
    throw new Error(
      `e2e: ${message}: the browser's network process crashed while this journey was running ` +
        '(the page console carries "Network process crashed"), so its in-flight requests failed and the ' +
        'page came back without this product\'s in-memory session. Read the console in the trace before ' +
        'treating this red as a product or gate defect: the execution environment failed, not the gate.',
    )
  }
  if (outcome === 'session-lost') {
    throw new Error(
      `e2e: ${message}: the app is back on the sign-in surface, so this journey's session is gone. ` +
        'This product keeps the session in memory (never in storage), so a full page load discards it -- ' +
        'and the suite drives a vite dev server, whose client reloads the page when its HMR socket is lost ' +
        '(a browser network-process crash does that; the console in the trace carries "[vite] server connection lost" ' +
        'followed by a document load). Read the trace before treating this as a product defect; a session the SERVER ' +
        'ended shows the "Session ended" screen instead.',
    )
  }
  if (outcome === 'unobservable') {
    const settled = await assertion
    if (typeof settled === 'object') {
      throw settled.error
    }
    return
  }
  if (typeof outcome === 'object') {
    throw outcome.error
  }
}

/**
 * Waits for `locator` the way `expect(locator, message).toBeVisible`
 * would (same timeout, same failure when it never appears), but stops
 * early -- and says why -- when the app has returned to the sign-in
 * surface or the browser's network process has crashed instead;
 * whileSignedIn carries the full reasoning, and the assertion itself is
 * unchanged.
 */
export async function expectWhileSignedIn(
  page: Page,
  locator: Locator,
  message: string,
  timeout?: number,
): Promise<void> {
  await whileSignedIn(
    page,
    message,
    expect(locator, message)
      .toBeVisible({ timeout })
      .then(
        () => 'settled' as const,
        (error: unknown) => ({ error }),
      ),
  )
}

/**
 * The enabled-state sibling of expectWhileSignedIn, for a control whose
 * enablement depends on network work. The case-create submit is the one
 * that needs it: it stays disabled until every attached photo has
 * finished uploading (case-create-view.tsx's createBlocked), so a raw
 * click on it waits out the whole budget on anything that kills the
 * upload and then reports a click timeout, which names neither the
 * upload nor the crash.
 */
export async function expectEnabledWhileSignedIn(
  page: Page,
  locator: Locator,
  message: string,
  timeout?: number,
): Promise<void> {
  await whileSignedIn(
    page,
    message,
    expect(locator, message)
      .toBeEnabled({ timeout })
      .then(
        () => 'settled' as const,
        (error: unknown) => ({ error }),
      ),
  )
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
  await expectWhileSignedIn(
    page,
    page.getByRole('heading', { name: heading, level: 1 }),
    `the frame never showed the ${heading} surface`,
  )
}

/**
 * Navigates the signed-in frame to one of the app's hash-routed
 * surfaces, the way a person on that screen size would.
 *
 * Below the md breakpoint AppShell collapses its navigation behind the
 * menu button and no nav link is in the DOM until someone opens the
 * drawer, so a helper that clicks the link directly works only on wide
 * screens -- and this matters more here than in most products, because
 * the iPad IS the device a dentist shows a patient their simulation on.
 * Opening the drawer first is what a person does, not a workaround: on
 * a narrow screen the menu button is the navigation.
 */
export async function openSurface(page: Page, name: string | RegExp): Promise<void> {
  const link = page.getByRole('link', { name })
  const menu = page.getByRole('button', { name: SHELL_TEXT.openNav })

  // Wait for whichever entrance THIS viewport offers, rather than asking
  // whether one is there right now. `.or()` waits for either and settles
  // as soon as one is visible, so a wide viewport does not pay for the
  // narrow one's menu -- while an `isVisible()`-first check would race
  // the frame's own render: a journey that signs in and navigates
  // immediately, with no signInAs to settle the frame first, would find
  // neither the link nor the menu while both are about to appear.
  await expectWhileSignedIn(
    page,
    link.or(menu).first(),
    `no way to reach ${String(name)}: the frame offers neither that nav entry nor the menu button that would hold it`,
  )

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
  // computed for an element that is not in the tree. Returning half a
  // step early would make a caller's `readCurrentTenant` report that
  // the frame shows no tenant switcher while a screenshot shows the
  // switcher plainly.
  await link.waitFor({ state: 'hidden' })
}

/**
 * Asserts the page shows exactly the specific error text, and NOT the
 * whitelist's generic fallback. Both halves matter: the first says the
 * server's answer arrived, the second says it arrived as a code the UI
 * could resolve -- the property a scripted-fetch suite cannot pin,
 * because its doubles answer envelopes their own authors shaped.
 */
export async function expectSpecificError(page: Page, text: string): Promise<void> {
  await expect(page.getByRole('alert')).toContainText(text)
  await expect(page.getByRole('alert')).not.toContainText(AUTH_ERROR_TEXT.genericFallback)
}
