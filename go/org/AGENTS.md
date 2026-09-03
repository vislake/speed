# org

Multi-level organization trees for a tenant. This file is the module-level discipline that ships with `go/org` to consuming projects; the design rationale is `docs/internal/05-identity-and-access.md`, and the repository-wide rules are the root `CLAUDE.md` plus `.claude/skills/backend-coding-standards`.

**Status.** The tree, the roster and the invitation flow are implemented: models, dual-dialect migrations, the tree operations, memberships, the `Scope` seam consumers accept without importing org, invitations with their rate-limited delivery through the `pkgcore.Mailer` seam, the resilient `authn.user.created` subscription, the registry wiring, the error catalog and the bilingual message bundle. The HTTP surface (spec fragment, generated server interface, handler) and the `integration_test/` tier are the module's remaining block and are **not** in the tree yet — judge what exists by the tree, not by this sentence.

## Scope

**In scope.** The organization tree: one root per tenant, arbitrary depth beneath it, nodes carrying a business-defined `Kind`. Creating, renaming, moving and deleting nodes; ancestor, child, subtree and descendant queries. The roster: one membership per person per tenant, bound to a node, with the subtree beneath it as their data scope. Invitations: issuing, delivering, withdrawing and accepting them. The read-only `Scope` view authorization and data-visibility consumers use. The subscription that gives a brand-new user a workspace.

**Out of scope, deliberately.**

| Not here | Where it belongs | Why |
|---|---|---|
| `users` — the table, the type, the concept beyond an id string | `authn` | Identity data (`docs/internal/04`). org learns a user id from a domain event or an authenticated subject and stores nothing else. This is the root `CLAUDE.md`'s canonical module-boundary example and the defining constraint of this module |
| Roles on a membership | `rbac` | `docs/internal/05` sketches `Membership.Roles []string`; org does not store one. Role state is Casbin policy keyed by (tenant, user, node path), and a `[]string` column has no portable dual-dialect representation anyway — native arrays are banned |
| Sending anything other than the invitation email | `notification` | Business modules publish domain events and let subscribers decide what to send |
| Revoking a removed member's sessions | `authn` | org publishes `org.member.removed`; session state is authn's, and reaching into it is what the event exists to avoid |
| Reading `org.max_depth` / `org.invitation_ttl` from dynamic configuration | a later round | See "org declares no dynamic-config schema" |
| PostgreSQL RLS policies on `org_nodes` | Deployment / ops | `dbkit` supplies the session-variable wiring only; provisioning the role and the policy is deployment-side, per its own "Out of scope" |

## Adjudications a reviewer should not "correct"

### Materialized path plus parent edge — not a closure table, not a recursive CTE

Each node stores `ParentID` (the authoritative structural edge) and `Path` (the derived query index). `TreeService` writes both together; a row where they disagree is corrupt, not a supported state.

A closure table costs O(depth) rows per node and rewrites O(subtree × depth) rows per move, to buy ancestor queries the path already answers by splitting a string in Go. A recursive CTE works on both engines today, but portability here is by construction rather than by "both happen to support it".

**The dialect-identity proof lives in `path.go`'s doc comment and is enforced by `path_test.go`**, not asserted in prose. Its load-bearing property: the id alphabet is `[0-9a-f-]`, so neither LIKE metacharacter (`%`, `_`) can occur, no `ESCAPE` clause is ever written, and — the real hazard — SQLite's ASCII-case-insensitive `LIKE` and PostgreSQL's case-sensitive one select identical rows, because no two distinct stored paths can differ only by case. **Changing the id scheme to anything containing uppercase characters (a Crockford-base32 ULID, say) breaks this and must not be done without re-adjudicating the whole representation.** `TestValidateNodeID_UUIDNewString_IsAlwaysInTheAlphabet` fails the build if the generator ever drifts.

The path carries a **trailing** separator, which is what makes "self and descendants" a plain prefix test for variable-length ids: `/a/` is not a prefix of `/ab/`, while `/a` would be. `TestSubtreePrefix_SiblingSharingAnIDPrefix_DoesNotMatch` and `TestTreeService_Move_SiblingSharingAnIDPrefix_IsNotDraggedAlong` pin it from both ends.

A move rewrites paths **in Go**, never with a SQL `replace()`. That keeps the only structurally risky operation free of dialect-specific SQL.

### `parent_id` is an empty-string sentinel, not `NULL`

`docs/internal/05` sketches `ParentID *string`; the column here is `NOT NULL DEFAULT ''`. The sibling-uniqueness index is `UNIQUE(tenant_id, parent_id, name)`, and `NULL` is distinct from itself in a unique index on both engines — so two roots with the same name would coexist under `NULL`. `go/config/migrations/postgres/0001_create_configs.sql` solved the identical problem the identical way on its `tenant_id` column. `TestRepository_SiblingNameUniqueness_IsEnforcedByTheDatabase`'s last subtest is the regression guard.

### `org_nodes` is tenant data, and its primary key is `id` alone

Tenant data per `docs/internal/04`, so `Repository` embeds `dbkit.Repository[OrgNode]` and `tenancytest.AssertIsolated` — not `AssertNotTenantScoped` — is the correct suite.

`OrgNode` embeds `dbkit.TenantModel` rather than declaring `TenantID` with its own `primaryKey` tag, so the primary key is `id` alone. That is legitimate here for the same reason it is in `examples/reference-app/internal/notes`: the id is an application-generated UUID, already globally unique, so a plain indexed `tenant_id` column is enough. **Do not add a `primaryKey` tag by shadowing the promoted `TenantID` field** — `dbkit`'s `tenant_scope.go` documents exactly how that silently breaks `GetTenantID` and denies a row to its own owner.

One consequence worth knowing when writing tests: two tenants can never hold byte-identical *valid* paths, since paths are built from globally unique ids. The adversarial identical-path case is therefore exercised one layer down, against hand-seeded rows, in `repository_test.go`.

### Deleting never re-parents orphans

A node with children reports `org.node_has_children` unless the caller explicitly asks for a cascade. Re-parenting orphans to the grandparent would silently widen the data scope of every member bound beneath the deleted node — a privilege escalation performed by a delete. The tenant root is never deletable.

### Moving the root needs no rule of its own

Every node of a tenant descends from its single root, so every candidate target is one of the root's own descendants and the cycle check already refuses it (`org.cycle_not_allowed`). That falls out of the one-root invariant `CreateRoot` enforces; do not add a separate "root is not movable" branch.

### `memberships` is LINK data, and link data IS tenant-scoped

This is the adjudication most likely to be "corrected" in review, so it is stated at length.

`docs/internal/04-data-and-tenancy.md`'s data-domain table classifies link data as isolated by `tenant_id`, and says outright that `AssertIsolated` is mandatory for tenant data **and** link data. The backend coding standard's own table says the same (`Link data (memberships) | Yes | AssertIsolated`). So `Membership` implements `dbkit.TenantScoped`, `MembershipRepository` embeds `dbkit.Repository[Membership]`, and `TestMembershipRepository_AssertIsolated` is the suite it runs.

The confusion this exists to prevent: `users` is identity data and is deliberately **not** tenant-scoped, because one person belongs to several tenants. That is precisely *why* the bridging row must be tenant-scoped — it is the per-tenant half of that relationship, and a membership readable across tenants would expose one tenant's roster to another. `AssertNotTenantScoped` on this table would assert the opposite of the requirement.

`org` creates no identity or platform table at all, so `AssertNotTenantScoped` appears nowhere in this module.

### The invitation email is the verification-class exception, not a waiver of it

The security rules forbid messaging an unverified address — *"the verification message itself is the only exception and is rate limited"* — and `docs/internal/07` confirms verification-class messages are the one synchronous, direct-call exception to the events-and-`notification` rule.

**The invitation IS that message.** It is the single consent-establishing message org ever sends, it carries the token the invitee is waiting for, and org sends **nothing else** to somebody who never accepts: no reminders, no automatic resends, nothing promotional. A new message only goes out when a member makes a fresh explicit `Invite` call, and that call revokes the previous token.

It is rate limited on two independent dimensions, composed by the caller as `ratelimit.Limiter`'s contract requires (one `Allow` per dimension, any denial denying):

| Key | Budget | Protects |
|---|---|---|
| `org:invite:tenant:<tenantID>` | 50 / hour | the platform, from a compromised member account |
| `org:invite:email:<blindIndex>` | 3 / 24h | the recipient, from being invited into harassment |

**The second key is built from the HMAC blind index, never the address.** A rate-limit key lives in the KV store, is visible in Redis and turns up in diagnostics; an email address in one is a PII leak. `TestInviteService_Invite_RateLimitKeyIsTheBlindIndexNotTheAddress` consumes the budget under the index-derived key and then shows the next invite denied — if the service ever keyed on plaintext, that test fails.

Once the M2 `notification` module subscribes to `org.member.invited`, a host turns the `org.invitation_email` flag off and this leg goes quiet with no code change.

### The invitation token is never stored, and the address is encrypted but queryable

The token is 32 bytes from `crypto/rand`, base64url-encoded for the link and returned exactly once in `InviteResult.Token`. The row keeps `token_hash`, the hex SHA-256 of it. A leaked backup therefore yields no usable link. (SHA-256 rather than a password hash is correct here: the input is full-entropy randomness, so there is no dictionary to slow anybody down with.)

`InviteResult.Token` is a bearer credential — whoever holds it joins the tenant. It must never reach an API response body, a log line, an event payload or a trace attribute. `TestMail_NoPlaintextTokenOrAddressInLogs` captures everything org logs through a whole invite-and-accept cycle and fails if either the token or the address appears.

The address is encrypted at rest through the serializer named by `org.EmailSerializerName`, and made queryable by `dbkit.NewBlindIndexer("email_index", key, dbkit.NormalizeEmail)` — the exact facility the root `CLAUDE.md`'s Traps section requires instead of a reimplementation. Every write goes through `Index`, every lookup through `Equal`. **The blind-index key must be a different secret from the encryption key**; dbkit takes them through two constructors for that reason.

`dbkit.NormalizeEmail` deliberately performs no structural validation (its doc comment calls that "the caller's input-quality problem"). org is that caller, so `validateInviteEmail` does one minimal syntactic check before anything is indexed, stored, counted or sent.

### Deleting a node with members in it is refused

`TreeService.Delete` asks the roster whether anybody is bound inside the subtree, and reports `org.node_has_members` if so — for a cascade as well as for a leaf. Without it a cascading delete would leave memberships pointing at rows that no longer exist, and a dangling membership is a person whose data scope can no longer be resolved.

org refuses rather than deleting the memberships too, for the same reason it refuses to re-parent orphans: silently changing who is in a tenant, or what they can see, is not something a structural edit should do on its own. Move the members first.

(`ScopeService.MemberNodeIDs` still fails **closed** on a dangling row it should never see — empty set plus a `Warn`, never an error and never everything.)

### Two seams that exist so no import does

`Scope` (scope.go) and `FeatureGate` (module.go) are both interfaces whose every method signature is built from `context.Context`, `string`, `[]string`, `bool` and `error` — **no org type appears in either**. That is the whole mechanism: a consumer declares the identical method set in its own package and accepts org's implementation structurally.

- `Scope` is what rbac needs (a node's materialized path, a subtree's ids, a subject's reachable node set). org never imports rbac — no such edge in `docs/internal/01`'s graph — and rbac never learns what an `OrgNode` is. `scope_test.go` declares `rbacShapedScope` locally, without naming org's own `Scope` type, and asserts `*ScopeService` satisfies it. **A method that returned `[]OrgNode` would destroy the property and force the import back**; that test is what stops it.
- `FeatureGate` is the same trick pointed at config: `*config.Service` satisfies it through its own `IsEnabled` method, and neither module imports the other. The host wires them together, and the host is the only place both names appear.

### Two wirings are required at boot, and refused loudly

`Register` fails rather than starting half-configured, mirroring `config.Attach`'s `ErrCipherRequired`:

| Missing | Error | Why it cannot be deferred |
|---|---|---|
| `WithEmailIndexer` | `org.email_indexer_required` | an invitation whose address cannot be indexed can never be found again |
| `WithMailFrom` **and** `WithInvitationLinkBuilder`, while the invitation email is on | `org.invitation_mail_required` | `pkgcore.Mailer` rejects an empty `From` outright, and a message with no link is not actionable |

A host that lets something else deliver the invitation calls `WithInvitationEmailDisabled()` and needs neither mail option.

### org declares no dynamic-config schema

`maxDepth`, `maxNameLen`, the invitation TTL and the two rate-limit budgets are package constants, per option 3 of the backend standard's configuration rule. org must not import `config` (there is no such edge in `docs/internal/01`'s graph), and `config.Service.Get` returns a config-owned `Value` type org cannot restate structurally the way the boolean `IsEnabled` gate can be. Declaring a schema org would then silently ignore would be a lying schema. `TestModule_Register_DeclaresItsSurface` asserts the absence deliberately.

Feature **flags** are different, and org does declare two: a flag is a boolean, and `FeatureGate` restates the one method needed to read one without naming a config type. The numbers stay constants, overridable per host through `WithMaxDepth` / `WithInvitationTTL`.

## Public API

### Wiring — `module.go`

| Signature | Purpose |
|---|---|
| `func NewModule(db *gorm.DB, opts ...Option) *Module` | Constructs the module. Performs no I/O; the host opens and migrates `db` before `Bootstrap` |
| `WithEmailIndexer` / `WithFeatureGate` / `WithMaxDepth` / `WithInvitationTTL` / `WithMailFrom` / `WithInvitationLinkBuilder` / `WithInvitationEmailDisabled` | The host wiring. See "Two wirings are required at boot" for the two that are not optional |
| `func (m *Module) Tree() *TreeService` | The tree runtime. Returns the same service on every call |
| `func (m *Module) Members() *MemberService` | The roster runtime |
| `func (m *Module) Invitations() *InviteService` | The invitation runtime |
| `func (m *Module) Scope() Scope` | The read-only query view consumers accept structurally |
| `Name` / `DependsOn` / `Migrations` / `Locales` / `OpenAPISpec` / `Register` | `pkgcore.Module`. `DependsOn` is nil — a real answer, not a stub (see the doc comment) |
| `PermissionRead`, `PermissionManage`, `PermissionInviteMember`, `PermissionRemoveMember` | Declared in `Register`; enforcement is rbac's |
| `AuditActionNodeCreate` / `Rename` / `Move` / `Delete`, `AuditActionMemberInvite` / `Accept` / `Remove` | The audit vocabulary org contributes |
| `FeatureInvitations`, `FeatureInvitationEmail` | The two feature flags, both on by default; the second `DependsOn` the first |
| `EventNodeCreated`, `EventNodeMoved`, `EventNodeDeleted` | Published by `TreeService`. `org.node.moved` matters widely: a move changes every descendant's path, the dimension rbac's prefix policies and every subtree listing are written against |
| `EventMemberInvited`, `EventMemberJoined`, `EventMemberRemoved` | Published by the roster and invitation runtimes. `MemberInvited` carries the blind index, never the address |
| `EventUserCreated` | **Subscribed to, never declared.** authn owns it; see the Rules |

`Module` reads the host's `*pkgcore.Registry` at **call time** through the unexported `hostSeams` view — catalog, mailer, bus and KV store — rather than capturing the seams during `Register`. That is not style: `Registry.Locales()` is documented to be nil while modules are registering, so a captured catalog is a nil catalog and the first invitation panics or renders blank.

### The tree — `tree.go`

Every method takes the tenant from `ctx` and nothing else. There is no parameter anywhere on this type through which a caller could name a tenant.

| Signature | Notes |
|---|---|
| `func NewTreeService(db *gorm.DB) *TreeService` | |
| `Root(ctx)` | The tenant's root, or `org.node_not_found` |
| `Get(ctx, nodeID)` | Another tenant's id reports `org.node_not_found`, never a distinguishable error |
| `Children(ctx, nodeID)` | Direct children, by name. An unknown node errors rather than returning an empty list |
| `CreateRoot(ctx, name, kind)` | One per tenant; a second reports `org.root_already_exists` |
| `CreateChild(ctx, parentID, name, kind)` | Path and depth are derived from the parent's stored **path**, so a fresh row can never carry a depth that disagrees with it |
| `Rename(ctx, nodeID, name)` | Never touches `Path`. Renaming to the current name is a no-op, not a self-collision |
| `Move(ctx, nodeID, newParentID)` | Carries the whole subtree. See the limitation below |
| `Delete(ctx, nodeID, cascade)` | See "Deleting never re-parents orphans" |
| `Ancestors(ctx, nodeID)` | Root first, self excluded. One query at any depth — read from the node's own path |
| `Descendants(ctx, nodeID)` / `Subtree(ctx, nodeID)` | Strictly beneath / inclusive of the node. One indexed prefix scan |

### The roster — `membership.go`

| Signature | Notes |
|---|---|
| `func NewMemberService(db *gorm.DB, tree *TreeService) *MemberService` | Publishes nothing until a host wires it through `Register` |
| `Get(ctx, userID)` | `org.membership_not_found` for a stranger and for another tenant's member alike |
| `Add(ctx, userID, nodeID)` | One seat per person per tenant; a second reports `org.membership_exists` |
| `List(ctx, nodeID)` | The **subtree** roster: standing at a group returns every store's members under it |
| `Remove(ctx, userID)` | Publishes `org.member.removed`. Refuses the tenant's last **active** member (`org.member_not_removable`) |
| `Repository()` | The promoted `dbkit.Repository[Membership]` surface, for a host's own isolation test |
| `MembershipStatusActive` / `Invited` / `Suspended` | The closed status set. Only `active` grants visibility |

### The scope seam — `scope.go`

`Scope` is the interface; `ScopeService` implements it; `Module.Scope()` returns it. `Path`, `DescendantIDs` and `MemberNodeIDs` — see "Two seams that exist so no import does" before changing any signature.

`MemberNodeIDs` returns an empty slice and a **nil error** for a person with no membership, an inactive one, or a probe from the wrong tenant. "Sees nothing" is an ordinary answer to a visibility question, and an empty `IN` list is the fail-closed outcome for a consumer filtering a listing.

### Invitations — `invitation.go`, `invite.go`, `mail.go`

| Signature | Notes |
|---|---|
| `func NewInviteService(db, tree, members, indexer) *InviteService` | |
| `Invite(ctx, InviteRequest) (*InviteResult, error)` | Gate → node → address → both rate limits → supersede → row → event → message. A delivery failure revokes the invitation it just created |
| `Accept(ctx, token, userID) (*Membership, error)` | Resolves the token **inside the tenant `ctx` already carries**; the tenant is never read from the token |
| `Revoke(ctx, invitationID)` | Idempotent. Refuses an accepted invitation — removing a member is `MemberService.Remove`'s job |
| `List(ctx)` | Pending invitations, expired ones included: expiry is evaluated at acceptance, not by a sweeper, and hiding them would make a sent invitation look unsent |
| `InvitationLinkBuilder`, `EmailSerializerName` | Host wiring. The link must point at the tenant's own host, because acceptance is tenant-scoped |
| `InvitationStatusPending` / `Accepted` / `Revoked` | The closed status set |

The message is rendered from the catalog under `org.invitation.subject` / `body_text` / `body_html`, in the **recipient's** locale — captured on the row at invite time from the request's `Accept-Language`, falling back to `zh-CN`, because the invitee may not be a user and so has no profile to read a preference from. `Catalog.Lookup` never falls back across languages, so a missing id surfaces as an error rather than as the other language's text, and the render path propagates that error rather than sending a blank body. The node name is HTML-escaped for the HTML body; go-i18n renders with `text/template`, which does not escape.

### The authn subscription — `events.go`

`EventUserCreated` is the one coordination point with the parallel authn module, and the string appears in exactly one place in org. The handler's contract, all four cases tested:

1. **No publisher** — the subscription is installed and never fires. `Subscribe` cannot fail.
2. **Unrecognized payload** — `Warn`, **return nil**. On the in-memory bus a handler error propagates back into the publisher's `Publish`, so returning one would make org's ignorance look like a failed user creation inside authn.
3. **No tenant on the event** — `Debug`, do nothing. A self-registering user genuinely has no tenant yet; the tenant-creating path is an explicit `CreateRoot`.
4. **A tenant** — rebuild it with `pkgcore.WithTenant` (a remote-bus handler carries none), then idempotently ensure a root and a membership. A redelivered event creates neither a second root nor a second membership.

`userIDFromPayload` normalizes any payload shape through a JSON round-trip and probes `user_id` / `userId` / `UserID` / `userID`. **It never type-asserts the publisher's struct** — doing so would require importing it.

### Data access — `repository.go`

`Repository` embeds `*dbkit.Repository[OrgNode]` (Create / FindByID / Update / Delete / List promoted unchanged) and adds the query shapes `Repository[T]`'s minimal surface cannot express: `subtree`, `children`, `findRoot`, `bySiblingName`, `byIDs`, `deleteLeaf`, `deleteSubtree` — all unexported, all reached through `TreeService`.

Read `deleteLeaf` before touching `Delete`. It counts the rows its own `DELETE` matched **inside** the transaction and rolls back when the count is not exactly one. A `children`-then-`Delete` pair would instead leave a window in which a concurrent create adds a child, and the delete would orphan it — `parent_id` pointing at a removed row, `path` naming a node that no longer exists. A self-referencing foreign key cannot close that gap either: SQLite leaves FKs unenforced unless `foreign_keys` is switched on, which would make the two dialects disagree. Do not "simplify" the in-transaction count back out into a pre-check.

Each is written as option 1 of `go/dbkit/AGENTS.md`'s Known-limitations guidance — built on the same `*gorm.DB` isolation layer 1 protects, against a `TenantScoped` destination, inside `dbkit.WithTenantSession` so layer 3's RLS session variable is set too. **There is no `tenant_id = ?` string anywhere in this package, and no `db.Table` / `db.Model` / `db.Raw`**; `go/org` has no allowlist entry in either semgrep rule, deliberately.

### Errors — `errors.go`

Tree: `org.node_not_found`, `org.node_name_required`, `org.node_name_too_long`, `org.parent_not_found`, `org.max_depth_exceeded`, `org.cycle_not_allowed`, `org.node_has_children`, `org.node_has_members`, `org.root_already_exists`, `org.root_not_deletable`, `org.duplicate_sibling_name`, `org.invalid_node_id`, `org.internal_error`.

Roster and invitations: `org.membership_not_found`, `org.membership_exists`, `org.member_not_removable`, `org.invitation_not_found`, `org.invitation_expired`, `org.invitation_already_accepted`, `org.invitation_revoked`, `org.invitation_rate_limited` (HTTP 429), `org.invalid_email`, `org.invitations_disabled`, `org.email_indexer_required`, `org.invitation_mail_required`.

**No error parameter ever carries an email address or a token.** An error's parameters are rendered, logged and traced.

Match on `Code` through `apperr.As`, never by identity — `WithParam`/`WithCause` derive a new `*apperr.Error` every time. Every code has a `zh-CN` and an `en-US` entry under the identical id; `TestErrorCatalog_EveryCodeHasBothTranslations` fails if one is added without both, and `TestErrorCatalog_IsComplete` parses `errors.go` so a new sentinel cannot skip the table at all.

## Known limitations

**A subtree move is not atomic.** `Move` saves each rewritten row through `dbkit.Repository[OrgNode].Update`, one transaction per row, because `Repository[T]` exposes no transactional batch seam. A process that dies mid-move leaves a partially re-parented subtree whose paths disagree with their parent edges until the move is repeated. The cheap fix — one statement with a SQL `replace()` over the path column — is exactly what the dialect-identity proof forbids. The right fix is a `dbkit` round that grows `Repository[T]` a transactional batch write, with org as one of its two real consumers. A cascading **delete** is already one statement and does not have this problem.

**`Repository[T].WithinSubtree(nodeID)`, which `docs/internal/05` sketches, is not built.** It is a change to *dbkit's* public generic surface, not org's, and `go/dbkit/AGENTS.md` states that surface grows with real consumers' shapes in hand. org exposes descendant ids instead; consumers filter with `IN ?`.

**Sibling-name collisions are pre-checked, and the database is the backstop.** Two concurrent creates racing on the same name both pass the pre-check; the `UNIQUE(tenant_id, parent_id, name)` index rejects the loser, and `mapWriteError` translates `gorm.ErrDuplicatedKey` back into `org.duplicate_sibling_name`, so the caller sees one error for one condition either way.

**A membership is not moved when its node is.** `Move` rewrites paths, and `MemberNodeIDs` resolves the subtree at read time, so a member's scope follows the tree automatically — but nothing caches it for them. A consumer that *does* cache a node path must invalidate on `org.node.moved`.

**No operation writes `suspended` or `invited`.** `MembershipStatus*` declares the closed vocabulary the column may hold, and `Scope.MemberNodeIDs` already honors it — a suspended member sees nothing — but org ships no Suspend/Reinstate call, and an unaccepted invitee occupies no seat, so `invited` is never written either. Both statuses are readable and enforced; setting them is a later round's API surface, not an accident.

**Invitations are not swept.** An unaccepted invitation stays `pending` in the table forever; expiry is evaluated at acceptance time, which is enough for correctness but leaves rows accumulating. A cleanup job belongs to a round that has the `jobs` queue wired into this module.

**Depth and name bounds are enforced in Go, not by the column widths.** SQLite does not enforce `VARCHAR(n)` under type affinity, so a standalone-mode deployment would otherwise accept what the distributed mode rejects.

## Rules

- **Never import `authn`, and never import any other business module's structs.** Users are id strings learned from events. An `authn` type in this module's import graph is a defect, not a shortcut.
- **Never declare `authn.user.created` with `reg.Events.Publishes`.** Only subscribe to it. Declaring another module's event type collides at bootstrap the moment both modules boot in one host; `TestModule_Register_DoesNotDeclareAuthnsEvent` guards it.
- **Never write an `OrgNode` row outside `TreeService`.** It is what keeps `ParentID` and `Path` in lockstep.
- **Never hand-write a tenant filter, and never reach for `db.Table` / `db.Model` / `db.Raw`.** See the Data access section.
- **Never introduce an id scheme outside `[0-9a-f-]`.** See the dialect-identity proof.
- **Never add a recursive CTE or a SQL string function over the `path` column.** Path arithmetic happens in Go.
- **Never localize an error into a returned string.** Return the code; the text lives in `locales/`.
- **Never capture `reg.Locales()`, `reg.Mailer()`, `reg.KVStore()` or `reg.Events.Bus()` during `Register`.** Store the registry and read the seam when you use it. `Registry.Locales()` is nil while modules register — this is the single easiest mistake in this module.
- **Never put an email address or an invitation token in a log line, an event payload, a rate-limit key, an error parameter or an API response.** The blind index is what identifies a recipient in all of those.
- **Never render user-facing text from a Go string literal.** Even the automatic workspace name comes from the catalog; its only fallback is an identifier, never a word in one of the two languages.
- **Never send a second unsolicited message to an unaccepted invitee.** The single consent-establishing message is the entire exception org is allowed.
- Migrations are versioned SQL, one file per dialect, byte-identical apart from the header note. No `AutoMigrate`, no `NOW()`, no `gen_random_uuid()`, no native arrays, no JSONB.

## Testing

Unit tests run with no external dependency: `go test ./... -race` from `go/org`. `internal/testutil.NewSQLite` applies the module's **real** migration files from zero through `dbkit.MigrationRegistry`, so every DB-backed test doubles as proof that those files run on an empty database.

`internal/testutil` deliberately does not import `go/org` — org's own tests are in `package org`, so a helper package importing org could not be imported back by them. `NewSQLite` therefore takes a module name and an `embed.FS`. `go/dbkit/internal/testutil` avoids the identical cycle the identical way.

Tests that touch `Invitation` must register the encrypted-email serializer first (`newInvitationTestDB` does it): the column is written through a named GORM serializer, and GORM cannot parse the model when nothing is registered under that name.

`internal/testutil.NewPostgres` is the integration tier's counterpart, applying the identical files from `postgres/`, so the same assertions can run against both dialects by swapping the constructor alone. The integration tier itself lands with this module's remaining block.
