/**
 * That a practice can add a colleague without an engineer.
 *
 * The invitation machinery -- go/org mints the token, hashes it,
 * encrypts and blind-indexes the address, rate-limits the send on two
 * dimensions and renders the mail in the recipient's locale -- is proven
 * end to end by org-invitation-sign-in.spec.ts. This spec asks the
 * question that spec cannot: can a dentist reach the surface and act on
 * it? Every assertion goes through the browser; nothing is read from the
 * server's output, and no request here can make the gate pass by API
 * call.
 *
 * A dental practice is not one person: the product's premise is a
 * multi-level organization -- a receptionist opens cases, a dentist runs
 * the simulations, an owner pays for them. So the question is asked of
 * both populations the product serves: an owner of a boot-seeded demo
 * clinic, and a practice that registered itself, whose tenant derives
 * from its registrant rather than from host configuration. Three tests
 * cover it: the invite goes out and stays visible as outstanding, the
 * members list names people rather than raw user ids, and the
 * self-registered practice can invite too. The last question spends a
 * registration, which is why these tests carry the @budget tag.
 *
 * The spec deliberately does not prescribe where the people-management
 * surface lives (a nav entry, the account page, a clinic settings area),
 * what it is called, whether the invitee picks a role at invite time, or
 * how the pending state is shown; each test reaches the surface through
 * the nav first and, failing that, through the account page, so an
 * implementation is never told where the feature must live. Acceptance
 * stops before the invitee accepts -- org-invitation-sign-in.spec.ts
 * covers that half.
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
 * Deliberately NOT matching "Account": that surface is about the
 * signed-in person's own sessions, bindings and MFA -- not about who
 * else is in the clinic. A pattern loose enough to reach a team surface
 * that might live there is loose enough to land on the personal one and
 * pass.
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
 * the spec permits.
 *
 * The header above promises not to prescribe where the surface lives --
 * "a nav entry, the account page, a clinic settings area" -- so this
 * helper must not accept only a nav entry: an implementation that put
 * member management on the account page, exactly as permitted, would
 * otherwise be told the practice has nowhere to manage its people. A
 * reachability gate asserts that a person can reach the capability, not
 * that it landed in a particular place.
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

// The team surface is "Team", its own nav entry beside Cases, Notes,
// Credits and Account, and lists an invitation as its own row: the
// address, the pending state, when it was sent and when it expires.
//
// @budget rather than untagged: it signs in.
test(
  'an owner can invite a colleague into the clinic by clicking',
  { tag: '@budget' },
  async ({ page }) => {
    await signInAs(page, DEMO_OWNER)
    await expectSignedIn(page)

    // Which clinic this is about, read from the frame rather than
    // assumed: the clinic an account lands in is the first row of its
    // own membership enumeration, a fact about the account and the
    // seeding rather than a contract a gate should name.
    const clinic = await readCurrentTenant(page)

    // REACHABLE, asserted first and on its own. A failure here says the
    // practice has nowhere to manage its people, which is a different
    // failure from an invite form that does not work -- and the second
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
 * this clinic, and who has been invited but has not joined yet." The
 * members table answers the first half with rows that are the app's own
 * render of go/org members. A dentist who saw raw ids there could not
 * tell which colleague is which, could not decide whom to remove, and
 * could not recognise a stranger -- the whole question the surface
 * exists to answer.
 *
 * WHY A NAME IS NOT SIMPLY MISSING FROM THE VIEW
 *
 * The data is not there to render. go/org's member object carries a
 * userId and no other identity data (a module-boundary rule: org stores
 * no authn identity), so the name has to come from an authn-side lookup
 * the app composes -- a design decision, not a missing line in a view.
 * The invitation half needs nothing: an invited row already shows the
 * address, because org holds the address it was asked to invite. It is
 * only a member, once joined, who becomes an id.
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

// The app composes an authn lookup of its own
// (GET /api/reference-app/team-members) and enriches every member row
// from the users table; the naming ladder is You -> display name ->
// email -> a bilingual "unknown member" fallback, and a raw id is never
// rendered as a name.
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
const SIGNUP_PASSWORD = 'e2e-invite-pass-2026'

/**
 * That a practice which signed ITSELF up can invite a colleague too.
 *
 * The tests above ask the same questions of boot-seeded demo accounts.
 * This one covers the population that actually buys the product: a
 * self-registered clinic's tenant is derived from its registrant's user
 * id and can never appear in the configured host list, so invitation
 * delivery takes a different origin than a boot-configured demo
 * tenant's. Seeded-account coverage alone cannot see a failure confined
 * to that path.
 *
 * Registration is rate-limited per IP (go/authn's limitRegisterByIP,
 * ten per hour), and this test spends one real registration per run --
 * which is why it carries the @budget tag, and why the suite asks for
 * one engine per invocation (e2e/README.md's budget table).
 */
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
