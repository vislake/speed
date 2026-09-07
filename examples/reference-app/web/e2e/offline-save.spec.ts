/**
 * What happens when the clinic's network drops mid-work.
 *
 * A dentist types a patient's consultation note, the practice wifi
 * drops, and they press save. Two things then matter more than anything
 * else on screen: their text must still be there, and they must be told
 * it is the network rather than the product.
 *
 * The first is already true and this gate keeps it that way -- if the
 * field were cleared, a clinician would have to retype a patient record
 * from memory, which is how records get wrong. The second is not: the
 * surface renders "Something went wrong. Try again later.", which sends
 * a clinician looking for a product fault while the answer is their
 * router. The app already ships the right sentence for this --
 * notes.errors.client, "Could not reach the server. Check your
 * connection and try again." -- and simply never reaches it, so this is
 * a code-mapping defect rather than missing copy: the same shape as the
 * envelope-contract defect, where good text sat behind a code that never
 * arrived.
 *
 * The network is cut by routing the page's own API calls to failure
 * rather than by stopping a server, so the gate does not depend on how
 * the suite's backend happens to be run -- and so it works identically
 * against a local server and against a real deployment.
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import { APP_TEXT, openSurface, signInAs } from './test-utils/journeys.js'

/** What a clinician must be told, in substance rather than in wording. */
const NAMES_THE_NETWORK = /connect|connection|network|reach the server|offline/i

/** The fallback that must NOT be what they are told. */
const GENERIC_FALLBACK = /something went wrong/i

test(
  'a save that fails offline keeps the text and blames the network, not the product',
  { tag: '@pending' },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await openSurface(page, APP_TEXT.navNotes)

    const note = 'Patient consultation notes - crown recommended for tooth 14'
    await page.getByRole('textbox', { name: APP_TEXT.notesTextLabel }).fill(note)

    // The wifi drops: every API call the page makes from here fails at
    // the transport, which is what @speed/api-client reports as
    // client.network.
    await page.route('**/api/**', (route) => route.abort('internetdisconnected'))

    await page.getByRole('button', { name: APP_TEXT.notesCreateSubmit }).click()

    const alert = page.getByRole('alert')
    await expect(alert).toBeVisible()

    // Told what actually happened.
    await expect(
      alert,
      'a clinician whose wifi dropped is told the product failed, so they retry instead of checking their network',
    ).toHaveText(NAMES_THE_NETWORK)
    await expect(alert, 'the generic fallback stands in for a known transport failure').not.toHaveText(
      GENERIC_FALLBACK,
    )

    // And their words are still on screen. This half already works; the
    // assertion exists so it keeps working, because losing a half-typed
    // patient record is worse than any wording.
    await expect(
      page.getByRole('textbox', { name: APP_TEXT.notesTextLabel }),
      'the note a clinician had typed was cleared by a failed save',
    ).toHaveValue(note)
  },
)

test(
  'the work survives the network coming back',
  { tag: '@pending' },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await openSurface(page, APP_TEXT.navNotes)

    const note = `Patient note written across an outage ${Date.now()}`
    await page.getByRole('textbox', { name: APP_TEXT.notesTextLabel }).fill(note)

    await page.route('**/api/**', (route) => route.abort('internetdisconnected'))
    await page.getByRole('button', { name: APP_TEXT.notesCreateSubmit }).click()
    await expect(page.getByRole('alert')).toBeVisible()

    // The wifi comes back. The clinician presses save again -- the whole
    // point of keeping their text -- and the record lands.
    await page.unroute('**/api/**')
    await page.getByRole('button', { name: APP_TEXT.notesCreateSubmit }).click()

    await expect(
      page.getByRole('row', { name: new RegExp(note.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')) }),
      'retrying after the network returned did not save the note the clinician kept',
    ).toBeVisible()
  },
)
