/**
 * The suite's own failure guards, gated the same way the product is.
 *
 * Every shared journey wait runs through test-utils/journeys.ts's
 * whileSignedIn, which lets three outcomes race: the wait's own
 * condition, the app's return to the sign-in surface, and the browser's
 * own report that its network process crashed. Without those branches a
 * dead browser is reported as whatever control the journey happened to
 * be missing -- a red that reads like a product defect and invites a
 * fix to the gate instead. The sign-in wait (awaitSignInAnswer) races
 * its own set of answers, and the one it stops on that no healthy
 * server can produce is the surface's generic fallback -- the text a
 * response outside the client's mapped error codes degrades to. A wait
 * blind to that answer sits until the whole test budget is spent and
 * reports a timeout. This spec pins the three branches a journey cannot
 * otherwise prove cheaply:
 *
 *   - the crash branch must NAME the crash it observed, not leave the
 *     missing control as the whole story;
 *   - the sign-in wait must stop on an answer the surface cannot name,
 *     and say so, rather than wait on a frame that answer rules out;
 *   - the enabled-state sibling must be satisfied by ENABLEMENT, not by
 *     visibility: a control that is on screen but disabled (the
 *     case-create submit before its photo's upload settles) is exactly
 *     the shape that used to burn a whole test budget as a click
 *     timeout.
 *
 * Nothing here touches the product: no navigation, no server surface,
 * no sign-in, so the spec runs in the default tier on every engine
 * without spending the login budget. What it drives is a blank page, a
 * fabricated alert, and the console messages a dying browser prints.
 */
import { expect, test } from '@playwright/test'
import { DEMO_READER } from './test-utils/accounts.js'
import {
  AUTH_ERROR_TEXT,
  awaitSignInAnswer,
  expectEnabledWhileSignedIn,
  expectWhileSignedIn,
} from './test-utils/journeys.js'

/**
 * The console line a WebKit network-process crash prints, in the shape
 * the suite's own artifacts carry it (the URL and token vary per run;
 * only the tail is the signature).
 */
const CRASH_CONSOLE_LINE =
  "WebSocket connection to 'ws://127.0.0.1:1/?token=probe' failed: WebSocket network error: Network process crashed."

test('a wait stopped by a browser crash names the crash, not the missing control', async ({ page }) => {
  // Started first, both handlers attached on the spot: the guard
  // installs its console watch synchronously and the report below lands
  // mid-wait, so the rejection must have somewhere to go the moment it
  // happens.
  const waiting = expectWhileSignedIn(
    page,
    page.getByRole('button', { name: 'probe control that is never present' }),
    'probe: the control never appeared',
    5_000,
  ).then(
    () => undefined,
    (rejection: unknown) => rejection as Error,
  )
  await page.evaluate((line) => console.error(line), CRASH_CONSOLE_LINE)

  const failure = await waiting
  expect(
    failure?.message,
    'the crash must be named by the wait it stopped, not left as a missing control',
  ).toContain('Network process crashed')
})

test('a sign-in answered with an unnameable error names that answer, not the missing frame', async ({
  page,
}) => {
  // A page shaped like the sign-in surface after a submission the
  // client could not name: the fallback alert is up, the frame this
  // wait is looking for can never come, and no healthy server produces
  // this state. No navigation, no server, no sign-in -- the same
  // reasoning the crash case above states.
  await page.setContent(`<div role="alert">${AUTH_ERROR_TEXT.genericFallback}</div>`)
  // The deadline is this spec's own, not the wait's: against a helper
  // blind to the fallback the wait would stay pending (in a journey,
  // until the test budget ends), so the race is what turns that
  // blindness into a red here rather than a hang.
  const outcome = await Promise.race([
    awaitSignInAnswer(page, DEMO_READER).then(
      () => new Error('the unnameable answer settled as a signed-in frame'),
      (rejection: unknown) => rejection as Error,
    ),
    new Promise<Error>((resolve) => {
      setTimeout(
        () => resolve(new Error('the unnameable answer settled nothing: still waiting on a frame')),
        3_000,
      )
    }),
  ])
  expect(
    outcome.message,
    'the wait must name the answer it saw rather than keep waiting on the frame that answer rules out',
  ).toContain(AUTH_ERROR_TEXT.genericFallback)
})

test('the enabled guard is satisfied by enablement, not by visibility', async ({ page }) => {
  await page.setContent('<button type="submit" disabled>Create case</button>')
  const submit = page.getByRole('button', { name: 'Create case' })
  // The trap the guard exists for: the control is there, a person can
  // see it, and it is still not submittable.
  await expect(submit).toBeVisible()

  const refused: Error = await expectEnabledWhileSignedIn(
    page,
    submit,
    'probe: the submit never became enabled',
    1_000,
  ).then(
    () => new Error('the enabled guard settled for a disabled control'),
    (rejection: unknown) => rejection as Error,
  )
  expect(refused.message).toContain('probe: the submit never became enabled')

  // And it settles the moment the control does become enabled.
  const settled = expectEnabledWhileSignedIn(
    page,
    submit,
    'probe: the submit never became enabled',
    30_000,
  )
  await submit.evaluate((element: HTMLButtonElement) => {
    element.disabled = false
  })
  await settled
})
