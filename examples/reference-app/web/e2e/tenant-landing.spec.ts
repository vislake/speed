/**
 * Where a multi-tenant account lands when it signs in without naming a
 * tenant -- and that it lands in the SAME place every time.
 *
 * A multi-tenant account's landing tenant must be deterministic: two
 * sign-ins by one person must not open two different practice
 * locations, and two colleagues must not disagree about which location
 * "the app" meant. The host's tenant universe is explicitly ordered
 * (sorted by tenant id) before the membership answer reads it, so the
 * landing tenant is the same on every sign-in.
 *
 * This spec asserts the property that ordering buys rather than the
 * specific tenant it happens to name: sign in, note where the frame
 * landed, sign in again, and require the same answer. Asserting the
 * name itself would re-freeze a host decision (which tenant sorts
 * first) that the suite has no business pinning.
 *
 * Two sign-ins by one account, which is inside the per-account login
 * rate limit (go/authn's ratelimit.go). The reader is used rather than
 * the owner: both hold membership in every configured tenant, so both
 * can show the property, and spending the reader keeps the owner's
 * budget for the specs that need its write permission.
 */
import { expect, test } from '@playwright/test'
import { DEMO_READER } from './test-utils/accounts.js'
import { TENANT_NAMES, readCurrentTenant, signInAs } from './test-utils/journeys.js'

test('a multi-tenant account lands in the same tenant on every sign-in', async ({ page }) => {
  await signInAs(page, DEMO_READER)
  const first = await readCurrentTenant(page)
  expect(TENANT_NAMES).toContain(first)

  // A reload starts anonymous (the session is memory-only), so this is a
  // genuinely fresh sign-in rather than a re-render of the first one.
  await page.reload()
  await signInAs(page, DEMO_READER)
  const second = await readCurrentTenant(page)

  expect(second).toBe(first)
})
