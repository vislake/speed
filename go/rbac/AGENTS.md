# rbac

rbac is speed's role-based access control engine: the module that decides whether a subject may perform an action on a resource, and over which slice of a tenant's organization tree it may do so. It realizes the authorization half of `docs/internal/05-identity-and-access.md`.

**Read this first, before anything else in this file: rbac must never depend on `authn`.** Authorization knows exactly one thing about identity — `Subject{TenantID, UserID}` — and whoever authenticates a request assembles that Subject and calls in. There is no import of `go/authn` anywhere in this module, not in the source, not in the tests, and none may ever be added. The rule is in the root `CLAUDE.md` and in the backend coding standard §1 because the inverse dependency is the classic way an access-control layer becomes untestable and unreusable: an engine that knows how a user is authenticated cannot be reused by a host that authenticates differently, and cannot be tested without standing up a login stack. If you find yourself needing a fact about a user beyond their id, the fix is a seam the host implements, not an import.

## What it is, in one paragraph

A native engine over `dbkit`-backed, tenant-scoped tables. Three tables — roles, the permissions each role grants, and the bindings that give a user a role — plus a frozen in-memory catalog of every `resource:action` string the host's modules declared. Evaluation is `subject → bindings → role → permissions`, narrowed by a materialized-path prefix test when a binding is scoped to an organization subtree.

## Casbin: what the design doc says, and why this module does not use it

`docs/internal/05-identity-and-access.md` originally specified Casbin's `RBAC with domains` model on `casbin/gorm-adapter`. That storage choice was reversed during implementation. The correction, with the full reasoning, is recorded in that document; the short form:

- `casbin_rule` has no `tenant_id` column. The tenant lives inside a policy string (the `v0` domain value), which opts the single most security-critical table in the product out of **all three** tenant-isolation layers at once: the GORM plugin cannot inject a filter, `dbkit.Repository[T]` cannot manage the model, and a PostgreSQL row-level-security policy keying on `app.current_tenant` has no column to compare against. `tenancytest.AssertIsolated` cannot be run against it at all, and every isolation guarantee collapses to "the caller passed the right domain string".
- The adapter holds its own `*gorm.DB` and issues its own queries — the exact shape the backend coding standard §3.2 and the `raw-gorm-bypass` CI rule exist to prevent.
- Casbin's actual value here (a pluggable `model.conf`, ABAC, RESTful matchers) is unused by this design, so it would be two third-party dependencies in every consumer's `go.sum` for machinery nothing calls.

The **semantics** the document pins are preserved exactly: domain = tenant, `resource:action` permission naming, a `"system"` pseudo-tenant for platform-operations grants, materialized-path prefix matching for subtree scope, and a mandatory policy cache.

## Where things live

| Concern | Where |
|---|---|
| `Module`, the Register/Attach wiring seam, `Option`s (`WithSubtreeResolver`, `WithCacheTTL`), `DefaultCacheTTL`, `PermissionRead` / `PermissionManage` | `module.go` |
| `Authorizer`, the interface every consumer programs against, and `Permission(resource, action)` | `authorizer.go` |
| `Service`, the runtime handle `Attach` returns, plus `Can` / `ListPermissions` / `DataScope` and `Close` | `service.go` |
| `DefineRole` / `AssignRole` / `RevokeRole` and `RoleDefinition` | `assign.go` |
| The built-in roles (`BuiltinRoleOwner` / `Admin` / `Member`) and `EnsureBuiltinRoles` | `builtin.go` |
| The per-subject decision cache, its TTL expiry and its janitor | `cache.go` |
| `Subject`, `Scope`, `WithSubject` / `SubjectFromContext`, `SystemDomain` | `subject.go` |
| `SubtreeResolver`, the organization-tree seam; `DataScope` and `PathWithinSubtree` | `scope.go` |
| The three models and their table names | `model.go` |
| The three repositories and their filtered reads | `repository.go` |
| The frozen permission catalog (unexported) | `catalog.go` |
| Error vocabulary: every sentinel's code and status | `errors.go` |
| Event and audit-action declarations, the payload structs and their wire decoding | `events.go` |
| Versioned SQL migrations, one subdirectory per dialect | `migrations/` |
| The bilingual message bundle, one entry per error code | `locales/` |

**Dependencies.** `pkgcore` (registry contracts, tenant context, `EventBus`, `apperr`, the `i18n` catalog contract), `dbkit` (the `*gorm.DB` wrapper, `Repository[T]`, `MigrationRegistry`, `WithTenantSession`) and `tenancy` (test-only: `tenancytest`'s isolation suites). `gorm.io/gorm` and `github.com/google/uuid`. Nothing else, and — see the top of this file — never `authn`.

## The Register/Attach seam

Same two-phase shape `go/config` uses, for the same reason.

- `Register(reg)` declares this module's own vocabulary: the `rbac:read` / `rbac:manage` permissions, three domain events, three audit actions. It performs no I/O, mounts no routes, and deliberately does **not** read the permission registrar.
- `Attach(reg)` is called exactly once, **after** `Kernel.Bootstrap` returns, and freezes the snapshot of `reg.Permissions.Permissions()` — every module's declarations, not just rbac's. Modules register in bootstrap order, so a snapshot taken during `Register` would be partial. A second `Attach` fails with `ErrAlreadyAttached`: for the set that decides whether a grant is legal, a silently different second snapshot is a security difference, not a cosmetic one.

## The two no-import seams

rbac needs two facts from neighbours it must not import. Both arrive as interfaces declared here and implemented by the host.

| Seam | Interface | Implemented by |
|---|---|---|
| Who is asking | `Subject`, installed on the request context with `WithSubject` and read back with `SubjectFromContext` | the authenticating side (`authn` in production; a demo middleware in the reference app) |
| Where an organization node sits | `SubtreeResolver.NodePath(ctx, nodeID) (path string, ok bool, err error)`, wired with `WithSubtreeResolver` | the host (`org`, once it exists) |

`SubjectFromContext` reports an **incomplete** Subject as no subject at all, so every caller's "no subject, deny" branch covers both cases and no caller has to remember to re-check `Valid()`.

`SubtreeResolver` is optional. A host with no organization module wires none, and tenant-wide bindings keep working. What a missing or failing resolver must never do is widen a node-scoped binding into a tenant-wide one: an unresolvable narrowing **denies** (`ErrSubtreeUnresolved`). A resolver error propagates; a resolver that simply does not know the node denies.

## The decision surface

`Authorizer` is what consumers program against; `*Service` implements it.

| Call | Question it answers |
|---|---|
| `Can(ctx, sub, action, resource) (bool, error)` | Does this subject hold `resource:action` **anywhere** in its own tenant? The coarse gate. |
| `DataScope(ctx, sub, action, resource) (DataScope, error)` | Over **which slice of the organization tree** does it hold it? The row filter. |
| `ListPermissions(ctx, sub) ([]string, error)` | Everything it holds anywhere, sorted — the list `authn`'s `/me` renders from. |
| `AssignRole` / `RevokeRole(ctx, sub, role, scope) error` | Grant lifecycle. |
| `DefineRole(ctx, RoleDefinition) (*Role, error)` | Create a custom role and the permissions it carries. |
| `EnsureBuiltinRoles(ctx) error` | Seed and reconcile `owner` / `admin` / `member` in the tenant `ctx` carries. Idempotent; meant to run at every boot. |

Semantics that are frozen, and the reasoning behind each:

1. **Deny by default.** No matching grant is `(false, nil)` — a denial, never an error.
2. **A binding grants only inside its own tenant.** Guaranteed structurally by `Repository[T]`, and the reads use the **Subject's** tenant rather than whatever tenant `ctx` carries, so an ambient context that disagrees with the token cannot redirect the lookup.
3. **Permission matching is exact.** No wildcard grammar exists (D7): `notes:read` does not imply `notes:write`.
4. **An undeclared permission denies at check time and is rejected at grant time** (`ErrUnknownPermission`). Strictness belongs where a typo is still fixable; a check must never turn a request into a 500.
5. **`Can` alone is not sufficient for row-level filtering.** It ignores organization-tree scope and never consults the resolver. A handler returning tenant data must also call `DataScope` and filter with it.
6. **A node-scoped binding that cannot be resolved denies.** No resolver wired, or the node is gone, means that binding contributes nothing to a `DataScope` — never a widening to the tenant. This is the one case where `Can` and `DataScope` legitimately disagree (`Can` true, `DataScope` `Denied`), and it is documented on both.

`AssignRole` is **idempotent**; `RevokeRole` is **strict** (`ErrBindingNotFound`). The asymmetry is deliberate: assignment that finds its work already done has achieved the caller's goal, while revocation that finds nothing to revoke usually has not — the common cause is a scope mismatch, and reporting success there would say access was withdrawn while the user still holds it.

## Built-in roles

Their permission sets are a **function of the frozen catalog**, never a literal list — naming another module's permission here would be exactly the coupling this module exists to avoid.

| Role | Holds | Why |
|---|---|---|
| `owner` | every declared permission | An owner who could not do something could not delegate it either, since delegation is itself a permission. |
| `admin` | everything except `rbac:manage` | Without that one exclusion the two roles are identical and an admin could grant themselves anything an owner has. |
| `member` | nothing | Which permissions an ordinary member should hold is a product decision. Deny by default applies to seeding too. |

`EnsureBuiltinRoles` **reconciles** rather than only creating: adding a module to the host's build must widen every existing tenant's owner, and removing one must narrow it. A run that changes nothing writes nothing and publishes nothing, so a fleet restart does not flush every replica's cache.

## The decision cache

Keyed `(tenant, user)`, holding the subject's grants already flattened through their roles. A stale authorization cache keeps answering "yes" after a revoke, so it is built around invalidation first:

1. **Events.** Every assign, revoke and role change publishes on the `EventBus`; the `Service` subscribes to its own events, so a local write and a remote one converge through one code path. A binding change drops one subject; a role change drops the whole tenant (grants are stored already flattened, and there is no index from role back to subject).
2. **TTL expiry** (`DefaultCacheTTL`, 30s; `WithCacheTTL`) bounds the damage of a lost event to one TTL.
3. **A janitor** reclaims expired entries. `Service.Close()` stops it, is idempotent, and leaves decisions correct — expiry is enforced on read, not by the janitor.

Node **ids** are cached, never resolved paths: resolution happens per decision, so a member who moves in the organization tree changes scope immediately.

A publish failure is reported (`ErrStorage` wrapping the bus error) *after* the local cache is invalidated: the row is committed and this process is already correct, but the other replicas have not been told, and only the caller can decide whether to retry or accept up to one TTL of divergence.

## Data domains and which assertion each table runs

| Table | Domain | Access | Suite |
|---|---|---|---|
| `rbac_roles` | Tenant data | `RoleRepository` (embeds `dbkit.Repository[Role]`) | `tenancytest.AssertIsolated` |
| `rbac_role_permissions` | Tenant data | `RolePermissionRepository` | `tenancytest.AssertIsolated` |
| `rbac_role_bindings` | Link data (user × tenant × role — the `memberships` row of the data-domain table) | `RoleBindingRepository` | `tenancytest.AssertIsolated` |
| The permission catalog | Platform data **with no table** — the frozen in-memory snapshot of what the modules declared | n/a | n/a |

**rbac runs `AssertIsolated` three times and `AssertNotTenantScoped` zero times. The absence of the reverse assertion is deliberate, not an omission:** every table this module owns is tenant-owned, so there is no identity-domain or platform-domain model for it to assert against. The one platform-scoped thing rbac has — the catalog — has no table at all.

`role_bindings.user_id` references `users.id` in `authn` and `role_bindings.node_id` references a node in `org`. Both are **bare id columns**: no foreign key, no import, no struct relation. There are no foreign keys between rbac's own three tables either — the rows a constraint would protect are always written and removed together by the service that owns all three, and cross-table constraints make independently released migrations and cascading deletes unmanageable.

## The `"system"` pseudo-tenant

`SystemDomain` (`"system"`) carries platform-operations grants. It is an **ordinary tenant id** as far as every layer is concerned: its rows are stored through the same repositories, filtered by the same isolation plugin, covered by the same row-level-security policy, and evaluated by the identical code path. Nothing in this module branches on it — which is exactly what makes it trustworthy: there is no widened path to reach by accident. It is **not** a wildcard: a subject in the system domain gains no access to any customer tenant's data by holding it.

## Node paths are resolved, never stored

A binding stores the node's **id**, never its materialized path. `docs/internal/16-verification.md` requires a member's permissions to follow a move in the organization tree immediately; a denormalized path column would be stale at exactly that moment. The path is resolved through `SubtreeResolver` at evaluation time, on every decision.

## Rules

**Boundaries**
- Do not import `go/authn`, in source or in tests. It is an automatic review rejection.
- Do not import `go/org` to learn about the tree. Use `SubtreeResolver`.
- Do not import a concrete infrastructure SDK. Depend on the `pkgcore` interfaces.

**Data access**
- Do not hold a bare `*gorm.DB` and write queries against it. Every repository embeds `dbkit.Repository[T]`.
- Do not use `db.Table`, `db.Model` or `db.Raw`. The filtered reads on this module's repositories follow option 1 of `go/dbkit/AGENTS.md`'s "Known limitations": the query is built on the same `*gorm.DB` layer 1 protects, against a `TenantScoped` model, inside `dbkit.WithTenantSession` so layer 3 is engaged, with a Go-side tenant re-check on every returned row.
- Do not hand-write `WHERE tenant_id = ?`. The plugin injects it; `apply` in `findWithinTenant` only ever adds the module's own business condition.
- Do not add a query helper that returns rows without going through `findWithinTenant`.
- Do not use `AutoMigrate`. Both dialect files must stay identical in substance; `model_test.go` fails the build when a model's columns and the migrations drift apart, and when an index stops leading with `tenant_id`.

**Authorization posture**
- Do not make rbac authorize its own writes. `PermissionRead` / `PermissionManage` give a caller the vocabulary to gate role administration; rbac does not check them itself. An engine that authorized its own writes would need a special case for "who may grant the first role", and special cases in an authorization engine are where the holes live. Same posture as `config.Set`.
- Do not let a permission **check** return an error for an unknown permission — it denies. Errors are for the **grant** path (`ErrUnknownPermission`), because an error is far easier to accidentally treat as "allow" than a plain `false`.
- Do not treat `RoleBindingChangedEvent.ActorUserID` as an audit record. It is best-effort — the acting subject from `ctx` when the host installed one — because rbac takes no actor parameter and no audit-record layer exists yet (D10).
- Do not read a value from the `config` module. The cache lifetime is `DefaultCacheTTL` plus `WithCacheTTL`, deliberately not a dynamic config item: reading one would add an `rbac → config` edge the dependency graph does not have.

## Known limitations (deliberately deferred)

| # | Deferred | Why | Where it belongs |
|---|---|---|---|
| D1 | Role-management REST API; `OpenAPISpec()` returns nil | No consumer until the role-configuration page exists — the same posture `go/config` already ships with | admin console (M3) |
| D2 | A `/me` flat permission list endpoint | `authn` owns `/me`; rbac supplies the evaluation call its handler makes | authn |
| D3 | `dbkit.Repository[T].WithinSubtree(nodeID)` | `dbkit/AGENTS.md` warns against speculative generic growth with no consumer, and the real query shape is unknown until `org` exists. rbac ships the scope value and the prefix predicate; consumers apply them | dbkit, once org lands |
| D4 | `pkgcore.PermissionDecl{Key, Description, Group}` | Only the admin console's auto-rendered form needs the metadata, and changing `PermissionRegistrar.Add(perms ...string)` is a breaking change to a frozen API `notes` already calls | admin console |
| D5 | Role-to-role inheritance (Casbin's `g` grouping) | Not requested anywhere in the design; "permissions inherit along the tree" is **org-tree** inheritance, which the subtree seam implements | a round with a real need |
| D6 | Cache TTL as a tenant-overridable dynamic config item | Would add an `rbac → config` edge the graph does not have | a round willing to adjudicate that edge |
| D7 | Permission wildcards (`billing:*`) | A wildcard grammar is a security surface needing a design decision, not an implementation guess | a design round |
| D8 | The thousand-node prefix-match benchmark the design asks for | rbac cannot build a thousand-node organization tree; only `org` can | org |
| D9 | Subscribing to `org.member.removed` to reap orphaned bindings | `org` publishes no events yet | org |
| D10 | Impersonation dual-identity (`Actor` + `OnBehalfOf`) on rbac audit records | The audit-record persistence layer does not exist | compliance / admin |
| D11 | Frontend `usePermission` hooks | Web workspace | a web round |
| D12 | Editing a custom role after creation (`DefineRole` is create-only; only `EnsureBuiltinRoles` reconciles) | Role mutation is part of the role-configuration surface D1 defers, and shipping a narrower public API is the reversible choice under lockstep versioning | admin console (M3), with D1 |

## Testing

- Unit tier: `go test ./...` from this directory. No Docker, no external dependency — the standalone in-memory seams double as the test doubles.
- `model_test.go` carries the model/migration drift gate and the dual-dialect SQL rules; `repository_test.go` runs `tenancytest.AssertIsolated` against all three repositories plus a cross-tenant test for every filtered read; `module_test.go` applies the migrations from zero to head on SQLite and merges the locale bundle through the very `pkgcore/i18n` builder `Kernel.Bootstrap` uses.
- `scope_test.go` pins the segment-aware prefix rule, including `/g1/r2` vs `/g1/r20` — the classic materialized-path bug, and a required case rather than an optional one. `authorizer_test.go` pins every frozen semantic above. `cache_test.go` runs the concurrency hot spot under `-race`. `events_test.go` round-trips both payloads through `encoding/json` rather than a hand-written map, so a field rename cannot pass while the real bus stops decoding.
- Integration tier: when this module gains one it goes in `integration_test/` behind `//go:build integration`, so a plain `go test ./...` never touches it. There is no such directory yet — the unit tier above is the whole suite today, and the SQLite leg of the dual-dialect rule is what `newRBACTestDB` exercises; the PostgreSQL leg is not proven against a real server yet.
