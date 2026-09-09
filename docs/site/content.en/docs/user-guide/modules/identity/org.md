---
title: org
weight: 3
description: "A tenant's organization tree, the memberships bound to its nodes, and the invitations that create them — the data behind authn's membership checks and rbac's subtree-scoped grants."
---

# org

org is speed's organization module: one tree per tenant, a roster of
memberships bound to its nodes, and the invitations that create them.
It is the data behind both its neighbours' questions — membership
answers for `authn`'s fail-closed checks, node ids and paths for
`rbac`'s scoped grants — while importing neither of them.

## What it is for

The tree is `OrgNode{ParentID, Path, Depth}` — adjacency list plus
materialized path, no closure table and no recursive CTE: one root per
tenant, arbitrary depth, nodes carrying a business `Kind`.
`TreeService` creates, renames, moves and deletes; `memberships` is
link data — one seat per person per tenant, bound to a node whose
subtree is their data scope; `InviteService` issues, delivers,
withdraws and accepts the invitations that turn an email address into
a member. A subscription to `authn.user.created` — subscribed, never
declared — gives a brand-new user a workspace: an idempotent
root-plus-membership ensure.

Deliberately **not** here: the `users` table or the concept of a user
beyond an id string (`authn`); roles on a membership (`rbac`); any
message other than the invitation email itself; dynamic configuration
— depth and name bounds, the invitation TTL and the rate-limit
budgets are package constants, overridable per host through
`WithMaxDepth`/`WithInvitationTTL`, because org must not import
`config`.

## When to choose it

When membership is scoped to structure: multi-level organizations
whose members see and act on their subtree, rosters read back scoped,
invitations as the joining path. It is also the canonical
implementation behind `authn`'s `MembershipReader` and `rbac`'s
`SubtreeResolver` seams. A flat "organization" works too: one root,
memberships beneath it.

## Wiring and minimal use

Two wirings are required and refused loudly at `Register` — an
invitation whose address cannot be indexed can never be found again,
and `pkgcore.Mailer` rejects an empty `From`:

```go
m := org.NewModule(db,
    org.WithEmailIndexer(emailIndexer),      // dbkit.NewBlindIndexer over org.EmailIndexColumn
    org.WithMailFrom("team@example.com"),
    org.WithInvitationLinkBuilder(builder),  // turns the token into an accept URL
    // a host that delivers invitations elsewhere calls WithInvitationEmailDisabled()
    // and needs neither mail option
)
```

The module joins the `Kernel.Bootstrap` set; its accessors are
`Tree()`, `Members()`, `Invitations()` and the read-only `Scope()`
view, and it reads the host's registry at **call time**, never during
`Register`. The HTTP surface is eleven operations under `/api/v1/org`
— node CRUD plus move, subtree-scoped member listing and removal,
invitation create/list/accept. The host gates all but one on the four
declared permissions (`org:read`, `org:manage`, `org:invite_member`,
`org:remove_member`, via rbac); `accept` stays ungated — accepting an
invitation addressed to you needs no standing grant — and
**tenantless**: a freshly invited person holds no tenant claim, so the
host lets that one path through tenant-resolving middleware. Caller
identity for the create/accept operations comes through
`SubjectResolver`, failing closed (`401 org.subject_unresolved`) when
unwired.

## Core concepts and API essentials

- **The materialized path is the query index.** Subtree membership is
  one indexed prefix scan over a trailing-separator path — `/a/` is
  not a prefix of `/ab/` — and the id alphabet (`[0-9a-f-]`) is
  proof-pinned so the scan returns identical rows on both dialects.
- **Deletion never widens silently.** A node with members refuses
  deletion unless the caller explicitly cascades; deleting never
  re-parents orphans, the root is never deletable, and every write is
  concurrency-safe — a row lock or one database-arbitrated statement
  per operation.
- **Membership is one active seat per person per tenant.** The roster
  reads a subtree standing at any node; `Remove` refuses to drop the
  tenant's last active member; `TenantsOf` is the one cross-tenant
  read, gated on a system context.
- **The invitation is the verification-class exception.** The token is
  32 bytes of `crypto/rand`, returned exactly once, stored only as its
  SHA-256 hash; the address is encrypted at rest under a blind-indexed
  column (the index key must differ from the cipher key). Delivery is
  rate-limited per tenant and per recipient (the latter keyed by the
  blind index, never the plaintext) and rendered in the invitee's
  locale; a fresh `Invite` supersedes the previous token, and
  acceptance is a single-use compare-and-swap.
- **The read-only `Scope` view** — `Path`, `DescendantIDs`,
  `MemberNodeIDs` — is built from stdlib types only, so `rbac`
  declares the identical interface and accepts `*ScopeService` with no
  import; "sees nothing" is an ordinary answer.
- **Events and audit.** `org.node.*`/`org.member.*` events are what
  rbac's reaper subscribes to. Writes are audited through dbkit's
  automatic capture: the three tenant-data models carry the
  `Auditable` marker, and org exports the capture scope as
  `AuditableModels()`.
- **Soft deletion.** `OrgNode` and `Membership` implement
  `dbkit.SoftDeletable`: deletes mark, `Restore` undoes one row's own
  mark — per-node, never cascading, refusing a dead parent.

## Boundaries and pitfalls

- Never import `authn` or another business module's structs: users are
  id strings learned from events. Never write an `OrgNode` row outside
  `TreeService` — it keeps `ParentID` and `Path` in lockstep.
- An email address or invitation token never leaves the module
  identifying: no log lines, event payloads, rate-limit keys, error
  parameters or responses — the blind index is the most identifying
  thing out.
- `Restore` has no HTTP surface yet (a service-level call); no query
  lists a node's mark-deleted descendants; invitations are not swept —
  expiry is judged at acceptance, pending rows accumulate by design.
- Coded errors: see the [error code
  index](/docs/user-guide/error-codes/#org) — tree, roster and
  invitation groups.

## Source

- [go/org/AGENTS.md](https://github.com/vislake/speed/blob/main/go/org/AGENTS.md) — the authoritative document (tree shape, invitations, seams, concurrency, rules)
- HTTP fragment: [go/org/api/openapi.yaml](https://github.com/vislake/speed/blob/main/go/org/api/openapi.yaml)
- Design: [docs/internal/05-identity-and-access.md](https://github.com/vislake/speed/blob/main/docs/internal/05-identity-and-access.md)
- Related: the domain guides [Identity and access](/docs/user-guide/domains/identity-access/) and [Tenancy and organizations](/docs/user-guide/domains/tenancy-and-org/), and the group pages [authn](/docs/user-guide/modules/identity/authn/) and [rbac](/docs/user-guide/modules/identity/rbac/)
