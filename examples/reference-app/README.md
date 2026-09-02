# examples/reference-app

speed's mandatory first consumer (root `CLAUDE.md`'s "Reference App" section): every module API this app does not actually use is not considered done. This is the **M0-stage skeleton** of it — see `docs/internal/15-roadmap.md` for the milestone plan and `docs/internal/14-reference-app.md` for the full, eventual scope (an AI dental smile-simulation platform).

Today this app demonstrates real, end-to-end usage of `pkgcore`, `dbkit`, `tenancy`, and `observability` through one small, intentionally generic placeholder business module, `internal/notes` — **not** dental/business-specific content. It stands in for the real modules later milestones will add once `authn`, `org`, `storage`, `ai-gateway`, `billing` and the rest exist.

## What's here

| Path | What it is |
|---|---|
| `internal/notes/` | A complete `pkgcore.Module`: a tenant-scoped "Note" resource (`id`, `tenant_id`, `text`, `created_at`) with real SQL migrations (both dialects), a `dbkit.Repository[Note]`-based store, HTTP handlers, a real zh-CN/en-US locale pair, an OpenAPI fragment, and permission/event/audit-action declarations. |
| `cmd/server/` | The runnable entry point: initializes `observability` and serves its `/metrics` Prometheus endpoint, wires the notes `Module` into a `pkgcore.Kernel`, opens SQLite, runs migrations, and serves HTTP behind `tenancy.Middleware`. |

## Running it

```
cd examples/reference-app
go run ./cmd/server
```

This starts a server on `:8080` (override with `PORT`), backed by a SQLite file `reference-app.db` in the current directory (override with `SPEED_DB_PATH`), running in the standalone deployment mode (`SPEED_DEPLOYMENT_MODE=standalone`, the default — `distributed` is not wired up in this example yet and fails fast with a clear error, since no PostgreSQL/Redis wiring exists here).

### Tenants, for now

No `authn` module exists yet, so there is no real login. `cmd/server/server.go` resolves the tenant from the HTTP `Host` header via a **hard-coded, clearly-temporary** lookup (`demoHostTenants`) — this is explicitly a placeholder for the `Resolver` `authn` will eventually supply from a verified access token, not a pattern to copy into anything real. Two demo hostnames are pre-wired:

| Host header | Tenant |
|---|---|
| `acme.demo.localhost` | `tenant-acme` |
| `globex.demo.localhost` | `tenant-globex` |
| anything else (including no `Host` at all) | `403 Forbidden` (`tenancy.tenant_unresolved`) |

That third row is deliberate, not an oversight: this app wires its own `strictHostResolver`, not `tenancy.DomainResolver`, in front of the notes API. `DomainResolver`'s default-tenant fallback is documented and tested (see `go/tenancy`) as scoped to unauthenticated, pre-auth *display* decisions only — rendering a brand on a login page before anyone has proven who they are — never to gating a module's real, persisted data. Since notes CRUD is real data and this app has no pre-auth route that would need that leniency, an unrecognized `Host` fails the request instead of falling back to a shared bucket every anonymous caller could read and write. See `strictHostResolver`'s own doc comment in `cmd/server/server.go` for the full reasoning.

### Try it

```
curl -s localhost:8080/healthz
# ok -- no tenant required, works for any Host

curl -s -X POST localhost:8080/api/v1/notes \
  -H 'Host: acme.demo.localhost' -H 'Content-Type: application/json' \
  -d '{"text":"buy milk"}'

curl -s localhost:8080/api/v1/notes -H 'Host: acme.demo.localhost'

curl -s localhost:8080/api/v1/notes -H 'Host: globex.demo.localhost'
# empty list -- tenant-globex never saw tenant-acme's note

curl -s -i localhost:8080/api/v1/notes -H 'Host: some-unrecognized-host.example'
# HTTP/1.1 403 Forbidden -- {"code":"tenancy.tenant_unresolved"}
```

## Testing

```
go build ./...
go vet ./...
go test ./... -race
```

`internal/notes/repository_test.go` runs the mandatory `tenancytest.AssertIsolated` suite against the real repository. `cmd/server/server_test.go` builds the real, fully composed handler (via the same `buildServer` function `main()` calls) and drives it end to end with two different `Host` headers, proving cross-tenant isolation through the actual middleware + handler + repository stack — not a mocked shortcut.
