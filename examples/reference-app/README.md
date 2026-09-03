# examples/reference-app

speed's mandatory first consumer (root `CLAUDE.md`'s "Reference App" section): every module API this app does not actually use is not considered done. This is still an **M0/M1-stage skeleton** of it — see `docs/internal/15-roadmap.md` for the milestone plan and `docs/internal/14-reference-app.md` for the full, eventual scope (an AI dental smile-simulation platform).

Today this app demonstrates real, end-to-end usage of `pkgcore`, `dbkit`, `tenancy`, `observability`, `config` and `authn` through one small, intentionally generic placeholder business module, `internal/notes` — **not** dental/business-specific content. It stands in for the real modules later milestones will add once `org`, `storage`, `ai-gateway`, `billing` and the rest exist.

## What's here

| Path | What it is |
|---|---|
| `internal/notes/` | A complete `pkgcore.Module`: a tenant-scoped "Note" resource (`id`, `tenant_id`, `text`, `created_at`) with real SQL migrations (both dialects), a `dbkit.Repository[Note]`-based store, HTTP handlers, a real zh-CN/en-US locale pair, an OpenAPI fragment, and permission/event/audit-action declarations. Creating a note also records a real audit trail entry — see "Audit trail" below. |
| `cmd/server/` | The runnable entry point: initializes `observability` and serves its `/metrics` Prometheus endpoint, wires the `authn`, notes and `config` Modules (plus `go/dbkit/audit`'s persister `Module`) into a `pkgcore.Kernel`, opens SQLite, runs migrations, and serves HTTP behind the `authn` + `tenancy` middleware chain. |

### Audit trail

`go/dbkit/audit` is this app's mandatory-first-consumer proof: `internal/notes/handler.go`'s `NotesCreateNote` calls `audit.Emit` explicitly, after a note is created, under the already-declared `notes.note.create` audit action, and `cmd/server/server.go` wires `audit.New(db)` into the Kernel's module set, sharing notes' own database connection. There is no HTTP endpoint to read the trail back yet (the query/report API is `go/compliance`'s M4 scope) — `cmd/server/server_test.go`'s `TestBuildServer_NoteCreate_PersistsAuditEvent` is the executable proof instead, reading the row back through a second `dbkit.Open` connection to the same SQLite file.

This app deliberately does **not** wire `dbkit.Options.AuditBus` (the automatic GORM write-capture mechanism `notes.Note` is otherwise eligible for, via its `AuditResourceType() string { return "note" }` method) onto its own shared database connection: doing so deadlocks every note creation into `SQLITE_BUSY`, because the write-capture plugin's publish happens synchronously, inside the same still-open write transaction `dbkit.Repository[Note].Create` holds, and the persister on the other end would try to write into the very same SQLite file. See `go/dbkit/AGENTS.md`'s "Audit trail collection" section (Known limitation) and `cmd/server/server.go`'s own doc comment on its `dbkit.Open` call for the full write-up — a real, empirically-confirmed hazard this app's own wiring surfaced, not a hypothetical one.

## Running it

```
cd examples/reference-app
go run ./cmd/server
```

This starts a server on `:8080` (override with `PORT`), backed by a SQLite file `reference-app.db` in the current directory (override with `SPEED_DB_PATH`), running in the standalone deployment mode (`SPEED_DEPLOYMENT_MODE=standalone`, the default — `distributed` is not wired up in this example yet and fails fast with a clear error, since no PostgreSQL/Redis wiring exists here).

### Tenants: an access token, not a `Host` header

Every one of this app's own routes (the notes API included) resolves its tenant from the caller's **access token**, never from `Host`: `cmd/server/server.go` wires `authn.Middleware(verifier)` ahead of `tenancy.Middleware(authn.NewPrincipalResolver())`, so a request needs a valid, signed-in Principal before it can reach anything but authn's own pre-auth operations (register, sign in, refresh, social authorize/callback) and the two allowlisted routes below. See `server.go`'s own doc comment on the middleware chain, and `go/authn/AGENTS.md`'s "The middleware chain is authn, then tenancy" section, for the full reasoning (in short: `tenancy.Resolver`'s signature has nowhere to carry a verified JWT's claims, so verifying the token has to happen first).

`Host` still matters for exactly one thing: `config`'s two pre-auth display endpoints (`/api/config/public`, `/api/system/features`) resolve which tenant's brand to render from a **separate**, hard-coded `Host -> TenantID` lookup (`demoHostTenants`) — a placeholder for the custom-domain table a real deployment would use, documented in that map's own doc comment. Two demo hostnames are pre-wired:

| Host header | Tenant |
|---|---|
| `acme.demo.localhost` | `tenant-acme` |
| `globex.demo.localhost` | `tenant-globex` |

There is still no seed data and no `@speed/auth-ui` frontend, so the only way to reach a tenant is: register an account, grant it membership by hand (this app's `demoMemberships`, an in-process stand-in for `org`'s still-unbuilt membership store — see its own doc comment in `server.go`), then sign in. `cmd/server/authn_e2e_test.go` does exactly this for all three sign-in channels; the curl walkthrough below does it for one.

### Try it

```
curl -s localhost:8080/healthz
# ok -- no tenant, no credential required

# Register an account (password sign-in; phone+SMS and social work too --
# see authn_e2e_test.go for those two end to end).
curl -s -X POST localhost:8080/api/v1/authn/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"demo@example.com","password":"a perfectly fine passphrase"}'
# {"id":"<user-id>", ...} -- copy <user-id>; nothing today grants it
# membership in a tenant automatically (see "Tenants" above), so signing in
# for tenant-acme below will fail with authn.tenant_membership_required
# until something (a later `org`-round seed path, or your own test code
# using demoMemberships.Grant) grants it one.

curl -s -X POST localhost:8080/api/v1/authn/login/password \
  -H 'Content-Type: application/json' \
  -d '{"identifier":"demo@example.com","password":"a perfectly fine passphrase","tenant_id":"tenant-acme"}'
# {"access_token":"<jwt>", "refresh_token":"...", "principal": {...}}

curl -s -X POST localhost:8080/api/v1/notes \
  -H "Authorization: Bearer <jwt>" -H 'Content-Type: application/json' \
  -d '{"text":"buy milk"}'

curl -s localhost:8080/api/v1/notes -H "Authorization: Bearer <jwt>"

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

`internal/notes/repository_test.go` runs the mandatory `tenancytest.AssertIsolated` suite against the real repository. `cmd/server/server_test.go` builds the real, fully composed handler (via the same `buildServer` function `main()` calls) and drives it end to end with two different authn-issued access tokens, proving cross-tenant isolation through the actual middleware + handler + repository stack — not a mocked shortcut. That same file's `TestBuildServer_NoteCreate_PersistsAuditEvent` is the audit-trail equivalent: a real POST through the composed stack, then a real read back through `go/dbkit/audit`'s own `Repository.ListByTenant`. `cmd/server/authn_e2e_test.go` is the fuller proof: it drives all three of authn's sign-in channels — password, social (against a local test server standing in for GitHub, never a live provider), and phone plus an SMS code (the standalone deployment mode's console sender, captured for the test to read) — each to a working access token that then calls the notes API, plus the self-service session surface (list devices, view login history, revoke one device, and prove that device's refresh now fails while another device's still works).
