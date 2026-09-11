/**
 * That the credits ledger reads like a statement a clinic owner can
 * check, not like a log line.
 *
 * Each ledger row names what happened in the reader's own language
 * ("Simulation", "Top-up"). The annotation go/billing stores alongside
 * it is not display text: the field is declared "machine-readable
 * annotation with a declared shape, never free text" -- ASCII letters
 * and digits joined by ":" "_" "-", its documented vocabulary being
 * `ai_generation:job_123`, `plan:pro:monthly_included`,
 * `expiry:2026-09-policy` -- and narrowed precisely so it does NOT
 * carry writing meant for people: the same text is copied verbatim into
 * the audit trail's Changes column, where the content contract forbids
 * anything a caller could slip PII into. A billing statement is where a
 * person reads what happened to their money, so rendering the raw
 * annotation there shows machine text in a list a person reads to make
 * a decision -- and the token adds nothing, being the same fact in
 * machine form. The reason never enters the rendering; the meta line
 * carries the date alone.
 *
 * THE COUNTER-CASE, WHICH IS REAL
 *
 * `@speed/account-ui` renders session `amr` values deliberately as
 * opaque references, untranslated, and that is accepted. Showing a
 * machine token is therefore not wrong everywhere. The distinction drawn
 * here -- an audit reference in a security list read by a technical
 * reader, versus a billing statement read by a clinic owner, where the
 * token duplicates the label beside it -- is a product judgement, made
 * by the product side rather than by this gate.
 *
 * WHAT IT ASSERTS, AND WHAT IT LEAVES OPEN
 *
 * Only that no billing-reason-shaped token appears in the ledger rows.
 * Dropping it from the row, folding it behind a details affordance, or
 * translating it into a phrase are each a pass. The machine reason stays
 * on the API and in the audit trail either way -- removing it from the
 * screen loses no data, and nothing here asks `go/billing` to change.
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import { openSurface, signInAs } from './test-utils/journeys.js'
import { CASE_UI } from './test-utils/cases.js'

/**
 * A token in the shape `go/billing` declares for `Reason`.
 *
 * The FIRST segment must start with a letter, and that is the whole
 * safety of this pattern rather than a detail. Billing's shape is
 * "letters and digits joined by : _ -", and a pattern written that
 * literally matches the ledger's own legitimate text: `1:00` in a
 * timestamp is digits joined by a colon, and `Top-up` is letters joined
 * by a hyphen. Both would have been reported as machine tokens.
 *
 * Requiring a letter first and a ":" or "_" immediately after the first
 * segment separates them exactly: `smilesim:simulate`, `demo:seed`,
 * `ai_generation:job_123`, `plan:pro:monthly_included` and
 * `expiry:2026-09-policy` all match, while `1:00 AM`, `Sep 8, 2026`,
 * `Top-up`, `Simulation refunded`, `-10`, `+1,000` and the zh-CN labels
 * do not -- each was checked against this pattern.
 *
 * A pattern loose enough to catch the bad case catches good ones too:
 * a false positive here accuses a correct download of being an error
 * page.
 */
const BILLING_REASON_SHAPE = /\b[A-Za-z][A-Za-z0-9]*[:_][A-Za-z0-9][A-Za-z0-9:_-]*/

// The reason never enters the rendering at all: not a label-mapping
// table (that would go stale the moment a consumer names a tag nobody
// mapped) and not a translation, but absence, which is robust against
// the whole family rather than the two tokens this app happens to
// produce. The meta line carries the date alone. The gate keeps
// asserting the SHAPE, so a reason reaching the screen under any name
// fails here.
//
// @budget rather than untagged: it spends a sign-in and the default
// tier has none of demo-owner's allowance left.
test(
  'the credits ledger shows no machine annotation to the person reading it',
  { tag: '@budget' },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await openSurface(page, CASE_UI.navCredits)

    // On the credits surface, asserted rather than assumed: a gate that
    // read "the ledger" while standing on another page would be a false
    // pass.
    await expect(
      page.getByRole('heading', { level: 1 }),
      'the gate never reached the credits surface, so whatever it read next was not the ledger',
    ).toContainText(/credits/i)

    // The rows, not the whole surface: the balance line and the page
    // intro are not part of the statement, and scoping to the rows is
    // what keeps the failure message about the thing being judged.
    const rows = page.getByRole('main').getByRole('listitem')
    await expect(rows.first(), 'the ledger shows no rows to read').toBeVisible({
      timeout: 30_000,
    })

    const texts = await rows.allInnerTexts()
    const machine = texts
      .map((text) => BILLING_REASON_SHAPE.exec(text)?.[0])
      .filter((found): found is string => found !== undefined)

    expect(
      machine,
      `the ledger shows the annotation go/billing stores for its own use (${machine.join(' , ')}) to the person reading their billing history -- the row already names what happened in their own language, so this is the same fact in machine form, and go/billing declares the field "machine-readable annotation with a declared shape, never free text"`,
    ).toEqual([])
  },
)
