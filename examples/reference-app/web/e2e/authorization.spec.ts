/**
 * Authorization at the surface: what a read-only member can see, what the
 * same member is refused, and what the refusal actually says.
 *
 * The reference-app gates its notes routes on real rbac permissions
 * (notes:read / notes:write, granted per demo account in
 * cmd/server/demo_users.go), so this spec is the browser-level proof of
 * the two properties that matter to a dental practice with staff of
 * different seniority: a hygienist can read the chart and cannot write
 * it, and the refusal explains itself rather than looking like a bug.
 *
 * The refusal text is a third regression gate on the envelope-contract
 * defect (f23079d): the server answers rbac.permission_denied, and the
 * notes surface maps that code to its own message. Before the fix that
 * code never reached the surface as a code at all, so the page rendered
 * its unknown-error fallback for what is a perfectly ordinary,
 * explainable answer.
 *
 * Both tests align tenants explicitly rather than assuming one. Which
 * tenant an account lands in is not fixed (see TENANT_NAMES in
 * test-utils/journeys.ts), so a spec that wrote as one account and read
 * as another would fail for the wrong reason -- or, worse, pass for one.
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER, DEMO_READER } from './test-utils/accounts.js'
import {
  APP_TEXT,
  openSurface,
  otherTenant,
  readCurrentTenant,
  signInAs,
  switchTenant,
} from './test-utils/journeys.js'

test('a read-only member reads what an owner wrote in the same tenant', async ({ page }) => {
  // Seed one note as the owner so the reader has something to read: an
  // empty list would pass a read assertion for the wrong reason.
  const text = `note the reader must be able to read ${Date.now()}`
  await signInAs(page, DEMO_OWNER)
  const tenant = await readCurrentTenant(page)
  await openSurface(page, APP_TEXT.navNotes)
  await page.getByRole('textbox', { name: APP_TEXT.notesTextLabel }).fill(text)
  await page.getByRole('button', { name: APP_TEXT.notesCreateSubmit }).click()
  await expect(page.getByRole('row', { name: new RegExp(escapeForRegExp(text)) })).toBeVisible()

  // Sign in as the reader. No cookie clearing is needed or possible: the
  // session is memory-only by contract, so the reload signInAs starts
  // with is what makes the page anonymous again.
  await signInAs(page, DEMO_READER)
  await switchTenant(page, tenant)
  await openSurface(page, APP_TEXT.navNotes)
  await expect(page.getByRole('row', { name: new RegExp(escapeForRegExp(text)) })).toBeVisible()

  // The same member, writing, is refused -- with the specific permission
  // message and never the surface's unknown-error fallback: the code
  // arrived as a code.
  await page
    .getByRole('textbox', { name: APP_TEXT.notesTextLabel })
    .fill('a reader must not be able to write this')
  await page.getByRole('button', { name: APP_TEXT.notesCreateSubmit }).click()
  await expect(page.getByRole('alert')).toContainText(APP_TEXT.notesPermissionDenied)
  await expect(page.getByRole('alert')).not.toContainText('Something went wrong. Try again later.')
})

test('the notes a member sees are scoped to the tenant they are working in', async ({ page }) => {
  // A note written in one tenant must not appear after switching to the
  // other. This is the surface-level proof of the isolation the dbkit
  // Repository and the GORM tenant plugin enforce below it -- the
  // property no amount of component testing can observe, because a
  // scripted double answers whatever list it was told to.
  const text = `note scoped to one tenant ${Date.now()}`
  await signInAs(page, DEMO_OWNER)
  await openSurface(page, APP_TEXT.navNotes)
  await page.getByRole('textbox', { name: APP_TEXT.notesTextLabel }).fill(text)
  await page.getByRole('button', { name: APP_TEXT.notesCreateSubmit }).click()
  const row = page.getByRole('row', { name: new RegExp(escapeForRegExp(text)) })
  await expect(row).toBeVisible()

  const current = await readCurrentTenant(page)
  await switchTenant(page, otherTenant(current))
  await openSurface(page, APP_TEXT.navNotes)
  await expect(row).toHaveCount(0)

  // And it is still there on the way back: the switch scoped the read,
  // it did not lose the row.
  await switchTenant(page, current)
  await openSurface(page, APP_TEXT.navNotes)
  await expect(row).toBeVisible()
})

/** Escapes a note's text for use inside an accessible-name RegExp. */
function escapeForRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}
