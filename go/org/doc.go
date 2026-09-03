// Package org owns a tenant's organization structure: the multi-level
// organization tree (group -> region -> store, to whatever depth a tenant
// needs), the membership records that bind a person to a place in that
// tree, and the invitations that create them.
//
// # What org owns
//
// The tree itself. Every tenant has exactly one root node; every other node
// hangs beneath it. A node carries a parent edge, a materialized path, a
// depth, a business-defined Kind, and a display Name. TreeService is the
// only sanctioned way to change that structure -- it keeps the parent edge
// and the materialized path in lockstep, which nothing else may assume
// responsibility for.
//
// # What org deliberately does NOT own
//
//   - Users. The users table is identity data (docs/internal/04-data-and-
//     tenancy.md) and belongs to authn. org never creates, reads or imports
//     it -- not the table, and not authn's Go types. A person appears here
//     only as a user id string, learned from a domain event or from the
//     caller's authenticated subject. This is the canonical example the
//     root CLAUDE.md gives for the module-boundary rule ("authn publishes
//     UserCreated, org subscribes to create the default workspace; org
//     never imports authn.User"), and it is the defining constraint of this
//     module.
//
//   - Roles and permissions. docs/internal/05-identity-and-access.md
//     sketches a Roles field on Membership; org does not store one. Role
//     state is rbac's Casbin policy store, keyed by tenant, user and node
//     path -- and a []string column has no portable dual-dialect
//     representation anyway (native arrays are banned; see the backend
//     coding standard's data-model rules). org supplies the tree and the
//     membership; rbac decides what a member may do with them.
//
//   - Notification delivery. org publishes domain events and lets whoever
//     subscribes decide what to send. The one exception is the invitation
//     email: a consent-establishing verification-class message, sent
//     directly through the pkgcore.Mailer seam and rate limited on two
//     dimensions, exactly as the security rules allow for that one message
//     class. It is gated by the org.invitation_email feature flag, so a
//     host whose notification module takes delivery over switches org's own
//     leg off without a code change.
//
//   - Session and token state. A removed member's tokens for the tenant
//     must be revoked; org publishes org.member.removed and authn, which
//     owns that state, subscribes. Reaching into another module's state is
//     what the event exists to avoid.
//
// # Memberships and scope
//
// A Membership binds one person to one node of one tenant's tree -- one seat
// per person per tenant -- and the subtree beneath that node is their data
// scope. That query side is exposed as Scope, an interface whose every
// method signature is built from stdlib types alone, so an authorization
// consumer can restate it in its own package and accept org's implementation
// structurally rather than importing this module.
//
// # Tree representation
//
// A node stores both its parent edge (ParentID) and its materialized path
// (Path). The parent edge is authoritative for structure; the path is the
// derived query index that answers "everything beneath this node" as a
// single indexed prefix scan, with no recursive CTE and therefore no
// dialect-portability argument to make. See path.go for the grammar and
// the invariants that keep the prefix scan returning identical rows on
// SQLite and PostgreSQL.
package org
