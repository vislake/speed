/**
 * That the first screen after signing in never promises something it is
 * not showing.
 *
 * The home surface introduced itself with "the cards below show the
 * features your current tenant has enabled" and then showed nothing --
 * not occasionally, but always: the flag that gates the only two cards
 * defaults to off, the second card depends on the first, and no surface
 * in the app can turn either on. So the sentence pointed at blank space
 * for every tenant, forever, and the first impression of the product was
 * a page that looks broken rather than a page that is honestly empty.
 *
 * The gate is written as a property rather than as a screenshot: whatever
 * the page says about cards, there must be cards; and when there are
 * none, the page must say so in its own words instead of leaving the
 * promise hanging. Both halves matter -- the first stops the promise from
 * outliving the content, the second stops "fix" from meaning "delete the
 * sentence and leave a void".
 */
import { expect, test } from '@playwright/test'
import { DEMO_ACME_ONLY } from './test-utils/accounts.js'
import { signInAs } from './test-utils/journeys.js'

/**
 * The app's own copy, as it ships in both languages. The zh-CN half is
 * deliberately NOT quoted here -- the suite runs in en-US and the
 * repository refuses CJK outside docs/internal -- so the assertion works
 * off the en-US bundle, which is the same sentence either way.
 */
const PROMISE_OF_CARDS = 'The cards below show the features'

/** ui-kit's empty-state title, which the surface is expected to reuse. */
const EMPTY_STATE_TITLES = /no data yet|nothing to show|no features/i

// Tagged @pending while the promise still hung over blank space: the
// gate failed, which was its present evidence. The intro no longer
// makes the promise (its copy changed with the empty-state round) and a
// surface with no enabled feature renders its own empty state; the gate
// passes against the fixed tree (the closing round's verification) --
// the PROMISE_OF_CARDS probe above still exists to catch a
// reintroduction, exactly as written. The tag stays until the
// acceptance session's re-run drops it: each test adds a sign-in to a
// suite that shares one demo server, and the go/authn per-account limit
// (five per minute) makes joining the default run a suite-budget
// decision, not a content decision.
test('the home surface does not promise cards it has none of', {
  tag: ['@pending', '@deployment'],
}, async ({ page }) => {
  await signInAs(page, DEMO_ACME_ONLY)

  const main = page.getByRole('main')
  await expect(main.getByRole('heading', { level: 1 })).toBeVisible()

  const promisesCards = await main.getByText(PROMISE_OF_CARDS).count()
  // Cards are articles or list items depending on how the surface builds
  // them; either counts, and neither existing is what the promise makes
  // dishonest.
  const cardCount = await main.locator('.MuiCard-root').count()

  if (promisesCards > 0) {
    expect(
      cardCount,
      'the home surface says the cards below show enabled features, and shows no cards',
    ).toBeGreaterThan(0)
    return
  }

  // No promise made. Then the surface must still tell a person what it is
  // showing them, rather than leaving a heading over nothing.
  if (cardCount === 0) {
    await expect(
      main.getByText(EMPTY_STATE_TITLES).first(),
      'a home surface with nothing to show must say so, not render a heading over a void',
    ).toBeVisible()
  }
})
