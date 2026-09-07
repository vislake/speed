/**
 * That a practice can add a colleague without an engineer.
 *
 * This is the gap that green results hide best, and the reason is worth
 * stating plainly: the invitation machinery WORKS. go/org mints the
 * token, hashes it, encrypts and blind-indexes the address, rate-limits
 * the send on two dimensions and renders the mail in the recipient's
 * locale; org-invitation-sign-in.spec.ts proves the whole cycle end to
 * end and passes. What none of that proves is that a dentist can do it.
 * That spec drives the API directly -- it has to, because there is no
 * surface -- so its green says the backend is sound and says nothing
 * about whether the product is usable.
 *
 * A dental practice is not one person. The brief's whole premise is a
 * multi-level organization: a receptionist opens cases, a dentist runs
 * the simulations, an owner pays for them. A product that can only ever
 * hold the person who registered it is a single-user tool wearing a
 * multi-tenant backend, and the practice's second employee is where that
 * stops being a design opinion and becomes the thing blocking the sale.
 *
 * WHY THIS GATE EXISTS SEPARATELY FROM org-invitation-sign-in
 *
 * Same feature, two different questions, and this suite has learned to
 * keep them apart: "does the mechanism work" and "can a person reach
 * it". The invitation spec answers the first through the API. This one
 * answers the second through the browser only -- no request it makes
 * itself, nothing read from the server's output. If it can be made to
 * pass by an API call, it is not asking its question.
 *
 * WHAT IT ASSERTS, AND WHAT IT LEAVES OPEN
 *
 * That an owner signed into a clinic can reach a surface listing who is
 * in that clinic, invite an address from it, and see that the invitation
 * happened. It does not prescribe where that surface lives (a nav entry,
 * the account page, a clinic settings area), what it is called, whether
 * the invitee picks a role at invite time, or how the pending state is
 * shown. It deliberately stops before the invitee accepts: that half is
 * already proven, and requiring it here would spend a registration
 * against a ten-per-hour budget to re-prove something org's own gate
 * covers.
 *
 * @pending in the tag's second sense -- the surface does not exist yet
 * rather than being broken. The app's own en-US bundle carries no invite
 * or member-management copy at all, and the frame's navigation offers
 * Home, Cases, Notes, Account and Credits. The gate is written ahead of
 * the work, the way the core-journey blocks were, so the acceptance
 * criterion exists before the round rather than being argued after it.
 *
 * Tagged @pending ALONE, not also @budget: @budget is this suite's word
 * for "verified passing", and a red gate in that tier would cost it the
 * one thing it is for. The sign-ins this gate spends are covered by the
 * @pending tier being its own invocation against its own freshly booted
 * server, which is the same mechanism that makes @budget a real tier.
 */
import { expect, test, type Page } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import {
  APP_TEXT,
  expectSignedIn,
  openSurface,
  readCurrentTenant,
  signInAs,
} from './test-utils/journeys.js'

/**
 * Where the practice's people are managed. Expectations of accessible
 * names, not decrees.
 *
 * Deliberately NOT matching "Account": that surface exists today and is
 * about the signed-in person's own sessions, bindings and MFA -- not
 * about who else is in the clinic. A pattern loose enough to reach a
 * team surface that might live there is loose enough to land on the
 * personal one and pass, which is the exact mistake block D's nav
 * pattern made once already.
 */
const TEAM_UI = {
  /** The surface listing who is in this clinic. */
  surface: /team|members|colleagues|staff|people|clinic settings/i,
  /** The control that starts an invitation. */
  invite: /invite|add (a )?(member|colleague|user|person)/i,
  /** The field taking the colleague's address. */
  address: /email|address/i,
  /** The control that sends it. */
  send: /send|invite|add|confirm/i,
} as const

/** How the surface says an invitation is out and not yet accepted. */
const OUTSTANDING = /pending|invited|awaiting|not yet accepted|sent/i

/**
 * Gets to wherever the practice's people are managed, trying both places
 * this gate said it would allow.
 *
 * The first version of this called openSurface and nothing else, which
 * made the code contradict the header directly above it: the header
 * promises not to prescribe where the surface lives -- "a nav entry, the
 * account page, a clinic settings area" -- while openSurface accepts
 * only a nav entry. An implementation that put member management on the
 * account page, exactly as permitted, would have been told the practice
 * has nowhere to manage its people.
 *
 * That is the same mistake the org-invitation gate made once already,
 * asserting a landing rather than reachability, and it is worth catching
 * before the round that will be judged by it rather than after: a gate
 * whose comment and whose code disagree will be believed on the comment
 * and enforced on the code.
 */
async function reachThePractisesPeople(page: Page): Promise<void> {
  // A surface of its own, reached the way any surface is -- which also
  // handles the narrow viewport, where the entry sits in the drawer.
  try {
    await openSurface(page, TEAM_UI.surface)
    return
  } catch {
    // Allowed: it may not be a surface of its own at all.
  }

  // Or a section of the account surface, the other place it could
  // honestly live. The invite affordance rather than a heading is what
  // is looked for, because a section's title is a design choice and the
  // ability to invite is the capability.
  await openSurface(page, APP_TEXT.navAccount)
  await expect(
    page.getByRole('button', { name: TEAM_UI.invite }).first(),
    'a practice has nowhere to manage its people: no navigation entry for a team, members or clinic-settings surface, and no way to invite anyone from the account surface either',
  ).toBeVisible({ timeout: 15_000 })
}

test(
  'an owner can invite a colleague into the clinic by clicking',
  { tag: '@pending' },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await expectSignedIn(page)

    // Which clinic this is about, read from the frame rather than
    // assumed: an account's landing tenant comes from a Go map's
    // iteration order and is randomized per boot (journeys.ts's
    // documented finding), so a gate that named a clinic would be
    // asserting a coin toss.
    const clinic = await readCurrentTenant(page)

    // REACHABLE, asserted first and on its own. A failure here says the
    // practice has nowhere to manage its people, which is a different
    // finding from an invite form that does not work -- and the second
    // message would be wrong about the first.
    await reachThePractisesPeople(page)

    const colleague = `e2e-colleague-${Date.now()}@example.com`
    await page.getByRole('button', { name: TEAM_UI.invite }).first().click()
    await page.getByRole('textbox', { name: TEAM_UI.address }).fill(colleague)
    await page.getByRole('button', { name: TEAM_UI.send }).last().click()

    // The practice is told it happened. Asserted on the address rather
    // than on a success banner alone: a banner that says "invitation
    // sent" while the roster forgets the invitation leaves an owner
    // re-inviting the same colleague and wondering why nobody arrives.
    await expect(
      page.getByText(colleague),
      `the invitation to ${colleague} is not shown anywhere in ${clinic} after being sent, so an owner has no way to know whether it went out`,
    ).toBeVisible({ timeout: 30_000 })

    await expect(
      page.getByRole('main'),
      'the invitation is listed but nothing says it is still outstanding, so an owner cannot tell an invited colleague from one who has joined',
    ).toContainText(OUTSTANDING)
  },
)
