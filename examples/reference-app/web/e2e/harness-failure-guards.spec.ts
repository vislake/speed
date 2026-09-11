/**
 * The suite's own failure guards, gated the same way the product is.
 *
 * Every shared journey wait runs through test-utils/journeys.ts's
 * whileSignedIn, which lets three outcomes race: the wait's own
 * condition, the app's return to the sign-in surface, and the browser's
 * own report that its network process crashed. Without those branches a
 * dead browser is reported as whatever control the journey happened to
 * be missing -- a red that reads like a product defect and invites a
 * fix to the gate instead. This spec pins the two branches a journey
 * cannot otherwise prove cheaply:
 *
 *   - the crash branch must NAME the crash it observed, not leave the
 *     missing control as the whole story;
 *   - the enabled-state sibling must be satisfied by ENABLEMENT, not by
 *     visibility: a control that is on screen but disabled (the
 *     case-create submit before its photo's upload settles) is exactly
 *     the shape that used to burn a whole test budget as a click
 *     timeout.
 *
 * Nothing here touches the product: no navigation, no server surface,
 * no sign-in, so the spec runs in the default tier on every engine
 * without spending the login budget. What it drives is a blank page and
 * the console messages a dying browser prints.
 */
import { expect, test } from '@playwright/test'
import {
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
