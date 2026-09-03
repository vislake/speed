# org

Multi-level organization trees for a tenant. This file is the module-level discipline that ships with `go/org` to consuming projects; the design rationale is `docs/internal/05-identity-and-access.md`, and the repository-wide rules are the root `CLAUDE.md` plus `.claude/skills/backend-coding-standards`.

**Status.** The tree is implemented: models, dual-dialect migrations, the tree operations, the registry wiring, the error catalog and the bilingual message bundle. Memberships, invitations, the `Scope` seam for rbac and the HTTP surface are the module's later blocks and are **not** in the tree yet — judge what exists by the tree, not by this sentence.

## Scope

**In scope.** The organization tree: one root per tenant, arbitrary depth beneath it, nodes carrying a business-defined `Kind`. Creating, renaming, moving and deleting nodes; ancestor, child, subtree and descendant queries. Later blocks add memberships, invitations and the query-side seam authorization consumes.

**Out of scope, deliberately.**

| Not here | Where it belongs | Why |
|---|---|---|
| `users` — the table, the type, the concept beyond an id string | `authn` | Identity data (`docs/internal/04`). org learns a user id from a domain event or an authenticated subject and stores nothing else. This is the root `CLAUDE.md`'s canonical module-boundary example and the defining constraint of this module |
| Roles on a membership | `rbac` | `docs/internal/05` sketches `Membership.Roles []string`; org does not store one. Role state is Casbin policy keyed by (tenant, user, node path), and a `[]string` column has no portable dual-dialect representation anyway — native arrays are banned |
| Sending anything other than the invitation email | `notification` | Business modules publish domain events and let subscribers decide what to send |
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

### org declares no dynamic-config schema

`maxDepth` and `maxNameLen` are package constants, per option 3 of the backend standard's configuration rule. org must not import `config` (there is no such edge in `docs/internal/01`'s graph), and `config.Service.Get` returns a config-owned `Value` type org cannot restate structurally the way a boolean flag gate can be. Declaring a schema org would then silently ignore would be a lying schema. `TestModule_Register_DeclaresItsSurface` asserts the absence deliberately.

## Public API

### Wiring — `module.go`

| Signature | Purpose |
|---|---|
| `func NewModule(db *gorm.DB) *Module` | Constructs the module. Performs no I/O; the host opens and migrates `db` before `Bootstrap` |
| `func (m *Module) Tree() *TreeService` | The tree runtime. Returns the same service on every call |
| `Name` / `DependsOn` / `Migrations` / `Locales` / `OpenAPISpec` / `Register` | `pkgcore.Module`. `DependsOn` is nil — a real answer, not a stub (see the doc comment) |
| `PermissionRead`, `PermissionManage`, `PermissionInviteMember`, `PermissionRemoveMember` | Declared in `Register`; enforcement is rbac's |
| `AuditActionNodeCreate` / `Rename` / `Move` / `Delete` | The audit vocabulary org contributes |
| `EventNodeCreated`, `EventNodeMoved`, `EventNodeDeleted` | The event catalog entries. `org.node.moved` matters widely: a move changes every descendant's path, the dimension rbac's prefix policies and every subtree listing are written against |

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

### Data access — `repository.go`

`Repository` embeds `*dbkit.Repository[OrgNode]` (Create / FindByID / Update / Delete / List promoted unchanged) and adds the query shapes `Repository[T]`'s minimal surface cannot express: `subtree`, `children`, `findRoot`, `bySiblingName`, `byIDs`, `deleteLeaf`, `deleteSubtree` — all unexported, all reached through `TreeService`.

Read `deleteLeaf` before touching `Delete`. It counts the rows its own `DELETE` matched **inside** the transaction and rolls back when the count is not exactly one. A `children`-then-`Delete` pair would instead leave a window in which a concurrent create adds a child, and the delete would orphan it — `parent_id` pointing at a removed row, `path` naming a node that no longer exists. A self-referencing foreign key cannot close that gap either: SQLite leaves FKs unenforced unless `foreign_keys` is switched on, which would make the two dialects disagree. Do not "simplify" the in-transaction count back out into a pre-check.

Each is written as option 1 of `go/dbkit/AGENTS.md`'s Known-limitations guidance — built on the same `*gorm.DB` isolation layer 1 protects, against a `TenantScoped` destination, inside `dbkit.WithTenantSession` so layer 3's RLS session variable is set too. **There is no `tenant_id = ?` string anywhere in this package, and no `db.Table` / `db.Model` / `db.Raw`**; `go/org` has no allowlist entry in either semgrep rule, deliberately.

### Errors — `errors.go`

`org.node_not_found`, `org.node_name_required`, `org.node_name_too_long`, `org.parent_not_found`, `org.max_depth_exceeded`, `org.cycle_not_allowed`, `org.node_has_children`, `org.root_already_exists`, `org.root_not_deletable`, `org.duplicate_sibling_name`, `org.invalid_node_id`, `org.internal_error`.

Match on `Code` through `apperr.As`, never by identity — `WithParam`/`WithCause` derive a new `*apperr.Error` every time. Every code has a `zh-CN` and an `en-US` entry under the identical id; `TestErrorCatalog_EveryCodeHasBothTranslations` fails if one is added without both.

## Known limitations

**A subtree move is not atomic.** `Move` saves each rewritten row through `dbkit.Repository[OrgNode].Update`, one transaction per row, because `Repository[T]` exposes no transactional batch seam. A process that dies mid-move leaves a partially re-parented subtree whose paths disagree with their parent edges until the move is repeated. The cheap fix — one statement with a SQL `replace()` over the path column — is exactly what the dialect-identity proof forbids. The right fix is a `dbkit` round that grows `Repository[T]` a transactional batch write, with org as one of its two real consumers. A cascading **delete** is already one statement and does not have this problem.

**`Repository[T].WithinSubtree(nodeID)`, which `docs/internal/05` sketches, is not built.** It is a change to *dbkit's* public generic surface, not org's, and `go/dbkit/AGENTS.md` states that surface grows with real consumers' shapes in hand. org exposes descendant ids instead; consumers filter with `IN ?`.

**Sibling-name collisions are pre-checked, and the database is the backstop.** Two concurrent creates racing on the same name both pass the pre-check; the `UNIQUE(tenant_id, parent_id, name)` index rejects the loser, and `mapWriteError` translates `gorm.ErrDuplicatedKey` back into `org.duplicate_sibling_name`, so the caller sees one error for one condition either way.

**Depth and name bounds are enforced in Go, not by the column widths.** SQLite does not enforce `VARCHAR(n)` under type affinity, so a standalone-mode deployment would otherwise accept what the distributed mode rejects.

## Rules

- **Never import `authn`, and never import any other business module's structs.** Users are id strings learned from events. An `authn` type in this module's import graph is a defect, not a shortcut.
- **Never declare `authn.user.created` with `reg.Events.Publishes`.** Only subscribe to it. Declaring another module's event type collides at bootstrap the moment both modules boot in one host; `TestModule_Register_DoesNotDeclareAuthnsEvent` guards it.
- **Never write an `OrgNode` row outside `TreeService`.** It is what keeps `ParentID` and `Path` in lockstep.
- **Never hand-write a tenant filter, and never reach for `db.Table` / `db.Model` / `db.Raw`.** See the Data access section.
- **Never introduce an id scheme outside `[0-9a-f-]`.** See the dialect-identity proof.
- **Never add a recursive CTE or a SQL string function over the `path` column.** Path arithmetic happens in Go.
- **Never localize an error into a returned string.** Return the code; the text lives in `locales/`.
- Migrations are versioned SQL, one file per dialect, byte-identical apart from the header note. No `AutoMigrate`, no `NOW()`, no `gen_random_uuid()`, no native arrays, no JSONB.

## Testing

Unit tests run with no external dependency: `go test ./... -race` from `go/org`. `internal/testutil.NewSQLite` applies the module's **real** migration files from zero through `dbkit.MigrationRegistry`, so every DB-backed test doubles as proof that those files run on an empty database.

`internal/testutil` deliberately does not import `go/org` — org's own tests are in `package org`, so a helper package importing org could not be imported back by them. `NewSQLite` therefore takes a module name and an `embed.FS`. `go/dbkit/internal/testutil` avoids the identical cycle the identical way.

`internal/testutil.NewPostgres` is the integration tier's counterpart, applying the identical files from `postgres/`, so the same assertions can run against both dialects by swapping the constructor alone. The integration tier itself lands with this module's later blocks.
