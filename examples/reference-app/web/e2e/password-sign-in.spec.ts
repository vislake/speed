/**
 * The password sign-in journey: what a practice member does first, and
 * what the surface tells them when the server refuses.
 *
 * The refusal assertion here is the browser-level end of the envelope
 * contract: the server's answer must reach the surface as its code,
 * mapped by the reachable-error whitelist to the specific message --
 * if the code never arrived, every specific message would degrade to
 * the generic fallback. The component suites cannot pin that end: their
 * scripted fetch doubles write envelopes their own authors shaped. A
 * browser against a real server sees the real envelope on the first
 * refused sign-in.
 *
 * RATE LIMITING SHAPES THIS FILE. go/authn puts sign-in behind a sliding
 * window with progressive lockout (its ratelimit.go), and a single wrong
 * password against a real account answers authn.account_locked with a
 * retry-after for the next few seconds. So the suite spends exactly one
 * failed sign-in, against an address no account was ever registered with:
 * authn answers authn.invalid_credentials for a wrong password and for an
 * unknown account alike -- one answer for every failure reason, by design
 * (go/authn/errors.go) -- so the refusal path is covered without leaving
 * any seeded account locked for whichever spec runs next. The second
 * regression gate on the same defect lives in registration.spec.ts,
 * which reaches a specific error without a failed sign-in at all.
 */
import { expect, test } from '@playwright/test'
import { DEMO_ACME_ONLY, DEMO_OWNER } from './test-utils/accounts.js'
import {
  APP_TEXT,
  AUTH_ERROR_TEXT,
  SIGN_IN_TEXT,
  TENANT_NAMES,
  expectSignedIn,
  expectSpecificError,
  readCurrentTenant,
  signInAs,
  submitPasswordSignIn,
  visitSignIn,
} from './test-utils/journeys.js'

test('a seeded owner signs in and lands in the tenant frame', async ({ page }) => {
  await signInAs(page, DEMO_OWNER)

  // The frame's own chrome: the nav and the tenant the token resolved
  // to. That a tenant is named at all is the observable proof that
  // sign-in resolved a real membership rather than refusing for the
  // lack of one. WHICH tenant is deliberately not asserted: an account
  // in several tenants lands in the first of its memberships (the
  // enumeration is ordered by tenant id; see TENANT_NAMES in
  // test-utils/journeys.ts), a fact about the account rather than a
  // contract to pin.
  // The frame is identified by the sign-out control rather than by a nav
  // link: below the md breakpoint the navigation is collapsed behind the
  // menu button and no link is in the DOM, which made this assertion
  // desktop-only. That the nav itself WORKS is proven by the gates that
  // actually walk it (back-button, current-clinic-is-visible), on every
  // engine including the iPad project.
  await expectSignedIn(page)
  expect(TENANT_NAMES).toContain(await readCurrentTenant(page))
})

test('a refused sign-in renders the specific credentials message, not the generic fallback', async ({
  page,
}) => {
  await visitSignIn(page)

  // A never-registered address UNIQUE to this attempt, and the
  // uniqueness is load-bearing rather than tidiness.
  //
  // go/authn's progressive lockout accrues per ACCOUNT on recorded
  // failures (ratelimit.go), so a fixed address would make this gate
  // fail ITSELF across engines: run on three projects in one
  // invocation, the first engine's deliberate failure starts a lockout
  // on that address and the next engine's attempt, seconds later, is
  // answered "The account is locked" instead of the credentials message
  // -- a reported product defect that is entirely the gate's own shared
  // state.
  //
  // The identity of the address was never part of what this gate
  // checks: a never-registered address is refused with
  // authn.invalid_credentials whichever one it is, and a fresh one
  // carries no failure history to be locked out over. The suite's own
  // sign-in ledger cannot help here -- it paces the sliding windows,
  // and the lockout is a separate mechanism driven by failures rather
  // than by attempts.
  await submitPasswordSignIn(
    page,
    `nobody-was-ever-registered-here-${Date.now()}@example.com`,
    'wrong-password',
  )

  await expectSpecificError(page, AUTH_ERROR_TEXT.invalidCredentials)
  // Still on the sign-in surface: a refusal changes no session state.
  await expect(page.getByRole('button', { name: SIGN_IN_TEXT.submit })).toBeVisible()
})

test('the SMS channel replaces the password form when its tab is chosen', async ({ page }) => {
  await visitSignIn(page)

  // Skipped, not failed, when the channel is not offered at all.
  //
  // What this test is about is the channel SWITCH -- that choosing
  // another channel unmounts the previous form rather than hiding it --
  // and a deployment with nothing able to deliver a code hides the tab
  // entirely (offered-channels-work.spec.ts holds that choice). The two
  // gates want opposite things about the same tab -- this one exercises
  // the switch, the other requires the tab's absence -- so without this
  // guard this test would assert the presence of the very entrance the
  // other gate asks to be removed.
  const smsTab = page.getByRole('tab', { name: SIGN_IN_TEXT.smsTab })
  test.skip(
    (await smsTab.count()) === 0,
    'the SMS channel is not offered here, so there is no channel switch to exercise',
  )

  await smsTab.click()

  // Switching channels unmounts the previous form, so the password field
  // is gone rather than merely hidden -- half-typed state and a whole
  // attempt's error reset with it.
  await expect(page.getByRole('textbox', { name: 'Phone number' })).toBeVisible()
  await expect(page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel })).toHaveCount(0)
})

test('a single-tenant account reaches only the tenant it is a member of', async ({ page }) => {
  // demo-acme-only is granted in the demo's single tenant and nowhere
  // else. Signing in with no tenant named resolves its first (and only)
  // tenant, so the frame appears -- and the tenant it names is that one,
  // never the other configured tenant.
  await signInAs(page, DEMO_ACME_ONLY)

  await expect(page.getByRole('button', { name: APP_TEXT.tenantAcme })).toBeVisible()
  await expect(page.getByRole('button', { name: APP_TEXT.tenantGlobex })).toHaveCount(0)
})
