/**
 * That a person always knows which clinic they are working in.
 *
 * A multi-location practice manager wrote a patient record in one clinic,
 * switched to the other, and the screen told them nothing: the switcher
 * that names the current clinic is invisible (its own blue on the app
 * bar's blue), the switch produced no confirmation, and no heading,
 * breadcrumb or body text carries the clinic's name. The only signal was
 * the record list going empty -- which reads as "the data is gone", not
 * as "you changed clinic".
 *
 * The stakes are not cosmetic. The next thing that manager does is write
 * another patient record. If they do not know where they are, that record
 * lands in the wrong clinic's chart -- a compliance problem in a medical
 * setting, not a data-entry slip -- and afterwards nobody can reconstruct
 * which clinic was selected, because the only thing that ever said so was
 * a button nobody could see.
 *
 * So this gate is deliberately stricter than "the switcher meets contrast
 * requirements" (visible-controls.spec.ts holds that separately). A
 * control can be perfectly legible and still be scanned past. The
 * question here is whether the clinic's identity reaches a person who is
 * looking at the work they are doing -- which is the main content area,
 * not the chrome around it.
 *
 * It asserts a property, not a layout: the current clinic's name must
 * appear somewhere inside the main landmark. A page heading carrying it
 * passes. A subtitle passes. A banner-only mention does not, which is the
 * whole point.
 */
import { expect, test } from '@playwright/test'
// DEMO_READER, not DEMO_OWNER: this gate only reads (both demo
// tenants hold the reader's membership, so the cross-clinic switch is
// legal for it), and the suite spends sign-ins deliberately -- the
// demo server's per-account limit (go/authn's ratelimit.go: five per
// account per minute) is shared with the gates that need the owner's
// write grant, so a read-only gate never draws on the owner's budget.
import { DEMO_READER } from './test-utils/accounts.js'
import {
  APP_TEXT,
  SIGN_IN_TEXT,
  expectSignedIn,
  openSurface,
  otherTenant,
  readCurrentTenant,
  readTenantLabel,
  signInAs,
  submitPasswordSignIn,
  switchTenant,
  visitSignIn,
} from './test-utils/journeys.js'

/** A password that satisfies authn's real policy (12 characters minimum). */
const SIGNUP_PASSWORD = 'e2e-new-clinic-2026'

/**
 * What a tenant id looks like when nothing has given it a name: this
 * host derives a self-service clinic's tenant id from the registrant's
 * user id (cmd/server/self_service.go's clinicTenantOf), so an
 * unnamed clinic surfaces as exactly this shape.
 */
const RAW_TENANT_ID = /^tenant-[0-9a-f-]{8,}$/i

// Both tests were tagged @pending while the defect they found was open:
// no surface named the clinic it was scoped to, and a switch produced no
// announcement at all -- the acceptance story in the file header. The
// surfaces now render the clinic-context line at page-title level (the
// host's CurrentClinicLine on the home and notes surfaces) and the
// tenant switcher announces its committed switch through its live
// region; both tests pass against the fixed tree (the closing round's
// verification), and the acceptance session re-ran both against a
// freshly deployed tree and confirmed it. So the tag is @budget now,
// not @pending: verified, and out of the default run only because its
// sign-ins do not fit the suite's per-account budget -- a suite-budget
// decision, never a visibility one. `pnpm test:e2e:budget` runs them
// with a budget of their own (see e2e/README.md).
test(
  'the clinic being worked in is named in the main content, not only in the chrome',
  { tag: '@budget' },
  async ({ page }) => {
    await signInAs(page, DEMO_READER)
    const clinic = await readCurrentTenant(page)

    // On every surface where a person does or reads work. The home
    // surface is included deliberately: it is where someone lands, and
    // "which clinic am I in" is a question they have before they act.
    for (const surface of [APP_TEXT.navHome, APP_TEXT.navNotes] as const) {
      await openSurface(page, surface)
      await expect(
        page.getByRole('main').getByText(clinic, { exact: false }),
        `the ${surface} surface never names the clinic being worked in (${clinic}), so a person writing a patient record cannot tell where it lands`,
      ).toBeVisible()
    }
  },
)

test(
  'switching clinic says so, and the new clinic is named where the work is',
  { tag: '@budget' },
  async ({ page }) => {
    await signInAs(page, DEMO_READER)
    await openSurface(page, APP_TEXT.navNotes)
    const from = await readCurrentTenant(page)
    const to = otherTenant(from)

    await switchTenant(page, to)

    // Said out loud: a context change that silently alters which
    // patients' records are on screen must announce itself, and it must
    // do so in a live region -- a person using a screen reader cannot
    // glance at the chrome to check.
    await expect(
      page.getByRole('status').filter({ hasText: to }),
      'switching clinic changed which records are shown without telling anyone it happened',
    ).toBeVisible()

    // And the work area now names where the work goes.
    await openSurface(page, APP_TEXT.navNotes)
    await expect(
      page.getByRole('main').getByText(to, { exact: false }),
      `after switching to ${to}, the work area still does not say so`,
    ).toBeVisible()
    await expect(
      page.getByRole('main').getByText(from, { exact: false }),
      `after switching away from ${from}, the work area still names the old clinic`,
    ).toHaveCount(0)
  },
)

test(
  'a clinic a practice just created for itself is named, not left as an id',
  // @pending, not @budget: the defect this checks is OPEN. Its two
  // siblings above are @budget because they pass and only the sign-in
  // budget keeps them out of the default run -- conflating the two is
  // exactly what this suite split the tags to prevent, and tagging this
  // one @budget would have filed a live defect under "verified".
  { tag: '@pending' },
  async ({ page }) => {
    // THE OBSERVATION THIS GATE HAD WRONG
    //
    // Its two tests above sign in as a demo account, and they passed --
    // while the property they exist for was failing for the newest kind
    // of clinic in the product. The clinic's NAME is resolved from the
    // host's own hard-coded demo roster, so a clinic created at run time
    // by self-service registration is not in it: the switcher shows the
    // raw tenant id and the work area names no clinic at all. Verified
    // by hand on the real deployment during acceptance, on every surface
    // (home, cases, new case), while these gates stayed green.
    //
    // A gate whose subject is "can a person tell which clinic they are
    // working in" cannot only ever ask it about the clinics that were
    // configured before the server booted.
    //
    // It deliberately does NOT assert what the clinic should be called
    // -- that is a product decision (the practice's own name at signup,
    // an editable field later, something else). It asserts the two
    // things any answer has to satisfy: a person can recognise it, and
    // the work area says it.
    const email = `e2e-named-clinic-${Date.now()}@example.com`

    await visitSignIn(page)
    await page.getByRole('button', { name: SIGN_IN_TEXT.registerAction }).click()
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(email)
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill(SIGNUP_PASSWORD)
    await page
      .getByRole('textbox', { name: SIGN_IN_TEXT.displayNameLabel })
      .fill('Northside Dental')
    await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()
    await expect(page.getByRole('status')).toContainText(APP_TEXT.registerSuccess)
    await page.getByRole('button', { name: APP_TEXT.registerBackToSignIn }).click()
    await submitPasswordSignIn(page, email, SIGNUP_PASSWORD)
    await expectSignedIn(page)

    // Recognisable: whatever the switcher shows, it is not a bare id. A
    // dentist asked "which clinic are you in" cannot answer
    // "tenant-84ef467d-3653-41d8-a604-a1e686eb8be6".
    const clinic = await readTenantLabel(page)
    expect(
      clinic,
      `the clinic this practice just created is shown as a raw tenant id (${clinic}), which names nothing a person can recognise`,
    ).not.toMatch(RAW_TENANT_ID)

    // And said where the work happens, the same property the demo
    // clinics get from the two tests above.
    await openSurface(page, APP_TEXT.navHome)
    await expect(
      page.getByRole('main').getByText(clinic, { exact: false }),
      `the home surface never names the clinic being worked in (${clinic}), so a person opening a patient record cannot tell where it lands`,
    ).toBeVisible()
  },
)
