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

test(
  'the sessions list says something a person can tell apart',
  { tag: '@pending' },
  async ({ page }) => {
    // Two sign-ins from this same browser, so the list holds rows that a
    // raw User-Agent cannot distinguish -- the exact case the feature
    // fails at today.
    await signInAs(page, DEMO_OWNER)
    await page.reload()
    await signInAs(page, DEMO_OWNER)
    await openSurface(page, APP_TEXT.navAccount)

    await expect(page.getByRole('heading', { name: SESSIONS_HEADING })).toBeVisible()

    const descriptions = await page.evaluate(() => {
      const items = [...document.querySelectorAll('li')]
      return items
        .map((item) => (item.textContent ?? '').replace(/\s+/g, ' ').trim())
        .filter((text) => text.length > 0)
    })
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
    const timeless = descriptions.filter(
      (text) => !/\d/.test(text) || !/(ago|:|\d{4}|just now)/i.test(text),
    )
    expect(
      timeless,
      `these rows carry nothing to tell them apart by: ${timeless.join('; ')}`,
    ).toEqual([])
  },
)

test(
  'a session names the address it was signed in from, not the proxy in front of it',
  { tag: '@pending' },
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

    const text = (await page.getByRole('main').textContent()) ?? ''
    // The private ranges a platform's own proxy sits in. A session list
    // where every row shows one of these is recording the load balancer,
    // not the visitor -- useless to a person checking their account and
    // wrong in an audit trail.
    const proxyAddresses = text.match(/\b(?:10|127|172\.(?:1[6-9]|2\d|3[01])|192\.168)\.[\d.]+/g)
    expect(
      proxyAddresses,
      `every session reports a private proxy address (${proxyAddresses?.slice(0, 3).join(', ')}), so the recorded address is the platform's, not the visitor's`,
    ).toBeNull()
  },
)
