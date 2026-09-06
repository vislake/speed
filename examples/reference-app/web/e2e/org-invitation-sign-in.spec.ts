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
 * an organization. The setup steps use the API because the surfaces for
 * them do not exist yet -- there is no team-management UI, and an
 * invitation token is never in an API response (see
 * test-utils/invitations.ts) -- while the two steps that decide whether
 * the product works, the refusal before and the sign-in after, are both
 * driven through the browser.
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
  APP_TEXT,
  AUTH_ERROR_TEXT,
  TENANT_NAMES,
  expectSpecificError,
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
  // does, and belongs to no organization yet.
  const inviteeUserId = await registerThroughApi(request, invitee, INVITEE_PASSWORD)

  // Before the invitation is accepted, signing in is refused: an account
  // with no membership anywhere has no tenant to be issued a token for.
  // This is the fail-before half of the gate, asserted rather than
  // assumed -- if it ever starts succeeding, the acceptance below would
  // prove nothing.
  await visitSignIn(page)
  await submitPasswordSignIn(page, invitee, INVITEE_PASSWORD)
  await expectSpecificError(page, AUTH_ERROR_TEXT.tenantMembershipRequired)

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
  await expect(page.getByRole('link', { name: APP_TEXT.navNotes })).toBeVisible()
  expect(TENANT_NAMES).toContain(await readCurrentTenant(page))
})
