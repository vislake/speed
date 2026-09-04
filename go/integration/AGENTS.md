# integration

go/integration is a tenant's outward-facing API surface: API keys a tenant issues to its own scripts and third-party systems, and the three-layer rate limiting that protects the platform, the tenant and the individual key from one another. This file is the module-level discipline that ships with `go/integration` to consuming projects; the design rationale is `docs/internal/07-platform-services.md`'s integration section, and the repository-wide rules are the root `CLAUDE.md` plus `.claude/skills/backend-coding-standards`.

**Status: round 1 of at least 2.** This round ships API key issuance, listing, rotation and revocation, plus the three-layer rate-limiting composition on top of `go/ratelimit` and its HTTP 429 translation as a reusable middleware. Outbound webhooks -- event subscription, the internal-to-public event schema mapping, HMAC signing, SSRF-protected delivery, retry and dead-lettering -- are the design doc's other half of "integration" and are a later round's work. Judge what exists by the code, not by this sentence.

## Scope

**In scope this round.** One table (`integration_api_keys`) with dual-dialect versioned SQL migrations, registered through `dbkit.MigrationRegistry` the same way every other module does. `Service` (`service.go`): `Create` (generates the raw key, returns it to the caller exactly once), `List` (never exposes the raw key or its hash), `Rotate` (create-new + revoke-old, documented below) and `Revoke`. `LayeredLimiter` (`ratelimit.go`): three independent `go/ratelimit.Allow` calls -- global, tenant, key -- composed with short-circuit-on-first-denial semantics, built entirely from `go/ratelimit`'s existing public API with no change to that package. `HTTPGuard` (`httpguard.go`): the handler-layer translation of a denied `LayeredDecision` into a 429 response with `Retry-After` and `X-RateLimit-*` headers -- a directly usable `net/http` middleware, not (yet) wired behind a mounted route or an OpenAPI fragment. `pkgcore.Module` wiring: permissions (`integration:apikey:read`, `integration:apikey:manage`), audit actions (`integration.apikey.create`/`.rotate`/`.revoke`), and the bilingual locale bundle for this round's ten error codes.

**Deliberately not in scope this round:**

| Not here | Where it belongs | Why |
|---|---|---|
| Outbound webhooks (event subscription, public event schema mapping, HMAC signing, SSRF-protected delivery, retry via `jobs`, dead-letter, delivery log) | A later round | The design doc treats API key management and webhooks as two separable halves of "integration"; this round is bounded to the first half by explicit instruction |
| A mounted HTTP surface / OpenAPI fragment for API key CRUD (create/list/rotate/revoke as `/api/v1/integration/...` operations) | A later round | `Module.OpenAPISpec()` returns `nil` this round -- a minimal, directly-testable Go middleware (`httpguard.go`) was judged the right size for this round's rate-limiting HTTP story; a full CRUD surface would meaningfully expand the round |
| Authenticating an inbound request with an API key (hashing the presented value, looking it up by `(tenant_id, hash)`, writing `LastUsedAt`) | A later round | `Service` issues and revokes keys but nothing in this module yet sits in an incoming request's authentication path; `LastUsedAt` and the `uq_integration_api_keys_tenant_hash` index exist now so that a later round's lookup needs no migration of its own |
| An expiry-sweep job (revoking or reaping keys whose `ExpiresAt` has passed) | A later round | `idx_integration_api_keys_tenant_expires_at` is added now for the identical "get the table shape right first" reason; this module does not depend on `go/jobs` yet |
| Reference-app wiring as the mandatory first consumer | A later round | Explicitly out of this round's scope by instruction -- see "No reference-app consumer yet" below for the compensating obligations this carries, mirroring `go/pki`'s own X.509-layer exception |
| A PostgreSQL integration tier (`integration_test/`) | A later round | `go/integration/internal/testutil.NewPostgres` exists (mirroring `NewSQLite`) so a later round needs no `db.go` of its own, but no `integration_test/` package is shipped yet |
| `integration:apikey:rotate`/`:revoke` as separate permissions from `:manage` | Not planned | The design doc's own permission list treats key lifecycle management as one capability; splitting it further is not asked for anywhere |

## Data model

One table, tenant data (`docs/internal/04-data-and-tenancy.md`): `integration_api_keys`, isolation proven by `tenancytest.AssertIsolated` (`repository_test.go`), never `AssertNotTenantScoped` -- a key belongs to exactly one tenant, never to the individual who created it (`CreatedBy` is an id reference, not an ownership tie; see model.go's own doc comment).

**The primary key is `(id)` alone**, matching `go/storage`'s `Object` precedent rather than a composite `(tenant_id, id)` key: `id` is an application-generated UUID (`uuid.NewString`), globally unique on its own, so `tenant_id` rides along as a plain, non-key column promoted by the embedded `dbkit.TenantModel`.

**The key material is never stored.** `Hash` is the hex-encoded SHA-256 of the raw key value `Service.Create` generates once and returns to the caller; it is never persisted, logged, or returned by any other call. `keygen.go`'s `newAPIKeyToken` explains why plain SHA-256 (not a deliberately-slow password hash) is the right primitive here: the input is 32 bytes of `crypto/rand` full-entropy randomness, not a human-chosen secret, so there is no dictionary an attacker could use to make a slow hash worth paying for -- the same reasoning `go/org`'s invitation token hashing already documents. `Prefix` is the plaintext, non-secret display portion (`sk_` plus eight characters of the encoded random value) a key list shows so an operator can tell two keys apart without ever seeing the rest.

**Scopes are frozen at issuance, never re-derived.** `APIKey.Scopes` is the subset of `CreatedBy`'s permissions `Service.Create` validated at the moment the key was issued, through the mandatory `PermissionLister` seam. A later change to the creator's own permissions -- promoted, demoted, or removed from the tenant entirely -- never widens or shrinks an already-issued key; the design doc is explicit that a key's scope does not change along with its creator's later permission changes. Changing what a key may do means issuing a new one (`Rotate`); nothing in this module ever rewrites `Scopes` after `Create`.

**The creator leaving does not revoke the key.** `CreatedBy` is the responsible party on record, not an ownership tie -- the design doc is explicit that it is unacceptable for an integration to break just because someone left the tenant. `APIKeySummary.CreatorLeft`, computed at `List` time through the optional `MembershipChecker` seam, is the visibility the design doc does ask for, so a tenant administrator notices a key needs a new owner of record without the key itself being touched.

**Every key has a mandatory, forced expiry.** `ExpiresAt` is required; `Service.Create` defaults an unspecified request to `MaxAPIKeyLifetime` (one year) from now and refuses a request asking for anything further out (`ErrExpiryExceedsMaximum`) or already past (`ErrExpiryInPast`), per the design doc's forced-expiry-ceiling rule -- a key that never expires is the most common credential-leak surface, so this is refused rather than silently clamped.

## Seams

**`PermissionLister` (mandatory for any non-empty `Scopes` request).** `Service.Create` calls it exactly once per request to validate that the requested `Scopes` are a genuine subset of the creator's own permissions right now. `go/rbac`'s `Authorizer.ListPermissions(ctx, sub Subject) ([]string, error)` is the real, already-shipped method this interface mirrors structurally -- no new `rbac` method was invented for this round; a host wires it with a one-line closure, spreading `rbac.Subject`'s two fields into plain parameters so this module needs no `rbac.Subject` value to call it. This module still does not import `go/rbac` for it, for the same reason `org.FeatureGate` and `rbac.SubtreeResolver` are structurally-typed seams rather than direct imports across the module boundary: `go/rbac` carries `dbkit`, `gorm` and its own migrations along with it, and every one of those becomes a cost every consumer of this otherwise storage-and-ratelimit-only module would pay just to reference one interface's shape. An empty `Scopes` request needs no lister at all -- there is nothing to check a request for zero scopes against -- so a `Module` built with no `WithPermissionLister` option can still issue scope-less keys; a non-empty request with none wired fails closed with `ErrPermissionListerUnavailable` rather than silently skipping the check.

**Why one-at-a-time, not a bulk check.** `go/rbac`'s public API has no "is this whole set of permissions held" call, only `ListPermissions` (list everything a subject holds) and, presumably, a single-permission check elsewhere in its `Authorizer` -- this round validates each requested scope by testing set membership against `ListPermissions`'s answer, which needs exactly one `rbac` call per `Create`/`Rotate` regardless of how many scopes are requested. If `go/rbac` grows a genuine bulk "are these N permissions all held" call in a later round, `validateScopes` in `service.go` is the one place that would change to use it.

**`MembershipChecker` (optional).** Answers whether a given user is still an active member of a tenant, the one fact `Service.List` needs for `APIKeySummary.CreatorLeft`. Unlike `PermissionLister`, this is a display convenience, not a security control -- nothing about whether a key authenticates, what it may do, or whether it keeps working depends on `CreatorLeft`'s value. A host that wires none simply never sees the flag raised: `List` reports `CreatorLeft` as `false` for every row, exactly as if every creator were still active. Membership is owned by whichever module actually tracks who belongs to a tenant -- `go/org`'s roster in this codebase, but nothing requires that; the design doc's own text only ever says "creator has left", never naming `org` or `authn` as the source of truth, so this is a structurally-typed interface exactly like `org.FeatureGate` and `rbac.SubtreeResolver`.

Both seams follow the `PermissionListerFunc`/`MembershipCheckerFunc` adapter shape `http.HandlerFunc` popularized, so a host wires a closure without declaring a named type.

## Rate limiting

`LayeredLimiter.Allow` evaluates the global, tenant and key layers in that fixed order, short-circuiting on the first denial: a request already known to be denied at the global layer never touches the tenant or key counters at all. A zero-value `ratelimit.Limit` on any of `LayeredLimits`' three fields disables that layer entirely (`Allow` never calls the underlying `Limiter` for it), so `LayeredLimits{}`'s zero value legally disables all three -- every request is allowed. This module never modifies `go/ratelimit` itself; the three layers are entirely a composition of its existing `Limiter.Allow(ctx, key, limit)` call, one per layer, matching the design doc's own layering description.

`HTTPGuard.Middleware` wraps an `http.Handler`, deriving the tenant and key dimensions from the request through a host-supplied `Extractor` function -- this module takes no position on how a host authenticates a request or resolves its tenant. A denied `LayeredDecision` becomes a 429 with `Retry-After` (whole seconds, rounded up per RFC 9110 §10.2.3) and `X-RateLimit-Remaining`/`X-RateLimit-Reset`/`X-RateLimit-Layer` headers; an error from the underlying limiter itself (a `KVStore` failure) becomes a 500, never a silent allow -- `go/ratelimit.Limiter`'s own doc comment is explicit that it decides no fail-open policy on the caller's behalf, and `HTTPGuard` honors that.

## No reference-app consumer yet

**Reference-app does not wire this module in this round**, by explicit instruction bounding this round's scope. Per the root `CLAUDE.md`'s mandatory-first-consumer rule, this is a deliberate, documented exception -- the same shape `go/pki`'s `AGENTS.md` records for its X.509 layer -- carrying the identical compensating obligations:

1. **A godoc `Example` covering the round's main path** -- `example_test.go`'s `Example` issues a key (showing the plaintext key returned exactly once), checks a request against the three-layer rate limiter, and revokes the key, entirely through this module's exported API. CI compiles and runs it inside this module's unit suite.
2. **This section, stating plainly that this module has no real consumer yet.** It is not "done" in the sense every other module in this codebase's census is done -- no application has ever tried to wire `Service` and its seams together and found a parameter it could not actually supply.
3. **This module's public API is not frozen** the way the rest of this codebase's public API is. The first real consumer's integration -- almost certainly reference-app, in a later round -- is explicitly permitted to break `Service`'s signatures, the seam interfaces' shapes, or anything else in this module, without that being a design failure.

## Known limitations

- **No inbound request authentication.** This module issues, lists, rotates and revokes keys, but nothing here verifies an incoming request's presented key against the stored hash, and `LastUsedAt` is never written by this round's code. `uq_integration_api_keys_tenant_hash` exists now so a later round's lookup needs no migration of its own.
- **`Rotate` is not atomic.** The new key's `Create` and the predecessor's `Revoke` are two separate database writes, not one transaction -- `dbkit.Repository[T]` exposes no cross-call transaction seam a business module can reach. A failure between the two leaves two live keys rather than zero, a safe-direction surplus (never a lockout) that `Rotate` reports to its caller rather than silently swallowing. See `service.go`'s `Rotate` doc comment for the full argument.
- **No expiry-sweep job.** `idx_integration_api_keys_tenant_expires_at` exists now; nothing in this round enqueues or runs a sweep against it. This module does not depend on `go/jobs` yet.
- **`MaxAPIKeyLifetime` is a package-level constant, not a dynamic configuration item.** Reading one from `go/config` would add a dependency edge this module's position in `docs/internal/01-architecture.md`'s graph does not have, paid for by every consumer that boots this module without config. A host that genuinely needs a different ceiling is a later round's `WithMaxAPIKeyLifetime` option.
- **No PostgreSQL integration tier.** See the scope table above.
- **No reference-app consumer, and this module's public API is not frozen.** See "No reference-app consumer yet" above.

## Testing

- **Unit tests**: SQLite only, no Docker required. `go/integration/internal/testutil` provides `NewSQLite`/`NewPostgres`/`Migrate`, mirroring `go/pki/internal/testutil`'s exact shape, so every test in this module applies the real, versioned migration files from zero rather than an `AutoMigrate` or a hand-written `CREATE TABLE`.
- `repository_test.go` runs the mandatory `tenancytest.AssertIsolated` suite against `APIKeyRepository`.
- `model_test.go` covers `APIKey`'s `TenantScoped` implementation, `IsRevoked`/`IsExpired`, and the `scopesJSON`/`parseScopes` round trip including the nil-becomes-empty-array contract.
- `keygen_test.go` covers `newAPIKeyToken`'s prefix/hash shape, uniqueness across calls, and `hashAPIKeyToken`'s determinism.
- `seams_test.go` covers the `PermissionListerFunc`/`MembershipCheckerFunc` adapters.
- `ratelimit_test.go` covers `LayeredLimiter`'s short-circuit ordering (global denies without touching tenant/key, tenant denies without touching key), per-layer independence, disabled-layer skipping, and invalid-limit propagation.
- `httpguard_test.go` covers `HTTPGuard.Middleware`'s allow/deny/limiter-error paths and the header/body shape of a 429 response.
- `service_test.go` covers `Create`/`List`/`Rotate`/`Revoke`'s full validation surface: scope validation (including the no-lister-needed empty-scopes case and the lister-unavailable refusal), expiry default/ceiling/past-refusal, cross-tenant not-found indistinguishability, `CreatorLeft` with and without a `MembershipChecker`, and `Rotate`'s create-new-then-revoke-old behavior including its scope-revalidation-on-rotation property.
- `module_test.go` covers `pkgcore.Module` identity, `Register`'s declared permissions and audit actions, `Attach`'s single-call contract, and the injected-clock seam (`withClock`) reaching a real `Service`.
- `errors_test.go` covers the error index's `apperr` shape (every code prefixed `integration.`, `ErrRateLimited`'s 429 status).

## Error index

| Code | `apperr` kind | Raised by |
|---|---|---|
| `integration.key_not_found` | `NotFound` | `Service.Rotate`/`Revoke` for an id that does not exist, or belongs to another tenant (the two are deliberately indistinguishable) |
| `integration.key_already_revoked` | `Conflict` | `Service.Rotate`/`Revoke` against a key whose `RevokedAt` is already set |
| `integration.created_by_required` | `Invalid` | `Service.Create` with an empty creator user id |
| `integration.expiry_exceeds_maximum` | `Invalid` | `Service.Create` whose requested `ExpiresAt` is further out than `MaxAPIKeyLifetime` |
| `integration.expiry_in_past` | `Invalid` | `Service.Create` whose requested `ExpiresAt` is not strictly after now |
| `integration.scope_not_held_by_creator` | `Forbidden` | `Service.Create`/`Rotate` requesting a scope the creator does not currently hold; `WithParam("scope", ...)` names it |
| `integration.permission_lister_unavailable` | `Internal` | `Service.Create`/`Rotate` requesting a non-empty `Scopes` with no `PermissionLister` wired |
| `integration.rate_limited` | 429 (not an `apperr` builder kind -- a struct literal, matching `go/authn`'s identical `ErrRateLimited` shape) | `HTTPGuard.Middleware` (via `WithRateLimitParams`) for a denied `LayeredDecision` |
| `integration.already_attached` | `Internal` | `Module.Attach` called a second time on the same `Module` |
| `integration.internal_error` | `Internal` | A repository failure, a `PermissionLister`/`MembershipChecker` call that itself failed, or a `crypto/rand` failure |

Every code above has a matching description entry in `locales/{zh-CN,en-US}.toml` under the identical id, per this codebase's i18n convention.

## Adjudications a reviewer should not "correct"

**The primary key is `(id)` alone, not `(tenant_id, id)`.** See "Data model" above -- `id` is a globally-unique application-generated UUID, matching `go/storage`'s `Object` precedent rather than the composite-key shape a module-local id would need.

**Rate limiting is never hard-wired to any specific storage key naming beyond this module's own `layerStorageKey` prefix.** `LayeredLimiter.Allow` treats `globalKey`, `tenantKey` and `apiKeyID` as opaque caller-supplied strings, exactly as `go/ratelimit.Limiter.Allow` itself treats its own `key` parameter -- this module adds no interpretation of what those strings mean.

**`Rotate` carries `Scopes` forward from the predecessor, but re-validates them against the creator's CURRENT permissions rather than trusting them as already-proven.** This is not a bypass of `Create`'s own scope check: routing the predecessor's scopes back through `Create` means a creator whose permissions shrank since the predecessor was issued can have a `Rotate` fail with `integration.scope_not_held_by_creator` on a scope it no longer holds. This is a deliberate, safe-direction consequence of "scope validation happens at issuance" applying to every issuance, including a rotation's, not a special carve-out.

**`ExpiresAt` is NOT carried forward on `Rotate`.** A rotation gets a fresh full-lifetime expiry (`Create`'s own default), not a continuation of the predecessor's absolute timestamp -- copying that timestamp would defeat the point of rotating a key that is itself close to (or, for a caller rotating a stale-but-still-live key, already past) its own expiry.
