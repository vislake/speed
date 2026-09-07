/**
 * That a practice can keep the simulation it paid for.
 *
 * A generated simulation is a thing the clinic bought: credits left the
 * ledger for it. Today it lives only inside the product -- it can be
 * looked at, and it can be turned into a link for the patient -- and
 * there is no way to save the image itself. That is the gap this gate
 * names, and it is the smallest of the brief's five unbuilt surfaces,
 * which is why it is first.
 *
 * WHY A LINK IS NOT THE SAME THING
 *
 * Sharing (block C) and keeping are different needs, and the product
 * already meets only one of them. A share link is deliberately temporary
 * -- go/sharing forces a default expiry and offers no never-expiring
 * option, by design -- and revocable on the very next access check. That
 * is right for something handed to a patient. It is wrong for the
 * clinic's own record: a dentist who wants the before-and-after in the
 * patient's chart, in a treatment plan, or in a message to a lab needs
 * the file, not a URL that stops working. A practice told "you already
 * have a link" has been answered a question it did not ask.
 *
 * WHAT IT ASSERTS, AND WHAT IT LEAVES OPEN
 *
 * That pressing the control the surface offers produces a file the
 * browser actually receives -- a real download event, with a name, of
 * non-trivial size. It does not prescribe the format, the filename, one
 * button or two (the original and the result are both plausibly worth
 * saving), a zip, or whether the bytes come from go/storage directly or
 * through a fresh render. Any of those passes. What does not pass is a
 * surface where the only way out is a screenshot.
 *
 * @pending, in the tag's second sense: not "this is broken" but "this
 * surface does not exist yet". The app's own en-US bundle contains no
 * download or save copy at all, so there is nothing here to be wrong --
 * the gate is written ahead of the work, the way the core-journey blocks
 * were, so that the acceptance criterion exists before the round does
 * rather than being argued about after it.
 *
 * Tagged @pending ALONE, not also @budget: @budget is this suite's word
 * for "verified passing", and a red gate in that tier would cost it the
 * one thing it is for. The sign-ins this gate spends are covered by the
 * @pending tier being its own invocation against its own freshly booted
 * server, which is the same mechanism that makes @budget a real tier.
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import { signInAs } from './test-utils/journeys.js'
import { openCaseWithSimulation } from './test-utils/cases.js'

/**
 * The control that hands the file over. An expectation of an accessible
 * name, not a decree -- see test-utils/cases.ts's own note.
 *
 * Deliberately not matching "share": a share control exists today and
 * would make this gate pass on the very thing it says is not enough.
 * The trap is worth naming, because the loose pattern that reaches the
 * control you want is the one that also reaches the control you have --
 * which is exactly how block D's nav pattern once landed on the wrong
 * page and passed.
 */
const KEEP_IT = /download|save (the )?(image|result|simulation|photo)|export/i

/** Below this a "file" is an error page or an empty placeholder. */
const PLAUSIBLE_IMAGE_BYTES = 200

test(
  'a practice can save the simulation it paid for, not just look at it',
  { tag: '@pending' },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await openCaseWithSimulation(page)

    // Asserted before the download is attempted, and separately, so the
    // failure says "there is no way to save this" rather than "a
    // download never started" -- which is also true when the control is
    // there and broken, and does not distinguish the two.
    const control = page.getByRole('button', { name: KEEP_IT }).or(
      page.getByRole('link', { name: KEEP_IT }),
    )
    await expect(
      control.first(),
      'a practice can generate a simulation and share a temporary link, but has no way to keep the image it spent credits on -- the only way out of the product is a screenshot',
    ).toBeVisible({ timeout: 15_000 })

    const arriving = page.waitForEvent('download', { timeout: 30_000 })
    await control.first().click()
    const file = await arriving

    // A real file, named, with bytes in it. Each of the three is a
    // separate way this can be shipped and still not work: a click that
    // opens a new tab and nothing else, a download named for the app's
    // own route rather than the patient's case, and a zero-byte or
    // error-page body that a person only finds out about when they try
    // to open it.
    expect(
      file.suggestedFilename(),
      'the download arrives with no filename, so it lands in a folder as an unidentifiable blob',
    ).not.toBe('')

    const saved = await file.path()
    expect(saved, 'the browser reported a download that has no file behind it').not.toBeNull()

    const { size } = await import('node:fs/promises').then((fs) => fs.stat(saved as string))
    expect(
      size,
      `the saved file is ${size} bytes, which is not an image -- an error page or an empty placeholder saves just as successfully as a simulation does`,
    ).toBeGreaterThan(PLAUSIBLE_IMAGE_BYTES)
  },
)
