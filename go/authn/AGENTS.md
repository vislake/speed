# authn

Authentication: who a caller is. Never what they may do.

Design rationale lives in `docs/internal/05-identity-and-access.md` (Chinese). This
file is the discipline that ships with the module to consuming projects.

---

## Scope

| In | Out |
|---|---|
| Users, sessions, refresh tokens, login history | Roles, permissions, policy evaluation (`rbac`) |
| Password storage and verification (argon2id) | Memberships, the organization tree (`org`) |
| Access-token issue and verification (Ed25519 JWT) | Sending notifications (`notification` subscribes to this module's events) |
| Refresh rotation, replay detection, session revocation | Authorization decisions of any kind |
| Tenant switching within one session | Memberships, the organization tree (`org`) |
| Social login (Google, GitHub, WeChat, DingTalk, Feishu) and enterprise OIDC single sign-on | SAML, WebAuthn/passkeys — not implemented |
| Account-binding management (list, bind, unbind) | QQ / Weibo / Alipay and other providers — not implemented |
| Phone-plus-SMS-code sign-in on the existing blind index, plus the real carrier adapters — Aliyun, Tencent Cloud and Twilio under `go/pkgcore/sms/` | Proving the carrier adapters against each vendor's real gateway, which needs live account credentials: the env-gated integration legs self-skip without them (see "The three carrier adapters" below) |
| TOTP enrollment, confirmation, recovery codes, step-up re-verification | WebAuthn/passkeys — not implemented |
| Sliding-window plus progressive-lockout rate limiting on login/register/code-send/code-verify/step-up | Anomalous-login detection (new device/region) — not implemented; it needs GeoIP, which no resolver supplies |
**`rbac` must never import this package.** Authorization takes a tenant and a
user, assembled by whoever authenticated. The dependency runs one way, and an
import in the other direction is a merge blocker rather than a style note.

---

## Public API

### Wiring

| Symbol | Purpose |
|---|---|
| `NewModule(db, opts...) (*Module, error)` | The `the module contract`. Options are validated eagerly, so a missing key is a startup error. |
| `NewService(db, bus, kv, opts...) (*Service, error)` | The service alone, for a host that does not bootstrap through a registry. |
| `RegisterPIISerializer(cipher) error` | Registers the field-encryption serializer under `SerializerName`. **Call before opening the `*gorm.DB`.** |
| `WithKeySource`, `WithBlindIndexKey` | **Required.** No safe default exists for either. The former static-key options (`WithSigningKeys` and the `KeySet` API) do not exist in this version — breaking, with no back-compat path; `WithKeySource` is the only way in (see "Tokens and passwords" below). |
| `WithMembershipReader` | The seam through which membership is asked. Absent means "refuse", not "allow". |
| `WithFeatureGate` | Makes this module's declared feature flags (`authn.password_login`, `authn.sms_login`, the five `authn.social.*` channels, `authn.sso.oidc`) effective at request time. `*config.Service` satisfies the `FeatureGate` interface structurally, and `FeatureGateFunc` adapts a closure or a method value to the seam. See "Feature flags are enforced through a host-supplied gate" below. |
| `WithSettingsReader` | Makes this module's declared dynamic config items (`authn.password_min_length`, the token/session/OAuth-state/SMS TTLs, `authn.social.trusted_providers`, the per-channel social credentials) effective at runtime. `*config.Handle` satisfies `SettingsReader` structurally (the compile-time proof is in `settings.go`), so a host passes `configModule.Handle()` with no adapter, and the component descriptor self-wires it when a config component is assembled. See "Declared config items are read through the settings seam" below. |
| `WithClock`, `WithIssuer`, `WithAccessTokenTTL`, `WithRefreshTokenTTL`, `WithSessionTTL`, `WithRevocationMode`, `WithPasswordParams`, `WithPasswordPolicy` | Everything else. A nil or non-positive value leaves the default in place. `WithRevocationMode(RevocationModeImmediate)` needs no companion middleware wiring — enforcement is default — see "Immediate revocation is enforced by default, not by host ceremony". |
| `WithSMSSender`, `WithDeploymentMode`, `WithSMSCodeTTL`, `WithSMSCodeMaxAttempts` | The phone-login transport and its lifetime/attempt budget. See "A distributed deployment must wire an `SMSSender`" below for what `WithDeploymentMode` is for. |
| `WithTrustedProxies(proxies ...string)` | The IP addresses and CIDR prefixes of the reverse proxies requests arrive through, so `Handler.clientIP` recovers the real client address from the `X-Forwarded-For` chain those proxies append instead of recording the proxy itself -- see "Every recorded address is the client's, gated on host-declared trusted proxies" below. Empty (the default) keeps every request recording its direct connection address. |
| `WithVendorClientIPHeaders(headers ...VendorClientIPHeader)` | The per-header opt-in that authorizes reading a single-hop vendor client-address header (`VendorClientIPHeaderFlyClientIP`, wire value `Fly-Client-IP`) for a request whose peer is a declared trusted proxy. The `VendorClientIPHeader` set is closed -- any other value is refused at wiring time -- and the default is none. See "Every recorded address is the client's, gated on host-declared trusted proxies" below for why the trusted-proxy declaration alone must never authorize such a header. |

### Declared config items are read through the settings seam

Every `ConfigKey*` this module declares on `reg.ConfigSeat()` is consumed at runtime through `WithSettingsReader` (`settings.go`). The rule for every read: an explicit config row wins; an unset row, a nil reader or a failed read falls back to the construction-time option (`WithPasswordPolicy`, `WithAccessTokenTTL`, `WithTrustedProviders`, ...), so a host that configures nothing dynamically behaves exactly as before the seam existed. The one deliberate exception is `authn.social.trusted_providers`: a READ FAILURE fails closed to the empty list rather than falling back, because falling back on error could resurrect auto-linking an operator had explicitly turned off. Every read site is at the operation that consumes it (a sign-in, a password set, a mint, an SMS issue), with two documented timings: the access-token TTL is resolved on the signer's first use and frozen for the process (it sizes the signing-key lifecycle; see `effectiveTTL`), and the password policy refuses an incoherent pair back to the construction-time policy (`passwordPolicyFor`). The credential items (`authn.social.<channel>.client_id`/`client_secret`) are resolved per OAuth flow and reach the provider through `CredentialedProvider.WithCredentials`; both keys must carry explicit rows or the provider keeps its construction-time pair.

The end-to-end proof is `unittest/settings_wiring_test.go`: a real config module stores `authn.social.trusted_providers` and the trusted/untrusted arms of automatic account linking are observed through `SocialCallback` -- including the arm that pins the unwired-reader behavior (a config row alone never links).

### Declared bootstrap keys

`Register` declares two process-start keys on the component descriptor's `BootstrapKeys`: `authn.pii_cipher_key` (the AES key behind `RegisterPIISerializer`'s field serializer) and `authn.blind_index_key` (the HMAC key behind `WithBlindIndexKey`). Both are `hexkey` and Sensitive, both documented as a non-secret development default, and both are separate secrets on purpose -- an AES key never doubles as an HMAC key, and the rule spans modules, not only authn's own two keys. The declaration states the module's contract; the host resolves the values (flags, environment, an optional file, its own defaults) and injects them through the options above. The module never reads the environment itself. `pkgcore/AGENTS.md` carries the seat's own contract, including why a key declared on both the bootstrap and the runtime configuration layers is refused.

### Feature flags are enforced through a host-supplied gate

This module declares eight feature flags in `Register` (`authn.password_login`,
`authn.sms_login`, one per social channel, `authn.sso.oidc`) and enforces them
at request time through the `FeatureGate` seam (`WithFeatureGate`): with a gate
wired and a channel's flag off, every entry point of that channel refuses with
`authn.channel_disabled` -- the password endpoint stops issuing tokens, the
SMS endpoints stop sending and redeeming codes, the social authorize/callback
pair stops starting or completing flows, and the enterprise relying party's
`AuthorizeURL`/`Callback` refuse. The refusal makes the API agree with the
login page, whose channel visibility comes from the same flag values served by
the config module's pre-authentication features endpoint.

The gate is structurally satisfied by `*config.Service` -- authn never imports
config. **A host that has the config module in its deployment should wire its
service here** (read lazily at call time: `FeatureGateFunc` over the config
module's lazy `Handle`, `authn.FeatureGateFunc(configModule.Handle().IsEnabled)`,
is the canonical shape, since `configModule.Attach` produces the service only
after the assembly returns and the handle reports the config module's own
not-attached refusal until then). A
nil gate -- the no-config-module deployment -- leaves every channel enabled:
there is no feature store for an operator to have disabled anything in, so
there is no intent for the module to enforce.

### Platform search: `SearchUsers` does no authorization of its own

`Service.SearchUsers(ctx, UserSearchQuery) ([]User, error)` (`search.go`) is authn's one cross-tenant, platform-operator search entry point; no existing signature in this module changed for it. Unlike `FindByID`/`FindByEmail`/`FindByPhone`, which answer "does this one identifier resolve to an account" for ordinary business code that already knows which tenant it is asking about, `SearchUsers` answers "which account (if any) does this identifier or name fragment belong to" with no tenant in scope at all -- `users` is identity data, not tenant data, so only a platform-wide search makes sense here.

`UserSearchQuery` takes exactly one of `Email` (exact match via the email blind index), `Phone` (exact match via the E.164 phone blind index) or `DisplayNamePrefix` (case-insensitive prefix match, the only criterion that can return more than one row, bounded by `Limit` -- zero uses a small default, anything above a hard ceiling is clamped to it). Naming none of the three returns `ErrSearchCriteriaRequired` (`authn.search_criteria_required`, an `apperr.Invalid`) rather than silently answering with every user on the platform.

**`Service` has no opinion on who may call it, by design.** This module never imports `rbac`, so `SearchUsers` performs no internal permission check of any kind -- the caller (`go/admin`'s HTTP handler, gating on `admin:search_users`) is entirely responsible for authorizing the call before it ever reaches here. Treat this the same way as any other security-sensitive, no-internal-authz method in this codebase: never expose it behind a route that is not already gated by the caller's own permission check.

### Authentication

| Symbol | Purpose |
|---|---|
| `Principal` | The authentication result: user, current tenant, session, AMR. No roles, no permissions. |
| `Service.Register / Login / Refresh / SwitchTenant / Logout` | The flows. |
| `MembershipReader` | `ActiveMembership` and `TenantsOf`. Implemented by the host; `org`'s membership service is the canonical implementation. |

### Tokens and passwords

| Symbol | Purpose |
|---|---|
| `KeySource` | Declared here, structurally satisfied by `*pki.Service` with zero import edge. The deleted `TokenKey`/`KeySet`/`NewKeySet`/`GenerateTokenKey` static-injection API has no back-compat path: there is deliberately no second, fallback key-source path. `Signer`/`Verifier` resolve keys from it on every `Issue`/`Verify` call rather than holding a fixed key set; `Signer` calls `KeySource.EnsurePurpose` lazily on the first `Issue`, under one mutex serializing concurrent callers -- success is remembered permanently, while a failed call is retried once by the first `Issue` after a bounded window rather than cached for the process's lifetime -- since `Module.Register` may perform no I/O. |
| `Signer.Issue(ctx, principal)`, `Verifier.Verify(ctx, raw)` | Access tokens, algorithm-pinned to EdDSA; both take a `context.Context`. `Verify`'s `keyFunc` additionally checks the token header's `alg` against the signing key's own `KeySource`-declared `Algorithm` -- defense in depth on top of the parser's single-EdDSA allowlist, so an algorithm addition that forgets this check cannot slip past by accident. |
| `HashPassword`, `VerifyPassword`, `NeedsRehash`, `PasswordParams`, `PasswordPolicy` | argon2id with PHC-encoded parameters. |

### HTTP

| Symbol | Purpose |
|---|---|
| `Middleware(verifier, opts...)` | Optional authentication. Puts a `Principal` in the context. |
| `RequireAuthenticated(next)` | Per-route enforcement. |
| `NewPrincipalResolver()` | Adapts the verified `Principal` to `tenancy.Resolver`. |
| `PrincipalFromContext`, `WithPrincipal` | Context access. |
| `RevocationChecker`, `WithRevocationChecker` | The session-revocation question, and the explicit-checker override. Enforcement is default-wired: a `Service` attaches its own `*SessionManager` to the `Verifier` it hands out, and `Middleware` consults that source with no option at all — see "Immediate revocation is enforced by default, not by host ceremony" below. |

### HTTP surface (spec-first)

| Symbol | Purpose |
|---|---|
| `api/openapi.yaml` | The single source of this module's HTTP surface — every operation under `/api/v1/authn`, path prefix and `operationId`/schema-name conventions per `.claude/skills/backend-coding-standards/SKILL.md` §6.1. `Module.OpenAPISpec()` returns it embedded. |
| `api/authn-server.gen.go` | Generated by `task api:gen` (pinned `oapi-codegen` v2.8.0) from the fragment above — **never hand-edited**. Defines `api.ServerInterface` and every request/response model. |
| `NewHandler(svc) *Handler` | Implements `api.ServerInterface`; its inner routing comes entirely from the generated `HandlerFromMux`. Runs standalone — it is **not** mounted downstream of `tenancy.Middleware` the way a tenant-scoped module's handler is (see "This handler does not run downstream of tenancy.Middleware" below). |
| `ExemptSubtree(routes) (subtree, rest)` (`routes.go`) | The route-partition helper a host's router calls before composing its chains: every route at the module's mount point (`/api/v1/authn`) or below it is returned as `subtree`, so the host can dispatch it on a branch behind `authn.Middleware` alone — outside `tenancy.Middleware`, whose fail-closed resolution would refuse the sign-in flows this module serves (see "This handler does not run downstream of `tenancy.Middleware`" below; a runnable example ships as `ExampleExemptSubtree`). |
| `Handler.requirePrincipal` (unexported) | This module's per-route enforcement for its own HTTP surface: every protected operation calls it first and answers a missing `Principal` with `authn.authentication_required`, exactly what `RequireAuthenticated` does for a handler composed the usual way — see "Per-route enforcement lives inside Handler, not around it" below for why. |
| `Service.ListSessions`, `Service.RevokeSession`, `Service.RevokeOtherSessions`, `Service.ListLoginHistory` (`history.go`) | The self-service device-list and login-history surface: list every session (including revoked ones, newest first), revoke one of the caller's own sessions by id, revoke every session except the caller's current one, list the caller's own login attempts (newest first, limit clamped). |
| `ErrSessionNotFound` | `RevokeSession`'s answer for both "no such session" and "that session belongs to someone else" — deliberately the same answer either way, so the endpoint never confirms another account's session id exists. |

### Federation

| Symbol | Purpose |
|---|---|
| `SocialProvider`, `ExternalIdentity` | The channel interface and what a channel reports about the person who authorized. Every field of `ExternalIdentity` is untrusted third-party input. |
| `NewGoogleProvider`, `NewGitHubProvider`, `NewWeChatProvider`, `NewDingTalkProvider`, `NewFeishuProvider` | The five shipped channels. Each takes injectable base URLs and an injectable `*http.Client`, defaulting to the channel's production hosts and a `pkgcore/safehttp`-guarded client. |
| `WithSocialProviders`, `WithTrustedProviders`, `WithRedirectAllowlist`, `WithOAuthStateTTL`, `WithFederationHTTPClient` | `NewService`/`NewModule` options wiring the channels a deployment offers, which of them may auto-link, where a flow may return to, and (enterprise SSO only) the HTTP client the relying party uses. |
| `Service.SocialAuthorizeURL`, `Service.SocialCallback` | The social sign-in and account-binding flow. |
| `Service.Identities`, `Service.ListIdentities`, `Service.UnbindIdentity` | Binding management. |
| `Service.SSO()` `*SSOService` | The enterprise relying party: `SaveConfig`, `AuthorizeURL`, `Callback`, `Configs()`. |
| `TenantSSOConfig`, `SSOConfigRepository` | The one tenant-domain table this module owns. |
| `RedirectAllowlist`, `NewRedirectAllowlist` | Exact-match redirect URI validation. |
| `PermissionSSOManage` | The one permission this module declares, for writing a tenant's SSO configuration. |

### Preferences (locale and timezone)

| Symbol | Purpose |
|---|---|
| `Service.Preferences(ctx, userID)`, `Service.UpdatePreferences(ctx, userID, patch)` (`preferences.go`) | The stored locale/timezone pair: read, and partial update (nil field = unchanged, non-nil + `""` = cleared), returning the pair as stored afterwards. |
| `PreferencesInput`, `PreferencesPatch` | The pair, and the three-state partial-update shape the PATCH contract needs. |
| `GET/PATCH /api/v1/authn/me/preferences` (`authn_getPreferences` / `authn_updatePreferences`) | The HTTP surface, per-principal by construction. An unstorable value answers `authn.invalid_locale`/`authn.invalid_timezone` (400); the empty value is the "not chosen yet" state and reaches the wire as an absent field, like every other response in this module. |
| `DefaultTimezone = "UTC"`, `DefaultLocale = "en-US"` | The platform defaults: the last tier of every chain, applied when the stored value is empty. |
| `TimeZoneResolver`, `WithTimeZoneResolver` | The registration timezone chain's IP-resolution tier. Optional, and deliberately NOT fail-closed — see below. |
| `canonicalLocale`, `canonicalTimezone`, `registrationLocale`, `registrationTimeZone`, `smsLocale`, `supportedLocales` (unexported) | The validation and chain rules themselves; `supportedLocales()` derives the module's language set from its embedded locale files, the single source the validation, the SMS loader and the Accept-Language negotiation all read. |

### Phone-plus-SMS-code sign-in

| Symbol | Purpose |
|---|---|
| The SMS seam is pkgcore's: `pkgcore.SMS`, `pkgcore.SMSSender`, `pkgcore.NewConsoleSMSSender(w)`, `pkgcore.NewHTTPSMSSender(endpoint, opts...)`, carriers under `pkgcore/sms/` | The message, the delivery seam and every implementation live on the dependency floor, shared with go/notification's sms channel — see "The SMS seam is pkgcore's" below. This module contributes only the wiring option `WithSMSSender` and the wiring-time sentinel below; its former in-package `SMS`/`SMSSender`/constructors are gone, a deliberate breaking change under lockstep versioning. |
| `Service.RequestSMSCode`, `Service.LoginWithSMSCode` | Issue-and-deliver, then verify-and-sign-in. Both never disclose whether a phone number is registered. |
| `ErrMissingDistributedSMSSender` | What `NewModule`/`NewService` fail with when `WithDeploymentMode(pkgcore.DeploymentModeDistributed)` was given and no `SMSSender` was wired (see "A distributed deployment must wire an `SMSSender`" below). |

### TOTP, recovery codes, step-up

| Symbol | Purpose |
|---|---|
| `Service.EnrollTOTP(ctx, principal)`, `Service.ConfirmTOTP` | Start enrollment (returns a secret and an `otpauth://` provisioning URI); confirm with a real code (returns ten recovery codes, shown once). `EnrollTOTP` takes the caller's whole `Principal`, not just a user id: replacing an ALREADY ACTIVE factor requires `principal.AMR` to already carry a completed step-up (`ErrStepUpRequired` otherwise) — a brand-new enrollment, with no active factor to replace, proceeds regardless of AMR. See "A bare access token cannot silently seize an already-active MFA factor" below. |
| `Service.VerifyStepUp` | Re-verifies the CURRENT session with a TOTP code or a recovery code and mints a freshly enriched access token, reusing the existing refresh token. |
| `Service.RegenerateRecoveryCodes` | Replaces a user's whole recovery-code batch; requires an active TOTP factor. The HTTP handler additionally wraps this operation in `RequireStepUp` (unconditionally: every call acts on an already-active factor, so there is no first-time-setup case to carve out the way `EnrollTOTP` has). |
| `RequireStepUp(next)` | Per-route enforcement, `RequireAuthenticated`'s stricter sibling: refuses unless the calling `Principal.AMR` already carries a second factor. |
| `MethodMFATOTP`, `MethodMFARecoveryCode` | The two AMR values a completed step-up can carry. |
| `internal/totp` (`GenerateSecret`, `Code`, `Validate`, `ProvisioningURI`) | RFC 6238 on the standard library only — SHA-1/6-digit/30-second, the one convention every mainstream authenticator app assumes. Not part of this module's public API; `mfa.go` is the only caller. |

---

## Rules

### Do not put a `tenant_id` on any identity table

`users`, `sessions`, `refresh_tokens` and `login_attempts` are identity-domain
data. A person belongs to several tenants, so scoping the person to one makes
the multi-tenant case unrepresentable. None of these models may implement
`dbkit.TenantScoped`, and embedding `dbkit.TenantModel` into one is caught by
`tenancytest.AssertNotTenantScoped` in `model_test.go`.

`sessions.current_tenant_id` is the one column that looks like an exception and
is not. It records which tenant the session's access tokens are currently issued
for, so a refresh knows what to mint. Membership is re-verified against it on
every refresh rather than trusted.

### Repositories hold a plain `*gorm.DB`, and that is the documented pattern

`dbkit.Repository[T]` is constrained to `T: dbkit.TenantScoped`, which identity
data must not satisfy — the plain `*gorm.DB` shape is the documented one for
identity and platform data. The compensating controls are the assertion above
plus two rules that apply to `repository.go` specifically:

* **No `.Table`, `.Model` or `.Raw`.** Nothing here needs them: every conditional
  update passes its target struct to `Updates`, from which GORM parses the same
  schema `.Model` would have named. A semgrep rule in repo-checks watches these
  three entry points.
* **No hand-written `WHERE tenant_id = ?`.** There is no such column to filter on,
  and writing one would mean the model was put in the wrong data domain.

Keep every GORM call in `repository.go`. Nothing else in the module imports gorm.

### Store calls on the sign-in and preferences surfaces retry a bounded number of times on database contention

`concurrency.go` carries this module's one retry envelope: `withConflictRetry`
runs a single store call up to `txRetryBudget` (5) times, retrying only when
`dbkit.IsRetryableConflict` classifies the failure as transient contention --
SQLite's `SQLITE_BUSY`, PostgreSQL's detected deadlock / serialization failure
-- with a small fixed backoff between attempts (the same budget and shape
`go/org`'s `withRetry` and `go/sharing`'s `withTxRetry` use). Any other error
surfaces on the first attempt, unwrapped and unretried, so a real failure is
never masked. Exhausting the budget returns the last conflict error raw: the
caller's existing mapping still answers what a single un-retried attempt
answers today -- `ErrInternal` (500, `authn.internal_error`) -- because this
module deliberately adds no distinct contention code.

`withInsertConflictRetry` is the envelope's insert-shaped sibling for the two
writes that mint their row's primary key (`newID`): a duplicate-key refusal
raised by a RETRY, with the row verifiably present
(`sessionRowExists`/`refreshTokenRowExists`), is the earlier attempt's own
write having landed -- completion, not a failure -- while a duplicate on the
FIRST attempt, or one whose row is absent, surfaces unchanged.

Which calls are wrapped is `concurrency.go`'s own doc comment's authority
(`grep withConflictRetry(` is exact), not this paragraph's: the reads and
writes of the client-visible sign-in and preferences surfaces --
`findByIdentifier`, `SessionManager.Start` (the session and first
refresh-token inserts), `Preferences` and `UpdatePreferences`. The envelope
wraps ONE repository call per attempt, never a business step, which is what
keeps a retry from duplicating the sign-in flow's other effects: the guard's
rate-limit accounting (`ratelimit.go`, KVStore-backed, no SQLite), the
login-history row, the audit emission and the domain events all run outside
it, exactly once per request.

Attempts are not free under contention: each failed SQLite attempt first
waits through the dialect's bounded busy_timeout
(`dbkit/dialect/sqlite`'s 5s default, which applies to readers and writers
alike once another connection holds the file's write lock), so one wrapped
call's worst case is `txRetryBudget` expired busy windows before it surfaces
the same error it always did. The envelope exists so that a lock which frees
within that span answers the request instead of a 500; it changes nothing on
the uncontended path, where the first attempt succeeds.

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

The chain is this way round, not the reverse, because of the resolver
signature: `Resolve(r *http.Request) (pkgcore.TenantID, error)` returns a
tenant and no context, so a resolver that verified the JWT would have nowhere
to hand the claims it just validated. A tenancy-first order would force
verifying every token twice, through two code paths free to diverge, with the
tenant decided by the one that is *not* authorising the request.

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

### Preferences are lenient at registration and strict at PATCH

Locale and timezone reach an account through two write paths with two
different validation contracts, and both are deliberate. Registration
(`Service.Register`, and the social/SSO account mints) is LENIENT: each
value resolves through its chain, a value that cannot be stored is skipped
rather than refused, and the registration succeeds with the field's empty
("not chosen yet") state when no tier yields a storable value. The
preferences PATCH is STRICT: an unstorable value is a coded 400 and
nothing is written. The split exists because a sign-up must not fail over
an optional preference someone can fix in settings a minute later, while
an explicit preference change is a real request that deserves a real
refusal.

The chains (each ending at the platform default, `en-US` for language,
`UTC` for timezone):

- Registration locale: the declared `body.locale` (or, for a social mint,
  the provider-reported profile locale — Google is the shipped channel
  that reports one, read as the standard `locale` claim) → the request's
  `Accept-Language`, the transport channel the frontend's own language
  chain sent its resolved value through → empty.
- Registration timezone: the declared browser report (`RegisterForm`'s
  `Intl.DateTimeFormat` value) → a provider profile field (none of the
  five shipped social channels reports one; the OAuth callback carries no
  browser value either, which is why the social chain enters at the next
  tier) → `TimeZoneResolver.TimeZoneForIP` → empty.
- The SMS body (the "requester is recipient" shape — the person requesting
  the code is the person receiving it): `Accept-Language` (the frontend
  chain's value) → the account's stored locale → `DefaultLocale`.

`TimeZoneResolver` is the one seam here that deliberately does NOT fail
closed: a nil resolver, a resolver error, or an answer that is not a known
IANA zone all yield the empty string and the registration proceeds,
because the tier is a convenience whose absence only means the account
starts at the platform default. `canonicalTimezone` re-validates whatever
the resolver answered, so a wrong zone can never be stored silently; an
invalid stored value would be worse than none.

Two implementation facts that must survive refactors: `preferences.go`
carries `_ "time/tzdata"` so `time.LoadLocation`'s validation runs
identically in deployments whose image has no system zoneinfo — removing
that import makes validation environment-dependent — and
`supportedLocales()` derives the shipped language set from the embedded
locale files rather than a hardcoded pair, so validation, the SMS loader
and the negotiation cannot disagree about which languages exist.

### Immediate revocation is enforced by default, not by host ceremony

`RevocationModeImmediate` promises that sign-out takes effect on outstanding
access tokens at once, and the shipped composition gets that promise with no
extra wiring step: `NewService` attaches its own `*SessionManager` as the
revocation source of the `Verifier` `Service.Verifier()` hands out, and
`Middleware` consults that source on every request whose token verifies (an
explicit `WithRevocationChecker` replaces the source for a `Middleware` built
over a bare `NewVerifier`, or when the revocation authority is not the session
manager). Natural mode — the module default — answers false without touching
the store, so the check costs nothing there; immediate mode pays one
key-value read per request, its documented price.

Selecting `WithRevocationMode(RevocationModeImmediate)` genuinely enforces
sign-out on outstanding access tokens, and `examples/reference-app` runs in
immediate mode as the mandatory first consumer of the enforced mechanism.
There is deliberately no dynamic-configuration twin of the option; see Known
limitations for the removed `authn.session_revocation_immediate` item.

### Sign-in must not answer what it refuses to answer

Every failed password sign-in returns `ErrInvalidCredentials` with no parameters:
unknown account, wrong password, no password set, suspended account, and a
correct password whose account resolves no membership (no membership reader
wired, no membership anywhere, or none of an explicitly requested tenant) are
indistinguishable. An unknown account still costs one argon2id derivation, so a
stopwatch cannot reopen the oracle the error message closed. The membership
collapse matters most: the membership question is asked only after the password
verified, so a distinguishable membership error there (the 403
`tenant_membership_required` / `tenant_membership_unavailable` answers) would
certify the password -- which is why those errors are reserved for paths
answering an already-authenticated caller (tenant switching, refresh). The
specific reason goes on the `login_attempts` row, for the operator and the
account owner.

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

### Refresh re-verifies membership and user status BEFORE consuming the token, not after

`SessionManager.Rotate` is `resolveRotation` (read-only: locate the presented
token, catch an already-consumed one as a replay, load its session) followed
by `commitRotation` (the atomic consume-and-mint). `Service.refresh` calls
them as two separate steps with its own membership and user-status
re-verification run in between, rather than calling `Rotate` as one call and
checking membership afterward.

This ordering is load-bearing, not cosmetic. Consuming the token before
re-verifying would leave a re-verification failure (a `MembershipReader`
timeout, a status flag flapping) with the presented token permanently spent
and the caller never having received its replacement. The client's own,
entirely legitimate retry with that same token would then hit the replay
detector, which cannot distinguish that retry from an actually stolen token,
and pay the real-theft price for it: the whole refresh-token family and the
session revoked, `EventSessionReplayDetected` fired. A two-second
membership-store outage must never look identical to a stolen refresh token.

Do not re-merge `resolveRotation` and `commitRotation` back into one call
inside `refresh` "for simplicity" — that reopens this exact failure. Any
re-verification `refresh` needs must go between the resolve and the commit,
never after the commit.

### `Rotate` is one atomic call; the two-step form is `refresh`'s own

`Rotate` composes `resolveRotation` then `commitRotation` internally, so
callers other than `Service.refresh` (and `SessionManager`'s own tests) get
the atomic consume-and-mint and the replay detection in one call, with no
second step to remember. The two halves are unexported internals — they exist
so `refresh` can slot its own checks between them, not as a public two-step
contract.

### Tokens carry no email and no permissions

No email claim is minted. A `Principal` recovered from a token has an empty
`Email`; it is populated only where the caller actually read the user record. A
bearer credential gets copied into client storage, proxy logs and trace
attributes, where nothing this module controls can redact it.

No permission claim either: a permission list inside a token freezes for the
token's whole lifetime, so a permission revoked at 10:00 would keep working until
it expired.

### Auto-link an existing account only when verified AND trusted

`resolveSocialAccount` (social channels) and `SSOService.resolveAccount`
(enterprise SSO) both implement the same rule: an unrecognised external
identity whose email address already belongs to an account here may be linked
to that account automatically **only when the provider asserts the address is
verified AND the channel is on the platform's trusted-provider list**
(`ConfigKeyTrustedProviders`, default empty). Anything else — verified but
untrusted, trusted but unverified — is refused with
`ErrIdentityRequiresBinding`, never linked. This is the classic social-login
account-takeover: a provider that will hand out an account carrying somebody
else's address, verified or not, hands out that person's account here too if
this module trusts it blindly. The refused path's answer to the caller is
"sign in the way you already can, then bind this identity from your settings
page" — never a hint about which condition failed.

Enterprise SSO adds a **third** condition on top: the existing account must
already be an **active member of the tenant that configured the identity
provider** (`MembershipReader.ActiveMembership`). Without it, a tenant
administrator — who configures the issuer and the allowed email domains, and
may run the identity provider themselves — could allowlist a public domain
and sign straight into any platform user's account at that domain. With it,
the worst they can reach is an account already inside their own tenant, which
they already administer.

The same verified-assertion bar governs SSO's just-in-time branch: for a
subject with no account yet, an account is minted **only** from a claim the
identity provider asserted as verified. An unverified address claim —
`email_verified` absent or false — is refused with `ErrIdentityRequiresBinding`
before any account lookup, whether the address is registered or not, and
provisions nothing: an unverified claim is not usable evidence of anyone's
control over the address, and minting would seat the claimed address in the
platform-unique email index. The mint still grants no membership and no
session; membership of the tenant is the host's decision, made in reaction to
the published `EventUserCreated`, and the subject's next attempt completes
the sign-in.

A VERIFIED claim's just-in-time mint is deliberately **email-less**: the
account is created with no address, the claimed address living only on the
identity row as display data. The verified claim behind a mint is
tenant-grade — an identity provider this tenant's own administrator
configured, for a domain they registered, and that administrator may run the
identity provider themselves (the same premise the membership condition
above bounds). The platform-unique verified-email index is what the social
channels' verified-and-trusted auto-link rule resolves against, and seating
a tenant-grade claim there would let such an administrator mint a "verified"
account at any address whose domain they listed and have the address's true
owner's later, genuinely verified sign-in through a trusted social channel
absorbed INTO that account — a takeover reaching far beyond the tenant the
administrator already administers. Keeping the seat free for its true owner
is what makes the membership condition's bound — "the worst they can reach
is an account already inside their own tenant" — actually hold. The
email-less mint is the same account shape the module already provisions for
a trusted social identity that carries no address, and its cost is confined
to the tenant's own flows: the account's address is not on the users row, so
host-side consumers of `users.email` (notifications, profile display) see
none — no flow exists for the holder to add an address to such an account —
and the account cannot be merged by email with another account of the same
person: merges by email equality alone are refused everywhere else in this
module on purpose.

A channel that reports **no email at all** (WeChat) can never satisfy either
rule's first condition, so it never auto-links. It is not an error path: with
no address to match against, the safe and correct behaviour is to provision a
brand new account, which is what happens.

### The WeChat unionid trap

WeChat's OAuth2 answers with both an `openid` (scoped to the calling
application) and a `unionid` (stable across every application under one WeChat
Open Platform account). `WeChatProvider.Exchange` returns `unionid` as
`ExternalIdentity.ExternalID` and **refuses** with
`ErrSocialIdentityIncomplete` if the response carries no `unionid` at all — it
never falls back to `openid`. Keying on `openid` would silently split one
person into a different `user_identities` row per application that calls this
code, which is invisible until someone asks "why did I have to bind WeChat
twice".

### Unbinding cannot leave an account with no way in

`UnbindIdentity` refuses (`ErrLastLoginMethod`) when removing the identity
would leave `LoginMethodCount` at zero. There is no self-service recovery from
a locked account with no password, no verified phone and no remaining
identity, so the operation is refused rather than confirmed. A password
counts once; a **verified** phone counts once (an unverified one enables no
sign-in method, so it does not count); every bound identity counts once. An
identity that belongs to someone else answers exactly like one that does not
exist (`ErrIdentityNotFound`) — the endpoint never confirms that a binding is
real.

### `state` is single-use, tenant-scoped for SSO, and checked before the code is exchanged

`StateStore.Consume` uses `KVStore.CompareAndSwap` rather than a read followed
by a delete, so two callbacks racing on the same `state` produce exactly one
winner rather than two credited sign-ins. A social flow's `state` is bound to
`(provider, sessionBinding)`; an enterprise flow's channel name is
`"oidc:" + tenantID` (`SSOChannelName`), so a `state` issued for one tenant's
identity provider cannot be redeemed against a different tenant's callback —
the tenant is folded into the channel identity `Consume` checks, not passed as
a trusted parameter. `SocialCallback` and `SSOService.Callback` both consume
the state **before** exchanging the code, so a forged or replayed callback
never reaches the third party at all.

### `tenant_sso_configs` has no database-level "one row per tenant" constraint

Exactly one SSO configuration per tenant is the rule, and `SaveConfig`
enforces it in the normal path by reading `Current` first and updating the
existing row rather than creating a second one. There is
deliberately **no** `UNIQUE` index on `tenant_id` alone backing that up: such
a constraint would reject the second of the two rows per tenant that the
**mandatory** `tenancytest.AssertIsolated` suite creates to prove `List`
actually filters by tenant (a single-row list cannot distinguish "correctly
scoped" from "returned everything"). `Current` resolves the residual — a rare
race between two concurrent first-time `SaveConfig` calls could momentarily
leave two rows — deterministically, by most-recently-updated row with ID as
the tie-break, so every reader agrees and the next `SaveConfig` collapses back
to one row by updating whichever row `Current` returned. See
`TenantSSOConfig.TenantID` and `SSOConfigRepository.Current` in `oidc.go`.

### The enterprise channel has a tenant-id budget, and the SSO configuration is refused, never truncated, past its column widths

Both are the REFUSE branch of this module's dual-dialect width rule (the
per-column cut-versus-refuse decisions are in `model.go`'s column-width doc
comment): PostgreSQL enforces a declared `VARCHAR(n)` width where SQLite
ignores it, and on these two surfaces the value cannot be cut.

- **The tenant id.** An enterprise identity is stored under the synthetic
  provider name `"oidc:" + tenantID`, which lands in
  `user_identities.provider`, `VARCHAR(64)` (migration 0005) — so a tenant id
  of 60 runes or more (the 59-rune budget is `oidc.go`'s
  `ssoTenantIDMaxWidth`, derived from the column width minus the prefix) can
  never be represented at all. Truncating the name is not an option: the
  `(provider, external_id)` unique index would then conflate two tenants'
  identities. `SSOService.SaveConfig`, `AuthorizeURL` and `Callback` all
  refuse such a tenant id with `authn.sso_tenant_id_too_long`, so the host
  learns which tenant name is too long at configuration/entry time — never
  through a broken identity write at some later sign-in.
- **The configuration values.** A tenant administrator's own specification
  (`issuer` `VARCHAR(512)`, `client_id` `VARCHAR(255)`, `allowed_domains`
  `VARCHAR(1024)`, migration 0006) is refused with an error naming the field
  (`authn.sso_issuer_too_long`, `authn.sso_client_id_too_long`,
  `authn.sso_allowed_domains_too_long`), never silently shortened — cutting
  an issuer URL would point enterprise single sign-on at the wrong endpoint.
  The refusals fire in `SaveConfig` (against the stored forms: trimmed issuer
  and client id, the joined domain list) and again in
  `SSOConfigRepository.Create`/`Update`, so no write path can slip an
  over-width row past the boundary. Bounds are applied in runes, the count
  PostgreSQL's width is in.

### Cost parameters are bootstrap config; policy is dynamic config

`PasswordParams` (argon2id memory, iterations, parallelism) depends on the
machine, must be identical across replicas, and must not be tunable from an admin
console — an operator could make sign-in uselessly cheap or slow enough to be a
self-inflicted denial of service. `PasswordPolicy` (minimum length, denylist) is
the opposite and is dynamic.

The parameters travel *inside* each stored hash, so raising the cost is a
configuration change rather than a migration: existing hashes keep verifying, and
`Login` upgrades one on its owner's next successful sign-in.

### A distributed deployment must wire an `SMSSender`

`WithDeploymentMode(pkgcore.DeploymentModeDistributed)` is how a host tells
`NewModule`/`NewService` which deployment mode it is being wired for, solely
so construction can enforce that a distributed deployment supplies an
explicit SMS sender (`WithSMSSender`) rather than silently defaulting to
`pkgcore.NewConsoleSMSSender`, which prints to a writer nobody in a
distributed replica pool is reading. It is the ONE piece of deployment-mode
awareness this module carries, and it lives in `newOptions`' validation —
never in `Service`'s business logic — because the fallback it refuses is this
module's own: the assembly resolves the sender the composition selected (this
module's component descriptor reads it with `pkgcore.Get` and hands it to
`WithSMSSender`; see `pkgcore.SMSSender`'s doc comment), while a module that
receives none falls back to `pkgcore.NewConsoleSMSSender`, a fallback no
assembly mechanism sees — only this module's wiring-time validation refuses
it under a distributed deployment — at the same
moment `newOptions` validates `WithKeySource` and `WithBlindIndexKey`.
Omitting `WithDeploymentMode` — every standalone deployment — is equivalent
to standalone and keeps working with the console default.

### The SMS seam is pkgcore's

The delivery seam phone-login verification codes go out on is pkgcore's own
(`pkgcore.SMS` and `pkgcore.SMSSender`, mirroring the `Mail`/`Mailer`
contract), shared with go/notification's sms channel: notification sits
below this module in the dependency graph, so a seam this module owned
could not serve it, and a host wiring both modules now hands ONE
implementation to both `WithSMSSender` options. The implementations are
pkgcore's too: `pkgcore.NewConsoleSMSSender(w)` (the standalone console
transport, which doubles as the test double), `pkgcore.NewHTTPSMSSender`
(an operator-run JSON gateway, SSRF-guarded through `pkgcore/safehttp`),
and the three real carrier adapters under `pkgcore/sms/` (next section).
The seam registers two component descriptors of its own in pkgcore's assembly
machinery — `sms.console` and `sms.http`, each carrying its capability bits —
and a composition that selects one resolves the sender through the registry,
each consumer's own component descriptor reading it with `pkgcore.Get` into
its module option (`WithSMSSender` here and in go/notification); this
module's distributed-mode requirement above is the wiring-time enforcement
that stays here.

### The three carrier adapters (aliyun, tencent, twilio)

This seam has three real carrier adapters, each in its own subpackage of
pkgcore — `go/pkgcore/sms/aliyun`, `go/pkgcore/sms/tencent`,
`go/pkgcore/sms/twilio` — so a host wires whichever carrier it has an
account with, exactly as it wires the console or HTTP-gateway transport:
construct with the package's `NewSender`, hand the result to `WithSMSSender`
(here, or to go/notification's). The subpackage split is the same packaging
answer `go/billing/gateway` gives for payment channels: a consumer that
never wires a carrier imports none of the three, and none of the three adds
a third-party `require` to pkgcore's `go.mod` (each adapter is
stdlib-only; the measured cost of the official SDKs is recorded below).

Each adapter implements its vendor's REAL signing and request shape, verified
offline where the vendor's signing is deterministic:

- `aliyun` POSTs Aliyun SMS's SendSms action (Dysmsapi, version 2017-05-25) to
  the fixed `dysmsapi.aliyuncs.com` gateway with the full RPC parameter set
  in the request line's query string and an empty body -- the wire placement
  both official dysmsapi SDK generations use -- signed with the RPC
  mechanism Aliyun's own documentation fixes: parameters sorted by key,
  percent-encoded per RFC 3986 (space as `%20`, never `+`), then
  `base64(HMAC-SHA1(AccessKeySecret + "&",
  method + "&%2F&" + encode(canonicalQueryString)))`. Its unit tier pins the
  mechanism against the worked example of Aliyun's own signature
  documentation (signature `9NaGiOspFP5UPcwX8Iwt2YJXXuk=` for the doc's
  fixed inputs) and a SendSms-shaped request against a signature an
  independent implementation precomputed.
- `tencent` POSTs Tencent Cloud SMS's SendSms action (version 2021-01-11) as
  JSON to the fixed `sms.tencentcloudapi.com` gateway, signed with
  TC3-HMAC-SHA256 per Tencent's API-3.0 signature documentation: a canonical
  request over the content-type and host headers and the payload's SHA-256,
  a string-to-sign binding the Unix timestamp and the
  `date/sms/tc3_request` credential scope, and the `"TC3"+SecretKey`-seeded
  key derivation chain. The signed-header set (`content-type;host`, no
  x-tc-action) mirrors the current official tencentcloud-sdk-go signer byte
  for byte -- the signing shape exercised against Tencent's real gateway by
  every production SDK customer. Its unit tier pins the canonical request,
  the string to sign and the final Authorization header against
  independently precomputed values.
- `twilio` POSTs the account's Messages resource
  (`/2010-04-01/Accounts/{AccountSid}/Messages.json`, fixed base URL) with
  Basic auth over AccountSID/AuthToken and an `application/x-www-form-urlencoded`
  body of To/Body plus exactly one sender (From or MessagingServiceSid —
  mutually exclusive, refused at construction). Twilio signs nothing, so its
  unit tier pins the entire wire shape offline.

**No vendor SDK is used — raw signed HTTP throughout, and the choice is
measured, not assumed.** pkgcore tracks dependency cost with the
repository's own method (a bare consumer, `GOWORK=off go mod tidy`, count
`// indirect` entries); measured for the three official Go SDKs in
throwaway modules of the work that adopted the raw-HTTP shape: the
official Aliyun dysmsapi Tea SDK
(`alibabacloud-go/dysmsapi-20170525/v2`) costs 16 indirect entries, the
legacy `aliyun/alibaba-cloud-sdk-go` costs 6, Tencent's `tencentcloud-sdk-go`
sms submodule costs 1 (its `common` pinned to the same version train as every
other Tencent product submodule), and `twilio/twilio-go` costs 2. Each
adapter's signing is a small, deterministic, officially documented algorithm
(an RPC canonical form, the TC3 chain, or Basic auth) implementable in
well-understood stdlib code — the SDKs' real surface (credential chains,
retries, whole-product client trees) far exceeds the one action this seam
needs, and even a subpackage-scoped SDK dependency would land in the
owning module's `go.mod` and every workspace member's build. The adapters
therefore add +0 dependencies and keep the seam light; the offline vectors
pin the algorithms against values no Go code produced, so the usual SDK
benefit — "the vendor maintains the signature" — is replaced by a
maintained, test-pinned transcription of the vendor's published
specification.

Three boundaries bind every adapter, each documented in its package doc:

- **Templates.** Aliyun and Tencent have no free-text send — every message
  instantiates an approved account template, and each vendor has one
  approved template per kind of message. Those two adapters are driven by
  the seam's template identity: `pkgcore.SMS` carries the locale message id
  the body was rendered from, the locale it was rendered in, and the
  interpolation values, and each adapter's `Config.Templates` maps
  `"<locale>/<message-id>"` to an approved template plus the variables it
  declares (Aliyun by name, Tencent positionally). An unmapped pair, or a
  declared variable with no value, is refused before any request — so a host
  wiring one must register a template per (locale, message-id) pair its
  codes can produce, with variable names matching the parameters the copy
  references. Twilio alone carries the text free-form in `Body`. This
  module's send carries `MessageID` = `authn.sms.verification_code` and
  `Locale` = the locale the body was ACTUALLY rendered in — the
  post-fallback value `renderSMSCode` reports, never the raw request-side
  one, so an empty-locale account's code still maps.
- **Phone forms.** The seam's contract — pass `SMS.To` through unchanged,
  never normalize — holds for all three adapters; each package doc records
  the form its vendor's API accepts (Aliyun: domestic numbers with `+`,
  `+86`, `0086`, `86` or no prefix, international as country-code-plus-number;
  Tencent and Twilio: E.164 with a leading `+`), and the vendor's own refusal
  surfaces through `Send`'s error for anything else.
- **Credentials.** `Config` is validated eagerly in `NewSender`, which
  returns an error naming the missing field — never its value; no error or
  log path in any adapter echoes a secret, and all three talk TLS to fixed
  gateway constants through the `pkgcore/safehttp` guarded client by default
  (`WithClient` exists for tests pointing at a loopback server, mirroring
  `pkgcore.NewHTTPSMSSender`'s own option).

Every adapter's `integration_test/` carries an env-gated live leg (the alipay
sandbox-leg precedent): it runs only when the operator's own credentials are
present (`ALIYUN_SMS_*`, `TENCENT_SMS_*`, `TWILIO_SMS_*` — names recorded in
each leg's package doc), otherwise it skips itself with a note saying exactly
which variables are missing and where they come from. Each leg sends ONE
real, billable message (roughly CNY 0.045 for the two Chinese carriers,
USD 0.008 for Twilio) — none of it runs without those credentials; see
Known limitations for the standing boundary.

### Verification codes and recovery codes are hashed, not argon2id'd

`VerificationCode.CodeHash` and `UserRecoveryCode.CodeHash` are plain
SHA-256 digests, the same choice `RefreshToken.TokenHash` already makes and
for the identical reason: the plaintext is drawn by the SERVER with full
entropy over a space small enough (six digits) or large enough (recovery
codes) that there is no offline dictionary attack a slow hash would need to
defend against. Brute-force resistance comes from the attempt counter plus
lockout (`VerificationCode.Attempts`/`MaxAttempts`) and the `go/ratelimit`
guards in `ratelimit.go`, not from hashing cost. Do not "upgrade" either to
argon2id — it would only slow down the legitimate verify path.

### A phone-login code, once consumed, cannot be replayed — and neither can a TOTP code

Both `VerificationCodeRepository.Consume` and `RefreshTokenRepository.Consume`
share the identical compare-and-swap shape (`WHERE id = ? AND status =
'active'`, then `Updates`): two concurrent verifications of the same code
must produce exactly one winner, decided by the database, never by a read
followed by a write. `MFAFactorRepository.UpdateLastUsedStep` and
`RecoveryCodeRepository.MarkUsed` are the same pattern applied to TOTP's
`last_used_step` counter and to a recovery code's `used_at IS NULL` check,
respectively. Do not "simplify" any of these four into a plain read-then-branch — that reopens exactly the race replay detection exists to close.

The REFUSAL is one control, but its classification is not one code:
`ErrMFACodeUsed` (`authn.mfa_code_used`) answers a code that passed the
actual TOTP check or matched an issued recovery row but is spent — the
replay guard already advanced past its step, the row is marked used, or
the compare-and-swap lost to a concurrent use of the same code — while
`ErrMFAInvalidCode` (`authn.mfa_invalid_code`) answers a code that was
never valid (a wrong TOTP code, or a recovery code no issued row
matches). The split exists so a holder of a consumed code is told it is
consumed instead of "invalid, try again"; it discloses nothing a
brute-forcer can use, because only a caller already in possession of a
genuine code can observe it (a wrong guess can never match a spent
row's step or hash). Verification therefore looks a used recovery code
up too (`RecoveryCodeRepository.FindByUserAndHash`; the unused-only
`FindUnusedByUserAndHash` stays for callers that only ever handle an
unused code). Do not collapse the spent-code shape back into
`ErrMFAInvalidCode` — a user who re-submits a code their own earlier
submit consumed would be told it is wrong and retryable when it can
never verify again — and do not weaken the refusal itself: spent is
still refused, only the answer differs.

### Step-up elevation is NOT persisted to the session — that is what bounds it

`VerifyStepUp` mints a fresh access token whose AMR gained `mfa:totp` or
`mfa:recovery_code`, but it never writes that back to `sessions.amr`. The
elevation therefore lives only as long as that ONE access token
(`ConfigKeyAccessTokenTTL`, 15 minutes by default): a subsequent natural
`Refresh` mints from `session.AMRList()` — the session's ORIGINAL
authentication methods — which is what makes step-up a periodic re-proof
rather than a permanent unlock for the rest of the session, with no separate
expiry timer needed. Do not add one. Do not make `VerifyStepUp` persist the
enriched AMR onto the session row "for convenience" — that removes the
property entirely.

### Every session-mutating call checks `session.ExpiresAt`, not just `session.Status`

A session past its own `ExpiresAt` is not usable, even while its `Status` row
still reads `active` — nothing in this module proactively flips `Status` away
from active when a session merely times out; expiry is a read-time check, not
a write nobody performs. Every session-mutating path — `Rotate`
(`session.go`), `SwitchTenant` (`service.go`) and `VerifyStepUp` (`mfa.go`)
— checks both `session.Status != SessionStatusActive` and
`!now().Before(session.ExpiresAt)` together, refusing either with
`ErrSessionRevoked`. A path that checked only `Status` would leave a session
that had genuinely expired — but whose row nobody had touched — usable for
as long as the caller's already-issued access token remained valid, a
session's practical lifetime stretched past its own configured TTL by one
access-token lifetime. Any new session-mutating method must carry the same
pair of checks together — never `Status` alone.

### A bare access token cannot silently seize an already-active MFA factor

`Service.EnrollTOTP` replaces any existing PENDING factor before enrolling a
fresh one and leaves an existing ACTIVE factor in place until a confirm
genuinely succeeds (see `MFAFactorRepository.ReplacePending`/`Confirm` and
the `(user_id, type)` partial indexes, migrations 0010 and 0011). Without a
check, a bare, unelevated access token — the shape a stolen one has — could
call `AuthnEnrollTOTP` then `AuthnConfirmTOTP` with a secret it chose
itself, replacing a victim's active factor with no re-proof at all.
`EnrollTOTP` therefore takes the caller's whole `Principal` and refuses with
`ErrStepUpRequired` when an ACTIVE factor already exists and
`principal.AMR` does not already carry one (`hasSecondFactor`) — this is
enforced INSIDE the service rather than by wrapping the route in
`RequireStepUp`, because whether step-up is even required depends on
whether an active factor exists to protect, information only the service
has without an extra round trip. A brand-new enrollment (nothing active to
replace) needs no step-up and proceeds regardless of AMR: turning MFA on
for the first time is not a step-up case, while changing an existing factor
is. `AuthnRegenerateRecoveryCodes` has no such first-time case (it
requires an active factor as its own precondition), so its handler wraps
the whole operation in `RequireStepUp` directly instead.

### `RequireStepUp` has no password-re-entry fallback for an account with no MFA

An account with no second factor enrolled has nothing to step up WITH, so
`RequireStepUp` blocks a sensitive action unconditionally for it rather than
falling back to, say, re-entering a password. This is a stated, deliberate
gap, not an oversight — see Known limitations.

### Rate limiting is two layers, and they answer different questions

`go/ratelimit`'s `Limiter.Allow` gives the raw sliding-window counters
(`rateGuard.allow` in `ratelimit.go`) — it deliberately understands nothing
about "account", "progressive" or "lockout": `Allow` answers one dimension
per call and nothing else. This module's `rateGuard` adds the one thing
`go/ratelimit` does not: a
progressive login-failure delay (`RecordLoginFailure`/`RecordLoginSuccess`)
that grows exponentially from `loginLockoutBase` and saturates at
`loginLockoutMax`, which is what turns "delay" into an effective, bounded
"lockout" with no separate threshold constant to keep in sync. Every check —
the plain sliding window AND the progressive lockout — fails CLOSED on a
`KVStore` error: an unanswerable rate-limit question is a refusal, the same
policy `resolveTenant` and the revocation check already apply to theirs.
Every code-send and code-verify endpoint, plus login, registration and
step-up, goes through `rateGuard`; do not add a new endpoint that skips it.

### This handler does not run downstream of `tenancy.Middleware`

Every other module's `Handler` in this codebase (see `examples/reference-app/internal/notes`)
runs behind `tenancy.Middleware` and reads its tenant from `ctx` via
`pkgcore.TenantFromContext`. `handler.go`'s `Handler` deliberately does not:
most of its operations happen BEFORE any tenant is known at all (registration,
sign-in, token refresh, the social/SSO callbacks), and the ones that do act
inside a tenant (`SwitchTenant`, and indirectly `Login`/`LoginWithSMSCode`/
`SocialCallback`'s optional `tenant_id` request field) read it from the
Principal's own `TenantID` claim, never from `ctx`. Do not add a
`pkgcore.MustTenantFromContext` call to this handler "for consistency" — there
is no tenant in `ctx` for this handler to read, by design.

### Per-route enforcement lives inside `Handler`, not around it

`RequireAuthenticated` (`middleware.go`) is built for a handler where an
ENTIRE mounted route is protected or not. `Handler` serves both public paths
(`register`, `login/*`, `token/refresh`, `social/*/authorize`,
`social/*/callback`) and protected ones (everything else) on ONE mounted
handler, so wrapping the whole thing in `RequireAuthenticated` would lock out
the public paths too. `Handler.requirePrincipal` is the per-OPERATION
equivalent: every protected method calls it first and writes
`authn.authentication_required` on a miss, exactly `RequireAuthenticated`'s
own answer. Do not add a route-level `RequireAuthenticated` wrapper around
`Handler` as a whole — it breaks every public operation.

### The pre-authentication cookie is what the social `state` is actually bound to

`AuthnSocialAuthorize` mints (or reuses) an opaque `authn_preauth` cookie and
passes `BindingFromCookie(value)` — its SHA-256 digest, never the raw value —
as `SocialAuthorizeInput.SessionBinding`. `AuthnSocialCallback` reads the SAME
cookie back and derives the SAME digest to pass as
`SocialCallbackInput.SessionBinding`; a callback carrying no cookie at all is
refused with `authn.oauth_state_invalid` before the state store is even
consulted, since it cannot possibly have originated from this server's own
authorize step. This is the concrete implementation of
`SocialAuthorizeInput.SessionBinding`'s "derive it from a pre-authentication
cookie" doc comment (identity.go) for this module's own HTTP surface — a
consumer that builds a DIFFERENT transport (a native mobile client with no
cookie jar, say) must derive `SessionBinding` some other way, but it must
still be something the callback request could not have without having seen
the authorize response.

The cookie is `HttpOnly` and carries `Secure` when the request arrived over
TLS (`r.TLS`) OR the host assembled the service with `WithSecureCookies(true)`.
The option exists because `r.TLS` is nil for every request in the most common
production topology — TLS terminated at a load balancer or reverse proxy, the
Go process itself only ever seeing plaintext HTTP — so a host serving HTTPS
externally must pass it (it knows its own topology; the handler deliberately
never infers the scheme from a client-supplied header, the same reasoning
`clientIP`'s doc comment gives for reading forwarding headers only from a
request whose peer is a declared trusted proxy, never from a client that can
set its own). A host not actually serving over HTTPS anywhere must not pass
it.

### Every recorded address is the client's, gated on host-declared trusted proxies

The address this module records -- the per-IP rate-limiter key, the `Session.IP`
column, the `LoginAttempt.IP` login-history column, the address carried by the
session and login events -- is resolved by `Handler.clientIP` (handler.go).
The resolution has one trusted-proxy-aware shape: a request's direct
connection address (`RemoteAddr`) is the answer UNLESS the host declared the
proxies it receives requests through (`WithTrustedProxies`) AND that
request's peer is one of them, in which case the forwarding headers are
read. Everything else -- no proxies declared, a peer that is not one of them,
a malformed chain, a chain naming only proxies -- falls back to the
connection address. That gate is what keeps a direct client from minting its
own recorded address with a spoofed header: a header is only read from a
request whose peer the HOST declared trustworthy.

WHICH header may be read under that gate is a second, separate decision. Two
kinds exist, on two footings:

- **`X-Forwarded-For` needs no per-header declaration.** The chain protects
  itself: a proxy appends the peer it saw, so `clientIP` walks the chain from
  the right, stripping the entries that name declared proxies until the first
  untrusted entry -- the address the leftmost trusted proxy actually saw -- is
  found; an entry that does not parse as an IP ends the walk with no answer.
  The entries that decide the answer are the declared proxies' own appended
  work, so nothing a client wrote can displace them.
- **A single-hop vendor header (`VendorClientIPHeaderFlyClientIP`, wire value
  `Fly-Client-IP`) is read ONLY when the host opted into that specific header
  (`WithVendorClientIPHeaders`) -- and even then only when the chain walk
  produced no answer.** The reason it cannot share `X-Forwarded-For`'s
  gate-only footing is that the gate cannot tell the two topologies apart: a
  request from Fly's proxy (which overwrites `Fly-Client-IP` per request) and
  a request from a generic reverse proxy (nginx, ALB, Envoy, Cloudflare --
  which forwards unknown headers verbatim) are identical to the process behind
  them, so reading a vendor header under the trusted-peer gate alone would
  let a client smuggle its own value through any non-Fly proxy a host had
  declared -- minting its own recorded address AND its own rate-limiter
  bucket (the register budget's only dimension) -- which is why the
  per-header opt-in exists. Only the host knows which topology it
  runs, so the opt-in is its declaration, mirroring `WithSecureCookies`; the
  closed `VendorClientIPHeader` set (validated at wiring time) keeps the
  option from becoming a bare list of arbitrary header names, each
  recreating the same hole. A Fly.io deployment declares its proxy ranges with
  `WithTrustedProxies` AND opts into the Fly header here; a deployment behind
  a generic proxy configured to append to `X-Forwarded-For` needs only
  `WithTrustedProxies`.

Entries that are neither IP addresses nor CIDR prefixes are refused at wiring
time (`newOptions`), and a declared proxy must overwrite or strip the
`X-Forwarded-For` it receives from its own clients, so a client cannot smuggle
a header through the proxy it is trusted for (Fly.io's proxy does; a generic
reverse proxy must be configured to). The module never guesses a proxy range
or a header's provenance from the request itself.

### `RevokeSession` on someone else's session answers 404, never 403

`ErrSessionNotFound` is deliberately the same answer for "no such session"
and "that session belongs to a different account" (`history.go`'s own doc
comment) — the same no-existence-disclosure shape `ErrIdentityNotFound`
already has for `UnbindIdentity`. Do not special-case "session exists but
isn't yours" into a 403: that confirms the id is real, which is exactly what
a caller enumerating other users' session ids is trying to learn.

### A 429's `Retry-After` header is `writeAppError`'s job, not `ratelimit.go`'s

`ErrRateLimited` and `ErrAccountLocked` carry a `retry_after_seconds`
PARAMETER (an integer, for `{code, params}` interpolation); the HTTP
`Retry-After` HEADER is a transport-specific translation of that parameter,
so `writeAppError` (`middleware.go`) is what emits it — the HTTP translation
is the caller's job, not the business-logic layer's — one implementation
shared by `Middleware`, `RequireAuthenticated` and every `Handler` operation
below. Do not duplicate this logic inside `handler.go`; call the shared
`writeAppError`.

### A revoked session's reason reaches the sessions list tiered, never verbatim for a security mechanism

`AuthnSession.revoke_reason` (the spec fragment's schema, `api/openapi.yaml`)
is the sessions list's answer to "why did this session stop", exported only
on a revoked row. The answer is a TIERED PROJECTION, never the stored
reason: a reason recording the owner's own action exports as itself
(`logout`, `user_revoked`, `revoke_others` — the value space is the schema's
enum), while a reason recording a security mechanism's action (today:
`replay_detected`) never exports as itself and folds into the schema's one
generic value `security_revoked`. The owner reads "closed by a security
mechanism, not by me"; an observer of the response — an attacker who may
well have caused the mechanism to act — never learns which mechanism fired,
so a replay theft is not confirmed to the thief through the victim's own
device list.

The classification rule is written at the vocabulary's declaration sites:
every `RevokeReason*` constant in `model.go` and `history.go` states its
class (owner-action or security-mechanism) in its own doc comment, and
`exportRevokeReason` (`model.go`) is the rule's single implementation,
naming every member of both classes — declaring a reason without taking
that position is incomplete, and the projection's default folds an
undeclared value rather than passing it through, so no unclassified value
can ever leak. The fold happens only at the API boundary: it is applied in
`toSessionResponse` (`handler.go`) and never in any write path — the stored
`Session.RevokeReason` (the `revoke_reason` column) keeps the REAL reason
(`replay_detected` stays in the column, the forensics/audit record). The
in-process `Service.ListSessions` rows still carry the stored values; only
the wire projection folds. Do not "simplify" the projection into the write
path or into `ListSessions` itself: both would destroy the stored evidence
or hand in-process consumers a value that no longer matches the column.

The projection's value space is closed and defined by the schema enum, and
a rendering consumer follows the same known-token discipline as the
login-history failure reasons (the account surface passes a reason token
through `t()` only when it is on the surface's known list, anything else
rendering the generic label): `security_revoked` is a security event worth
acting on, never a mechanism name. The storage half is pinned by
`TestSessionManager_Rotate_ReplayRevokesTheFamilyAndTheSession`
(`session_test.go`), the wire half by
`TestHandler_ListSessions_TieredRevokeReasonExport` (`handler_test.go`),
which drives a genuine replay and a genuine logout and asserts the two rows
export differently while the stored reasons stay verbatim.

---

### `demoseed` seeds demo accounts, and only demo accounts

`authn/demoseed` (`NewSeeder`, `(*Seeder).Register`) is the shared shape behind a
host's boot-time demo seeding: register a demo account through the composed
handler's real register route — so the password hashing, the password policy and
the per-IP register budget a demo exercises are the real ones — after asking
`authn.Service.SearchUsers` (passed as the `SearchUsers` method value) whether
the account already exists. Only a genuinely absent account is POSTed, which is
what keeps a restart from debiting the public register budget for accounts a
previous boot created (under the distributed deployment mode that budget lives
in a shared store and accumulates across restarts), and the register answer is
classified by code: the already-registered conflict is recovered into the id
authn assigned, the rate-limit refusal is named with its limit and remedy, every
other non-201 fails loudly.

**It exists for demonstration and development deployments only; production code
must not import it.** Registering accounts at boot from an operator-set password
is not a provisioning flow, and the lookup it recovers ids with is the
platform-operator search. The package carries that stance in its shape, not only
in prose: the package name and every symbol say "demo", `NewSeeder` refuses to
construct without a caller-declared demo email domain, and `Register` refuses —
before any lookup and before any request — an email outside that domain, so a
production call site either writes the contradiction down (a real domain
declared as its demo domain) or fails loudly on its first call.
`examples/reference-app` is the first consumer (`internal/app/demo/demo_users.go`'s
`SeedDemoUsers` and `internal/app/demo/demo_admin.go`'s `SeedDemoPlatformStaff`).

---

## Testing

```
go -C go/authn test ./... -race                    # unit tier, no Docker
golangci-lint run ./...                             # from inside go/authn
go -C go/authn test -tags=integration -race ./integration_test/...   # PostgreSQL, Docker required
```

All three are wired into CI: the unit tier and lint run through the shared
`go-module-ci` matrix in both `fast-check.yml` (every PR) and `full-check.yml`
(`full-ci`-labeled PRs), and the PostgreSQL integration tier runs in
`full-check.yml`'s `integration-tiers` matrix — `go/authn` is a row in both
matrices, and `Taskfile.yml`'s `INTEGRATION_DIRS` carries the same entry for
`task test:full`'s local loop.

`examples/reference-app` is this module's mandatory first consumer:
`internal/app/server.go` wires the real `Module` into the
composed server ahead of `notes`' and `config`'s, and
`flowtests/authn_e2e_test.go` drives all three sign-in channels (password,
social, phone+SMS) plus session revoke/refresh end to end through that real,
composed HTTP server — the proof this module's own package-level tests
cannot give, because they never assemble the actual middleware chain a host
wires (`authn.Middleware` then `tenancy.Middleware(NewPrincipalResolver())`,
see "The middleware chain is authn, then tenancy" above) around a real
`net/http` server.

`Handler`'s own `recordAudit` (backing every declared audit action except
`AuditActionSSOConfigure`, whose emission lives at the service layer — see
this file's Known limitations row) is proven at the
`Handler` layer, not `Service`: `handler_test.go`'s `newAuditTestHandler`
builds a real `*pkgcore.ComponentRegistry` (`pkgcore.NewComponentRegistry`, the same
construction `module.go`'s `Register` runs in production) with the declared
actions already added, wires it into the `Handler` under test, and
subscribes a `testutil.EventRecorder` to `audit.EventRecorded` on the same
bus — `TestHandler_LoginWithPassword_ValidCredentials_RecordsLoginAuditEvent`,
its wrong-password sibling, and
`TestHandler_Logout_ValidPrincipal_RecordsSessionRevokeAuditEvent` are the
representative sample (login success, login failure, session revocation).
One audit record is proven at the layer that emits it rather than through
the `Handler`: the replay response, which no handler can record (a refresh
request is credential-less, so the handler answering its 401 has nothing to
attribute a row to), is proven by
`TestService_Refresh_ActualReplay_LeavesADurableAuditRecord`
(`service_test.go`), which drives a real `Module.Register` over a real
`*pkgcore.ComponentRegistry` -- so the registrar wiring under test is module.go's
own -- and asserts the resulting `authn.session.revoke` row's
refused-presentation shape (`Result.Success` false, `RevokeReasonReplay` as
`FailureReason`), its attribution to the account owner, its tenant stamp,
and that the replay added no login-attempt row. `recordAudit`'s own
bus-less short-circuit is pinned by
`TestHandler_NilBus_LogsTheInoperativeAuditStateOnce` (`handler_test.go`),
which asserts the inoperative state is announced exactly once per Handler.

Every new model gets `tenancytest.AssertNotTenantScoped` (identity data) or
`AssertIsolated` (tenant data). Shared fakes live in `internal/testutil`, which
deliberately does **not** import this package — a test file in `package authn`
cannot import anything that imports `authn`, so the membership fake there
satisfies `MembershipReader` structurally.

Concurrency is not optional to test here. The single-winner property of
`RefreshTokenRepository.Consume` is what replay detection rests on, and it is
exercised under `-race` by many goroutines racing one token at the unit
tier (`session_test.go`) and again against a REAL PostgreSQL server at the
integration tier (`integration_test/postgres_refresh_rotation_test.go`) —
SQLite's coarse table-level locking can pass the unit-tier version even with
a broken CAS, which is exactly why the same property gets a second,
real-database proof.

The conflict envelope's contract is pinned deterministically by
`concurrency_test.go` — retry only on `dbkit.IsRetryableConflict`, immediate
unretried surface of any other error, the `txRetryBudget` attempt bound, the
raw last-conflict exhaustion answer, and the insert residue rule (a duplicate
on a retry with the row present is completion; without the row it surfaces) —
and its end-to-end half runs a real cross-connection tournament on a
file-backed SQLite unit-tier database:
`TestLoginAndPreferences_SecondConnectionHoldingTheWriteLock` checks a second
connection out of the pool, has it hold the file's write lock (`BEGIN
EXCLUSIVE`) past the dialect's busy_timeout while the real `Handler` serves a
password sign-in and a preferences read, and asserts both answer 200 once
the holder commits. Without the envelope both answer 500
(`authn.internal_error`) after the losing attempt's expired busy window —
the exact behavior the tournament exists to keep closed.

`handler_test.go` exercises every operation through the actual composed
`Handler` (`httptest`, never calling `Service` methods directly), including
the mandatory deployment-mode-consistency suite: the same request
sequence run against a `Handler` wired with `pkgcore.NewConsoleSMSSender` and again
against one wired with `pkgcore.NewHTTPSMSSender` (an `httptest` server standing in
for a distributed gateway), asserting identical status codes and error codes
on both. `history_test.go` covers the same session/history operations at the
`Service` layer directly — ordering, the no-existence-disclosure answer for
someone else's session, and the login-history pagination clamp.

The PostgreSQL integration tier (`integration_test/`, `//go:build integration`,
package `authn_test`) additionally proves: migrations apply from zero and
produce every table this module owns (`postgres_migrations_test.go`); the
mandatory isolation suite against a real database where the isolation
plugin and RLS session wiring genuinely apply, for all eight identity-domain
tables plus the one tenant-domain table (`postgres_isolation_test.go`); and
that the blind-index unique constraint on `users.email_index`/`phone_index`
behaves identically to the unit tier's SQLite proof — collation and
unique-index case-sensitivity genuinely differ between the two dialects
(`postgres_blind_index_test.go`).

Every federation test runs offline. `testutil.OIDCServer` is a complete local
identity provider — discovery document, JWKS, authorization and token
endpoints, backed by a freshly generated RSA key — so the Google channel and
the enterprise relying party are proven against a real signed, real-verified
ID token with no network call. The five social channels each get injectable
base URLs and an injectable `*http.Client`, pointed at `httptest.NewServer` in
every test; pkgcore/safehttp's own tests separately prove the
production default client actually refuses loopback and every other
non-public range, including under a DNS-rebinding resolver stub.

`internal/totp/totp_test.go` pins the generic HOTP/TOTP core directly against
the OFFICIAL RFC 4226 Appendix D and RFC 6238 Appendix B test vectors — SHA-1,
SHA-256 and SHA-512, exactly as published — not merely against a round trip
through this package's own `Code`/`Validate`. A round trip alone cannot catch
a truncation or modulus bug that is wrong in a way consistent with itself;
the official vectors can.

A time-based test that generates a code and immediately validates it is
inherently sensitive to which 30-second step the wall clock is in at each of
those two moments — `mfa_test.go`'s step-up tests account for this
explicitly (see their own comments) rather than assume `time.Now()` called
twice in a row always lands in the same step. `internal/totp`'s public
`Code`/`Validate` intentionally take no injectable clock (see that package's
own doc comment for why); tests that need to avoid the ambiguity generate a
code for `time.Now().Add(totp.Period)` and rely on `totpSkewSteps`' tolerance
rather than trying to synchronize on the exact step boundary.

---

## Known limitations

| Limitation | Why, and what closes it |
|---|---|
| Access-token signing keys live in `go/pki`'s database tables rather than purely in this process's memory. | Accepted trade-off: real rotation, multi-replica key consistency and an expiry scan, at the cost that a database compromise plus a `dbkit` master-key compromise together are enough to sign arbitrary tokens (mitigated by the `vault`/`kmsaws` `Signer` implementations, under which the key never enters this process's memory at all). |
| `Signer.EnsurePurpose` runs lazily on the FIRST `Issue` call of each process, not once at deployment-wide bootstrap. | `the module contract.Register` may perform no I/O (`Module.Register`'s own doc comment), so the earliest point a real `context.Context` and a certain need for the purpose exist is the first real token issuance. `EnsurePurpose` is idempotent past a purpose's first-ever key, so this costs nothing once the deployment has issued a single token; a process that only ever verifies (never issues) never calls it at all, and does not need to -- `Verifier` reads whatever key rows already exist in the database, created by whichever replica issued first. |
| A brand-new deployment's very first `EnsurePurpose` calls can still race across replicas exactly like any other first-write race -- two replicas both finding no active key and both attempting the first `Create` -- but the loser of the database arbitration is answered as success, never as a token-issuance error. | The convergence is `go/pki`'s, not this module's: `Service.EnsurePurpose` treats a `Create` refused by `pki_signing_keys`' partial unique index -- at most one active row per purpose -- by re-reading the purpose and answering nil when an in-validity active key now exists, the "someone else just created the active key counts as success" shape. The path covers both the first-boot double-create and the expiry-heal race, pinned by `TestService_EnsurePurpose_ConcurrentHeals_OneReplacementBothSucceed` (the index itself by `TestSigningKeyRepository_ActivePurposeUniqueness_IsEnforcedByTheDatabase`). This module's own share is unchanged: `Signer.ensure` serializes in-process callers under one mutex (row above), and a purpose that still genuinely lacks an in-validity key after the failed insert keeps surfacing the store error loudly, retried after the bounded window. |
| Registration reports a duplicate identifier as a conflict, which makes it an account-enumeration oracle in a way sign-in deliberately is not. | Closing it means answering every registration with "check your inbox" and moving the conflict into an email, which needs the delivery and verification flows. |
| `sessions.ip_region` and `login_attempts.ip_region` ship empty. | Resolving an IP to a region needs a local GeoIP database whose licence has to clear the licence scanner first. The columns exist so a resolver, when one is added, needs no migration. |
| The declared dynamic-config items are not read back at runtime; the values are injected through options with the same defaults. | The schema is declared, which is what a module owes the config module. The read-through binding is unbuilt; it needs the live `config` module wiring that would consume it. |
| The dynamic-config item `authn.session_revocation_immediate` does not exist. | It was declared-but-never-read: its description promised that setting it enforces immediate revocation, and no code read it — and no runtime read could ever deliver what the description promised, because the revocation mode is fixed at `SessionManager` construction and gates which revocations are even recorded, so a value read at request time cannot retrofit enforcement onto a natural-mode manager. The mode's one real selector is the `WithRevocationMode` construction option ("Immediate revocation is enforced by default, not by host ceremony" above). Reintroducing a dynamic switch would require the typed-config read-through binding the row above records as unbuilt, plus a mode that can change at runtime without contradicting natural mode's zero-cost model. |
| The generated authn surface of `@speed/api-sdk` has no browser-driven, real-server end-to-end consumer. | In-form runtime consumption exists: `@speed/auth-ui`'s `src/usage-example.test.tsx` compiles and executes the composed sign-in family over a real `@speed/api-client` — `createClient` with a memory access-token store and an injectable fetch whose stand-in answers genuine `Response` objects, bound through the same `bindRequestFn` seam a host's client binds — driving a password sign-in, a silent credential-less refresh (the retried request carries the fresh token), and a server-side session death whose refused refresh converges the snapshot to anonymous, six requests pinned in order. The generated half stays compile-consumed in-workspace by `@speed/auth-core`; `@speed/auth-ui`'s public `RegisterForm` callback (the generated `AuthnUser`) adds a second type-level consumer. What remains is the browser-and-real-server leg. |
| A brand-new account provisioned by an unmatched, trusted external identity (social or enterprise SSO) cannot sign in until something makes it an active member of the requested tenant. | Membership is `org`'s data and this module fails closed on it by design (see "Fail closed on membership"). The account and its identity are provisioned regardless — only the session is refused — so a later membership grant (an `org` subscriber reacting to `authn.user.created`, or a host-side grant) lets the same sign-in succeed with no further action here. `examples/reference-app`'s `flowtests/authn_e2e_test.go` sidesteps the same limitation the same honest way — register, grant, then sign in — for exactly this reason. |
| The reference app's demo users reach tenants through an opt-in boot-time seed, not `task seed`. | Only a boot with `APP_DEMO_USERS_PASSWORD` set registers the three real demo accounts (`examples/reference-app/internal/app/demo/demo_users.go`'s `SeedDemoUsers`, through the real composed register route, over this module's own `demoseed` helper) and grants each its org membership and rbac role per configured tenant — the memberships in org's own table are what make real sign-ins succeed, via `signInMemberships` — while an unset variable leaves only the demo header actors (`internal/app/demo/demo_subject.go`), which carry grants but no database row and cannot sign in. The membership half is org data this module cannot write by design (authn and org are peers; nothing here imports org, and the app grants memberships under each tenant's own context). `Taskfile.yml`'s `seed` task remains a stub with no loader; what remains unbuilt is a Taskfile `seed` loader that generates demo data outside boot, a tooling item, not authn's. |
| QQ/Weibo/Alipay social providers, SAML, and WebAuthn/passkeys are not implemented. | Each needs credentials, a live account, or a design decision this module has not made. |
| The Aliyun/Tencent Cloud/Twilio adapters (`go/pkgcore/sms/`) have never been proven against each vendor's real gateway in this repository's own runs. | Proving them needs live accounts and credentials, which are never committed. Each adapter's `integration_test/` carries an env-gated leg that self-skips with a recorded note until its `ALIYUN_SMS_*`/`TENCENT_SMS_*`/`TWILIO_SMS_*` variables are set, then sends one real (billable) message each — the alipay sandbox-leg precedent. Until an operator runs one, the offline request-shape tests — vectors from Aliyun's own documentation and values an independent implementation precomputed — are the shipped proof, and the signing transcribes each vendor's published specification rather than trusting a maintained SDK's behavior. |
| The Aliyun and Tencent adapters send only through an approved account template mapped by (locale, message id): `Config.Templates` keyed `"<locale>/<message-id>"`, each entry naming the template plus the variables it declares; Twilio sends free text. | Aliyun/Tencent have no free-text send and one approved template per message kind; the adapters map `pkgcore.SMS`'s template identity (message id, rendered locale, params) through that map exactly as each package doc records. The templates themselves are account data this codebase cannot provision or verify — a pair with no mapped template, or a mapped template missing a declared variable, is refused before any request (fail-closed, no fallback), and a live-leg run with a mismatched template fails with the vendor's own `TemplateParamSet`-class error. |
| `RequireStepUp` has no fallback for an account with no MFA factor enrolled — it blocks the sensitive action unconditionally rather than, say, accepting a re-entered password. | A password-re-entry fallback needs its own design decision (how long that proof stays valid, whether it composes with MFA), which has not been made. |
| MFA (TOTP) is not enforced at LOGIN time — only `RequireStepUp`-gated sensitive actions require it. A password or SMS sign-in for an account WITH an enrolled factor still succeeds on the first factor alone. | Full second-factor-at-login is a larger design question (an interactive "enter your code now" challenge mid-flow); the shipped shape is enrollment, recovery and step-up only. |
| Phone-login and TOTP/recovery-code lifetimes (`ConfigKeySMSCodeTTL`, `ConfigKeySMSCodeMaxAttempts`) are declared as dynamic-config schema but, like every other dynamic-config item in this module, are not read back at runtime — values are injected through options with matching defaults. | Same read-through gap `NewService`'s existing options carry; the binding is unbuilt (see the dynamic-config row above).
| The `otpauth://` provisioning URI is rendered as a plain string; no QR image is generated server-side. | Deliberate — see `internal/totp`'s own doc comment. QR rendering is display logic and belongs on the frontend, which already owns every other rendering decision in this codebase. A QR-generation dependency was weighed and rejected for the same reason `pquerna/otp` was: every dependency added here lands in every consumer's build. |
| Members who sign in through the enterprise relying party (`SSOService.Callback`) produce no audit row, and tenant SSO configuration has no HTTP surface. | Configuration WRITES and replay responses are covered from the service layer: `AuditActionSSOConfigure` is emitted by `SSOService.SaveConfig` after every committed create and update (`oidc.go`'s `emitConfigSavedAudit`, its registrar wired by `module.go`'s `Register`), recording the writing operator (the `pkgcore.Actor` the caller's ctx attests) and the written configuration (issuer, client id, enabled, allowed domains) without the client secret; a detected replay is recorded by `SessionManager.handleReplay`'s `emitReplayAudit` (`session.go`) under `AuditActionSessionRevoke` with `Result{Success: false, FailureReason: RevokeReasonReplay}` — the 401 the replayed refresh answers, `RevokeReasonReplay` the same vocabulary the revoked session row stores in `revoke_reason` — attributed to the account owner and stamped with the revoked session's tenant. Every declared audit action has a live emit site: the handler-layer ones through `recordAudit` (`handler.go`) — `session.revoke` among them, whose replay responses are the manager-layer site just described, so an `authn.session.revoke` row records either an owner-initiated revocation (`Success=true`) or the theft response, told apart by the Result — and `AuditActionSSOConfigure` from the service layer; emission belongs where the write commits, not where an HTTP handler would be. An SSO-config HTTP surface, were one mounted, would add only its own `recordAudit` call, never new mechanism. See this file's Testing section for the proofs. The sign-in half of the gap stays open for the same structural reason: `SSOService.Callback` runs with no handler-layer funnel, and no HTTP surface exists to host one; it closes with that surface. |
