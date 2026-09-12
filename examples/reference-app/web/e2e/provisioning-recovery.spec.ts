/**
 * That a practice whose clinic failed to open on the first try still
 * gets in.
 *
 * Registration provisions the registrant's own clinic synchronously
 * inside the register request, and the register route deliberately does
 * not fail a registration whose account was created -- so a provisioning
 * failure is swallowed and the account exists with no clinic. The
 * account cannot re-register and no event redelivery will come to it,
 * so recovery rests on a retry job whose row is the durable record of
 * the unfinished work. The recovery is only observable if provisioning
 * can be made to fail on purpose:
 * APP_FAIL_SELF_SERVICE_PROVISION=N fails the first N attempts of EACH
 * account, counted per user id, and leaves every boot without it
 * unchanged.
 *
 * N IS 1, AND THAT IS A BUDGET DECISION
 *
 * The gate signs in exactly once, after convergence. With a larger N the
 * retry backoff doubles into minutes and the only way to know when to
 * sign in would be to try repeatedly -- which is a retry-past-a-rate-
 * limit loop wearing different clothes, and this suite refuses those:
 * sign-in is rate-limited per account (go/authn's ratelimit.go), and a
 * gate that retried past the limit would stop being able to tell a real
 * regression from its own impatience.
 *
 * BOTH HALVES ARE ASSERTED, WHICH IS THE POINT
 *
 * "The person can sign in" on its own would pass just as well if the
 * injection did nothing -- provisioning would simply have succeeded, and
 * the gate would report a recovery that never happened. So the server's
 * own output has to show the failure it was told to inject. The pair of
 * assertions is what makes this a gate about recovery rather than about
 * registration.
 */
import { expect } from '@playwright/test'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { INJECT_API_PORT } from '../playwright.config.js'
// routeApiTo's interception is retired with the test -- this module's
// own `test` fixture, not the context torn out from under it.
import { bootServer, routeApiTo, test } from './test-utils/servers.js'
import {
  APP_TEXT,
  expectOutsideDemoOrganizations,
  expectSignedIn,
  registerThroughUi,
  submitPasswordSignIn,
} from './test-utils/journeys.js'

/** A password that satisfies authn's real policy (12 characters minimum). */
const SIGNUP_PASSWORD = 'e2e-recovery-2026'

/**
 * This spec's own database. The run's shared server cannot be the one
 * under test here: arming the injection on it would fail the first
 * provisioning attempt of every other gate's registration too.
 */
const databasePath = join(
  tmpdir(),
  `reference-app-e2e-recovery-${Date.now()}-${process.pid}.db`,
)

test(
  'a practice whose clinic failed to open on the first try still gets in',
  { tag: '@budget' },
  async ({ page }) => {
    const server = await bootServer({
      port: INJECT_API_PORT,
      databasePath,
      // Fail the first provisioning attempt of every account: the one
      // the register request makes synchronously. The retry job's first
      // attempt then succeeds.
      env: { APP_FAIL_SELF_SERVICE_PROVISION: '1' },
    })

    try {
      await routeApiTo(page, INJECT_API_PORT)

      const email = `e2e-recovery-${Date.now()}@example.com`
      // The registration answers as it always does -- a person cannot
      // tell from this screen that anything went wrong behind it, which
      // is exactly why the recovery has to be real rather than advisory.
      await registerThroughUi(page, {
        email,
        password: SIGNUP_PASSWORD,
        displayName: 'Recovery Dental',
      })

      // BOTH THE FAILURE AND THE RECOVERY REALLY HAPPENED, read from the
      // server's own output before the sign-in is attempted.
      //
      // The failure first, so a green result cannot be explained by the
      // injection having done nothing -- provisioning would simply have
      // succeeded and the gate would report a recovery that never
      // occurred.
      await expect
        .poll(() => server.said(), { timeout: 30_000 })
        .toMatch(/provisioning failed synchronously/)

      // Then the retry's own success. A poll for any mention of
      // provisioning would be satisfied by the failure line immediately,
      // and signing in then -- ahead of the retry -- would be refused as
      // memberless and look like a product defect. The signal has to
      // name the thing being waited for instead of something that merely
      // appears near it.
      await expect
        .poll(() => server.said(), { timeout: 60_000 })
        .toMatch(/job succeeded.*self_service\.provision_clinic/)

      // Doing exactly what the product told them to do -- once. By now
      // the retry has had the register round trip plus this poll to
      // converge; if it has not, this sign-in is refused and the gate
      // fails, which is the honest outcome. It is never attempted twice.
      await page.getByRole('button', { name: APP_TEXT.registerBackToSignIn }).click()
      await submitPasswordSignIn(page, email, SIGNUP_PASSWORD)

      // In, and in a clinic of their own: the recovery produced the same
      // end state a first-try success produces, not a half-provisioned
      // account that can sign in but belongs nowhere.
      await expectSignedIn(page)
      await expectOutsideDemoOrganizations(page)
    } finally {
      await server.stop()
    }
  },
)
