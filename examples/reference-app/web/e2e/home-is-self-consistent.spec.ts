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
import { readSettledText, signInAs } from './test-utils/journeys.js'

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
// passes against the fixed tree (the closing round's verification), and
// the acceptance session re-ran it against a freshly deployed tree on
// all three engines and confirmed it -- the PROMISE_OF_CARDS probe above
// still exists to catch a reintroduction, exactly as written.
//
// It carries NO budget tag, unlike its three sibling gates from the same
// round: its one sign-in is on the single-tenant demo account, which the
// rest of the suite barely touches, so it fits the default run where two
// owner sign-ins would not. That is the whole of the difference between
// this gate and the ones behind `pnpm test:e2e:budget` -- which demo
// account they need, not how much anyone trusts them.
/** The sentence 2c383a0 removed, kept as the thing that must not return. */
const UNACTIONABLE = 'Ask an administrator to enable a feature for this clinic.'

test('the home surface does not promise cards it has none of', {
  tag: '@deployment',
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

  // AND NOTHING ON IT ASKS FOR SOMETHING NOBODY CAN DO.
  //
  // Two properties of one screen, asserted in one test on purpose: they
  // need the same account, the same sign-in and the same page load, and
  // as two tests they cost the default tier a second sign-in that
  // pushed demo-acme-only past its five-per-minute allowance -- turning
  // a twenty-second tier into a seventy-second one for nothing. Each
  // still fails with its own message, which is what "one gate per
  // finding" is actually protecting.
  //
  // The finding: when no card's flag is enabled -- which is every tenant
  // today, seeded or self-registered -- this surface used to say "Ask an
  // administrator to enable a feature for this clinic". Nobody could act
  // on it: home-view.tsx's own comment said "no mechanism in the app
  // able to enable any", so an administrator who received the request
  // could not fulfil it either, and a self-registered practice had no
  // administrator to ask. Meanwhile the whole journey runs from an
  // account seeing that message.
  //
  // Found by walking the journey by hand, and it carries a correction:
  // I first reported it as a wrong-population finding, assuming the
  // seeded account carried those flags. It does not -- the seeded owner
  // saw the identical message -- so the gate above could have caught it
  // all along and simply never questioned the empty state's own words.
  //
  // Closed by 2c383a0, by rewriting the sentence rather than deleting
  // the panel, which is what this asked for: the copy now names where
  // the work is ("Cases for patients and smile simulations, Credits for
  // the balance, Notes for the team") and says the missing part outright
  // -- "with no feature to enable and no one to ask." Verified on all
  // three engines.
  //
  // What it deliberately does not prescribe: the replacement wording.
  // Listing the capabilities, naming the trial, pointing at the
  // navigation, or saying nothing would each pass. What fails is
  // instructing a reader to ask a person who either does not exist or
  // could not help.
  const home = await readSettledText(main)
  expect(
    home,
    `the home surface tells the reader to ask an administrator to enable a feature, which no administrator can do (home-view.tsx: "no mechanism in the app able to enable any") and which a self-registered practice has nobody to ask for. Home said: ${home}`,
  ).not.toContain(UNACTIONABLE)
})
