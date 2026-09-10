# examples/reference-app

speed's mandatory first consumer (root `CLAUDE.md`'s "Reference App" section): every module API this app does not actually use is not considered done. This is still an early-stage skeleton of the full vision — see `docs/internal/15-roadmap.md` for the milestone plan and `docs/internal/14-reference-app.md` for the full, eventual scope (an AI dental smile-simulation platform).

Today this app is the composed whole every module's mandatory-first-consumer proof runs against: `internal/app/server.go`'s `BuildServer` registers seventeen `pkgcore.Module` implementations into one `Kernel.Bootstrap` call — pki, authn, the notes module, org, config, rbac, storage, sharing, integration, the demo identity module, notification, ai-gateway, billing, metering, compliance, admin and the `go/dbkit/audit` persister — while `internal/notes`, `internal/cases`, `internal/consult` and `internal/smilesim` carry the app's own business surfaces on top of them. A second consumer surface sits at the frontend edge of the same composition: `web/`, the consumer shell that mounts the `@speed` packages the way a delivered project would (its own README covers it).

## What's here

| Path | What it is |
|---|---|
| `internal/notes/` | A complete `pkgcore.Module`: a tenant-scoped "Note" resource (`id`, `tenant_id`, `text`, `created_at`) with real SQL migrations (both dialects), a `dbkit.Repository[Note]`-based store (including soft-delete and hard-delete exercises), HTTP handlers, a real zh-CN/en-US locale pair, an OpenAPI fragment, and permission/event/audit-action declarations. Creating a note also records a real audit trail entry — see "Audit trail" below. |
| `internal/cases/` | Product round P2b's Case domain layer, the record structure the P3 web UI will sit on: tenant-scoped `cases` (a treatment scenario carrying the clinic-given patient name/reference, the creator, and `created_at`) and `case_photos` (references to already-uploaded go/storage photo objects, in attachment order) — two `dbkit.Repository[T]`-backed tables created by the app's CREATE TABLE IF NOT EXISTS pattern, each running the `tenancytest.AssertIsolated` suite. Three hand-mounted routes (`internal/app/cases.go`) serve create, the caller's own case list, and detail-with-photos; per-photo simulations stay on the P2a smile-simulation enumeration route. Its own package doc comment records the shape decision, the honest not-built list (no per-case permissioning, no status workflow, no patient deduplication or real PHI handling), and what P3 inherits. |
| `internal/app/` | The importable assembly library the command runs: `BuildServer` registers the seventeen `pkgcore.Module` implementations the opening paragraph names into one `Kernel.Bootstrap` call, opens the dual-dialect database and runs its migrations, seeds the demo identity layer ("Demo accounts" below), and serves HTTP behind the `authn` + `tenancy` middleware chain with rbac permission gates on the module routes — while `ConfigFromEnv` resolves this app's bootstrap surface through `go/pkgcore/config`'s loader (`bootstrap.go`'s loader target pins each variable's exact `APP_` name, its six key-material fields are `derive`-tagged so the loader resolves them from `APP_ROOT_KEY` or their own variable, and the boot proves the target binds every declared key), and the demo identity layer, host-side route guards and self-service provisioner the wiring uses live here too (doc.go states why the assembly is importable and why its exports are what they are). |
| `cmd/server/` | The runnable process shell: `main.go` resolves `ConfigFromEnv`, boots `BuildServer`, initializes `observability` from the resolved options and drives the `http.Server` lifecycle (signal handling, graceful shutdown, the Dockerfile `healthcheck` re-invocation) — the composition, wiring, database opening, migrations and demo seeding all happen inside `internal/app`, which this directory's own suites exercise through the command's tests. |
| `web/` | The consumer shell frontend: the `@speed` packages hosted at exactly the location a delivered consumer project occupies — an external member of the `web/` pnpm workspace, never versioned. Its bootstrap composes the i18n namespaces, the memory session, the api-client and the product-shell view machine, and its vitest suites pin the composed answers over a scripted demo-server fetch stand-in that answers the way the real server does; the real-server leg of the same demo facts is the Go-side suites under `cmd/server/`. See `web/README.md`. |
| `integration_test/` | The Docker-backed integration tier (build-tagged `integration`): boots the real server against real infrastructure and asserts on real answers. |

### Audit trail

`go/dbkit/audit` is this app's mandatory-first-consumer proof: `internal/notes/handler.go`'s `NotesCreateNote` calls `audit.Emit` explicitly, after a note is created, under the already-declared `notes.note.create` audit action, and `internal/app/server.go` wires `audit.New(db)` into the Kernel's module set, sharing notes' own database connection. There is no HTTP endpoint to read the trail back yet (the query/report API is `go/compliance`'s M4 scope) — `flowtests/server_test.go`'s `TestBuildServer_NoteCreate_PersistsAuditEvent` is the executable proof instead, reading the row back through a second `dbkit.Open` connection to the same SQLite file.

This app deliberately does **not** wire `dbkit.Options.AuditBus` (the automatic GORM write-capture mechanism `notes.Note` is otherwise eligible for, via its `AuditResourceType() string { return "note" }` method) onto its own shared database connection — not because doing so would deadlock any more: a later round closed exactly that hazard for this app's own write shape (`dbkit.Repository[Note].Create`, which always runs inside `dbkit.WithTenantSession`) by deferring the write-capture plugin's publish until after the enclosing transaction actually commits, reproduced and proven closed against two real connections to one real SQLite file in `go/dbkit/audit_capture_test.go`'s `TestAuditCapturePlugin_WithTenantSession_SameFileSynchronousPersister_NoLongerDeadlocks`. This app simply never revisited the choice once the underlying hazard it was made to avoid was gone: persisting the audit trail through the same-connection automatic mechanism was never revalidated as worth adopting once a genuinely separate persister connection — the choice this app already makes, below — remained the simpler and clearer option regardless. See `go/dbkit/AGENTS.md`'s "Audit trail collection" section (Known limitation, now marked resolved for this exact shape) and `internal/app/server.go`'s own doc comment on its `dbkit.Open` call for the full write-up.

## Breaking change: environment variable prefix renamed `SPEED_` → `APP_`

Every environment variable this app's own bootstrap code declares and reads used to carry a `SPEED_` prefix. That prefix was never a framework requirement — it is this consumer app's own naming convention, independent of `go/pkgcore/config`'s default `SPEED_` prefix (which is what the loader derives a variable name from when a field does not pin one), and it is now declared to that loader explicitly: `internal/app/bootstrap.go` drives `config.New(config.WithEnvPrefix("APP_"))`, so the `APP_` family is a property of this app's own bootstrap rather than a coincidence of naming. The rename below was a pure rename with no behavior change: an existing `.env` file, `fly secrets`, or CI environment still using the old `SPEED_*` names will simply have no effect — those variables are no longer read, and every affected seam silently falls back to its documented default (or, for `APP_DEPLOYMENT_MODE=distributed` compositions, Bootstrap's own capability validation fails and names the missing seam). Update any deployment configuration you maintain using the mapping below; `PORT` is unaffected — it was never namespaced.

| Old name | New name |
|---|---|
| `SPEED_DEPLOYMENT_MODE` | `APP_DEPLOYMENT_MODE` |
| `SPEED_DB_PATH` | `APP_DB_PATH` |
| `SPEED_CONFIG_KEY` | `APP_CONFIG_KEY` |
| `SPEED_ORG_INDEX_KEY` | `APP_ORG_INDEX_KEY` |
| `SPEED_NOTIFICATION_INDEX_KEY` | `APP_NOTIFICATION_INDEX_KEY` |
| `SPEED_REDIS_ADDR` | `APP_REDIS_ADDR` |
| `SPEED_S3_ENDPOINT` | `APP_S3_ENDPOINT` |
| `SPEED_S3_BUCKET` | `APP_S3_BUCKET` |
| `SPEED_S3_ACCESS_KEY` | `APP_S3_ACCESS_KEY` |
| `SPEED_S3_SECRET_KEY` | `APP_S3_SECRET_KEY` |
| `SPEED_S3_REGION` | `APP_S3_REGION` |
| `SPEED_S3_USE_SSL` | `APP_S3_USE_SSL` |
| `SPEED_SMTP_HOST` | `APP_SMTP_HOST` |
| `SPEED_SMTP_PORT` | `APP_SMTP_PORT` |
| `SPEED_SMTP_USERNAME` | `APP_SMTP_USERNAME` |
| `SPEED_SMTP_PASSWORD` | `APP_SMTP_PASSWORD` |
| `SPEED_SMS_GATEWAY_URL` | `APP_SMS_GATEWAY_URL` |
| `SPEED_DISABLE_QUEUE_WORKER` | `APP_DISABLE_QUEUE_WORKER` |
| `SPEED_DEMO_USERS_PASSWORD` | `APP_DEMO_USERS_PASSWORD` |
| `PORT` | `PORT` (unchanged) |

## Running it

```
cd examples/reference-app
go run ./cmd/server
```

This starts a server on `:8080` (override with `PORT`), backed by a SQLite file `reference-app.db` in the current directory (override with `APP_DB_PATH`), running in the standalone deployment mode (`APP_DEPLOYMENT_MODE=standalone`, the default) with every infrastructure seam resolved from the standalone preset to its in-process implementation — zero external dependencies. Nothing needs to be running to try it.

`APP_DEPLOYMENT_MODE=distributed` is a different story, though less of one than it used to be: the deployment mode only *constrains* which implementations are permissible (see below), and this app's env-driven wiring (`APP_REDIS_ADDR`/`APP_S3_*`/`APP_SMTP_*`/`APP_SMS_GATEWAY_URL`, all documented in `.env.example`) can compose a real, `MultiReplicaSafe` implementation for every registered seam except the database itself — `internal/app/server.go` still hard-codes the SQLite dialect, a deliberate deviation that dialect covers on its own — so a distributed boot with all four sets of variables configured genuinely passes Bootstrap's capability validation and runs (proven both by `examples/reference-app/integration_test/distributed_mode_test.go`, which runs two such replicas against real Redis/RustFS/mailpit, and by `examples/reference-app/docker-compose.distributed.yml`'s own one-container demo, below). Configuring only *some* of them still fails exactly as before: capability validation, not a hard-coded `if mode == "standalone"` check, decides which seam trips first, and `flowtests/server_test.go` pins several such partial-composition failure shapes.

### Real Redis inside a standalone topology

Deployment mode and implementation composition are two orthogonal axes (`docs/internal/03-deployment-modes.md`): the mode constrains which implementations are *permissible* — it never selects one. This app demonstrates the point with no code changes: set `APP_REDIS_ADDR` and the same standalone topology keeps its SQLite file, console mailer and local object store, but the EventBus and KVStore seams become real Redis-backed ones over one shared client:

```
docker run --rm -p 6379:6379 redis:7-alpine
APP_REDIS_ADDR=127.0.0.1:6379 go run ./cmd/server
```

`BuildServer` constructs the go-redis client itself — the app is the assembly host `eventbus/redis`'s `NewEventBus` names as the client's owner, so cleanup closes the bus and the client in turn — and injects the bus via `WithEventBus(redisBus, MultiReplicaSafe|SurvivesRestart)`, the capabilities the Redis implementation genuinely carries (see `go/pkgcore/eventbus/redis/eventbus.go`). Standalone mode requires no capabilities, so the mixed composition passes Bootstrap's validation and runs; every event the app publishes — the notes audit-trail `audit.event.recorded` included — is appended to a real Redis stream before it reaches the in-process subscribers, so any other consumer group (a second replica, an observer process) reads the same events. The example's integration tier proves that crossing end to end: `TestServer_RealRedisEventBusComposition_NotesAuditEventCrossesProcesses` in `integration_test/` boots this very binary against a real testcontainers Redis, creates a note over real HTTP, and sees the audit event arrive in a consumer group owned by the test process, then reads the SQLite row back through `go/dbkit/audit`'s own `Repository`.

Injecting some seams still isn't injecting all of them: `APP_REDIS_ADDR` alone composes both the "eventbus" and the "kv" seam (one shared `*redis.Client` backs both, see `BuildServer`'s own kernel-options doc comment), so `APP_DEPLOYMENT_MODE=distributed APP_REDIS_ADDR=127.0.0.1:6379` with nothing else set now clears those two seams and fails Bootstrap's validation on the next one instead — "mailer", still on its in-process console default — the failure shape `TestBuildServer_DistributedDeploymentMode_RedisConfigured_StillFailsOnMailer` pins. Compose every seam's variables together (`APP_REDIS_ADDR` + `APP_S3_*` + `APP_SMTP_*` + `APP_SMS_GATEWAY_URL`) and the distributed boot succeeds outright — see "Running it in Docker" below for a one-command demo of exactly that.

### Running it in Docker

`Dockerfile` is a multi-stage build — a `node:24-bookworm` frontend stage running this app's own `pnpm build` exactly as CI's npm-package-ci leg does (frozen-lockfile install from the `web/` workspace root, build from the app's web directory), a `golang:1.26.8-bookworm` builder (`CGO_ENABLED=0` since `go/dbkit`'s SQLite driver is pure Go), and a `gcr.io/distroless/static-debian12:nonroot` runtime carrying the static binary, the built frontend at `/app/web/dist` and `APP_WEB_DIST` naming it — so the deployed product URL serves its own page, not just the API (internal/app/frontend.go). Build context is the **repository root**, not this directory — see the Dockerfile's own header for why:

```
docker build -f examples/reference-app/Dockerfile -t speed-reference-app .
```

`docker-compose.yml` is the zero-extra-services profile — one `app` service, SQLite on a named volume, standalone mode, nothing else running:

```
cd examples/reference-app
docker compose up --build
curl localhost:8080/healthz   # ok
curl localhost:8080/          # the app's own page: the composed sign-in surface
```

Data survives a container restart (`docker compose restart app`), since the SQLite file lives on the named volume rather than the container's writable layer.

`docker-compose.distributed.yml` is an override — not a second demo — that layers Redis, RustFS and a mailpit SMTP catcher onto the same `app` service and flips `APP_DEPLOYMENT_MODE` to `distributed`, genuinely passing Bootstrap's capability validation now that every registered seam composes a real, `MultiReplicaSafe` implementation (see the previous two sections):

```
docker compose -f docker-compose.yml -f docker-compose.distributed.yml up --build
curl localhost:8080/healthz        # ok, deployment_mode=distributed in the boot log
open http://localhost:8025         # mailpit's web UI
```

The database stays SQLite on the same volume either way — this app never attempts a second dialect (see "hard-codes the SQLite dialect" above).

### Deploying to Fly.io

`fly.toml`, at the **repository root** (not this directory — the identical build-context reason `Dockerfile`'s own header and "Running it in Docker" above give), is a real, validated Fly.io deployment config for this app: standalone deployment mode, SQLite on a 1GB persistent volume, one `shared-cpu-1x` (1 shared vCPU, 256MB memory) machine that scales to zero when idle. Full prerequisites, the exact command sequence, which secrets to set first, and the free-tier facts this sizing was checked against (as of this writing — Fly's terms change) live in `DEPLOY.md` next to this file.

### Tenants: an access token, not a `Host` header

Every one of this app's own routes (the notes API included) resolves its tenant from the caller's **access token**, never from `Host`: `internal/app/server.go` wires `authn.Middleware(verifier)` ahead of `tenancy.Middleware(authn.NewPrincipalResolver())`, so a request needs a valid, signed-in Principal before it can reach any tenant-scoped route. Two classes of surface are reachable without one: authn's own subtree (`/api/v1/authn`), which this app serves on a branch outside the tenancy chain — dispatched straight from `authn.Middleware`'s output the way the admin subtree is, because authn's routes derive their tenant from the Principal's own claim and its Handler is the per-operation authority on who may call what (`requirePrincipal` answers an anonymous request to a protected operation with authn's coded `authn.authentication_required`; register, sign in, refresh and the social authorize/callback pair need no Principal at all) — and the routes `tenancy.Middleware`'s allowlist names: healthz/metrics, config's two pre-auth display endpoints, sharing's and integration's self-resolving routes and org's accept-invitation path, each exempted for its own reason (`internal/app/server.go`'s composition comment enumerates them). See `internal/app/server.go`'s own doc comment on the middleware chain, and `go/authn/AGENTS.md`'s "The middleware chain is authn, then tenancy" section, for the full reasoning (in short: `tenancy.Resolver`'s signature has nowhere to carry a verified JWT's claims, so verifying the token has to happen first).

`Host` still matters for exactly one thing: `config`'s two pre-auth display endpoints (`/api/v1/config/public`, `/api/v1/config/features`) resolve which tenant's brand to render from a **separate**, hard-coded `Host -> TenantID` lookup (`demoHostTenants`) — a placeholder for the custom-domain table a real deployment would use, documented in that map's own doc comment. Two demo hostnames are pre-wired:

| Host header | Tenant |
|---|---|
| `acme.demo.localhost` | `tenant-acme` |
| `globex.demo.localhost` | `tenant-globex` |

### Demo accounts and the demo identity layer

Reaching a tenant from a browser means signing in as a real account that holds membership there. This app wires authn's host-injected `MembershipReader` to `sign_in_memberships` (see its own doc comment in `internal/app/sign_in_memberships.go`), whose customer-tenant answers are read live from org's own memberships table — the same rows org's invitation accept writes and this app's seed writes, which is what makes a membership survive a server restart instead of dying with the process that granted it. Registering an arbitrary account through the open register route grants it **no** membership anywhere, so its sign-in is refused with the uniform 401 `authn.invalid_credentials` a wrong password also gets — the no-membership reason recorded in the login history, never the response — until something grants it one. Two things grant one on a running server: the boot-time demo-account seed below, and — the real, general path a genuine SaaS deployment relies on — actually accepting an org invitation through org's own HTTP flow (`POST /api/v1/org/invitations/accept`). An invited user who accepts for real can sign in immediately afterward — in the accepting process and in every later process booted against the same database — no header and no server restart required. Three demo accounts come pre-granted when the server boots with `APP_DEMO_USERS_PASSWORD` set — registered through the real register route at boot, then granted membership and roles under each tenant's own context, so the browser flow never needs a special header:

| Account | Roles | Tenants |
|---|---|---|
| `demo-owner@example.com` | built-in owner (every permission any module declared) | every configured tenant (`tenant-acme`, `tenant-globex`) |
| `demo-reader@example.com` | custom `note-reader` role (`notes:read` and nothing else) | every configured tenant |
| `demo-acme-only@example.com` | custom `note-reader` role | `tenant-acme` only — its grant lives in exactly one tenant, which is the point: a grant is a fact about a (tenant, user) pair, never about a user |

All three share the `APP_DEMO_USERS_PASSWORD` value as their password. A fourth account is seeded separately, from its own variable: `demo-platform-staff@example.com` (`internal/app/demo/demo_admin.go`) is the platform-staff demo account whose only membership is the `rbac.SystemDomain` pseudo-tenant, holding the built-in owner role there — every permission any module declared, `go/admin`'s `admin:*` permissions included — and `APP_DEMO_PLATFORM_STAFF_PASSWORD` is the variable that gates its seed. The split is deliberate: the platform administrator must never share the ordinary demo users' passphrase, so a boot seeds each account set only from its own variable (`APP_DEMO_USERS_PASSWORD` seeds exactly the three accounts above, `APP_DEMO_PLATFORM_STAFF_PASSWORD` exactly the staff account). Seeding runs against the real composed register route and is fully idempotent, so it is safe on every boot of one database file: a boot that finds an account already registered re-asserts that account's org memberships and roles under the user id authn assigned on the first boot (every step is repeatable by design), which is what keeps the demo accounts — and the `demo-platform-staff` account's `rbac.SystemDomain` membership — signing in after a restart instead of failing closed until `APP_DB_PATH` points at a fresh file, which used to be the price of the seed's once-per-database run.

Alongside those real accounts, the rbac demonstration keeps a fixed actor set (`demo-owner`, `demo-reader`, `demo-acme-only`) addressable through the `X-Demo-User` request header — and, on the surfaces that attribute work to a user id rather than gate a permission (notes' create handler, the cases surface, org's caller-scoped invitation endpoints, the notification surface), through the `X-Demo-User-Id` header (`internal/app/server.go`'s `DemoOrgUserHeader`). Neither header is **authentication** — an unauthenticated header is a claim, not an identity — and neither is a pattern to copy: both predate authn, and by default they still take precedence over a verified token's Principal by deliberate choice (the pre-auth flows were built around them), with removal deferred to the org-web round. Note what the headers cannot do: the tenant half of the authorization subject always comes from the request context `tenancy.Middleware` resolved server-side, never from anything the caller controls. A request without a header and without a verified Principal fails closed (403 for the rbac gate, the module's own coded 401 for the attribution surfaces). An operator deploying this app where a real, non-demo user might reach it sets `APP_DISABLE_DEMO_USER_HEADER` — that switch disables BOTH demo headers at once, so every gated route and every attribution surface resolves its acting user from the verified access token alone (the same promise DEPLOY.md's header section makes).

### Try it

```
# Boot against a fresh database with the demo accounts enabled. The demo
# platform-staff account (the admin console's demo sign-in) is a separate
# seed under its own variable -- APP_DEMO_PLATFORM_STAFF_PASSWORD -- never
# this one (internal/app/demo/demo_admin.go), and a boot that wants both sets both.
APP_DB_PATH=/tmp/ref.db APP_DEMO_USERS_PASSWORD='a demo passphrase' \
  APP_DEMO_PLATFORM_STAFF_PASSWORD='an admin demo passphrase' \
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
# the uniform 401 authn.invalid_credentials a wrong password also gets,
# the no-membership reason recorded in the login history alone -- a
# grant is a (tenant, user) fact, never a user fact
# (cmd/server/demo_users_test.go pins this shape). The read refusal that
# fails the frontend gate closed lives in web/README.md's gate section.

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

`internal/notes/repository_test.go` runs the mandatory `tenancytest.AssertIsolated` suite against the real repository. `flowtests/server_test.go` builds the real, fully composed handler (via the same `BuildServer` function `main()` calls) and drives it end to end with two different authn-issued access tokens, proving cross-tenant isolation through the actual middleware + handler + repository stack — not a mocked shortcut. That same file's `TestBuildServer_NoteCreate_PersistsAuditEvent` is the audit-trail equivalent: a real POST through the composed stack, then a real read back through `go/dbkit/audit`'s own `Repository.ListByTenant`. `flowtests/authn_e2e_test.go` drives all three of authn's sign-in channels — password, social (against a local test server standing in for GitHub, never a live provider), and phone plus an SMS code (the standalone deployment mode's console sender, captured for the test to read) — each to a working access token that then calls the notes API, plus the self-service session surface (list devices, view login history, revoke one device, and prove that device's refresh now fails while another device's still works). `cmd/server/demo_users_test.go` and `cmd/server/demo_subject_test.go` pin the demo identity layer: the three seeded accounts' sign-in/membership/role answers through the real composed stack, the header's precedence over a verified Principal, and the fail-closed shapes. `flowtests/org_flow_test.go` and `flowtests/storage_flow_test.go` port the same composed-stack treatment onto org's and storage's surfaces.

### The frontend host

The consumer shell's vitest suites (run from the app directory — see `web/README.md` for the exact commands and for what each suite pins) exercise the other half of the composition: the real `@speed` packages bound through the shell's bootstrap and driven through a real api-client, over a scripted demo-server fetch stand-in (`web/src/test-utils/demo-server.ts`) that answers the endpoints a real reference-app server answers — the notes surface's rbac refusals included, so the gate's denied branch runs on a genuine 403 answer, never a locally stubbed shape (any endpoint the demo does not serve fails the test loudly). The stand-in's demo facts cite the Go suites that pin the same facts against the real server; that real-server leg is `cmd/server/demo_users_test.go` and `cmd/server/demo_subject_test.go`, which drive the demo identity layer and the notes gate end to end through the real composed stack. `src/codes-alignment.test.ts` pins the ten surfaces' reachable-error whitelists against the server codes themselves, each cited to the Go source of the sentinel that defines it.
