# sharing

go/sharing implements public share links: a controlled entry point that lets an unauthenticated external visitor view one internal resource -- a patient viewing their own simulation result, a customer viewing a report, an anonymous one-time result page. This file is the module-level discipline that ships with `go/sharing` to consuming projects; the design rationale is `docs/internal/07-platform-services.md`'s sharing section, and the repository-wide rules are the root `CLAUDE.md` plus `.claude/skills/backend-coding-standards`.

**Status: round 1 of 1 landed in this history.** The Share domain model and its access log, the Service that creates, accesses and revokes share links, the jobs-driven expiry sweep, and the module's `pkgcore.Module` wiring (permissions, one audit action, an event catalog, one configuration item) are all landed and tested. What round 1 does **not** ship -- an HTTP surface and a live reference-app consumer -- is recorded below, not half-built. A reader checking a later main tree may find a round 2 that adds either; in that case this line is stale and the code, not this sentence, is authoritative.

## Scope

**In scope this round.** Two tables (`sharing_shares`, `sharing_access_log`) with dual-dialect versioned SQL migrations. `Service`: `Create` mints a share (forcing a default expiry, refusing an explicit request for one that never expires, hashing an optional password, drawing a fresh high-entropy token), `Access` resolves a bearer token into the share it names or refuses with one outward-identical answer, `Revoke` withdraws a share with immediate effect, `Get`/`ListAccessLog` serve an owner's own view. `Service.Sweep` plus the `sharing.expiry_sweep` jobs task mark expired or view-exhausted shares. `pkgcore.Module` wiring: permissions (`sharing:read`, `sharing:create`, `sharing:revoke`), one audit action (`sharing.share.create_sensitive`), three declared domain events (`sharing.share.created`/`.accessed`/`.revoked`), and one configuration item (`sharing.default_expiry`).

**Deliberately not in scope this round** (see "Known limitations" below for the full reasoning behind each):

| Not here | Where it belongs | Why |
|---|---|---|
| An HTTP surface / OpenAPI fragment (the actual unauthenticated public endpoint a viewer's browser hits) | A later round | `Module.OpenAPISpec()` returns `nil` this round, the same "no fragment yet" answer `go/config`'s and `go/pki`'s `Module` give. This round ships the Service/Access API only |
| Resolving `ResourceRef` into actual bytes (calling `go/storage` or any other resource-owning module) | A later round, likely the same one that adds the HTTP surface | `Access` returns the validated `Share` row; turning its `ResourceRef` into a byte stream is the HTTP layer's job, so this round never imports `go/storage` at all |
| Reference-app wiring as a live consumer | A later round, or the HTTP round | See "No real consumer yet" below |
| Compliance's own consumption of `sharing.share.accessed` | `go/compliance` (M4) | `compliance` does not exist yet; this module only publishes, the identical publish-now-subscriber-later shape `go/dbkit/audit`'s write-capture events used before `go/dbkit/audit` itself existed to consume them |
| A live per-tenant reader of `ConfigDefaultExpiry` | A later round, or whichever host wires `WithTenantConfigReader` | See "Tenant-configured default expiry" below |
| Rate limiting share creation or access attempts | A later round | `docs/internal/07-platform-services.md`'s catalog names `go/ratelimit` as the shared primitive several modules use for exactly this kind of abuse; this round does not add the dependency, since a viewer-facing anti-abuse limit needs the HTTP layer to be real first |

## Data model

Two tables, one tenant-scoped, both round 1's own design:

| Table | Domain | Repository shape | Isolation proof |
|---|---|---|---|
| `sharing_shares` | Tenant | `ShareRepository`, embeds `dbkit.Repository[Share]` | `tenancytest.AssertIsolated` |
| `sharing_access_log` | Tenant | `AccessLogRepository`, embeds `dbkit.Repository[AccessLogEntry]` | `tenancytest.AssertIsolated` |

Both are tenant data: a share belongs to the tenant whose resource it exposes, and an access log entry belongs to the same tenant as the share it was recorded against. Neither table carries a SQL foreign key to the other, or to any cross-module row -- `AccessLogEntry.ShareID` is a plain, unenforced string column, following this codebase's blanket no-FK convention even for a same-module reference.

**`Share.TokenHash`, never `Share.Token`.** The design sketch in `docs/internal/07-platform-services.md` shows a `Token` field on the model; this module deliberately stores only the hex-encoded SHA-256 of the token (`token.go`'s `hashShareToken`), mirroring `go/org`'s `Invitation.TokenHash` precedent exactly. The raw token is returned to the caller of `Service.Create` exactly once, inside `CreateResult.Token`, and is never persisted anywhere. A leaked database backup therefore yields no usable share link, and `Service.Access`'s lookup is a hash comparison rather than a comparison against a stored secret.

**`Share.ExpiresAt` is a `*time.Time` that is never actually nil once a row exists.** The Go type matches the design sketch's optional shape (`Service.Create`'s own `CreateParams.ExpiresAt` really is optional), but `Service.Create` resolves a nil request into a concrete time before the row is ever written, and refuses outright (`ErrExpiryRequired`) a caller that explicitly asks for one that never expires (`CreateParams.Forever`). The column is declared `NOT NULL` as a second, database-level line of defense.

**`Share.PasswordHash` is an argon2id PHC digest, never plaintext.** `password.go` implements a small, self-contained argon2id hasher at fixed, un-configurable cost parameters (OWASP's first recommended configuration, matching `authn.DefaultPasswordParams`'s own floor) rather than importing `go/authn` for its `HashPassword`/`PasswordParams` -- `go/authn` is a heavy module several tiers away in the dependency graph, and pulling in its full SMS/OIDC/session surface for one hashing function would violate this codebase's "measure what a dependency costs" discipline. The two implementations are deliberately not unified; if a policy-driven, host-configurable share password ever matters, that is this module's own future round, not a `go/authn` dependency.

**`ShareRepository.tryRecordView` is a compare-and-swap guard, not a raw SQL increment.** It matches the exact row state (`view_count = ?`) a caller last observed and refuses the write if the row has moved on -- the identical shape `org.InvitationRepository.acceptIfPending` uses for its own single-use guard -- rather than `view_count = view_count + 1` reached through `.Model(...)`, which this codebase's `raw-gorm-bypass` semgrep rule flags on any receiver. `Service.recordView`'s retry loop (bounded by `maxRecordViewAttempts`) re-reads a lost race and retries against the row's latest state, so ordinary concurrency between two viewers of one share is never misreported as "not accessible" -- only a share that has genuinely stopped being live short-circuits the loop as a real refusal. `TestService_Access_ConcurrentAccessesRespectMaxViews` races many goroutines against a `MaxViews`-limited share under `-race` and asserts exactly `MaxViews` of them are granted.

## The five mandatory rules

Every rule `docs/internal/07-platform-services.md`'s own "必须遵守的规则" (mandatory rules) list states is enforced, each with a passing test:

1. **Tokens are cryptographically random, at least 128 bits, never derived from a predictable value.** `token.go`'s `newShareToken` draws 32 bytes (256 bits) from `crypto/rand` and nothing else. `TestNewShareToken_MeetsTheEntropyFloor`, `TestNewShareToken_IsUnpredictable`, `TestNewShareToken_LeakingOneTokenDoesNotRevealAnother` (`token_test.go`).
2. **A share created with no explicit expiry gets the tenant's configured default, and a request for a never-expiring link is refused outright.** `Service.Create`'s `resolveExpiry` (nil `ExpiresAt`) and its `CreateParams.Forever` check (`ErrExpiryRequired`). `TestService_Create_NoExpiryFallsBackToDefault`, `TestService_Create_NoExpiryUsesTenantConfiguredDefault`, `TestService_Create_ForeverRefused` (`service_test.go`).
3. **Revocation takes effect on the very next access check, with no caching involved.** `Service.Access` re-reads the row from the database on every call; there is no process-local or otherwise cached share state anywhere in this module. `TestService_Access_RevokedShare_ImmediatelyDenied` (`service_test.go`) is the exact create/access-succeeds/revoke/access-immediately-fails sequence the design doc calls for. See "Revocation and caching" below for the obligation this places on a future HTTP layer.
4. **Every access is logged, and a resource owner can read back who viewed a share and how many times.** `AccessLogEntry` (timestamp, IP, user agent, referrer, outcome) is written on every `Service.Access` call that resolves to a known share of the caller's tenant, granted or denied alike; `Service.ListAccessLog` is the owner-facing read. `TestService_Access_LogsEveryAttempt`, `TestService_Access_LogsDeniedAttemptsAgainstAKnownShare` (`service_test.go`).
5. **The share surface leaks nothing about the tenant: every refusal reason looks identical from the outside.** `Service.Access` answers an unrecognized token, a revoked share, an expired share, a view-exhausted share, and a missing or wrong password all with the exact same `ErrNotAccessible` -- same `Code`, same `Status`, no distinguishing `Params` -- mirroring the existence-disclosure suppression `go/authn` already applies to account enumeration. `TestService_Access_EveryRefusalReasonIsOutwardlyIdentical` (`service_test.go`) asserts this across all five reasons in one table-driven test.

## Sensitive-resource confirmation

Rule 4 of the doc's "sensitive resource sharing" note (not to be confused with the five mandatory rules' own numbering above): creating a share for a resource carrying sensitive personal information is itself an audit event. `CreateParams.Sensitive` is an explicit, caller-supplied boolean -- this module deliberately builds no generic sensitivity-classification system, per this round's own scope boundary; a create call simply says "this resource is sensitive" or does not.

When `Sensitive` is true, `Service.Create`'s `emitSensitiveAudit` fires the `sharing.share.create_sensitive` audit action through `go/dbkit/audit`'s **declarative `Emit` path**, not `dbkit.Options.AuditBus`'s automatic write-capture plugin. This is a deliberate choice, not an oversight: `go/dbkit/AGENTS.md`'s "Audit trail collection" section records that `AuditBus` must never point a persister at the same database a `dbkit.Repository[T]` write's own open transaction is still writing to, or the write-capture plugin's synchronous, same-goroutine publish deadlocks into `SQLITE_BUSY`. `emitSensitiveAudit` is called *after* `s.shares.Create` has already returned -- by which point `Create`'s own transaction has committed -- exactly mirroring `examples/reference-app/internal/notes/handler.go`'s `recordNoteCreatedAudit`, the established precedent for this exact hazard. A failure to emit the audit event is logged at Error level and never turns an otherwise successful share creation into an error for the caller, per `docs/internal/10-compliance-and-audit.md`'s "must alert, must not be silently dropped" rule. `TestService_Create_SensitiveEmitsAuditEvent` / `TestService_Create_NotSensitiveEmitsNoAuditEvent` (`service_test.go`).

## Tenant-configured default expiry

`ConfigDefaultExpiry` (`sharing.default_expiry`) is declared on the registry by `Register`, with its `Default` set to `defaultShareExpiry` (30 days) -- visible today, and eventually editable, through `go/config`'s own admin-console machinery once a host wires that path. Declaring the schema is as far as this round goes, mirroring `go/pki`'s identical round-1 choice for its own validity-period config items: no module in this codebase's history yet reads a live per-tenant `go/config` value from business code (the closest precedent, `org.FeatureGate`, is a structurally-typed *feature-flag* seam, a narrower shape), so there is no established pattern to extend, and adding a full `go/config` dependency for one round's single read would be a meaningfully larger dependency footprint than this module's own scope warrants.

Instead, `Service`'s `TenantConfigReader` interface (`service.go`) is a structurally-typed, no-import seam -- `ShareDefaultExpiry(ctx, tenant) (time.Duration, bool, error)` -- the same shape `org.FeatureGate` and `org.Scope` use to reach `go/config`-shaped or `go/org`-shaped behavior without an import edge in either direction. `Module.WithTenantConfigReader` wires one in; without it (the default), `Service.Create` always falls back to `defaultShareExpiry`, which is exactly what rule 2's own "30 days if the tenant has not configured one" fallback means when nothing has been configured at all. The seam is fully implemented and tested (`TestService_Create_NoExpiryUsesTenantConfiguredDefault`, `TestService_Create_TenantConfigReaderReportingUnconfigured_FallsBackToDefault`) against a fake reader -- **no host wires a real adapter over `*config.Service` yet**, which is this round's honest limitation, not a missing feature: writing that adapter is a `go/config`-side or reference-app-side concern for whichever round actually needs per-tenant-tunable share expiry.

## Revocation and caching

Rule 3 (`docs/internal/07-platform-services.md`'s "revocation takes effect immediately" rule) is explicit that a share page and the resource it serves **must declare `Cache-Control: no-store` and must never sit behind a CDN** -- CDN caching is named as the single most common way revocation silently fails to take effect. **This round cannot enforce that obligation: there is no HTTP surface yet.** `Service.Access` itself has no caching problem (it re-reads the database on every call, per rule 3's own test above), but that guarantee is only as strong as whatever a future HTTP layer does with the answer. This is recorded here as a binding obligation on whichever round adds the public HTTP endpoint: that endpoint's response **must** set `Cache-Control: no-store` on every share-content response and **must not** be placed behind a CDN or any other shared cache, full stop.

## No real consumer yet

**The reference app does not create, access or revoke a share link anywhere in this history.** Per this repository's mandatory-first-consumer rule, that would ordinarily mean this module "is not considered done" -- the exception here is scoping, not a quality shortcut: round 1's own instructions deliberately exclude reference-app wiring (no HTTP surface exists yet for a browser or a Go test driving real HTTP to exercise, and wiring the Service API directly into the reference app's server without an HTTP surface would mean inventing a fake integration point that a later round would have to redo anyway). This carries the same two compensating obligations `go/pki`'s X.509 layer records for its own "no real consumer yet" exception:

1. **A godoc `Example` covering the module's full main path** -- `example_test.go`'s `Example` creates a share, accesses it as an unauthenticated viewer would, revokes it, and observes the very next access refused. CI compiles and runs it inside this module's unit suite: the API compiles and works from an external caller's own import, the strongest guarantee available without a real consumer.
2. **This section, stating plainly that this module is unverified by any real consumer.** No project has ever tried to integrate against `Service`'s public API and found a parameter it could not actually supply, or a shape that did not fit an HTTP handler's own request/response cycle.

Unlike `go/pki`'s X.509 layer, this module's public API is **not** granted an explicit "not frozen" concession -- the round that adds the HTTP surface is expected to build on `Service` as it stands, not redesign it, since `Service`'s shape (`Create`/`Access`/`Revoke`/`Get`/`ListAccessLog`) was designed with an HTTP handler's own request/response cycle in mind from the start.

## Known limitations

- **No HTTP surface.** See "Deliberately not in scope this round" above. `Module.OpenAPISpec()` returns `nil`.
- **`Access` never resolves `ResourceRef` into actual bytes.** It returns the validated `Share` row (including `ResourceRef`); turning that into a byte stream by calling `go/storage` or any other resource-owning module is explicitly deferred, so this round's `go.mod` carries no dependency on `go/storage` at all.
- **No live consumer of the `sharing.share.accessed` event.** `docs/internal/07-platform-services.md` names `go/compliance` (M4) as the eventual subscriber; this module only publishes, per this repository's established publish-now-subscriber-later pattern.
- **No live `TenantConfigReader` adapter over `go/config`.** See "Tenant-configured default expiry" above.
- **No rate limiting on `Create` or `Access`.** A public, unauthenticated endpoint inviting repeated token guesses or share-creation abuse is exactly the shape `go/ratelimit` exists for; this round does not add the dependency, deferred to whichever round wires the HTTP surface those abuse vectors actually reach through.
- **`Sweep`'s reaping is row hygiene, not a correctness dependency.** `Share.isLive` is evaluated fresh on every `Service.Access` call regardless of whether a sweep has ever run; `Sweep` only converges an owner-facing listing's `RevokedAt` column onto "yes, this is gone" for rows nothing has looked at since they expired.
- **No PostgreSQL integration tier.** `go/sharing/internal/testutil.NewPostgres` exists (mirroring `NewSQLite`) so a later round's `integration_test/` package needs no `db.go` of its own, but no such package is shipped yet. This round's SQLite-backed unit suite runs every isolation and CAS-guard assertion against SQLite only.
- **`Service.Access`'s outward-identical-answer property is proven for the reasons this round's code paths can produce (unknown token, revoked, expired, view-exhausted, missing/wrong password) and no others.** A future round adding a new refusal reason to `Access` must extend `TestService_Access_EveryRefusalReasonIsOutwardlyIdentical`'s table alongside it, or the new reason's outward shape goes unverified.

## Testing

- **Unit tests**: SQLite only, no Docker required. `go/sharing/internal/testutil` provides `NewSQLite`/`NewPostgres`/`Migrate`, mirroring `go/pki/internal/testutil`'s exact shape.
- `model_test.go` covers `Share.isLive`'s full state matrix (revoked, expired, exactly-at-expiry, view-ceiling reached/exceeded, nil `ExpiresAt`) and both models' `TableName`.
- `token_test.go` covers the entropy floor, unpredictability across a sample, and that leaking one token's hash reveals nothing about another's.
- `password_test.go` covers hash/verify round-tripping, that two hashes of the same password differ (fresh salt), and a malformed stored hash reporting an error rather than a false match.
- `repository_test.go` covers both repositories' `tenancytest.AssertIsolated` suites, the token-hash lookup's tenant scoping, `tryRecordView`'s compare-and-swap guard (including a deliberately staged lost race), and the expiry-sweep listing.
- `service_test.go` is this module's largest suite: every one of the five mandatory rules (see above), `Create`'s full validation and defaulting matrix, the sensitive-resource audit emission, the three domain events' publication, and `Revoke`/`Get`/`ListAccessLog`'s owner-facing behavior including a `-race`-covered concurrent-access test.
- `cleanup_test.go` covers `Service.Sweep`'s marking behavior and idempotence, `expirySweepHandler`'s `Handle`/`Type`, and `Module.EnqueueExpirySweep`'s queue-required refusal and task-shape.
- `module_test.go` covers `pkgcore.Module` identity, the dual-dialect migration layout, locale-file parity, `Register`'s full declared surface (bootstrapped through a real `pkgcore.Kernel`, which also proves the locale bundle survives `i18n.Builder.AddModule`'s parity check), coexistence with another module, and the no-I/O contract of `Register`.
- `errors_test.go` covers the `hasCode` helper and that every declared error uses the `sharing.<reason>` code convention.

## Error index

| Code | `apperr` kind | Raised by |
|---|---|---|
| `sharing.resource_ref_required` | `Invalid` | `Service.Create` with an empty `ResourceRef` |
| `sharing.expiry_required` | `Invalid` | `Service.Create` with `CreateParams.Forever` true |
| `sharing.invalid_max_views` | `Invalid` | `Service.Create` with a non-positive `MaxViews` |
| `sharing.not_accessible` | `NotFound` | `Service.Access`, for every refusal reason -- see rule 5 above |
| `sharing.share_not_found` | `NotFound` | `Service.Revoke`/`Get`/`ListAccessLog` for an unknown share id in the caller's tenant |
| `sharing.internal_error` | `Internal` | A storage error this module cannot classify |

Every code above has a matching description entry in `locales/{zh-CN,en-US}.toml`, under the identical id.
