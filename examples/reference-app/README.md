# examples/reference-app

speed's mandatory first consumer (root `CLAUDE.md`'s "Reference App" section): every module API this app does not actually use is not considered done. This is still an early-stage skeleton of the full vision — see `docs/internal/15-roadmap.md` for the milestone plan and `docs/internal/14-reference-app.md` for the full, eventual scope (an AI dental smile-simulation platform).

Today this app demonstrates real, end-to-end usage of `pkgcore`, `dbkit`, `tenancy`, `observability`, `config`, `authn`, `org`, `rbac`, `storage` and `jobs` through one small, intentionally generic placeholder business module, `internal/notes` — **not** dental/business-specific content. It stands in for the real modules later milestones will add once `ai-gateway`, `billing` and the rest exist. A second consumer surface sits at the frontend edge of the same composition: `web/`, the consumer shell that mounts the `@speed` packages the way a delivered project would (its own README covers it).

## What's here

| Path | What it is |
|---|---|
| `internal/notes/` | A complete `pkgcore.Module`: a tenant-scoped "Note" resource (`id`, `tenant_id`, `text`, `created_at`) with real SQL migrations (both dialects), a `dbkit.Repository[Note]`-based store (including soft-delete and hard-delete exercises), HTTP handlers, a real zh-CN/en-US locale pair, an OpenAPI fragment, and permission/event/audit-action declarations. Creating a note also records a real audit trail entry — see "Audit trail" below. |
| `cmd/server/` | The runnable entry point: initializes `observability` and serves its `/metrics` Prometheus endpoint, wires the `authn`, `org`, `rbac`, `config`, `storage` and notes Modules (plus `go/dbkit/audit`'s persister `Module`, and `go/jobs`'s standalone `Queue` as `storage`'s derive-task queue) into a `pkgcore.Kernel`, opens the dual-dialect database and runs its migrations, seeds the demo identity layer ("Demo accounts" below), and serves HTTP behind the `authn` + `tenancy` middleware chain with rbac permission gates on the module routes. |
| `web/` | The consumer shell frontend: the `@speed` packages hosted at exactly the location a delivered consumer project occupies — an external member of the `web/` pnpm workspace, never versioned. Its bootstrap composes the i18n namespaces, the memory session, the api-client and the product-shell view machine, and its vitest suites pin the composed answers over a scripted demo-server fetch stand-in that answers the way the real server does; the real-server leg of the same demo facts is the Go-side suites under `cmd/server/`. See `web/README.md`. |
| `integration_test/` | The Docker-backed integration tier (build-tagged `integration`): boots the real server against real infrastructure and asserts on real answers. |

### Audit trail

`go/dbkit/audit` is this app's mandatory-first-consumer proof: `internal/notes/handler.go`'s `NotesCreateNote` calls `audit.Emit` explicitly, after a note is created, under the already-declared `notes.note.create` audit action, and `cmd/server/server.go` wires `audit.New(db)` into the Kernel's module set, sharing notes' own database connection. There is no HTTP endpoint to read the trail back yet (the query/report API is `go/compliance`'s M4 scope) — `cmd/server/server_test.go`'s `TestBuildServer_NoteCreate_PersistsAuditEvent` is the executable proof instead, reading the row back through a second `dbkit.Open` connection to the same SQLite file.

This app deliberately does **not** wire `dbkit.Options.AuditBus` (the automatic GORM write-capture mechanism `notes.Note` is otherwise eligible for, via its `AuditResourceType() string { return "note" }` method) onto its own shared database connection: doing so deadlocks every note creation into `SQLITE_BUSY`, because the write-capture plugin's publish happens synchronously, inside the same still-open write transaction `dbkit.Repository[Note].Create` holds, and the persister on the other end would try to write into the very same SQLite file. See `go/dbkit/AGENTS.md`'s "Audit trail collection" section (Known limitation) and `cmd/server/server.go`'s own doc comment on its `dbkit.Open` call for the full write-up — a real, empirically-confirmed hazard this app's own wiring surfaced, not a hypothetical one.

## Running it

```
cd examples/reference-app
go run ./cmd/server
```

This starts a server on `:8080` (override with `PORT`), backed by a SQLite file `reference-app.db` in the current directory (override with `SPEED_DB_PATH`), running in the standalone deployment mode (`SPEED_DEPLOYMENT_MODE=standalone`, the default) with every infrastructure seam resolved from the standalone preset to its in-process implementation — zero external dependencies. Nothing needs to be running to try it.

`SPEED_DEPLOYMENT_MODE=distributed` is a different story: the deployment mode only *constrains* which implementations are permissible (see below), and this app never composes the implementations a distributed deployment would need (PostgreSQL, a Redis-backed KVStore, SMTP, S3) — so a distributed boot fails inside the Kernel's own composition validation with pkgcore's `ErrCapabilityUnsatisfied`, naming the seam, the implementation, the missing capability and the mode. That is the design working, not an app-level refusal: capability validation, not a hard-coded `if mode == "standalone"` check, is what decides. `cmd/server/server_test.go` pins both failure shapes.

### Real Redis inside a standalone topology

Deployment mode and implementation composition are two orthogonal axes (`docs/internal/03-deployment-modes.md`): the mode constrains which implementations are *permissible* — it never selects one. This app demonstrates the point with no code changes: set `SPEED_REDIS_ADDR` and the same standalone topology keeps its SQLite file, in-process KVStore, console mailer and local object store, but the EventBus seam becomes a real Redis Streams bus:

```
docker run --rm -p 6379:6379 redis:7-alpine
SPEED_REDIS_ADDR=127.0.0.1:6379 go run ./cmd/server
```

`buildServer` constructs the go-redis client itself — the app is the assembly host pkgcore's `NewRedisEventBus` names as the client's owner, so cleanup closes the bus and the client in turn — and injects the bus via `WithEventBus(redisBus, MultiReplicaSafe|SurvivesRestart)`, the capabilities the Redis implementation genuinely carries (see `go/pkgcore/redis_eventbus.go`). Standalone mode requires no capabilities, so the mixed composition passes Bootstrap's validation and runs; every event the app publishes — the notes audit-trail `audit.event.recorded` included — is appended to a real Redis stream before it reaches the in-process subscribers, so any other consumer group (a second replica, an observer process) reads the same events. The example's integration tier proves that crossing end to end: `TestServer_RealRedisEventBusComposition_NotesAuditEventCrossesProcesses` in `integration_test/` boots this very binary against a real testcontainers Redis, creates a note over real HTTP, and sees the audit event arrive in a consumer group owned by the test process, then reads the SQLite row back through `go/dbkit/audit`'s own `Repository`.

Injecting one seam composes only that one: `SPEED_DEPLOYMENT_MODE=distributed SPEED_REDIS_ADDR=127.0.0.1:6379` still fails Bootstrap's validation on the KV seam, which remains the in-process preset implementation a distributed deployment cannot use — the failure shape `TestBuildServer_DistributedDeploymentMode_InjectedEventBus_StillFailsOnKV` pins.

### Tenants: an access token, not a `Host` header

Every one of this app's own routes (the notes API included) resolves its tenant from the caller's **access token**, never from `Host`: `cmd/server/server.go` wires `authn.Middleware(verifier)` ahead of `tenancy.Middleware(authn.NewPrincipalResolver())`, so a request needs a valid, signed-in Principal before it can reach anything but authn's own pre-auth operations (register, sign in, refresh, social authorize/callback) and the two allowlisted routes below. See `server.go`'s own doc comment on the middleware chain, and `go/authn/AGENTS.md`'s "The middleware chain is authn, then tenancy" section, for the full reasoning (in short: `tenancy.Resolver`'s signature has nowhere to carry a verified JWT's claims, so verifying the token has to happen first).

`Host` still matters for exactly one thing: `config`'s two pre-auth display endpoints (`/api/config/public`, `/api/system/features`) resolve which tenant's brand to render from a **separate**, hard-coded `Host -> TenantID` lookup (`demoHostTenants`) — a placeholder for the custom-domain table a real deployment would use, documented in that map's own doc comment. Two demo hostnames are pre-wired:

| Host header | Tenant |
|---|---|
| `acme.demo.localhost` | `tenant-acme` |
| `globex.demo.localhost` | `tenant-globex` |

### Demo accounts and the demo identity layer

Reaching a tenant from a browser means signing in as a real account that holds membership there. This app wires authn's host-injected `MembershipReader` to `demoMemberships`, an in-process roster (see its own doc comment in `server.go`): registering an arbitrary account through the open register route grants it **no** membership anywhere, so its sign-in is refused with `authn.tenant_membership_required` until something grants it one. Three demo accounts come pre-granted when the server boots with `SPEED_DEMO_USERS_PASSWORD` set — registered through the real register route at boot, then granted membership and roles under each tenant's own context, so the browser flow never needs a special header:

| Account | Roles | Tenants |
|---|---|---|
| `demo-owner@example.com` | built-in owner (every permission any module declared) | every configured tenant (`tenant-acme`, `tenant-globex`) |
| `demo-reader@example.com` | custom `note-reader` role (`notes:read` and nothing else) | every configured tenant |
| `demo-acme-only@example.com` | custom `note-reader` role | `tenant-acme` only — its grant lives in exactly one tenant, which is the point: a grant is a fact about a (tenant, user) pair, never about a user |

All three share the `SPEED_DEMO_USERS_PASSWORD` value as their password. Seeding runs against the real composed register route, so it only happens once per database file: a boot against a database that already carries the accounts logs a warning and leaves them alone (their memberships live in the in-process roster, which does not survive a restart, and inventing grants would misrepresent state) — point `SPEED_DB_PATH` at a fresh file to re-seed.

Alongside those real accounts, the rbac demonstration keeps a fixed actor set (`demo-owner`, `demo-reader`, `demo-acme-only`) addressable through the `X-Demo-User` request header. This is **not authentication** — an unauthenticated header is a claim, not an identity — and it is not a pattern to copy: it predates authn, it still takes precedence over a verified token's Principal by deliberate choice (the pre-auth flows were built around it), and its removal is deferred to the org-web round. Note what the header cannot do: the tenant half of the authorization subject always comes from the request context `tenancy.Middleware` resolved server-side, never from anything the caller controls. A request without the header and without a verified Principal fails closed (403).

### Try it

```
# Boot against a fresh database with the demo accounts enabled.
SPEED_DB_PATH=/tmp/ref.db SPEED_DEMO_USERS_PASSWORD='a demo passphrase' \
  go run ./cmd/server

curl -s localhost:8080/healthz
# ok -- no tenant, no credential required

# Sign in as the demo reader: membership and the note-reader role are
# pre-seeded for it in tenant-acme.
curl -s -X POST localhost:8080/api/v1/authn/login/password \
  -H 'Content-Type: application/json' \
  -d '{"identifier":"demo-reader@example.com","password":"a demo passphrase","tenant_id":"tenant-acme"}'
# {"access_token":"<jwt>", "refresh_token":"...", "principal": {...}}

# The list reads fine: the rbac gate on the notes route answers notes:read.
curl -s localhost:8080/api/v1/notes -H "Authorization: Bearer <jwt>"
# {"notes":[]}

# Creating is refused: demo-reader carries notes:read and nothing else.
curl -s -i -X POST localhost:8080/api/v1/notes \
  -H "Authorization: Bearer <jwt>" -H 'Content-Type: application/json' \
  -d '{"text":"buy milk"}'
# HTTP/1.1 403 Forbidden -- {"code":"rbac.permission_denied", ...}

# Sign in as the demo owner instead: the built-in owner role holds
# notes:write too.
curl -s -X POST localhost:8080/api/v1/authn/login/password \
  -H 'Content-Type: application/json' \
  -d '{"identifier":"demo-owner@example.com","password":"a demo passphrase","tenant_id":"tenant-acme"}'
curl -s -X POST localhost:8080/api/v1/notes \
  -H "Authorization: Bearer <jwt>" -H 'Content-Type: application/json' \
  -d '{"text":"buy milk"}'
# {"id":"<note-id>", ...}

# demo-acme-only holds membership in tenant-acme alone, so signing it in
# with tenant_id=tenant-globex is refused before it reaches any route:
# 403 authn.tenant_membership_required -- a grant is a (tenant, user)
# fact, never a user fact (cmd/server/demo_users_test.go pins this
# shape). The read refusal that fails the frontend gate closed lives in
# web/README.md's gate section.

curl -s -i localhost:8080/api/v1/notes
# HTTP/1.1 403 Forbidden -- {"code":"tenancy.tenant_unresolved"} -- no
# Authorization header at all, so there is no Principal to resolve a
# tenant from.

curl -s -i localhost:8080/api/v1/notes -H 'Authorization: Bearer not-a-real-token'
# HTTP/1.1 401 Unauthorized -- a credential that does NOT verify is a
# FAILED assertion of identity, answered differently from an absent one
# (go/authn/middleware.go's Middleware doc comment).
```

## Testing

```
go build ./...
go vet ./...
go test ./... -race
```

The unit suite needs no Docker and no external services. A second tier, `integration_test/` (tagged `//go:build integration`), exercises the same real composition against real infrastructure and does need Docker running (it starts Redis via testcontainers-go):

```
go test -tags=integration ./...
```

`internal/notes/repository_test.go` runs the mandatory `tenancytest.AssertIsolated` suite against the real repository. `cmd/server/server_test.go` builds the real, fully composed handler (via the same `buildServer` function `main()` calls) and drives it end to end with two different authn-issued access tokens, proving cross-tenant isolation through the actual middleware + handler + repository stack — not a mocked shortcut. That same file's `TestBuildServer_NoteCreate_PersistsAuditEvent` is the audit-trail equivalent: a real POST through the composed stack, then a real read back through `go/dbkit/audit`'s own `Repository.ListByTenant`. `cmd/server/authn_e2e_test.go` drives all three of authn's sign-in channels — password, social (against a local test server standing in for GitHub, never a live provider), and phone plus an SMS code (the standalone deployment mode's console sender, captured for the test to read) — each to a working access token that then calls the notes API, plus the self-service session surface (list devices, view login history, revoke one device, and prove that device's refresh now fails while another device's still works). `cmd/server/demo_users_test.go` and `demo_subject_test.go` pin the demo identity layer: the three seeded accounts' sign-in/membership/role answers through the real composed stack, the header's precedence over a verified Principal, and the fail-closed shapes. `org_flow_test.go` and `storage_flow_test.go` port the same composed-stack treatment onto org's and storage's surfaces.

### The frontend host

The consumer shell's vitest suites (run from the app directory — see `web/README.md` for the exact commands and for what each suite pins) exercise the other half of the composition: the real `@speed` packages bound through the shell's bootstrap and driven through a real api-client, over a scripted demo-server fetch stand-in (`web/src/test-utils/demo-server.ts`) that answers the endpoints a real reference-app server answers — the notes surface's rbac refusals included, so the gate's denied branch runs on a genuine 403 answer, never a locally stubbed shape (any endpoint the demo does not serve fails the test loudly). The stand-in's demo facts cite the Go suites that pin the same facts against the real server; that real-server leg is `cmd/server/demo_users_test.go` and `demo_subject_test.go`, which drive the demo identity layer and the notes gate end to end through the real composed stack. `src/codes-alignment.test.ts` pins the four surfaces' reachable-error whitelists against the server codes themselves, each cited to the Go source of the sentinel that defines it.
