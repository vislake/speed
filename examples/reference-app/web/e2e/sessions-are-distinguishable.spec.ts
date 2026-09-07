/**
 * That a person can tell their own sign-ins apart well enough to revoke
 * the one that is not theirs.
 *
 * The sessions list exists for exactly one job: someone suspects their
 * account is being used by another person, opens this list, finds the
 * session they do not recognise, and ends it. Today the list renders each
 * session's raw User-Agent string, so three sign-ins from the same
 * browser read as three identical walls of text -- truncated at the same
 * point, distinguishable only by a timestamp -- and every IP is the
 * platform proxy's rather than the visitor's. At the moment the feature
 * is needed, it tells its user nothing.
 *
 * This gate does not prescribe a design. It asserts the property that
 * makes the feature work at all: whatever the rows say, two different
 * sessions must not say the same thing, and what they say must be
 * something a person can read. A design that shows "Chrome · macOS · 2
 * hours ago" passes. So does one that shows a device nickname. A design
 * that shows 120 characters of User-Agent does not, because the person
 * holding the mouse still cannot answer "which of these is not me".
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import { APP_TEXT, openSurface, signInAs } from './test-utils/journeys.js'

/** account-ui's own en-US bundle. */
const SESSIONS_HEADING = 'Sessions'

/**
 * Substrings that only ever appear in a raw User-Agent string. Naming
 * them is more honest than measuring a row's length: the first version of
 * this gate compared character counts, and passed -- because a row also
 * carries an address and a timestamp, so the threshold that would have
 * caught a User-Agent also caught legitimate rows, and the threshold that
 * spared legitimate rows let the User-Agent through. A gate that passes
 * on the very defect it was written for is worse than no gate, so it
 * names the machine strings instead.
 */
const MACHINE_STRINGS = ['Mozilla/', 'AppleWebKit', 'Gecko', 'curl/', 'Safari/']

/**
 * account-ui's mark on the row for the session doing the asking. Its
 * presence is this suite's signal that the sessions query has actually
 * answered, rather than that the section has merely mounted.
 */
const CURRENT_SESSION_MARK = 'Current session'

// Closed: 6d56d71 summarizes each session's User-Agent into a
// browser/OS label and removes the raw string from the DOM. Verified
// here on all three engines with the fix in the tree, against the same
// gate that had been failing on every run before it.
//
// @budget rather than untagged: it signs demo-owner in twice, and the
// default tier already spends that account's whole five-per-minute
// allowance (see e2e/README.md's budget section), so in the fast tier
// those two attempts would be two paced waits rather than two sign-ins.
test(
  'the sessions list says something a person can tell apart',
  { tag: '@budget' },
  async ({ page }) => {
    // Two sign-ins from this same browser, so the list holds rows that a
    // raw User-Agent cannot distinguish -- the exact case the feature
    // fails at today.
    await signInAs(page, DEMO_OWNER)
    await page.reload()
    await signInAs(page, DEMO_OWNER)
    await openSurface(page, APP_TEXT.navAccount)

    await expect(page.getByRole('heading', { name: SESSIONS_HEADING })).toBeVisible()
    // The session rows have to have arrived, not merely the section that
    // will hold them.
    await expect(page.getByText(CURRENT_SESSION_MARK)).toBeVisible()

    // Scoped to the MAIN landmark, not the whole document.
    //
    // `document.querySelectorAll('li')` collected the frame's own
    // navigation too -- Home, Cases, Notes and Account are list items in
    // a list, ahead of main in the DOM -- and each of the three
    // assertions below was wrong because of it in a different way. The
    // count check could never fail, since four nav entries clear a
    // threshold of one on their own. The machine-string filter was the
    // only one that happened to survive, because a nav label contains no
    // User-Agent. And the "every row carries a time" check would have
    // flagged "Home" and "Account" as rows with nothing to tell them
    // apart -- a false accusation against the product, waiting to fire
    // the day the real defect above it is fixed and stops failing first.
    //
    // The same unscoped-locator mistake was found in this suite's
    // core-journey helper on the same day (getByRole('listitem').first()
    // clicking the nav's Home entry rather than the first case), which is
    // what prompted looking for its siblings.
    const descriptions = await page
      .getByRole('main')
      .getByRole('listitem')
      .allTextContents()
      .then((texts) =>
        texts.map((text) => text.replace(/\s+/g, ' ').trim()).filter((text) => text.length > 0),
      )
    expect(descriptions.length, 'the account surface listed no sessions').toBeGreaterThan(1)

    // Readable: no row hands a person a machine's own string. This is the
    // whole defect -- three sign-ins from one browser rendered as three
    // identical, truncated User-Agent walls, and the person who came here
    // to spot an intruder could not tell any of them apart.
    const machineRows = descriptions.filter((text) =>
      MACHINE_STRINGS.some((token) => text.includes(token)),
    )
    expect(
      machineRows,
      `these rows show a raw User-Agent instead of a device a person recognises: ${machineRows
        .map((t) => `"${t.slice(0, 70)}…"`)
        .join('; ')}`,
    ).toEqual([])

    // Distinguishable: every row carries a time, so two sessions from the
    // same browser can still be told apart. That is the bar this gate
    // holds -- "Chrome · macOS · 2 hours ago" against "Chrome · macOS ·
    // just now" is enough; two genuinely identical descriptions are not.
    //
    // The check is "does it name a moment", not "does it contain a
    // digit". The previous form required a digit AND a time word, which
    // rejected "Chrome on Windows · just now" -- a perfectly reasonable
    // design with no digit in it at all. This gate's own comment used
    // "just now" as an example of what should PASS, so it would have
    // failed the very shape it advertised.
    //
    // Found by testing the assertion against plausible fixed rows before
    // the round fixing the User-Agent rendering ran into it. That round
    // would have been told its correct work was wrong -- the same false
    // accusation the disclosure gate made an hour earlier, caught this
    // time by reading the assertion as though it had already passed.
    const NAMES_A_MOMENT = /\bago\b|just now|\d{1,2}:\d{2}|\d{4}|yesterday|today/i
    const timeless = descriptions.filter((text) => !NAMES_A_MOMENT.test(text))
    expect(
      timeless,
      `these rows carry nothing to tell them apart by: ${timeless.join('; ')}`,
    ).toEqual([])
  },
)

test(
  'a session names the address it was signed in from, not the proxy in front of it',
  // @deployment ONLY, and both halves of that are deliberate.
  //
  // @deployment is REQUIRED, not optional, and its absence meant this
  // gate had never run anywhere at all: the skip below excludes it
  // locally, and against a deployment the config narrows the run to
  // @deployment -- which it did not carry -- so `--grep @pending` there
  // answered "No tests found". Excluded at both ends, it looked like a
  // gate and was a comment. The rule that makes concrete: a gate that
  // skips itself unless E2E_BASE_URL is set MUST also be tagged
  // @deployment. Nothing enforces it automatically; e2e/README.md
  // states it.
  //
  // And no @pending, because the defect is closed: verified against the
  // real deployment on chromium and webkit after the trusted-proxy fix,
  // with this run's own session recorded at the visitor's public address
  // rather than the platform's 172.16.x. It needs one sign-in, which is
  // what the deployment tier is sized for, so it belongs in that tier
  // rather than behind a second command.
  //
  // Its sibling above is closed too now (6d56d71 summarizes the
  // User-Agent into a readable label), so this file's two findings --
  // the proxy address and the unreadable rows -- are both shut. It reads
  // as one gate per finding rather than one per surface, which is what
  // let them be reported, fixed and retired independently.
  { tag: '@deployment' },
  async ({ page }) => {
    // Only meaningful against a real deployment. Run locally, the visitor
    // genuinely IS on a private address (127.0.0.1), so a private address
    // there is the truth rather than the defect -- asserting otherwise
    // would make this gate permanently and wrongly red.
    test.skip(
      process.env.E2E_BASE_URL === undefined,
      'the proxy-address defect only exists behind a real deployment; locally a private address is correct',
    )

    await signInAs(page, DEMO_OWNER)
    await openSurface(page, APP_TEXT.navAccount)

    // Wait for the list to actually arrive before reading the page.
    //
    // Reading textContent straight after the navigation caught all three
    // account sections still saying "Loading sessions", and a page that
    // has not loaded contains no address for the same reason it contains
    // nothing else -- so the read had to be pushed past the queries or
    // its verdict would have been about latency, not about addresses.
    await expect(page.getByText(CURRENT_SESSION_MARK)).toBeVisible()

    // THIS sign-in's own row, not the whole list.
    //
    // The list is a history, and a history legitimately contains rows
    // recorded before any fix landed -- this deployment's older rows
    // still carry the proxy address they were written with, and always
    // will. Judging the whole list would make the gate permanently red
    // for a reason that is not a defect, and wiping the deployment to
    // clear it would be tuning the evidence to fit the assertion. The
    // property that actually has to hold is about the sign-in this run
    // just performed: the address recorded for it must be the visitor's.
    //
    // innerText, not textContent, and the difference decided a verdict
    // once already: textContent concatenates adjacent elements with
    // nothing between them, so the address arrived glued to the token
    // before it -- "password223.70.82.202" -- and a pattern anchored on
    // a word boundary cannot match a number beginning right after a
    // letter. The gate reported that no session showed any address while
    // three showed one plainly, which very nearly became a filed defect
    // against a fix that works. innerText is what the rendered layout
    // reads as, which is this suite's standard for visible anyway.
    const text = await page
      .getByRole('listitem')
      .filter({ hasText: CURRENT_SESSION_MARK })
      .first()
      .innerText()

    // BOTH halves, and the first one is why: "no private address is
    // shown" passes just as happily when NO address is shown at all, so
    // on its own it would go green for a session list that had stopped
    // reporting addresses entirely -- the same absent-evidence-reads-as-
    // success trap this file's MACHINE_STRINGS note already records for
    // its sibling gate. So the gate says an address must be there, and
    // then says which kind it must not be.
    const addresses =
      text.match(/\b\d{1,3}(?:\.\d{1,3}){3}\b|\b(?:[0-9a-f]{1,4}:){3,}[0-9a-f]{0,4}\b/gi) ?? []
    expect(
      addresses,
      // Quoting what it actually read, because the first version of this
      // message did not: "no session reports any address" sent the
      // investigation looking for a missing feature when the addresses
      // were on screen all along and this read was at fault.
      `this sign-in's own row reports no address at all, so nothing here can tell a person where it came from. The row read as: ${text.replace(/\s+/g, ' ').slice(0, 400)}`,
    ).not.toHaveLength(0)

    // The private ranges a platform's own proxy sits in. A row showing
    // one of these recorded the load balancer, not the visitor --
    // useless to a person checking their account and wrong in an audit
    // trail.
    const proxyAddresses = addresses.filter((address) =>
      /^(?:10|127|172\.(?:1[6-9]|2\d|3[01])|192\.168)\./.test(address),
    )
    expect(
      proxyAddresses,
      `this sign-in was recorded at a private proxy address (${proxyAddresses.join(', ')}), so the recorded address is the platform's, not the visitor's`,
    ).toEqual([])
  },
)
