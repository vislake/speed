/**
 * The password sign-in journey: what a practice member does first, and
 * what the surface tells them when the server refuses.
 *
 * The refusal assertion here is a regression gate for the
 * envelope-contract defect (fixed in f23079d): @speed/api-client required
 * a traceId field on the error envelope, no backend writer has ever
 * emitted one, so every real answer degraded to a synthetic
 * client.http.<status> code that no reachable-error whitelist maps --
 * rendering the generic fallback in place of every specific message. The
 * component suites could not see it, because their scripted fetch doubles
 * wrote envelopes their own authors shaped. A browser against a real
 * server sees it on the first refused sign-in.
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
  // sign-in resolved a real membership rather than refusing for the lack
  // of one -- the property the sign-in-membership defect broke on every
  // process restart. WHICH tenant is deliberately not asserted: an
  // account in several tenants lands in whichever one the host's
  // membership order happens to put first, and that order is not fixed
  // (see TENANT_NAMES in test-utils/journeys.ts).
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
  await submitPasswordSignIn(page, 'nobody-was-ever-registered-here@example.com', 'wrong-password')

  await expectSpecificError(page, AUTH_ERROR_TEXT.invalidCredentials)
  // Still on the sign-in surface: a refusal changes no session state.
  await expect(page.getByRole('button', { name: SIGN_IN_TEXT.submit })).toBeVisible()
})

test('the SMS channel replaces the password form when its tab is chosen', async ({ page }) => {
  await visitSignIn(page)
  await page.getByRole('tab', { name: SIGN_IN_TEXT.smsTab }).click()

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
