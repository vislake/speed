/**
 * org-api.ts -- the app's hand-written half of go/org's HTTP surface:
 * the three typed calls the team surface's invitation half needs. The
 * members half of the team surface deliberately reads through the app's
 * OWN roster-with-identity answer instead (team-api.ts -- GET
 * /api/reference-app/team-members, the host composition that enriches
 * org's membership rows with each member's display identity from
 * authn's users table, because org's rows carry opaque user ids only by
 * its own module-boundary rule), so org's raw member list has no typed
 * call here: a surface that needs the raw list would add it back over
 * ORG_MEMBERS_PATH, whose literal is kept below for that day.
 *
 * go/org's OpenAPI fragment ships a backend leg only -- it predates the
 * merge machinery and is deliberately not part of the merged application
 * document that drives @speed/api-sdk (go/org/AGENTS.md's own note, the
 * same status go/sharing's fragment holds), so no generated operation
 * exists for any org route and the app reaches them through the
 * api-client RequestFn the host bound, the same seam every generated
 * call travels -- exactly the no-generated-surface mirror of the
 * discipline that share-api.ts already documents for go/sharing. The
 * path literals are hand-kept in step with the module's own mounted
 * route (/api/v1/org -- demo_subject.go's orgRoutePath), and the wire
 * shapes mirror go/org/api/openapi.yaml's schemas field-for-field,
 * never the generator's Go types.
 *
 * The caller identity question deserves stating once: org's two
 * caller-scoped operations (create and accept an invitation) resolve
 * who is acting server-side, and the demo wiring's demoOrgSubjectResolver
 * falls back to the verified access-token Principal when no demo header
 * rides along (server.go, the org-web round's wiring) -- so the calls
 * below carry only the bearer the client attaches, never a header of
 * their own, exactly like a delivered consumer's calls would.
 */

import type { RequestFn } from '@speed/api-client'

/** GET/POST /api/v1/org/members -- org_listMembers / org_removeMember.
 * Unconsumed by this app's surfaces today (see the file header), kept
 * as the module route's literal for the day a surface needs the raw
 * list. */
export const ORG_MEMBERS_PATH = '/api/v1/org/members'
/** GET/POST /api/v1/org/invitations -- org_listInvitations /
 * org_createInvitation. */
export const ORG_INVITATIONS_PATH = '/api/v1/org/invitations'
/** GET /api/v1/org/nodes -- org_listNodes (the whole tree). */
export const ORG_NODES_PATH = '/api/v1/org/nodes'

/** One person's binding to a node of the tenant's organization tree
 * (the spec's OrgMembership schema). org deliberately stores no other
 * identity data about the user -- the user id is opaque to this
 * surface, which is why the naming of a roster row lives in the host
 * composition (team-api.ts) rather than here. */
export interface OrgMembership {
  readonly membershipId: string
  /** An id in authn's users table, carried as an opaque string. */
  readonly userId: string
  readonly nodeId: string
  /** One of "active", "invited", "suspended". */
  readonly status: string
  readonly createdAt: string
}

/** A node of the tenant's organization tree (the spec's OrgNode
 * schema). */
export interface OrgNode {
  readonly id: string
  readonly parentId: string
  readonly path: string
  readonly depth: number
  readonly name: string
  readonly kind: string
  readonly createdAt: string
  readonly updatedAt: string
}

/** GET /api/v1/org/nodes' 200 answer (OrgListNodesResponse). */
export interface OrgListNodesResponse {
  readonly nodes?: OrgNode[]
}

/** A pending (or resolved) offer to join the tenant (the spec's
 * OrgInvitation schema). The invitee's address is deliberately absent
 * from the schema -- it is PII, encrypted at rest, and org's own
 * convention is never to echo it across the process boundary -- so a
 * listed row names no one; the team surface pairs the row an invite
 * created with the address the inviting owner typed (see the surface's
 * invited-address memory in team-view.tsx). */
export interface OrgInvitation {
  readonly id: string
  readonly nodeId: string
  /** One of "pending", "accepted", "revoked". */
  readonly status: string
  readonly expiresAt: string
  readonly createdAt: string
}

/** GET /api/v1/org/invitations' 200 answer
 * (OrgListInvitationsResponse). */
export interface OrgListInvitationsResponse {
  readonly invitations?: OrgInvitation[]
}

/** The create operation's request body (OrgCreateInvitationRequest):
 * the invitee's address and the node the invitee will be bound to on
 * acceptance. locale is deliberately never sent: it is the RECIPIENT's
 * preferred language, something the inviting operator knows about the
 * invitee -- this surface has no such knowledge, so the platform
 * default renders the mail. */
export interface OrgCreateInvitationRequest {
  readonly email: string
  readonly nodeId: string
}

/**
 * Lists the caller's tenant's pending invitations, newest first, as the
 * server lists them (rows name no invitee -- see OrgInvitation).
 */
export async function listOrgInvitations(
  api: RequestFn,
): Promise<OrgListInvitationsResponse> {
  return api<OrgListInvitationsResponse>(ORG_INVITATIONS_PATH)
}

/** Lists the caller's tenant's whole organization tree. */
export async function listOrgNodes(
  api: RequestFn,
): Promise<OrgListNodesResponse> {
  return api<OrgListNodesResponse>(ORG_NODES_PATH)
}

/**
 * Invites an address into the caller's tenant at the given node. The
 * 201 answer carries the invitation row (no address, no token -- the
 * token travels only in the message the server sends the invitee).
 */
export async function createOrgInvitation(
  api: RequestFn,
  invitation: OrgCreateInvitationRequest,
): Promise<OrgInvitation> {
  return api<OrgInvitation>(ORG_INVITATIONS_PATH, {
    method: 'POST',
    body: invitation,
  })
}
