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
  openSurface,
  otherTenant,
  readCurrentTenant,
  signInAs,
  switchTenant,
} from './test-utils/journeys.js'

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
