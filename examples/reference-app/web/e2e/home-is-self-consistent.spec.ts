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
})

/**
 * That the empty home surface does not give an instruction nobody can
 * carry out.
 *
 * When no feature card's flag is enabled, this surface says "No features
 * are enabled yet" and "Ask an administrator to enable a feature for
 * this clinic". Both halves of that are trouble, and the second is the
 * defect:
 *
 *   - Nobody can act on it. home-view.tsx's own comment says so in as
 *     many words -- "with no mechanism in the app able to enable any" --
 *     so an administrator who received the request could not fulfil it
 *     either. The screen asks for something the product cannot do.
 *   - A self-registered practice has no administrator to ask. It
 *     registered itself; it IS the owner.
 *
 * And it is not a warning about a real limitation. The whole journey
 * runs from an account seeing this message: a case opened with a
 * photograph, the smile and shade chosen, a simulation generated and
 * shown beside the original, a patient link minted, ten credits
 * disclosed and deducted. Walked by hand, by clicking, from a
 * self-registered practice and confirmed on a seeded one. The features
 * these cards are keyed to (smilePreview, premiumUpsell) are demo flags
 * no tenant has; the product's actual capabilities do not come from
 * them. So the first thing the product says about itself is false.
 *
 * WHY IT SITS HERE AND NOT WITH THE NEW-PRACTICE GATES
 *
 * Because it is not about new practices, and I had it wrong when I first
 * reported it: I framed it as another wrong-population finding, since
 * this file's own gate signs in as a SEEDED account and I assumed the
 * seeds carried these flags. They do not. The seeded owner in Acme
 * Dental sees the identical message, which I found by signing in as one.
 * The gate above could have caught this all along -- it asks the
 * neighbouring question (does the surface promise cards it has none of)
 * and never questioned the empty state's own words.
 *
 * The empty state itself is a fix, not a regression: it replaced an
 * intro sentence pointing at blank space, which is the shape the gate
 * above refuses. What it needs is a sentence a reader can act on.
 *
 * WHAT IT DELIBERATELY DOES NOT PRESCRIBE
 *
 * Not the replacement. Listing what the practice can do, naming the
 * trial it was given, pointing at the surfaces in the navigation, or
 * saying nothing at all would each pass. What fails is instructing a
 * reader to ask a person who either does not exist or could not help.
 */
const UNACTIONABLE = 'Ask an administrator to enable a feature for this clinic.'

test('the empty home surface does not ask for something nobody can do', {
  tag: ['@pending', '@deployment'],
}, async ({ page }) => {
  await signInAs(page, DEMO_ACME_ONLY)

  const home = await readSettledText(page.getByRole('main'))
  expect(
    home,
    `the home surface tells the reader to ask an administrator to enable a feature, which no administrator can do (home-view.tsx: "no mechanism in the app able to enable any") and which a self-registered practice has nobody to ask for. Home said: ${home}`,
  ).not.toContain(UNACTIONABLE)
})
