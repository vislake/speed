# authn

Authentication: who a caller is. Never what they may do.

Design rationale lives in `docs/internal/05-identity-and-access.md` (Chinese). This
file is the discipline that ships with the module to consuming projects.

> **Scope note.** This file currently documents what has landed: identity models
> and dual-dialect migrations, password authentication, access-token issue and
> verification, refresh rotation with replay detection, sessions and tenant
> switching, the middleware chain, and module registration. Enterprise SSO, the
> five social providers, SMS, TOTP and recovery codes, rate limiting and the HTTP
> surface land in later blocks of the same round and extend this file rather than
> replacing it. Judge what exists by the tree, not by this note.

---

## Scope

| In | Out |
|---|---|
| Users, sessions, refresh tokens, login history | Roles, permissions, policy evaluation (`rbac`) |
| Password storage and verification (argon2id) | Memberships, the organization tree (`org`) |
| Access-token issue and verification (Ed25519 JWT) | Sending notifications (`notification` subscribes to this module's events) |
| Refresh rotation, replay detection, session revocation | Authorization decisions of any kind |
| Tenant switching within one session | |

**`rbac` must never import this package.** Authorization takes a tenant and a
user, assembled by whoever authenticated. The dependency runs one way, and an
import in the other direction is a merge blocker rather than a style note.

---

## Public API

### Wiring

| Symbol | Purpose |
|---|---|
| `NewModule(db, opts...) (*Module, error)` | The `pkgcore.Module`. Options are validated eagerly, so a missing key is a startup error. |
| `NewService(db, bus, kv, opts...) (*Service, error)` | The service alone, for a host that does not bootstrap through a registry. |
| `RegisterPIISerializer(cipher) error` | Registers the field-encryption serializer under `SerializerName`. **Call before opening the `*gorm.DB`.** |
| `WithSigningKeys`, `WithBlindIndexKey` | **Required.** No safe default exists for either. |
| `WithMembershipReader` | The seam through which membership is asked. Absent means "refuse", not "allow". |
| `WithClock`, `WithIssuer`, `WithAccessTokenTTL`, `WithRefreshTokenTTL`, `WithSessionTTL`, `WithRevocationMode`, `WithPasswordParams`, `WithPasswordPolicy` | Everything else. A nil or non-positive value leaves the default in place. |

### Authentication

| Symbol | Purpose |
|---|---|
| `Principal` | The authentication result: user, current tenant, session, AMR. No roles, no permissions. |
| `Service.Register / Login / Refresh / SwitchTenant / Logout` | The flows. |
| `MembershipReader` | `ActiveMembership` and `TenantsOf`. Implemented by the host, by `org` once it lands. |

### Tokens and passwords

| Symbol | Purpose |
|---|---|
| `TokenKey`, `KeySet`, `NewKeySet(active, retired...)`, `GenerateTokenKey` | Ed25519 key material and rotation. |
| `Signer.Issue`, `Verifier.Verify` | Access tokens, algorithm-pinned to EdDSA. |
| `HashPassword`, `VerifyPassword`, `NeedsRehash`, `PasswordParams`, `PasswordPolicy` | argon2id with PHC-encoded parameters. |

### HTTP

| Symbol | Purpose |
|---|---|
| `Middleware(verifier, opts...)` | Optional authentication. Puts a `Principal` in the context. |
| `RequireAuthenticated(next)` | Per-route enforcement. |
| `NewPrincipalResolver()` | Adapts the verified `Principal` to `tenancy.Resolver`. |
| `PrincipalFromContext`, `WithPrincipal` | Context access. |
| `RevocationChecker`, `WithRevocationChecker` | The immediate-revocation enforcement point. |

---

## Rules

### Do not put a `tenant_id` on any identity table

`users`, `sessions`, `refresh_tokens` and `login_attempts` are identity-domain
data (`docs/internal/04-data-and-tenancy.md`). A person belongs to several
tenants, so scoping the person to one makes the multi-tenant case
unrepresentable. None of these models may implement `dbkit.TenantScoped`, and
embedding `dbkit.TenantModel` into one is caught by
`tenancytest.AssertNotTenantScoped` in `model_test.go`.

`sessions.current_tenant_id` is the one column that looks like an exception and
is not. It records which tenant the session's access tokens are currently issued
for, so a refresh knows what to mint. Membership is re-verified against it on
every refresh rather than trusted.

### Repositories hold a plain `*gorm.DB`, and that is the documented pattern

`dbkit.Repository[T]` is constrained to `T: dbkit.TenantScoped`, which identity
data must not satisfy — see `go/dbkit/AGENTS.md`'s "Known limitations". The
compensating controls are the assertion above plus two rules that apply to
`repository.go` specifically:

* **No `.Table`, `.Model` or `.Raw`.** Nothing here needs them: every conditional
  update passes its target struct to `Updates`, from which GORM parses the same
  schema `.Model` would have named. A semgrep rule in repo-checks watches these
  three entry points.
* **No hand-written `WHERE tenant_id = ?`.** There is no such column to filter on,
  and writing one would mean the model was put in the wrong data domain.

Keep every GORM call in `repository.go`. Nothing else in the module imports gorm.

### Do not reimplement the blind indexer

Email and phone are encrypted at rest and therefore unqueryable. The exact-match
lookup goes through `dbkit.NewBlindIndexer` over `dbkit.NormalizeEmail` /
`dbkit.NormalizePhoneE164`, which is why the `UNIQUE` constraints mean *one
account per real-world address* rather than one per spelling. `UserRepository`
owns both indexers and derives the index columns from the same plaintext it
encrypts, so no caller can ever set an index that disagrees with its column.

Two consequences worth knowing:

* `NormalizePhoneE164` refuses a bare national number (`13800000000`). It never
  assumes a country, because guessing one would compute an index that matches
  nothing. Callers must supply E.164.
* `dbkit`'s encrypted serializer accepts a `string` or `[]byte` field and
  **rejects a `*string`**. So `User.Email` and `User.Phone` are plain strings with
  `""` meaning "none", while the index columns are pointers that store SQL NULL —
  which is what lets any number of accounts have no phone number while the unique
  index still means what it says.

### The middleware chain is authn, then tenancy

```
obs.Middleware -> authn.Middleware(verifier) -> tenancy.Middleware(authn.NewPrincipalResolver()) -> handler
```

This is a **deliberate deviation** from the order drawn in
`docs/internal/01-architecture.md`. The evidence is the resolver signature:
`Resolve(r *http.Request) (pkgcore.TenantID, error)` returns a tenant and no
context, so a resolver that verified the JWT would have nowhere to hand the
claims it just validated. The documented order therefore forces verifying every
token twice, through two code paths free to diverge, with the tenant decided by
the one that is *not* authorising the request.

* `authn.Middleware` **never calls `pkgcore.WithTenant`.** Injecting the tenant is
  `tenancy.Middleware`'s single job.
* Authentication is **optional** at the middleware: no credential proceeds without
  a Principal, an *invalid* credential is refused at once. Absence of an assertion
  and a failed assertion are not the same thing.
* Per-route enforcement is `RequireAuthenticated`, never a global.
* Pre-auth routes need `tenancy.WithAllowlist` entries. Matching is exact on
  (method, path) — no prefix, no trailing-slash normalization, and **no
  GET-implies-HEAD**.

### Fail closed on membership

`resolveTenant` refuses when there is no `MembershipReader`, when the reader
errors, and when the user is not an active member. It never falls back to a
permissive answer. This gates the tenant a token is minted for, which is the most
exploited horizontal-privilege-escalation entry point in a multi-tenant product.

The same rule governs revocation: an immediate-mode check that cannot reach the
key-value store returns `ErrRevocationCheckFailed`, and the middleware refuses.
A revocation check that could not run is not a revocation check that passed.

### Sign-in must not answer what it refuses to answer

Every failed password sign-in returns `ErrInvalidCredentials` with no parameters:
unknown account, wrong password, no password set, and suspended account are
indistinguishable. An unknown account still costs one argon2id derivation, so a
stopwatch cannot reopen the oracle the error message closed. The specific reason
goes on the `login_attempts` row, for the operator and the account owner.

`login_attempts` stores the blind index of the attempted identifier, never the
identifier. An attempt has to be countable per address — that is how credential
stuffing is spotted — but recording the plaintext would make this table a log of
every address anyone ever typed at the login form, most of which belong to people
with no relationship to the deployment.

### Refresh tokens are single-use, and a replay revokes everything

Every refresh consumes the presented token and mints a new one in the same
family. Presenting a consumed token means a second copy exists, so the response
is to revoke the **whole family and its session**, not just that token —
otherwise whoever stole it stays signed in with the token they already rotated.

The consequence to state to consumers: **two concurrent refreshes with the same
token are indistinguishable from a theft and are treated as one.** A client that
races itself loses its session. Clients must serialise their own refreshes.

### Tokens carry no email and no permissions

No email claim is minted. A `Principal` recovered from a token has an empty
`Email`; it is populated only where the caller actually read the user record. A
bearer credential gets copied into client storage, proxy logs and trace
attributes, where nothing this module controls can redact it.

No permission claim either: a permission list inside a token freezes for the
token's whole lifetime, so a permission revoked at 10:00 would keep working until
it expired.

### Cost parameters are bootstrap config; policy is dynamic config

`PasswordParams` (argon2id memory, iterations, parallelism) depends on the
machine, must be identical across replicas, and must not be tunable from an admin
console — an operator could make sign-in uselessly cheap or slow enough to be a
self-inflicted denial of service. `PasswordPolicy` (minimum length, denylist) is
the opposite and is dynamic.

The parameters travel *inside* each stored hash, so raising the cost is a
configuration change rather than a migration: existing hashes keep verifying, and
`Login` upgrades one on its owner's next successful sign-in.

---

## Testing

```
go -C go/authn test ./... -race       # unit tier, no Docker
golangci-lint run ./...               # from inside go/authn
```

Every new model gets `tenancytest.AssertNotTenantScoped` (identity data) or
`AssertIsolated` (tenant data). Shared fakes live in `internal/testutil`, which
deliberately does **not** import this package — a test file in `package authn`
cannot import anything that imports `authn`, so the membership fake there
satisfies `MembershipReader` structurally.

Concurrency is not optional to test here. The single-winner property of
`RefreshTokenRepository.Consume` is what replay detection rests on, and it is
exercised under `-race` by twenty goroutines racing one token.

---

## Known limitations

| Limitation | Why, and what closes it |
|---|---|
| Registration reports a duplicate identifier as a conflict, which makes it an account-enumeration oracle in a way sign-in deliberately is not. | Closing it means answering every registration with "check your inbox" and moving the conflict into an email, which needs the delivery and verification flows. |
| `sessions.ip_region` and `login_attempts.ip_region` ship empty. | Resolving an IP to a region needs a local GeoIP database whose licence has to clear the licence scanner first (`docs/internal/05-identity-and-access.md` says so explicitly). The columns exist now so no later table migration is needed. |
| The declared dynamic-config items are not yet read back at runtime; the values are injected through options with the same defaults. | The schema is declared, which is what a module owes the config module. The read-through binding lands with the block that needs a live value. |
| `Module.OpenAPISpec()` returns nil. | The HTTP surface is spec-first: the fragment appears together with the generated server interface and the handler that implements it. Returning a fragment before those exist would advertise endpoints nothing serves. |
| No permissions are declared. | Every endpoint this module will serve is self-service. The tenant-scoped SSO administration that does need one lands with the federation work. |
