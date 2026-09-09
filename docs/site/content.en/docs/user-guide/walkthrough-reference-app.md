---
title: Reference app walkthrough
weight: 3
description: Boot the reference app and drive sign-in, permissions and tenant isolation over real HTTP — demo accounts, access tokens and the error codes a composed speed product answers with.
---

# Reference app walkthrough

`examples/reference-app` is speed's mandatory first consumer: an
AI smile-simulation platform assembled from seventeen `pkgcore.Module`
implementations in one `Kernel.Bootstrap` call, with a demo identity
layer seeded at boot. It is also the fastest way to feel what a
composed speed product does over the wire. This walkthrough runs it in
the standalone deployment mode — one process, one SQLite file, every
infrastructure seam in-process, zero external dependencies — and drives
sign-in, permissions and tenant isolation with real HTTP requests.

## 1. Boot the app with demo accounts

```sh
cd examples/reference-app
APP_DB_PATH=/tmp/ref.db APP_DEMO_USERS_PASSWORD='a demo passphrase' \
  APP_DEMO_PLATFORM_STAFF_PASSWORD='an admin demo passphrase' \
  go run ./cmd/server
```

The first boot runs the migrations and seeds the demo accounts through
the real register route; later boots against the same database file
re-assert the seed idempotently. Health first:

```sh
curl -s localhost:8080/healthz
# ok -- no tenant, no credential required
```

## 2. Tenants, accounts and tokens

Two demo tenants are configured, `tenant-acme` and `tenant-globex`.
Three demo accounts are seeded when `APP_DEMO_USERS_PASSWORD` is set;
all three share its value as their password:

| Account | Role | Tenants |
|---|---|---|
| `demo-owner@example.com` | built-in owner (every permission any module declared) | `tenant-acme`, `tenant-globex` |
| `demo-reader@example.com` | custom `note-reader` (`notes:read` and nothing else) | `tenant-acme`, `tenant-globex` |
| `demo-acme-only@example.com` | custom `note-reader` | `tenant-acme` only |

The pattern to internalize: a grant is a fact about a `(tenant, user)`
pair, never about a user. A separate seed under its own variable,
`APP_DEMO_PLATFORM_STAFF_PASSWORD`, creates the platform-staff account
for the admin console — never the same passphrase as the demo users'.

The tenant travels inside the **access token**, never in a request
header. A sign-in names the tenant it wants; the answered token carries
the caller's membership; every later request sends
`Authorization: Bearer <token>`, and `tenancy.Middleware` resolves the
tenant from the verified principal. There is no `Host`-header routing
and no `X-Tenant-Id` header. (`Host` matters for exactly one thing in
this app: config's two pre-auth display endpoints pick a demo brand
from a hard-coded `acme.demo.localhost`/`globex.demo.localhost` map.)

## 3. Sign in and read as the reader

```sh
TOKEN=$(curl -s -X POST localhost:8080/api/v1/authn/login/password \
  -H 'Content-Type: application/json' \
  -d '{"identifier":"demo-reader@example.com","password":"a demo passphrase","tenant_id":"tenant-acme"}' \
  | jq -r .access_token)
```

The answer carries `access_token`, `refresh_token` and `principal`; if
you do not have `jq`, paste the `access_token` value into the shell
variable by hand. The reader holds `notes:read`, so listing works:

```sh
curl -s localhost:8080/api/v1/notes -H "Authorization: Bearer $TOKEN"
# {"notes":[]}
```

## 4. A refused write: `rbac.permission_denied`

The reader's role grants `notes:read` and nothing else — rbac is deny
by default:

```sh
curl -s -i -X POST localhost:8080/api/v1/notes \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"text":"buy milk"}'
# HTTP/1.1 403 Forbidden
# {"code":"rbac.permission_denied", ...}
```

## 5. Write as the owner

The built-in owner role holds `notes:write` too:

```sh
OWNER=$(curl -s -X POST localhost:8080/api/v1/authn/login/password \
  -H 'Content-Type: application/json' \
  -d '{"identifier":"demo-owner@example.com","password":"a demo passphrase","tenant_id":"tenant-acme"}' \
  | jq -r .access_token)

curl -s -X POST localhost:8080/api/v1/notes \
  -H "Authorization: Bearer $OWNER" -H 'Content-Type: application/json' \
  -d '{"text":"buy milk"}'
# {"id":"<note-id>", "text":"buy milk", ...}

curl -s localhost:8080/api/v1/notes -H "Authorization: Bearer $OWNER"
# {"notes":[{"id":"<note-id>", "text":"buy milk", ...}]}
```

## 6. Tenant isolation: a grant is a `(tenant, user)` fact

`demo-acme-only` holds its membership and reader grant in
`tenant-acme` alone. Signing it in for `tenant-globex` is refused
before any route exists:

```sh
curl -s -i -X POST localhost:8080/api/v1/authn/login/password \
  -H 'Content-Type: application/json' \
  -d '{"identifier":"demo-acme-only@example.com","password":"a demo passphrase","tenant_id":"tenant-globex"}'
# HTTP/1.1 401 Unauthorized
# {"code":"authn.invalid_credentials", ...}
```

That is the same answer a wrong password gets — deliberately. The
account is real and the password right, but the login endpoint must not
certify the password to an anonymous caller; the specific reason (no
resolvable membership) is recorded in the login history, never the
response. Signing the same account into `tenant-acme` succeeds.

## 7. Refusal shapes without identity

The notes route resolves its tenant from a verified principal, so an
anonymous request has nothing to resolve:

```sh
curl -s -i localhost:8080/api/v1/notes
# HTTP/1.1 403 Forbidden
# {"code":"tenancy.tenant_unresolved", ...}

curl -s -i localhost:8080/api/v1/notes -H 'Authorization: Bearer not-a-real-token'
# HTTP/1.1 401 Unauthorized
# {"code":"authn.token_invalid", ...}
```

A presented credential that does not verify is a failed assertion of
identity; an absent one is not an assertion at all. The two are
answered differently, and neither discloses anything.

## 8. What you saw

| Request | Answer | Meaning |
|---|---|---|
| Sign in `demo-reader` in `tenant-acme` | 200, token issued | membership and role resolved from the seed |
| `GET /api/v1/notes` as the reader | 200 `{"notes":[]}` | `notes:read` granted, nothing to read yet |
| `POST /api/v1/notes` as the reader | 403 `rbac.permission_denied` | deny by default; `notes:write` not granted |
| `POST /api/v1/notes` as the owner | 201, note returned | owner role holds the write permission |
| Sign in `demo-acme-only` in `tenant-globex` | 401 `authn.invalid_credentials` | no membership there — answered as a wrong password |
| Anonymous `GET /api/v1/notes` | 403 `tenancy.tenant_unresolved` | no principal to resolve a tenant from |
| `GET /api/v1/notes` with a garbage bearer | 401 `authn.token_invalid` | presented identity fails to verify |

Two demo affordances to know about before copying anything: the
`X-Demo-User`/`X-Demo-User-Id` request headers predate authn and stand
in for real sign-in on some surfaces — they are not authentication, and
a deployment a real user might reach sets `APP_DISABLE_DEMO_USER_HEADER`
to disable both. And the notes you created live in `/tmp/ref.db`: stop
the server with Ctrl-C and boot again against the same path to see the
seed and your notes survive.

## Next steps

- [Error code index](error-codes/) — every code a speed-based API can
  answer with, its status, locale message and triggering condition.
- [Identity and access](domains/identity-access/) — the middleware
  order this walkthrough exercised, and how to wire the same chain in
  your own project.
- [Operating a generated project](operating/) — deployment modes and
  the day-to-day `saasctl` commands.

## Source

- [reference-app README](https://github.com/vislake/speed/blob/main/examples/reference-app/README.md) —
  this walkthrough's commands and answers in their original form.
- [reference-app DEPLOY.md](https://github.com/vislake/speed/blob/main/examples/reference-app/DEPLOY.md) —
  deploying the same app to a real host.
- [authn AGENTS.md](https://github.com/vislake/speed/blob/main/go/authn/AGENTS.md) —
  the token, session and middleware contracts behind the answers above.
