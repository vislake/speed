/**
 * The journey that brings a colleague into a practice: someone registers,
 * gets invited, accepts, and can then sign in to the organization they
 * were invited to.
 *
 * Two properties carry the journey end to end. Sign-in reads membership
 * from org's real rows, so a genuinely invited person is answered by
 * the membership the acceptance wrote -- nothing depends on an
 * in-process shadow of boot-time seeding. And accepting resolves the
 * tenant from the invitation token itself, which is the only
 * tenant-bearing credential an invitee can have: the acceptance is what
 * creates the membership a tenant-scoped access token would need, so
 * the tenant has to travel in the invitation.
 *
 * The spec walks the whole loop and ends where it matters: the invitee
 * signs in through the browser, with no tenant named, and the frame
 * offers the organization it was invited to. The setup steps -- the
 * invitee's registration, the invitation itself, reading the acceptance
 * token -- go through the API, because they are harness plumbing rather
 * than the journey under test and an invitation token is never in an
 * API response (see test-utils/invitations.ts); the two steps that
 * decide whether the product works, where the invitee lands before
 * acceptance and the sign-in into the invited organization after, are
 * both driven through the browser. Before acceptance the invitee's
 * registration has already provisioned a clinic of its own
 * (self-service signup, internal/app/self_service.go), so the account can
 * sign in from the moment it exists -- what it cannot do yet is enter
 * the ORGANIZATION that invited it, and the pre-acceptance leg asserts
 * exactly that.
 */
import { expect, test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import {
  acceptInvitationAsFreshUser,
  inviteAndReadToken,
  registerThroughApi,
  rootNodeId,
  signInThroughApi,
} from './test-utils/invitations.js'
import {
  TENANT_NAMES,
  expectOutsideDemoOrganizations,
  expectSignedIn,
  readTenantOptions,
  submitPasswordSignIn,
  visitSignIn,
} from './test-utils/journeys.js'

/** A password that satisfies authn's real policy (12 characters minimum). */
const INVITEE_PASSWORD = 'e2e-invitee-password-2026'

test('an invited colleague accepts and can then sign in to that organization', async ({
  page,
  request,
}) => {
  const invitee = `e2e-invitee-${Date.now()}@example.com`

  // The invitee registers, the way a person following an invitation link
  // does. Registration provisions the registrant a clinic of its own
  // (self-service signup, internal/app/self_service.go), so the account
  // can sign in from the moment it exists -- what it cannot do yet is
  // enter the ORGANIZATION that invited it. This pre-acceptance leg
  // asserts exactly that: the browser-shaped sign-in lands in the
  // invitee's own clinic, never inside one of the demo organizations the
  // invitation below will open -- if it ever did land there, the
  // acceptance would prove nothing.
  const inviteeUserId = await registerThroughApi(request, invitee, INVITEE_PASSWORD)
  await visitSignIn(page)
  await submitPasswordSignIn(page, invitee, INVITEE_PASSWORD)
  await expectSignedIn(page)
  // The invitee's own clinic is not one of the demo organizations the
  // helper knows by name -- readCurrentTenant's failure to find one IS
  // the assertion's first half (the clinic's switcher names its raw
  // tenant id, never a demo practice's name).
  // Both halves, in order: a tenant is named at all, and it is not one
  // of the demo organizations. An assertion of only the second would
  // pass whenever the read THREW -- an unloaded frame, an unrendered
  // switcher, a bug in the helper -- so it could not tell "landed in
  // its own clinic" from "shows no tenant".
  await expectOutsideDemoOrganizations(page)

  // The acceptance steps below run through the API while the browser is
  // signed in as the invitee; a reload returns the page to the anonymous
  // sign-in surface for the closing leg (the session is memory-only, so
  // a reload starts anonymous by the app's own design).
  await page.reload()

  // An owner invites the address into their own tenant's root node.
  const owner = await signInThroughApi(request, DEMO_OWNER.email, DEMO_OWNER.password)
  const node = await rootNodeId(request, owner)
  const token = await inviteAndReadToken(request, owner, invitee, node)

  // The invitee accepts holding no tenant-scoped credential at all: the
  // tenant comes from the invitation token, not from an access token the
  // invitee could not have had before the acceptance created the
  // membership.
  await acceptInvitationAsFreshUser(request, inviteeUserId, token)

  // The step that decides whether the product works: the invitee signs in
  // through the browser and can now work in the organization that
  // invited them.
  //
  // Asserted on the clinics the switcher OFFERS, not on the one the
  // frame opened: self-service registration gives every registrant a
  // clinic of their own, so an invitee holds at least two tenants and
  // the frame opens on the first of the membership answer's rows
  // (ordered by tenant id) rather than on a clinic this assertion
  // chooses. The invitation working is the product fact; the landing
  // tenant is ordering.
  await visitSignIn(page)
  await submitPasswordSignIn(page, invitee, INVITEE_PASSWORD)
  await expectSignedIn(page)
  const clinics = await readTenantOptions(page)
  expect(
    clinics.filter((clinic) => (TENANT_NAMES as readonly string[]).includes(clinic)),
    `after accepting, the invitee cannot work in the organization that invited them -- their clinics are ${clinics.join(' , ')}`,
  ).not.toHaveLength(0)
})
