/**
 * That a practice can keep the simulation it paid for.
 *
 * A generated simulation is a thing the clinic bought: credits left the
 * ledger for it. The result downloads as a real file -- `<a download>`
 * over a Blob URL of the bytes the result image already fetched (no
 * second request), revoked when those bytes change or the panel
 * unmounts, and absent entirely when the data has not arrived or was
 * refused -- so there is never a control that hands over nothing.
 *
 * WHY A LINK IS NOT THE SAME THING
 *
 * Sharing and keeping are different needs. A share link is deliberately
 * temporary -- go/sharing forces a default expiry and offers no
 * never-expiring option, by design -- and revocable on the very next
 * access check. That is right for something handed to a patient. It is
 * wrong for the clinic's own record: a dentist who wants the
 * before-and-after in the patient's chart, in a treatment plan, or in a
 * message to a lab needs the file, not a URL that stops working. A
 * practice told "you already have a link" has been answered a question
 * it did not ask.
 *
 * WHAT IT ASSERTS, AND WHAT IT LEAVES OPEN
 *
 * That pressing the control the surface offers produces a file the
 * browser actually receives -- a real download event, with a name,
 * whose first bytes are an image (or an archive of them). It does not
 * prescribe the format, the filename, one button or two (the original
 * and the result are both plausibly worth saving), a zip, or whether
 * the bytes come from go/storage directly or through a fresh render.
 * Any of those passes. What does not pass is a surface where the only
 * way out is a screenshot.
 *
 * @budget rather than untagged: it signs in and generates.
 */
import { expect, test } from '@playwright/test'
import { readFile } from 'node:fs/promises'
import { DEMO_OWNER } from './test-utils/accounts.js'
import { signInAs } from './test-utils/journeys.js'
import { openCaseWithSimulation } from './test-utils/cases.js'

/**
 * The control that hands the file over. An expectation of an accessible
 * name, not a decree -- see test-utils/cases.ts's own note.
 *
 * Deliberately not matching "share": a share control exists and would
 * make this gate pass on the very thing it says is not enough. The trap
 * is worth naming, because the loose pattern that reaches the control
 * you want is the one that also reaches the control you already have.
 */
const KEEP_IT = /download|save (the )?(image|result|simulation|photo)|export/i

/**
 * What a real image file starts with. PNG, JPEG, RIFF (WebP), and PK for
 * an archive of several, since the gate allows a zip of the pair.
 *
 * IDENTITY, NOT SIZE: a byte threshold cannot separate an image from an
 * error page -- one loose enough to accept the good case is loose enough
 * to accept the bad one, and one tight enough to catch the bad case
 * catches the good one too. The simulation this suite generates comes
 * from its own fake vendor as a genuinely tiny real PNG, so a size floor
 * would reject the legitimate output. The magic number answers the
 * question a threshold would be groping for -- "is this actually an
 * image" -- exactly, and accepts a legitimately tiny one.
 */
const IMAGE_OR_ARCHIVE_MAGIC: readonly (readonly number[])[] = [
  [0x89, 0x50, 0x4e, 0x47], // PNG
  [0xff, 0xd8, 0xff], // JPEG
  [0x52, 0x49, 0x46, 0x46], // RIFF, which is how WebP starts
  [0x50, 0x4b, 0x03, 0x04], // PK: a zip of the pair
]

/**
 * The control that reveals a page's secondary actions, when there is
 * one. A download button next to Share is one honest layout; a download
 * item inside an overflow menu is another, and a gate that only looked
 * for the first would report "there is no way to save this" about the
 * second.
 */
const OVERFLOW = /more|actions|options|menu/i

// The download names itself after the case ("<case name> smile
// simulation.png"): the patient's name, what the image is, and an
// extension -- a name a dentist can still recognise in a downloads
// folder a week later, which is the actual point of keeping the file
// at all.
//
// @budget rather than untagged: it signs in and generates.
test(
  'a practice can save the simulation it paid for, not just look at it',
  { tag: '@budget' },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await openCaseWithSimulation(page)

    // Asserted before the download is attempted, and separately, so the
    // failure says "there is no way to save this" rather than "a
    // download never started" -- which is also true when the control is
    // there and broken, and does not distinguish the two.
    const control = page
      .getByRole('button', { name: KEEP_IT })
      .or(page.getByRole('link', { name: KEEP_IT }))
      .or(page.getByRole('menuitem', { name: KEEP_IT }))

    // Looked for behind an overflow menu too, before concluding there is
    // nowhere to save from. Not doing so would make this gate accuse a
    // correct implementation of having no control at all, purely because
    // it put the control where a secondary action usually goes.
    if (!(await control.first().isVisible().catch(() => false))) {
      const overflow = page.getByRole('button', { name: OVERFLOW })
      if (await overflow.first().isVisible().catch(() => false)) {
        await overflow.first().click()
      }
    }

    await expect(
      control.first(),
      'a practice can generate a simulation and share a temporary link, but has no way to keep the image it spent credits on -- neither beside the result nor in an overflow menu -- so the only way out of the product is a screenshot',
    ).toBeVisible({ timeout: 15_000 })

    const arriving = page.waitForEvent('download', { timeout: 30_000 })
    await control.first().click()

    // Named rather than left as a bare timeout. "waitForEvent(download)
    // exceeded 30000ms" says nothing about what happened; the likely
    // shapes are a control that opens the image in a tab instead of
    // handing it over, or one wired to nothing at all, and an
    // implementer reading the failure should be told which question to
    // ask.
    const file = await arriving.catch(() => {
      throw new Error(
        'the control was pressed and no file ever arrived: either it opens the image somewhere instead of handing it to the browser, or it is wired to nothing -- a practice that clicks it still has no copy of what it paid for',
      )
    })

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

    const bytes = await readFile(saved as string)
    const head = [...bytes.subarray(0, 4)]
    expect(
      IMAGE_OR_ARCHIVE_MAGIC.some((magic) => magic.every((byte, at) => head[at] === byte)),
      `the saved file does not begin like an image or an archive (first bytes ${head.join(' ')}, ${bytes.length} in total) -- an error page or an empty placeholder saves just as successfully as a simulation does, and a practice only finds out when it tries to open the file`,
    ).toBe(true)
  },
)
