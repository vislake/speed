/**
 * team-api.ts -- the app's typed access to its own roster-with-identity
 * answer: GET /api/reference-app/team-members (internal/app/team_members.go)
 * returns the tenant's org membership roster, every row enriched with the
 * display identity of the person behind it.
 *
 * Why the app has its own roster answer at all: org's member rows carry
 * opaque user ids only, by go/org's module-boundary rule (org stores no
 * other identity data about a user), and the name has to come from
 * authn's users table -- so the assembling host composes the two sides
 * its own modules keep apart, in-process, exactly like the clinic-name
 * answer composes org's tree (tenant-name.ts's own doc comment gives the
 * sibling argument for that route). The web reaches the composition
 * through the app's own api-client RequestFn -- the same transport every
 * generated call rides, the app never calls HTTP directly -- answered by
 * the host's own route, the same hand-kept-shape relationship the
 * clinic-name route has with tenant-name.ts. The path is a host
 * constant, not part of any module's OpenAPI fragment, so it is spelled
 * here and in internal/app/team_members.go (TeamMembersPath) and kept in
 * step by the Go flow test that mounts the route and the web suites that
 * drive it.
 *
 * What the rows carry: every field org's own OrgMembership answer
 * carries (membershipId, userId, nodeId, status, createdAt -- the
 * status and joined columns of the roster render from them, and userId
 * is the row's identity key the "You" naming compares against), plus
 * the two identity fields the composition adds: displayName, the name
 * the account registered with ('' when it registered none), and email,
 * the address it registered ('' when it has none). A member who typed
 * a display name is named by it; a member who did not is named by
 * their email; a member whose account row cannot be read carries both
 * empty and the surface renders its fallback label -- never the raw
 * user id.
 */

import type { RequestFn } from '@speed/api-client'

/** The path of the app's own roster-with-identity answer
 * (TeamMembersPath in internal/app/team_members.go). */
export const TEAM_MEMBERS_PATH = '/api/reference-app/team-members'

/** One roster row: the membership facts org's OrgMembership carries
 * (membershipId, userId, nodeId, status, createdAt), plus the member's
 * display identity from authn's users table. The five membership facts
 * are held here concretely rather than extended from the generated
 * OrgMembership: this is the host's own route's answer, its shape is
 * hand-kept against internal/app/team_members.go and the Go flow test
 * that mounts the route (flowtests/team_members_test.go), and every
 * field is always present -- unlike the generated type, whose fields
 * the spec leaves optional. */
export interface TeamMember {
  readonly membershipId: string
  /** An id in authn's users table, carried as an opaque string. */
  readonly userId: string
  readonly nodeId: string
  /** One of "active", "invited", "suspended". */
  readonly status: string
  readonly createdAt: string
  /** The account's own display name, or '' when it registered none. */
  readonly displayName: string
  /** The account's own email, or '' when it has none. */
  readonly email: string
}

/** GET /api/reference-app/team-members' 200 answer. */
export interface TeamMembersResponse {
  readonly members?: TeamMember[]
}

/**
 * Fetches the current tenant's roster with each member's display
 * identity through api. A non-2xx answer rejects as the client's
 * ApiError like every other request; the team surface classifies the
 * refusal exactly as it classifies the org reads' (the rbac gate's 403
 * is an authorization fact, anything else a load failure).
 */
export async function fetchTeamMembers(
  api: RequestFn,
): Promise<TeamMembersResponse> {
  return api<TeamMembersResponse>(TEAM_MEMBERS_PATH)
}
