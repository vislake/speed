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
  SIGN_IN_TEXT,
  expectSignedIn,
  openSurface,
  readCurrentTenant,
  signInAs,
  submitPasswordSignIn,
  visitSignIn,
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

// Closed by eeaabdff: a Team surface of its own in the frame's
// navigation (#/team, reachable through the drawer on a narrow
// viewport), NOT a section of the account page -- so the trap this
// gate's own note warns about was avoided rather than argued around.
//
// Verified on all three engines, and then by hand, because this gate's
// entire reason for existing is that a working mechanism is not the
// same as a dentist being able to reach it:
//   - "Team" is its own nav entry beside Cases, Notes, Credits and
//     Account;
//   - the surface says what it is for ("Who works in this clinic, and
//     who has been invited but has not joined yet");
//   - inviting dr.lin@clinic.example answered "The invitation was sent.
//     Your colleague can join once they open it." -- a sentence written
//     for a person, not a code;
//   - the invitation then listed as its own row: the address, Pending,
//     when it was sent, and when it expires. A reader can tell an
//     invited colleague from one who has joined, which is what this
//     gate asks, and can also see how long the invitation is good for,
//     which it does not ask and which is the better answer.
//
// @budget rather than untagged: it signs in.
test(
  'an owner can invite a colleague into the clinic by clicking',
  { tag: '@budget' },
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

/**
 * A raw user id in the members list, which only ever appears in one.
 *
 * The Team surface's own sentence says what it is for: "Who works in
 * this clinic, and who has been invited but has not joined yet." Opened
 * by hand as a person, the members table answers the first half with
 * two rows reading `04c09f52-f6b7-4555-ba8b-5fe23f57b8d9` and
 * `89b5819d-9066-421e-9788-779b0f83d8e5`, plus one reading "You".
 *
 * A dentist cannot tell which colleague is which, cannot decide whom to
 * remove, and cannot recognise a stranger -- which is the whole question
 * the surface exists to answer. It is the third instance of one shape
 * this suite has now found: machine text in a list a person reads to
 * make a decision (the sessions list's raw User-Agent, closed by
 * 6d56d71; the credits ledger's billing reason, closed by 48d54e9).
 *
 * WHY IT IS NOT AN IMPLEMENTATION OVERSIGHT
 *
 * The data is not there to render. `go/org`'s member object carries
 * `userId` and nothing else, and its own spec says why: "An id in
 * authn's users table, carried as an opaque string -- org stores no
 * other identity data about the user (root CLAUDE.md's module-boundary
 * rule)." So the name has to come from somewhere else -- an authn-side
 * lookup the app composes, or a seam org would have to grow -- and
 * that is a design decision rather than a missing line in a view.
 *
 * The invitation half needs nothing: an invited row already shows the
 * ADDRESS, because org holds the address it was asked to invite. It is
 * only a member, once joined, who becomes an id.
 *
 * WHY THE GATE ABOVE PASSES ANYWAY
 *
 * It asks whether an owner can invite by clicking, and that works. This
 * is a different question about the same surface, so it is a different
 * gate -- the one-gate-per-finding rule this suite keeps, and the reason
 * the sessions and credits findings could each be reported, fixed and
 * retired on their own.
 *
 * WHAT IT ASSERTS, AND WHAT IT LEAVES OPEN
 *
 * Only that no member row identifies a person by a raw id. A display
 * name, an email address, "You" for oneself, an invited-by line, even a
 * short stable label would each pass. What fails is a UUID where a
 * colleague's identity belongs.
 */
const RAW_USER_ID =
  /\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/i

// Closed by 42a14614, and by the route this gate hoped for rather than
// the cheap one: the app composes an authn lookup of its own
// (GET /api/reference-app/team-members) and enriches every row from the
// users table, so the naming holds for EVERY member -- team_members.go
// says so in as many words, and says it never knows which members the
// boot seeded and which joined through an invitation. There is no
// demo-layer-only leg, which is why this gate did not need widening to
// the self-registered population after all.
//
// The naming ladder is You -> display name -> email -> a bilingual
// "unknown member" fallback, and a raw id is never rendered as a name.
//
// Verified on three engines, then by hand: the roster reads
// demo-acme-only@example.com / You / demo-reader@example.com. A dentist
// can tell which colleague is which and decide whom to remove, which is
// the question the surface exists to answer and the one the UUIDs left
// unanswered.
//
// @budget rather than untagged: it signs in.
test(
  'the members list names people, not user ids',
  { tag: '@budget' },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await expectSignedIn(page)
    await reachThePractisesPeople(page)

    // The members table specifically, not the whole surface: an
    // invitation row legitimately carries an address, and a pending
    // invitation's own id is nobody's identity.
    const members = page.getByRole('main').getByRole('table').first()
    await expect(members, 'the team surface shows no members table').toBeVisible({
      timeout: 30_000,
    })

    const shown = await members.innerText()
    const ids = shown.match(new RegExp(RAW_USER_ID, 'gi')) ?? []
    expect(
      ids,
      `the members list identifies people by raw user id (${ids.join(' , ')}) on the surface whose own sentence promises to say who works in this clinic -- a reader cannot tell which colleague is which, whom to remove, or whether a row is a stranger. go/org carries only userId by module-boundary rule, so the name has to come from an authn-side lookup the app composes: a product decision, not a missing line in the view.`,
    ).toEqual([])
  },
)

/** A password that satisfies authn's real policy (12 characters minimum). */
const SIGNUP_PASSWORD = 'e2e-invite-population-2026'

/**
 * That a practice which signed ITSELF up can invite a colleague too.
 *
 * The gate above asks the same question of a SEEDED demo account, and
 * passes. This one asks it of the population that actually buys the
 * product, and it is here because that difference was a real defect:
 * a self-registered clinic pressing "Send invitation" was answered
 * "Sending the invitation failed. Try again later." while the identical
 * click in a boot-configured demo tenant succeeded.
 *
 * Narrowed by hand before it was reported, because a symptom is not a
 * finding: the browser's own span showed POST /api/v1/org/invitations
 * answering 500 for the self-registered tenant and 200 for tenant-acme;
 * called directly it answered `org.node_not_found` with an EMPTY
 * node_id; the surface was then cleared of suspicion (team-view.tsx
 * picks the root by `depth === 0` and sends its id, which is correct);
 * and with the correct node id supplied the answer became
 * `org.internal_error` -- a server-side failure, not a UI one. The
 * control experiment is what made it a finding rather than a guess: the
 * same call, same shape, new address, against the seeded tenant, minted
 * a pending invitation.
 *
 * WHY THE GATE ABOVE COULD NOT HAVE CAUGHT IT
 *
 * Population. It signs in as a seeded account, and this failed only for
 * a self-registered one -- the fourth time this suite has met that
 * shape (the clinic-naming gate asked only about boot-configured
 * clinics; the four core-journey gates only about seeded accounts; the
 * periodic scheduler's tenant universe was the configured list alone,
 * fixed in 5873f64). Every time the question was right and the
 * population was not.
 *
 * WHAT IT COSTS, AND WHY IT IS WORTH IT
 *
 * One registration, against `limitRegisterByIP`'s ten per hour -- which
 * is why it is @pending-and-then-@budget rather than in the default
 * tier, and why the suite asks for one engine per invocation
 * (e2e/README.md's budget table). A gate that spends a real
 * registration to cover the paying population is the trade this suite
 * has made three times before and not regretted.
 */
// Closed by d3b4c184, and the root cause is worth keeping because it
// corrects something this suite got wrong.
//
// The invitation mail's accept-link builder had exactly one source of
// hosts: hostByTenant, the reverse index of cfg.HostTenants. A
// self-registered clinic's tenant is derived from its registrant's user
// id, so it can never be in that map -- the link could not be built and
// invite.go's own ErrInternal came back. The fix is a deployment-level
// public origin (APP_PUBLIC_ORIGIN, defaulting to localhost:PORT), with
// the demo tenants' https://host links unchanged byte for byte.
//
// THE CORRECTION: when this suite swept cfg.HostTenants' uses after
// 5873f64 (the periodic scheduler's tenant universe, the same shape),
// it cleared hostByTenant as "correct by construction -- a Host to
// tenant map IS the definition of the configured hosts". That was
// right about resolving an INCOMING request's host and wrong about this
// direction: asked "which host belongs to this tenant", every tenant
// needs an answer, not only the configured ones. Two directions, one
// map, and only one of them is definitionally configured-only. So the
// sweep that reported "the wrong-population error is in exactly one
// place" had cleared the source of the next one.
//
// @budget rather than untagged: it registers, which the default tier's
// ten-per-hour register budget cannot absorb.
test(
  'a self-registered practice can invite a colleague too',
  { tag: '@budget' },
  async ({ page }) => {
    const email = `e2e-invite-pop-${Date.now()}@example.com`

    await visitSignIn(page)
    await page.getByRole('button', { name: SIGN_IN_TEXT.registerAction }).click()
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.identifierLabel }).fill(email)
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.passwordLabel }).fill(SIGNUP_PASSWORD)
    await page.getByRole('textbox', { name: SIGN_IN_TEXT.displayNameLabel }).fill('Northgate Dental')
    await page.getByRole('button', { name: APP_TEXT.registerSubmit }).click()
    await expect(page.getByRole('status')).toContainText(APP_TEXT.registerSuccess)

    await page.getByRole('button', { name: APP_TEXT.registerBackToSignIn }).click()
    await submitPasswordSignIn(page, email, SIGNUP_PASSWORD)
    await expectSignedIn(page)

    await reachThePractisesPeople(page)

    const colleague = `e2e-invited-${Date.now()}@example.com`
    await page.getByRole('button', { name: TEAM_UI.invite }).first().click()
    await page.getByRole('textbox', { name: TEAM_UI.address }).fill(colleague)
    await page.getByRole('button', { name: TEAM_UI.send }).last().click()

    // The invitation actually happened. Asserted on the address in the
    // roster rather than on a success banner, and NOT on the absence of
    // an error: a refusal banner and a forgotten invitation look the
    // same from the outside, and only one of them is what this gate is
    // about.
    await expect(
      page.getByText(colleague),
      `a practice that registered itself pressed Send invitation and ${colleague} never appeared as invited -- the same click succeeds in a boot-configured demo tenant, so the product works for the population that was seeded and not for the one that signs up`,
    ).toBeVisible({ timeout: 30_000 })

    await expect(
      page.getByRole('main'),
      'the invitation is listed but nothing says it is still outstanding',
    ).toContainText(OUTSTANDING)
  },
)
