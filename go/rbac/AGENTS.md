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
| `DefineRole` / `AssignRole` / `RevokeRole` / `RestoreRole` and `RoleDefinition` | `assign.go` |
| The built-in roles (`BuiltinRoleOwner` / `Admin` / `Member`) and `EnsureBuiltinRoles` | `builtin.go` |
| The per-subject decision cache, its TTL expiry and its janitor | `cache.go` |
| `Subject`, `Scope`, `WithSubject` / `SubjectFromContext`, `SystemDomain` | `subject.go` |
| `SubtreeResolver`, the organization-tree seam; `DataScope` and `PathWithinSubtree` | `scope.go` |
| The three models and their table names | `model.go` |
| The three repositories and their filtered reads | `repository.go` |
| The frozen permission catalog (unexported) | `catalog.go` |
| Error vocabulary: every sentinel's code and status | `errors.go` |
| Event and audit-action declarations, the payload structs and their wire decoding | `events.go` |
| `RequirePermission` / `RequirePermissionFunc`, the HTTP gate, and `WithSubjectResolver` | `middleware.go` |
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
| Who is asking | `Subject`, installed on the request context with `WithSubject` and read back with `SubjectFromContext` — or, at the HTTP edge, `RequirePermission` / `RequirePermissionFunc`'s `WithSubjectResolver(func(*http.Request) (Subject, bool))` option when a host's authenticating layer carries identity some other way | the authenticating side (`authn` in production; a demo middleware in the reference app) |
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

`RestoreRole(ctx, sub, role, scope) error` undoes exactly the mark-delete a matching `RevokeRole` made — see "Soft deletion" below. It sides with `AssignRole`'s idempotence, not `RevokeRole`'s strictness: a live binding already at the exact scope is the desired end state already achieved, so it is a no-op, not an error.

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

## Soft deletion

`RoleBinding` implements `dbkit.SoftDeletable` (`DeletedAt *time.Time` / `DeletedBy string`, the exact field shape and tags `examples/reference-app/internal/notes.Note` established as the precedent, and that `go/org`'s `OrgNode`/`Membership` round reused). That flips `dbkit.Repository[RoleBinding].Delete` — called by `RevokeRole`'s `bindings.Delete(ctx, binding.ID)` — from a physical `DELETE` onto a mark-delete `UPDATE`, and gives the model a working `dbkit.Repository[RoleBinding].Restore`, which `Service.RestoreRole` wraps.

**`RoleBinding` alone, of this module's three models.** `Role` and `RolePermission` are untouched: `rbac.Role` has no delete path through the service at all today (`DefineRole` is create-only, and `EnsureBuiltinRoles` only ever reconciles a role's permission set — see D12), so there is nothing on either table to retrofit. `RoleBinding`'s `RevokeRole` is the module's one real delete-shaped operation, exactly why this round targeted it and only it — the identical reasoning `go/org`'s own round applied to pick `OrgNode`+`Membership` over `org_invitations` (already single-use and expiring, a lifecycle shape mark-delete does not improve).

**The partial-unique-index migration, and why it was necessary.** `uq_rbac_role_bindings_tenant_user_role_node` predates this round as a **full**, unscoped index on `(tenant_id, user_id, role_id, node_id)`. A soft-deleted row still occupies whatever unique constraint it held while live, so adopting `SoftDeletable` without narrowing it would have been a real functional regression: a revoked binding's exact scope would stay permanently unavailable to a fresh `AssignRole`, reserved forever by a row nobody can see. `migrations/{sqlite,postgres}/0002_add_soft_delete.sql` (a new, append-only file — `0001` is never edited) adds the two soft-delete columns and then `DROP`s and re-`CREATE`s the index **under the same name**, scoped `WHERE deleted_at IS NULL`, following `go/org/migrations/{sqlite,postgres}/0004_add_soft_delete.sql`'s exact technique (itself following `go/pki/migrations/{sqlite,postgres}/0001_create_pki_signing_keys.sql`'s `uq_pki_signing_keys_active_purpose` for a different reason). Reusing the index name means the collision-handling in `AssignRole` (translating a lost race on it into "already granted") needs no change. `TestService_RevokeRole_ThenAssignRole_SameScope_Succeeds` is this round's own proof that reuse now works, re-run against a real PostgreSQL server by `integration_test/postgres_softdelete_test.go`'s `TestPostgres_RevokeRole_ThenAssignRole_SameScope_Succeeds` — the two engines' partial-index and collation behavior genuinely differ, so the SQLite-only unit proof alone cannot rule out a PostgreSQL-specific mistake.

**`RevokeRole`'s existing error handling needed no change.** `dbkit.Repository[T].Delete` reports `dbkit.ErrRecordNotFound` on zero rows affected whether the branch taken is a physical `DELETE` or, now, a mark-delete `UPDATE` (`go/dbkit/repository.go`'s `Delete`/`softDelete`) — the promoted method's contract is unchanged from `RevokeRole`'s point of view. `RevokeRole`'s own `hasCode(err, dbkit.ErrRecordNotFound.Code)` branch, written for the concurrent-double-revoke race (assign.go's own comment), therefore still fires correctly against a mark-delete `UPDATE` matching zero rows — nothing in that method was touched.

**`RestoreRole` is idempotent like `AssignRole`, not strict like `RevokeRole`.** `Service.RestoreRole(ctx, sub, role, scope) error` takes RevokeRole's own three arguments — deliberately, not a bare binding id the way `org.MemberService.Restore` takes a membership id — so a caller restores exactly what it just revoked without needing to have kept an opaque row id around. A binding's natural key genuinely is the `(tenant, user, role, node)` tuple the unique index enforces, which is why this shape fits here where it did not fit `Membership` (whose per-user key can accumulate several soft-deleted rows for entirely unrelated reasons over a person's whole tenure, not just one recent revoke). A live binding already at the exact scope — most commonly a fresh `AssignRole` since the revoke — means the desired end state already holds, so `RestoreRole` reports success without writing anything rather than attempting an `UPDATE` that would collide with the very index the paragraph above narrowed; this mirrors `AssignRole`'s own idempotent semantics, not `RevokeRole`'s strict ones, because restoring, like assigning, is fundamentally about making a grant *exist*. Otherwise it restores the **most recently revoked** row at that tuple (`RoleBindingRepository.findMostRecentlyRevoked`, ordered `deleted_at DESC, id DESC`): a revoke-then-reassign-then-revoke-again sequence leaves more than one soft-deleted row sharing the identical tuple once the partial index frees each slot in turn, and "restore exactly what I just revoked" means the newest one, never an earlier occupant of the same scope. `TestService_RestoreRole_RestoresTheMostRecentRevoke` pins the ordering; `TestService_RestoreRole_NothingToRestore_IsReported` pins the collapsed not-found signal (a tuple never granted, or one whose every past grant is still live, both report `ErrBindingNotFound` — the identical ambiguity `RevokeRole`'s own not-found path already carries).

**The Restore-safety decision: a dangling node reference is allowed, deliberately, unlike `org`'s dead-parent refusal.** `go/org`'s `TreeService.Restore` refuses to land a node back onto a mark-deleted parent, because `OrgNode` is a **structure** — a node whose parent is invisible corrupts every prefix-scan, ancestor walk and child-creation call built on `Path` agreeing with `ParentID`. `RoleBinding` is not a structure; it is a **leaf row** that only feeds one authorization *decision*, and that decision path already tolerates a `NodeID` that resolves to nothing, by design: `SubtreeResolver.NodePath` reports `ok == false` for a node it does not know, and `Service.DataScope` treats that as "this grant contributes nothing to scope" — denying the row-level view rather than widening to the tenant (see "The two no-import seams" and rule 6 of "The decision surface" above) — exactly the same way it already treats a **live** binding whose node was deleted out from under it sometime after `AssignRole` ran. rbac has never verified node liveness at grant time; only `DataScope` resolution checks it, at decision time, on every call. Restoring a binding whose node has since disappeared therefore reintroduces no new failure mode at all — it recreates a situation the module already handles safely and has always been able to reach without any soft-delete round. `RestoreRole` does re-resolve the **role** by key through the same `s.roles.ByKey` call `AssignRole`/`RevokeRole` both make, so a role that stopped existing (not reachable through this module today — see "`RoleBinding` alone" above — but the check is free) reports `ErrRoleNotFound` rather than restoring a binding that names nothing; that is the full extent of what `RestoreRole` checks before writing. Refusing on node liveness the way `org` does would require rbac to ask `org` whether a node is live, which the module boundary forbids outright — `SubtreeResolver` is the *only* fact rbac may learn about the tree, and only at decision time, never at grant or restore time (see "The two no-import seams" above and root `CLAUDE.md`'s module-boundary rule). See `assign.go`'s `RestoreRole` doc comment for this same reasoning at the call site.

**Cache invalidation and cross-replica convergence.** `EventRoleBindingRestored` (`"rbac.role_binding.restored"`) is published by `RestoreRole` through the same `publishBindingChanged` helper `AssignRole`/`RevokeRole` use, and `Module.Attach` subscribes `Service.onRoleBindingChanged` to it exactly as it already does for the assigned/revoked pair — a restore on one replica invalidates the one affected subject's cached grants on every other replica through the identical bus path, not merely on the writer's own process. This is a correctness requirement, not a nice-to-have: without it, a replica that had already cached the revoked (denied) decision would keep serving it until the anti-loss TTL caught up, which for a *restored* grant is the wrong-direction failure of the exact security property the cache's TTL-plus-events design exists to bound. `integration_test/redis_leg_test.go`'s `TestRedisBus_RestoreOnOneReplica_ConvergesTheOther` proves the convergence against a real distributed bus, mirroring its `...RevokeOnOneReplica...` sibling.

**Known limitation.** `RestoreRole` does not re-validate the restored binding against `AssignRole`'s own preconditions beyond the live-tuple check above (mirroring `org.Membership.Restore`'s identical documented choice) — it changes nothing about the row but its two soft-delete columns. A caller wanting every modern invariant re-checked calls `RevokeRole` (if anything needs undoing first) and a fresh `AssignRole` instead of `RestoreRole`.

## The HTTP gate

rbac mounts no routes of its own (`Module.OpenAPISpec()` returns `nil` — D1, D2). Its entire contribution to the HTTP layer is `RequirePermission(az, permission, opts...)` and `RequirePermissionFunc(az, permissionFor, opts...)`, the gate `docs/internal/01-architecture.md`'s fixed middleware chain names after authentication: `… → authn.Middleware → rbac.RequirePermission → …`.

Both fail closed identically and indistinguishably: no usable `Subject`, an unparseable permission string, and a plain denial all end as `403 rbac.permission_denied` — the three are not told apart in the response so an unauthenticated caller learns nothing about what exists. The one case reported differently is a check that could not be **performed** — an `Authorizer` error, meaning storage was unreachable — which is `500 rbac.storage_error`; the request is blocked either way, but a client sees a retryable failure rather than a permanent denial. The gate is **coarse**: it answers `Can`, not `DataScope`, so passing it means the request may proceed, not that every row it returns is in scope — a handler still filters with `DataScope`.

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
- Do not use `AutoMigrate`. Both dialect files must stay identical in substance; `model_test.go` fails the build when a model's columns and the migrations drift apart, and when an index stops leading with `tenant_id` — checked across EVERY migration file of a dialect since `0002_add_soft_delete.sql`, not only `0001`'s `CREATE TABLE`.

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
| D13 | An HTTP surface for `RestoreRole` | Same reasoning as `go/org`'s identical deferral for its own `Restore` methods: no `OpenAPISpec()` change without a host-side rbac permission route to gate it consistently with the rest of the admin surface D1 defers, and `RestoreRole` is already fully usable as a `Service` call | admin console (M3), with D1 |
| D14 | `RoleBinding.Restore` re-validating against `AssignRole`'s own preconditions | `RestoreRole` changes nothing about the restored row but its two soft-delete columns, mirroring `org.Membership.Restore`'s identical documented choice — see "Soft deletion" | a round that wants stricter Restore semantics |

## Testing

- Unit tier: `go test ./...` from this directory. No Docker, no external dependency — the standalone in-memory seams double as the test doubles.
- `model_test.go` carries the model/migration drift gate and the dual-dialect SQL rules; `repository_test.go` runs `tenancytest.AssertIsolated` against all three repositories plus a cross-tenant test for every filtered read; `module_test.go` applies the migrations from zero to head on SQLite and merges the locale bundle through the very `pkgcore/i18n` builder `Kernel.Bootstrap` uses.
- `scope_test.go` pins the segment-aware prefix rule, including `/g1/r2` vs `/g1/r20` — the classic materialized-path bug, and a required case rather than an optional one. `authorizer_test.go` pins every frozen semantic above. `cache_test.go` runs the concurrency hot spot under `-race`. `events_test.go` round-trips all three binding payloads (and the role-changed one) through `encoding/json` rather than a hand-written map, so a field rename cannot pass while the real bus stops decoding.
- The mark-delete/restore lifecycle lives in `assign_test.go` alongside `AssignRole`/`RevokeRole`'s own tests, never a separate file: `TestService_RevokeRole_ThenAssignRole_SameScope_Succeeds` is the partial-index reuse proof (mirroring `go/org/tree_test.go`'s `TestTreeService_Delete_ThenCreateChild_SameSiblingName_Succeeds`); `TestService_RestoreRole_UndoesTheRevokeAndAnnouncesIt` drives the full revoke → verify-denied-through-`Can`/`ListPermissions` → restore → verify-granted-again round trip and asserts the convergence event; `TestService_RestoreRole_AlreadyLiveAtThisScope_IsANoOp`, `TestService_RestoreRole_NothingToRestore_IsReported` and `TestService_RestoreRole_RestoresTheMostRecentRevoke` pin the three semantics "Soft deletion" above documents.
- Integration tier: `integration_test/`, behind `//go:build integration`, so a plain `go test ./...` never touches it — run with `go test -tags=integration ./...` (Docker required). Three legs, mirroring `go/config/integration_test/` (two) plus `go/org/integration_test/`'s own soft-delete leg (the third, added proactively in the soft-delete round rather than as a later fix commit):
  - `postgres_leg_test.go` runs the migrations from zero to head on a real PostgreSQL server (the second dialect the unit tier's SQLite leg cannot prove) and re-runs `tenancytest.AssertIsolated` and an end-to-end evaluation against it, including the reason `node_id` is an empty-string sentinel rather than `NULL` on the bindings' unique index — PostgreSQL treats `NULL`s as distinct inside a unique index, so a nullable column would silently accept a duplicate tenant-wide grant no single revoke could withdraw.
  - `postgres_softdelete_test.go` re-runs the revoke-then-reassign partial-index proof and the restore round trip (both directions: undoing a revoke, and reporting nothing-to-restore) against the same real PostgreSQL server — the engine whose partial-index syntax and collation genuinely differ from SQLite's, so the unit tier's SQLite-only proof cannot rule out a PostgreSQL-specific mistake in `0002_add_soft_delete.sql`.
  - `redis_leg_test.go` attaches two `Service` instances to one real Redis Streams bus, each with a one-hour cache lifetime so no convergence it proves can be explained by the anti-loss TTL, and shows an assign, a revoke, a restore, and a role-permission change on one replica reach the other — the property the standalone in-memory bus cannot demonstrate, since there is only ever one process to converge. `TestRedisBus_RestoreOnOneReplica_ConvergesTheOther` is this round's own addition, proving `EventRoleBindingRestored`'s cross-replica convergence the identical way its `...RevokeOnOneReplica...` sibling already proves the opposite direction.
