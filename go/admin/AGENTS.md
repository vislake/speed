# admin

go/admin is the operations-console backend: the platform-staff-facing operations surface over capabilities that already live in every other module. This file is the module-level discipline that ships with `go/admin` to consuming projects; the design rationale is `docs/internal/23-admin.md`, and the repository-wide rules are the root `CLAUDE.md` plus `.claude/skills/backend-coding-standards`.

**Status: round 1 of 2 landed** (docs/internal/23-admin.md section 8's own round split). Round 1 ships D3 (the tenant ledger, event-driven lazy population plus manual CRUD, record-only -- no enforcement), D5 in full (the impersonation pipeline: start/end/list, the request-pipeline identity-substitution middleware, the five mandatory properties, the dual-identity audit trail, the mandatory non-unsubscribable security notification), D6 (cross-tenant user search plus membership composition), and D7's read side (the audit-query HTTP shell over `compliance.AuditQuery`, no export leg). Round 2 (D4's enforcement seam, D8's role management, D9's usage dashboard, D10's notification send-record search, and D7's export leg) is **not** in this codebase yet. Judge what exists by the code, not by this sentence.

## Scope

**In scope this round:**

- **D3 -- tenant ledger** (`model.go`'s `Tenant`, `tenant_repository.go`, `tenant_service.go`): a platform-data table (`admin_tenants`, `dbkit.AssertNotTenantScoped`) recording which tenants the platform believes exist, an operator convenience and never the authoritative source of tenant existence. Populated two ways: `Module.Register` subscribes to org's real `org.node.created` event, and `TenantService.handleOrgNodeCreated` lazily creates an active, blank-`DisplayName` row the first time a tenant's ROOT node appears (`org.OrgNode.IsRoot()`'s real discriminator, `ParentID == ""` -- **not** node depth, a correction this round made against the design document's own initial assumption, verified against `go/org/model.go`); and manual CRUD through `POST`/`GET`/`PATCH /api/v1/admin/tenants`. `TenantService.SetStatus` records an explicit `admin.tenant.status_changed` audit event on every ledger edit. Writing `status=suspended` here is record-only: nothing in the request pipeline refuses a request because of it (D4, round 2).
- **D5 -- impersonation, the full pipeline** (`model.go`'s `ImpersonationGrant`, `impersonation_repository.go`, `impersonation_service.go`, `pipeline.go`): a short-lived (`defaultGrantTTL`, 30 minutes, no renewal), explicitly revocable authorization credential -- **never** a real authn access/refresh token minted for the target user (docs/internal/23-admin.md's D5 rejected-alternative rationale). `POST/DELETE/GET /api/v1/admin/impersonation(/{id})` start, end and list grants. `ImpersonationMiddleware` (see "The impersonation request pipeline" below) substitutes the target user's identity for the rest of one request when a valid `X-Admin-Impersonation` grant id is presented, alongside the administrator's own still-verified access token. The five mandatory properties (credential unchanged, permission check uses the target's own Subject, fail-closed on an invalid/expired/ended grant, dual-identity audit via explicit `audit.Emit`, mandatory non-unsubscribable notification on start) are each pinned by a real test -- see "Testing" below.
- **D6 -- cross-tenant user search** (`search.go`): `authn.Service.SearchUsers` (this round's one purely-additive change to `go/authn`) does the identity lookup; `SearchService.MembershipsOf` composes the second half -- which tenants a user belongs to -- by looping over every tenant in admin's own D3 ledger and calling `org.MemberService.Get` once per candidate tenant under a `tenancy.WithSystemContext` grant (D2's mechanism, below). No bypass method was added to `org`.
- **D7 -- audit query, read side only** (`audit.go`): a thin HTTP shell over `compliance.AuditQuery.Query`/`QueryAcrossTenants`/`Get`, which already implement the entire read side. `GET /api/v1/admin/audit-events` translates its query parameters into `compliance.QueryFilter` and paginates the result in Go (`compliance.AuditQuery` itself does no pagination -- an inherited, not a re-solved, known limitation). The cross-tenant path's tenant list is drawn from D3's own ledger plus the platform-level empty-string pseudo-tenant.
- **D2 -- the cross-tenant read mechanism**, used by D5's notification dispatch, D6's membership composition and D7's cross-tenant query alike: `tenancy.WithSystemContext(ctx, bus, pkgcore.SystemReason{Actor: operatorUserID, Purpose: SystemPurposeAdminCrossTenant})`, then the downstream module's own existing per-tenant method, looped in application code across however many tenants are relevant. Never a `ListAcrossTenants` bypass on any downstream module, never a second unscoped database connection.

**Explicitly out of scope this round** (round 2, per docs/internal/23-admin.md section 8): D4 (the `tenancy.TenantStatusResolver` enforcement seam -- suspend actually blocking requests), D8 (`rbac.Service.DeclaredPermissions()` and role-management HTTP), D9 (the cross-tenant usage/billing dashboard), D10 (`notification.SendRecordRepository`'s filtered-list method and its HTTP surface), and D7's export leg (`POST /api/v1/admin/audit-events/export` via `compliance.ExportService`). `admin.role.assigned`/`admin.role.revoked` are therefore **not** declared as audit actions this round either -- they are `rbac.AssignRole`/`RevokeRole`'s own already-published domain events, which this module does not yet call.

## Corrections against the design document

docs/internal/23-admin.md's own closing line invites exactly this: if a downstream module's real signature has since drifted from what the document cites, the real code wins and the document should be corrected accordingly. This round re-verified every cited signature against the real source before writing code against it, and found three real mismatches:

1. **The root-node discriminator is `OrgNode.ParentID == ""`, not node depth.** The document's D3 section describes `org` events without naming the exact field; this round's own task brief speculated `Depth == 0`. `go/org/model.go`'s real `OrgNode.IsRoot()` is `func (n OrgNode) IsRoot() bool { return n.ParentID == "" }` -- `tenant_service.go`'s `handleOrgNodeCreated` uses that field, and `TestTenantService_HandleOrgNodeCreated_ChildNode_DoesNotRegister` is the regression test that would catch a reversion to depth.
2. **`pkgcore.SystemReason.Actor` is a plain `string`, not a `pkgcore.Actor` struct.** The document's D2 code sample writes `Actor: pkgcore.Actor{Type: ..., ID: staffID}`; the real declaration (`go/pkgcore/tenant.go`) is `SystemReason{Actor string; Purpose SystemPurpose; Ticket string}` -- no `Detail` field either (the document's sample also names one; the real third field is `Ticket`). Every `tenancy.WithSystemContext` call in this module passes the operator's user id as a bare string.
3. **D5's mechanism is a request-pipeline `http.Handler` middleware, not a `tenancy.Resolver` decorator.** The document describes `admin.ImpersonationAwareResolver` "wrapping" `authn.NewPrincipalResolver()`. The real `tenancy.Resolver` interface is `Resolve(r *http.Request) (pkgcore.TenantID, error)` -- a bare tenant id, with no channel to also substitute which USER the rest of the pipeline believes it is talking to (`tenancy.Middleware`'s only side effect is `pkgcore.WithTenant`; it never touches the `authn.Principal` `authn.Middleware` already installed). `pipeline.go`'s `ImpersonationMiddleware` is therefore an ordinary `net/http` middleware inserted between `authn.Middleware` and `tenancy.Middleware` in the chain, reading the real `authn.Principal` and calling `authn.WithPrincipal` to substitute one for the rest of the request -- see `ImpersonationMiddleware`'s own doc comment for the full reasoning. This satisfies every one of the document's actual requirements (the admin's own token is still what verified; neither `authn` nor `tenancy` learns admin exists) while matching what the real interfaces can express.

## Wiring contract

`admin.NewModule(db *gorm.DB, opts ...Option)` never touches the database; `db` is opened and migrated by the host before `Bootstrap` ever calls `Register`, exactly like every other module. Four options are **mandatory** -- `Register` refuses to proceed (before declaring anything) without each:

- `WithAuthn(*authn.Module)` -- **not** `*authn.Service` directly. `authn.Module.Service()` is documented nil until authn's own `Register` has built it, and admin's own `Register` reads `authnModule.Service()` lazily, at Register time, never at option-application time (which runs before `Bootstrap`). This is why `Module.DependsOn()` returns `[]string{"authn"}` -- the one real registration-order dependency this module has; every other runtime read (`orgModule.Members()`, `complianceModule.AuditQuery()`, `notificationModule.Deliveries()`) is safe regardless of order, because each of those three is built inside its own module's `NewModule` constructor, not its `Register`.
- `WithOrg(*org.Module)` -- D6's membership composition and D3's event subscription both need it.
- `WithCompliance(*compliance.Module)` -- D7's audit-query HTTP shell has no read path without one. The host constructs this with the SAME `*audit.Repository` (over the same database connection) its own `dbkit/audit` wiring already uses.
- `WithNotification(*notification.Module)` -- D5's mandatory impersonation-started security notification has no transport without one.

Missing any of the four fails `Bootstrap` with a named error (`ErrAuthnServiceRequired`, `ErrOrgModuleRequired`, `ErrComplianceModuleRequired`, `ErrNotificationModuleRequired`) -- the same "fail with a named missing seam" shape `org`'s `ErrEmailIndexerRequired` and `config`'s `ErrCipherRequired` already use.

admin sits at the top of the module dependency graph (root `CLAUDE.md`'s own diagram: `... -> compliance -> admin`), so unlike most business modules it is explicitly permitted to import the concrete packages of every module below it directly -- `go/authn`, `go/org`, `go/rbac` (conceptually; no import exists in this round's code, since role management is round 2), `go/tenancy`, `go/compliance`, `go/notification`, `go/dbkit`, `go/pkgcore`, `go/observability` -- rather than through a structurally-typed, no-import seam. The one thing it still must not do is create a cross-module foreign key or hand-write another module's `WHERE tenant_id = ?` filter; every cross-tenant read goes through D2's `tenancy.WithSystemContext` plus the downstream module's own existing method.

### The impersonation request pipeline

The fixed middleware chain (`docs/internal/01-architecture.md`) is `authn.Middleware -> tenancy.Middleware(authn.NewPrincipalResolver())`. Impersonation inserts `admin.ImpersonationMiddleware(impersonationSvc)` between the two, never reordering the chain itself:

```go
authn.Middleware(verifier)(
    admin.ImpersonationMiddleware(adminModule.Impersonation())(
        tenancy.Middleware(authn.NewPrincipalResolver(), ...)(mux),
    ),
)
```

A request carrying no `X-Admin-Impersonation` header, or one naming a grant that is missing/expired/ended/belongs to a different administrator, passes through completely unmodified -- the fail-closed contract `ImpersonationMiddleware`'s doc comment and `pipeline_test.go` both pin. A valid grant substitutes an `authn.Principal` naming the target user and target tenant (the administrator's own `SessionID`/`AMR` carried over unchanged -- property (a): the credential is still the admin's own real, still-verified token) and sets `pkgcore.WithActor(target)` + `pkgcore.WithOnBehalfOf(admin)` on the context, so every audit record produced for the rest of the request -- admin's own explicit `Emit` calls and any other module's automatic write capture or explicit `Emit` alike -- carries the correct dual identity with no further wiring.

admin's OWN routes are gated the ordinary way (an `rbac.RequirePermission` wrap in `rbac.SystemDomain`, external to this module, mirroring how the reference app gates `notes`/`storage`) and do **not** sit behind `ImpersonationMiddleware` -- that decorator's effect is on the REST of the application's routes only.

## Data model

Two platform-data tables (`dbkit.AssertNotTenantScoped`, never `dbkit.TenantScoped`), both written through `dbkit.Open()`'s plain `*gorm.DB` directly -- never `dbkit.Repository[T]`, matching `go/authn`'s `users` table and `go/config`'s `row` precedent:

- `admin_tenants` (`Tenant`): `tenant_id` (PK), `display_name`, `status` (`active`/`suspended`), `suspended_reason`, `suspended_at` (nil means not currently suspended -- derived from the `Status` transition by `TenantRepository.Update`, never a caller-supplied value), `created_at`, `created_by` (empty on the event-driven path), `notes`.
- `admin_impersonation_grants` (`ImpersonationGrant`): `id` (PK, a random 128-bit hex credential, never derived from `admin_user_id`/`target_user_id`), `admin_user_id`, `target_user_id`, `target_tenant_id`, `reason` (required), `created_at`, `expires_at`, `ended_at`/`ended_by` (nil/empty until explicitly ended).

## HTTP surface

`go/admin/api/openapi.yaml` is the sixth module fragment (after notes, org, authn, storage, notification), generated by the same pinned oapi-codegen v2.8.0 into a committed `admin-server.gen.go`, implemented by `Handler` behind a `var _ api.ServerInterface` compile-time assertion. Every route requires an `rbac.SystemDomain` permission (`admin:access`, `admin:tenants_manage`, `admin:search_users`, `admin:impersonate`, `admin:audit_read`) and does **not** go through ordinary `tenancy.Middleware` tenant resolution -- these are platform-operations routes about tenants, not routes scoped to one. `Handler` resolves the calling operator's identity from `authn.PrincipalFromContext`, never from a request parameter, header or body.

| Method | Path | Decision |
|---|---|---|
| `GET`/`POST` | `/api/v1/admin/tenants` | D3 |
| `GET`/`PATCH` | `/api/v1/admin/tenants/{id}` | D3 + D4's record-only half |
| `GET` | `/api/v1/admin/users` | D6 |
| `GET` | `/api/v1/admin/users/{id}/memberships` | D6 + D2 |
| `POST`/`GET` | `/api/v1/admin/impersonation` | D5 |
| `DELETE` | `/api/v1/admin/impersonation/{id}` | D5 |
| `GET` | `/api/v1/admin/audit-events` | D7 |

## Testing

`go test ./...` from this directory runs the full unit suite with no external dependency (SQLite via `internal/testutil.NewDB`, real `pkgcore.NewMemoryEventBus`/`NewMemoryKVStore`/`NewConsoleMailer`). Notably:

- `model_test.go` runs `tenancytest.AssertNotTenantScoped` over both tables.
- `tenant_service_test.go` pins the root-node discriminator correction (a root node registers, a child node does not, a cross-replica `map[string]any` payload decodes identically to the publisher's own struct) and the D3 audit trail.
- `impersonation_service_test.go` pins all five of D5's mandatory properties as real, passing tests: dual-identity audit on both start and end, the mandatory notification dispatch (and that a notifier failure never fails `Start`), and `Lookup`'s fail-closed contract against an unknown, expired and explicitly-ended grant.
- `pipeline_test.go` proves `ImpersonationMiddleware` end to end at the HTTP layer: identity substitution on a valid grant, unmodified pass-through on an absent/expired/unknown/cross-administrator grant, and the session-id-carried-over proof for property (a).
- `search_test.go` and `audit_test.go` build real `authn.Module`/`org.Module`/`compliance.Module` instances (no test-only fakes for these three) to prove D6 and D7 against genuine downstream code, not mocks.
- `module_test.go`'s `buildTestAdminModule` wires a complete, real module graph (authn, org, compliance, notification, admin) through one real `pkgcore.NewKernel().Bootstrap` call and proves the wiring end to end: every declaration lands on the shared registry, and a real `org.Tree().CreateRoot` call's published event reaches admin's subscriber through the actual shared bus.
- `example_test.go`'s `Example()` and `go/authn`'s `ExampleService_SearchUsers` are the godoc-compiled usage documentation this round's new public API ships with.

No integration tier (`integration_test/`) exists yet -- everything this round needs is exercised by the in-process SQLite/memory-bus unit tier above.

## Known limitations

- **D3's ledger is not authoritative and enforces nothing.** A tenant can be suspended in the ledger while every request against it keeps succeeding; D4 is round 2.
- **D7's audit-query pagination is Go-side slicing over an already-fully-materialized result**, inherited from `compliance.AuditQuery`'s own identical, documented limitation -- not re-solved here.
- **The impersonation grant's TTL (30 minutes) is a fixed constant, not configurable** through `go/config` yet.
- **No rate limiting or anomaly detection on impersonation** (an administrator starting many grants in a short window) -- docs/internal/23-admin.md's section 9 defers this to `observability`/`compliance`, out of scope for admin itself in either round.
