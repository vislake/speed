/**
 * The notes surface: the app's one tenant-scoped business surface today,
 * driven end to end -- an empty list, a note created through the form,
 * the row that appears for it, and the field that answers for itself.
 *
 * The surface stands in for the patient-and-case surfaces the product
 * design calls for (examples/reference-app is an AI smile-simulation
 * platform; notes is the placeholder its own module doc admits to), so
 * this spec is deliberately shaped as the template those will follow:
 * read the list, write through the form, assert the row a person sees --
 * never a request or a cache key.
 *
 * One sign-in covers both behaviours. Sign-in is rate-limited per
 * account (the limits live in go/authn's ratelimit.go) and this file
 * needs the owner, the one account the write path depends on, so it
 * spends exactly one of the owner's sign-ins.
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import { APP_TEXT, openSurface, signInAs } from './test-utils/journeys.js'

test('an owner writes a note and sees it in the tenant list', async ({ page }) => {
  await signInAs(page, DEMO_OWNER)
  await openSurface(page, APP_TEXT.navNotes)

  await expect(page.getByRole('heading', { name: APP_TEXT.notesHeading, level: 1 })).toBeVisible()

  // An empty submit is refused by the form itself: the field answers for
  // itself and no request goes out, the same "field errors belong to the
  // fields" split the sign-in family follows.
  await page.getByRole('button', { name: APP_TEXT.notesCreateSubmit }).click()
  await expect(page.getByText('This field is required.')).toBeVisible()

  const text = `note from the e2e suite ${Date.now()}`
  await page.getByRole('textbox', { name: APP_TEXT.notesTextLabel }).fill(text)
  await page.getByRole('button', { name: APP_TEXT.notesCreateSubmit }).click()

  // The row is the assertion: the list re-reads from the server after the
  // write, so a row appearing here means the note was persisted under
  // this tenant and read back, not merely echoed by the form.
  await expect(page.getByRole('row', { name: new RegExp(escapeForRegExp(text)) })).toBeVisible()
})

/** Escapes a note's text for use inside an accessible-name RegExp. */
function escapeForRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}
