/**
 * That a practice whose clinic failed to open on the first try still
 * gets in.
 *
 * Registration provisions the registrant's own clinic synchronously
 * inside the register request, and the register route deliberately does
 * not fail a registration whose account was created -- so a provisioning
 * failure is swallowed and the account exists with no clinic. The
 * recovery the code first relied on did not exist: it expected a later
 * redelivery of authn.user.created, and the in-process bus delivers that
 * event exactly once. The account is real, so it cannot re-register; its
 * sign-in would answer the memberless refusal forever. That is the dead
 * end self-service signup was meant to close, reopened by the failure
 * half of its own guarantee, and it is what dcd091c fixed by enqueueing
 * a retry job whose row is the durable record of the unfinished work.
 *
 * WHY THIS GATE COULD NOT BE WRITTEN UNTIL NOW
 *
 * The recovery is only observable if provisioning can be made to fail on
 * purpose, and nothing could ask it to. Until the injection landed, this
 * suite could say the retry had not regressed -- never that it
 * converges, which is a different claim, and the difference is the whole
 * value of the fix. APP_FAIL_SELF_SERVICE_PROVISION=N now fails the
 * first N attempts of EACH account, counted per user id, and leaves
 * every boot without it byte-identical.
 *
 * N IS 1, AND THAT IS A BUDGET DECISION
 *
 * The gate signs in exactly once, after convergence. With a larger N the
 * retry backoff doubles into minutes and the only way to know when to
 * sign in would be to try repeatedly -- which is a retry-past-a-rate-
 * limit loop wearing different clothes, and this suite refuses those:
 * go/authn allows five sign-ins per account per minute, and a gate that
 * retried past it would stop being able to tell a real regression from
 * its own impatience.
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
import { expect, test } from '@playwright/test'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { INJECT_API_PORT } from '../playwright.config.js'
import { bootServer, routeApiTo } from './test-utils/servers.js'
import {
  APP_TEXT,
  SIGN_IN_TEXT,
  expectOutsideDemoOrganizations,
  expectSignedIn,
  submitPasswordSignIn,
  visitSignIn,
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
      await visitSignIn(page)
      await page.getByRole('button', { name: SIGN_IN_TEXT.registerAction }).click()
      await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(email)
      await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill(SIGNUP_PASSWORD)
      await page
        .getByRole('textbox', { name: SIGN_IN_TEXT.displayNameLabel })
        .fill('Recovery Dental')
      await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()

      // The registration answers as it always does. A person cannot tell
      // from this screen that anything went wrong behind it, which is
      // exactly why the recovery has to be real rather than advisory.
      await expect(page.getByRole('status')).toContainText(APP_TEXT.registerSuccess)

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

      // Then the retry's own success, and waiting for THIS is what the
      // first draft got wrong: it polled for any mention of provisioning,
      // which the failure line satisfies immediately, and then signed in
      // -- ahead of the retry. The sign-in was refused as memberless and
      // the gate looked like a product defect. Empirically the retry
      // converges in about five milliseconds, so the wait costs nothing;
      // the point is that the signal names the thing being waited for
      // instead of something that merely appears near it.
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
