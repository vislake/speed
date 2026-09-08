/**
 * That the first screen after signing in never promises something it is
 * not showing.
 *
 * The flags that gate the home cards default to off, and no surface in
 * the app can turn one on, so the promise and the content must stay in
 * lockstep: the intro's sentence about the cards renders only beside
 * the card list it describes. A sentence pointing at blank space reads
 * as a page that is broken rather than a page that is honestly empty.
 *
 * The gate is written as a property rather than as a screenshot:
 * whatever the page says about cards, there must be cards; and when
 * there are none, the page must say so in its own words instead of
 * leaving the promise hanging. Both halves matter -- the first stops
 * the promise from outliving the content, the second stops "fix" from
 * meaning "delete the sentence and leave a void".
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

// The gate passes: the intro renders only beside the cards it
// describes, and a surface with no enabled feature renders its own
// empty state. The PROMISE_OF_CARDS probe above stays to catch a
// reintroduction, exactly as written.
//
// It carries NO budget tag: its one sign-in is on the single-tenant
// demo account, which the rest of the suite barely touches, so it fits
// the default run where two owner sign-ins would not. That is the
// whole of the difference between this gate and the ones behind
// `pnpm test:e2e:budget` -- which demo account they need, not how much
// anyone trusts them.
/**
 * The two sentences that cannot both be true, kept as the pair that must
 * not come back together: the intro renders only beside the cards it
 * describes, so the two never share a screen.
 */
const CONTRADICTION = {
  dependsOnFlags: 'depends on the features this clinic has enabled',
  nothingToEnable: 'no feature to enable',
} as const

/** A sentence telling the reader to ask an administrator, kept as the thing that must not return. */
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
  // as two tests they would cost the default tier a second sign-in on
  // this account, near its per-minute login allowance. Each still fails
  // with its own message.
  //
  // When no card's flag is enabled -- every tenant, seeded or
  // self-registered -- no administrator exists who could act on a
  // request to enable a feature (no mechanism in the app can enable
  // one, and a self-registered practice has nobody to ask), so the
  // surface must not send a reader to one. The empty-state copy names
  // where the work is instead ("Cases for patients and smile
  // simulations, Credits for the balance, Notes for the team") and says
  // the missing part outright: "with no feature to enable and no one to
  // ask."
  //
  // What the gate deliberately does not prescribe: the replacement
  // wording. Listing the capabilities, naming the trial, pointing at
  // the navigation, or saying nothing would each pass. What fails is
  // instructing a reader to ask a person who either does not exist or
  // could not help.
  const home = await readSettledText(main)
  expect(
    home,
    `the home surface tells the reader to ask an administrator to enable a feature, which no administrator can do (home-view.tsx: "no mechanism in the app able to enable any") and which a self-registered practice has nobody to ask for. Home said: ${home}`,
  ).not.toContain(UNACTIONABLE)

  // AND IT DOES NOT ANSWER ITS OWN SENTENCE.
  //
  // home.intro says what is here depends on the features this clinic has
  // enabled; the empty state says there is no feature to enable. A
  // no-flags home must not render both claims one under the other --
  // the first and third sentences a reader met would contradict each
  // other, and the empty state's claim is the true one. The intro
  // renders only beside the card list its words describe, so a no-flags
  // home shows the honest empty state and no intro at all.
  //
  // Folded in here rather than left a separate test: as its own test it
  // would need a second sign-in as this same account, near its
  // per-minute login allowance. It asserts only that the two claims do
  // not CO-OCCUR -- rewriting the intro, dropping it when the cards are
  // empty, or removing the claim from the empty state would each pass,
  // the last being the one this would rather not see taken since that
  // clause is the part a reader can act on.
  const saysDependsOnFlags = home.includes(CONTRADICTION.dependsOnFlags)
  const saysNothingToEnable = home.includes(CONTRADICTION.nothingToEnable)
  expect(
    saysDependsOnFlags && saysNothingToEnable,
    `the home surface says both "${CONTRADICTION.dependsOnFlags}" and "${CONTRADICTION.nothingToEnable}", one under the other, so it contradicts itself in the space of two sentences. Home said: ${home}`,
  ).toBe(false)
})
