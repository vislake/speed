/**
 * That the credits ledger reads like a statement a clinic owner can
 * check, not like a log line.
 *
 * Found by walking the journey as a person: each ledger row carries a
 * translated label ("Simulation", "Top-up") and, under it, the raw
 * annotation the server stored -- `smilesim:simulate`, `demo:seed`.
 * A dentist reading their own billing history is shown text that was
 * never written for them.
 *
 * WHY THIS IS NOT AN AESTHETIC COMPLAINT
 *
 * `go/billing` says what that field is, in its own words, and it is not
 * display text. The audit-exit round (221d959) closed `Reason` to prose
 * and declared it "machine-readable annotation with a declared shape,
 * never free text" -- ASCII letters and digits joined by ":" "_" "-",
 * its documented vocabulary being `ai_generation:job_123`,
 * `plan:pro:monthly_included`, `expiry:2026-09-policy`. The reason it
 * was closed is the point: the same text is copied verbatim into the
 * audit trail's Changes column, where `go/dbkit/audit`'s content
 * contract forbids anything a caller could slip PII into. The field was
 * narrowed precisely so it would NOT carry writing meant for people.
 *
 * So this is a module's field used by a consumer for the one purpose its
 * owner ruled out, and `credits-view.tsx`'s own comment had gone stale
 * saying so ("the server's own free text, displayed verbatim").
 *
 * It is the same shape as the sessions list rendering raw User-Agent
 * strings, closed by 6d56d71: machine text in a list a person reads to
 * make a decision. And the token adds nothing here -- the row already
 * says "Simulation" in the reader's own language, so `smilesim:simulate`
 * is the same fact in machine form.
 *
 * THE COUNTER-CASE, WHICH IS REAL
 *
 * `@speed/account-ui` renders session `amr` values deliberately as
 * opaque references, untranslated, and that was accepted. Showing a
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
 * do not -- checked against all of them before this gate was committed.
 *
 * That check is not ceremony. Three assertions in this suite have failed
 * in exactly the opposite direction -- a threshold or pattern loose
 * enough to catch the bad case caught the good one too -- and the last
 * one would have told the round that implemented downloading that its
 * correct download was an error page.
 */
const BILLING_REASON_SHAPE = /\b[A-Za-z][A-Za-z0-9]*[:_][A-Za-z0-9][A-Za-z0-9:_-]*/

test(
  'the credits ledger shows no machine annotation to the person reading it',
  { tag: '@pending' },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await openSurface(page, CASE_UI.navCredits)

    // On the credits surface, asserted rather than assumed: a gate that
    // read "the ledger" while standing on another page is a false pass
    // this suite has already given once.
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
