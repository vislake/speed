/**
 * The journey that brings a colleague into a practice: someone registers,
 * gets invited, accepts, and can then sign in to the organization they
 * were invited to.
 *
 * This is the regression gate for the two defects that made the journey a
 * dead end, both found by driving the deployed app rather than by reading
 * it:
 *
 *  1. Sign-in read membership from an in-process shadow map that only
 *     boot-time demo seeding ever wrote, while real acceptances wrote
 *     org's own table. A genuinely invited person was answered
 *     authn.tenant_membership_required forever -- and the seeded demo
 *     accounts lost every membership on each process restart, which
 *     scale-to-zero made a matter of minutes. Fixed by reading org's real
 *     rows at sign-in.
 *  2. Accepting required an access token already scoped to the tenant
 *     being joined, which is exactly what an invitee cannot have: the
 *     acceptance is what would create the membership that a scoped token
 *     needs. A closed loop. Fixed by resolving the tenant from the
 *     invitation token itself.
 *
 * The spec walks the whole loop and ends where it matters: the invitee
 * signs in through the browser, with no tenant named, and the frame shows
 * the organization it was invited to. The setup steps use the API because
 * the surfaces for them do not exist yet -- there is no team-management
 * UI, and an invitation token is never in an API response (see
 * test-utils/invitations.ts) -- while the two steps that decide whether
 * the product works, where the invitee lands before acceptance and the
 * sign-in into the invited organization after, are both driven through
 * the browser. (Before acceptance the invitee's registration already
 * provisioned a clinic of its own -- self-service signup,
 * cmd/server/self_service.go -- so the pre-acceptance leg asserts the
 * invitee is NOT yet inside the inviting organization, in place of the
 * old refused-sign-in control that the memberless registration shape
 * used to provide.)
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
  readCurrentTenant,
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
  // (self-service signup, cmd/server/self_service.go), so unlike the old
  // memberless shape the account CAN sign in from the moment it exists
  // -- what it cannot do yet is enter the ORGANIZATION that invited it.
  // This pre-acceptance leg asserts exactly that, in place of the old
  // refused-sign-in control: the browser-shaped sign-in lands in the
  // invitee's own clinic, never inside one of the demo organizations the
  // invitation below will open -- if it ever did land there, the
  // acceptance would prove nothing.
  const inviteeUserId = await registerThroughApi(request, invitee, INVITEE_PASSWORD)
  await visitSignIn(page)
  await submitPasswordSignIn(page, invitee, INVITEE_PASSWORD)
  await expectSignedIn(page)
  // The invitee's own clinic is not one of the demo organizations the
  // helper knows by name -- readCurrentTenant's failure to find one IS
  // the assertion's first half (the clinic's trigger names its raw
  // tenant id, never a demo practice's name).
  // Both halves, in order: a tenant is named at all, and it is not one
  // of the demo organizations. The form this replaces asserted only the
  // second and passed whenever the read THREW -- an unloaded frame, an
  // unrendered switcher, a bug in the helper -- so it could not tell
  // "landed in its own clinic" from "shows no tenant", and the direction
  // it failed in was the one where it says yes.
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

  // The invitee accepts holding no tenant-scoped credential at all --
  // the shape that used to be impossible to serve, since the tenant now
  // comes from the token rather than from a token the invitee could not
  // have had.
  await acceptInvitationAsFreshUser(request, inviteeUserId, token)

  // The step that decides whether the product works: the invitee signs in
  // through the browser and lands in an organization.
  await visitSignIn(page)
  await submitPasswordSignIn(page, invitee, INVITEE_PASSWORD)
  await expectSignedIn(page)
  expect(TENANT_NAMES).toContain(await readCurrentTenant(page))
})
