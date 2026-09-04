# authn

Authentication: who a caller is. Never what they may do.

Design rationale lives in `docs/internal/05-identity-and-access.md` (Chinese). This
file is the discipline that ships with the module to consuming projects.

> **Scope note.** This file currently documents what has landed: identity models
> and dual-dialect migrations, password authentication, access-token issue and
> verification, refresh rotation with replay detection, sessions and tenant
> switching, the middleware chain, module registration, OIDC federation (an
> enterprise relying party plus five social channels), account-binding
> management, phone-plus-SMS-code sign-in, TOTP second-factor enrollment with
> recovery codes and step-up re-verification, progressive/sliding-window
> rate limiting on every login, registration, code-send, code-verify and
> step-up path, **the full spec-first HTTP surface** (`api/openapi.yaml` →
> `api/authn-server.gen.go` → `handler.go`, twenty operations covering every
> flow above), and **self-service session/device-list and login-history
> management** (`history.go`: list sessions, revoke one, revoke every other,
> list login history). Wiring this module into a real HTTP server (the
> reference app, `pr-check`/`pr-full` CI matrices, `Taskfile.yml`'s
> `INTEGRATION_DIRS`) is the next block's job — this module builds, lints
> and tests green entirely on its own regardless. Judge what exists by the
> tree, not by this note.

---

## Scope

| In | Out |
|---|---|
| Users, sessions, refresh tokens, login history | Roles, permissions, policy evaluation (`rbac`) |
| Password storage and verification (argon2id) | Memberships, the organization tree (`org`) |
| Access-token issue and verification (Ed25519 JWT) | Sending notifications (`notification` subscribes to this module's events) |
| Refresh rotation, replay detection, session revocation | Authorization decisions of any kind |
| Tenant switching within one session | Memberships, the organization tree (`org`) |
| Social login (Google, GitHub, WeChat, DingTalk, Feishu) and enterprise OIDC single sign-on | SAML (deferred, optional subpackage), WebAuthn/passkeys (deferred, post-v1.0) |
| Account-binding management (list, bind, unbind) | QQ / Weibo / Alipay and other phase-two providers |
| Phone-plus-SMS-code sign-in on the existing blind index | Real SMS carrier adapters (Aliyun, Tencent Cloud, Twilio) — the shipped `SMSSender` transports are console (standalone) and a generic HTTP gateway (distributed) |
| TOTP enrollment, confirmation, recovery codes, step-up re-verification | WebAuthn/passkeys (the `SecondFactor` shape is reserved, not implemented — post-v1.0) |
| Sliding-window plus progressive-lockout rate limiting on login/register/code-send/code-verify/step-up | Anomalous-login detection (new device/region), which needs GeoIP first (`notification`, M2) |

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
| `WithKeySource`, `WithBlindIndexKey` | **Required.** No safe default exists for either. `WithKeySource` replaced `WithSigningKeys` in the pki-integration round (breaking, no back-compat path) -- see "Tokens and passwords" below. |
| `WithMembershipReader` | The seam through which membership is asked. Absent means "refuse", not "allow". |
| `WithClock`, `WithIssuer`, `WithAccessTokenTTL`, `WithRefreshTokenTTL`, `WithSessionTTL`, `WithRevocationMode`, `WithPasswordParams`, `WithPasswordPolicy` | Everything else. A nil or non-positive value leaves the default in place. |
| `WithSMSSender`, `WithDeploymentMode`, `WithSMSCodeTTL`, `WithSMSCodeMaxAttempts` | The phone-login transport and its lifetime/attempt budget. See "A distributed deployment must wire an `SMSSender`" below for what `WithDeploymentMode` is for. |

### Platform search: `SearchUsers` does no authorization of its own

`Service.SearchUsers(ctx, UserSearchQuery) ([]User, error)` (`search.go`) is authn's one cross-tenant, platform-operator search entry point, added purely additively for `go/admin`'s D6 (no existing signature in this module changed). Unlike `FindByID`/`FindByEmail`/`FindByPhone`, which answer "does this one identifier resolve to an account" for ordinary business code that already knows which tenant it is asking about, `SearchUsers` answers "which account (if any) does this identifier or name fragment belong to" with no tenant in scope at all -- `users` is identity data, not tenant data, so only a platform-wide search makes sense here.

`UserSearchQuery` takes exactly one of `Email` (exact match via the email blind index), `Phone` (exact match via the E.164 phone blind index) or `DisplayNamePrefix` (case-insensitive prefix match, the only criterion that can return more than one row, bounded by `Limit` -- zero uses a small default, anything above a hard ceiling is clamped to it). Naming none of the three returns `ErrSearchCriteriaRequired` (`authn.search_criteria_required`, an `apperr.Invalid`) rather than silently answering with every user on the platform.

**`Service` has no opinion on who may call it, by design.** This module never imports `rbac` (root `CLAUDE.md`'s module-boundary rule), so `SearchUsers` performs no internal permission check of any kind -- the caller (`go/admin`'s HTTP handler, gating on `admin:search_users`, in the round that consumes it) is entirely responsible for authorizing the call before it ever reaches here. Treat this the same way as any other security-sensitive, no-internal-authz method in this codebase: never expose it behind a route that is not already gated by the caller's own permission check.

### Authentication

| Symbol | Purpose |
|---|---|
| `Principal` | The authentication result: user, current tenant, session, AMR. No roles, no permissions. |
| `Service.Register / Login / Refresh / SwitchTenant / Logout` | The flows. |
| `MembershipReader` | `ActiveMembership` and `TenantsOf`. Implemented by the host, by `org` once it lands. |

### Tokens and passwords

| Symbol | Purpose |
|---|---|
| `KeySource` | Declared here, structurally satisfied by `*pki.Service` with zero import edge (docs/internal/22-pki.md's "authn's integration" section). Replaces the deleted `TokenKey`/`KeySet`/`NewKeySet`/`GenerateTokenKey` static-injection shape -- there is deliberately no second, fallback key-source path (docs/internal/22-pki.md's "no second path" section). `Signer`/`Verifier` resolve keys from it on every `Issue`/`Verify` call rather than holding a fixed key set; `Signer` calls `KeySource.EnsurePurpose` lazily, once, on its first `Issue` (`Signer.ensureOnce`), since `Module.Register` may perform no I/O. |
| `Signer.Issue(ctx, principal)`, `Verifier.Verify(ctx, raw)` | Access tokens, algorithm-pinned to EdDSA; both now take a `context.Context` (the breaking half of the `KeySource` switch), and `Verify`'s `keyFunc` additionally checks the token header's `alg` against the signing key's own `KeySource`-declared `Algorithm` -- defense in depth on top of the parser's single-EdDSA allowlist, so a future second algorithm cannot slip past this check by accident (docs/internal/22-pki.md's "authn's signing algorithm" section). |
| `HashPassword`, `VerifyPassword`, `NeedsRehash`, `PasswordParams`, `PasswordPolicy` | argon2id with PHC-encoded parameters. |

### HTTP

| Symbol | Purpose |
|---|---|
| `Middleware(verifier, opts...)` | Optional authentication. Puts a `Principal` in the context. |
| `RequireAuthenticated(next)` | Per-route enforcement. |
| `NewPrincipalResolver()` | Adapts the verified `Principal` to `tenancy.Resolver`. |
| `PrincipalFromContext`, `WithPrincipal` | Context access. |
| `RevocationChecker`, `WithRevocationChecker` | The immediate-revocation enforcement point. |

### HTTP surface (spec-first)

| Symbol | Purpose |
|---|---|
| `api/openapi.yaml` | The single source of this module's HTTP surface — twenty operations under `/api/v1/authn`, path prefix and `operationId`/schema-name conventions per `.claude/skills/backend-coding-standards/SKILL.md` §6.1. `Module.OpenAPISpec()` returns it embedded. |
| `api/authn-server.gen.go` | Generated by `task api:gen` (pinned `oapi-codegen` v2.8.0) from the fragment above — **never hand-edited**. Defines `api.ServerInterface` and every request/response model. |
| `NewHandler(svc) *Handler` | Implements `api.ServerInterface`; its inner routing comes entirely from the generated `HandlerFromMux`. Runs standalone — it is **not** mounted downstream of `tenancy.Middleware` the way a tenant-scoped module's handler is (see "This handler does not run downstream of tenancy.Middleware" below). |
| `Handler.requirePrincipal` (unexported) | This module's per-route enforcement for its own HTTP surface: every protected operation calls it first and answers a missing `Principal` with `authn.authentication_required`, exactly what `RequireAuthenticated` does for a handler composed the usual way — see "Per-route enforcement lives inside Handler, not around it" below for why. |
| `Service.ListSessions`, `Service.RevokeSession`, `Service.RevokeOtherSessions`, `Service.ListLoginHistory` (`history.go`) | The self-service device-list and login-history surface: list every session (including revoked ones, newest first), revoke one of the caller's own sessions by id, revoke every session except the caller's current one, list the caller's own login attempts (newest first, limit clamped to `(0, 200]`). |
| `ErrSessionNotFound` | `RevokeSession`'s answer for both "no such session" and "that session belongs to someone else" — deliberately the same answer either way, so the endpoint never confirms another account's session id exists. |

### Federation

| Symbol | Purpose |
|---|---|
| `SocialProvider`, `ExternalIdentity` | The channel interface and what a channel reports about the person who authorized. Every field of `ExternalIdentity` is untrusted third-party input. |
| `NewGoogleProvider`, `NewGitHubProvider`, `NewWeChatProvider`, `NewDingTalkProvider`, `NewFeishuProvider` | The five shipped channels. Each takes injectable base URLs and an injectable `*http.Client`, defaulting to the channel's production hosts and a `safehttp`-guarded client. |
| `WithSocialProviders`, `WithTrustedProviders`, `WithRedirectAllowlist`, `WithOAuthStateTTL`, `WithFederationHTTPClient` | `NewService`/`NewModule` options wiring the channels a deployment offers, which of them may auto-link, where a flow may return to, and (enterprise SSO only) the HTTP client the relying party uses. |
| `Service.SocialAuthorizeURL`, `Service.SocialCallback` | The social sign-in and account-binding flow. |
| `Service.Identities`, `Service.ListIdentities`, `Service.UnbindIdentity` | Binding management. |
| `Service.SSO()` `*SSOService` | The enterprise relying party: `SaveConfig`, `AuthorizeURL`, `Callback`, `Configs()`. |
| `TenantSSOConfig`, `SSOConfigRepository` | The one tenant-domain table this module owns. |
| `RedirectAllowlist`, `NewRedirectAllowlist` | Exact-match redirect URI validation. |
| `PermissionSSOManage` | The one permission this module declares, for writing a tenant's SSO configuration. |

### Phone-plus-SMS-code sign-in

| Symbol | Purpose |
|---|---|
| `SMS`, `SMSSender` | The message and the delivery seam. Authn's own — it is not a `pkgcore` primitive, since not every module needs it. |
| `NewConsoleSMSSender(w)`, `NewHTTPSMSSender(endpoint, opts...)` | The standalone (writes to `w`) and distributed (SSRF-guarded JSON POST) transports — the dual-implementation rule applied to this module's own seam. |
| `Service.RequestSMSCode`, `Service.LoginWithSMSCode` | Issue-and-deliver, then verify-and-sign-in. Both never disclose whether a phone number is registered. |
| `ErrMissingDistributedSMSSender` | What `NewModule`/`NewService` fail with when `WithDeploymentMode(pkgcore.DeploymentModeDistributed)` was given and no `SMSSender` was wired. |

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

### Auto-link an existing account only when verified AND trusted

`resolveSocialAccount` (social channels) and `SSOService.resolveAccount`
(enterprise SSO) both implement the same rule from
`docs/internal/05-identity-and-access.md`: an unrecognised external identity
whose email address already belongs to an account here may be linked to that
account automatically **only when the provider asserts the address is
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

`docs/internal/05` specifies exactly one SSO configuration per tenant, and
`SaveConfig` enforces it in the normal path by reading `Current` first and
updating the existing row rather than creating a second one. There is
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
explicit `SMSSender` (`WithSMSSender`) rather than silently defaulting to
`NewConsoleSMSSender`, which prints to a writer nobody in a distributed
replica pool is reading. This mirrors `pkgcore.ErrMissingDistributedMailer`
exactly, and it is the ONE piece of deployment-mode awareness this module
carries. It lives entirely in `newOptions`' validation — never in `Service`'s
business logic — for the same reason `pkgcore.Kernel`'s own `resolveMailer`
and `resolveObjectStore` live in kernel wiring: the root CLAUDE.md's "do not
branch on deployment mode in business logic" governs behavior selection
inside a request, not a once-at-construction-time checked precondition.
Omitting `WithDeploymentMode` (every call site that predates this option, and
every standalone deployment) is equivalent to standalone and keeps working
with the console default.

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

### A bare access token cannot silently seize an already-active MFA factor

`Service.EnrollTOTP` unconditionally deletes any existing factor (pending or
active) before enrolling a fresh one (`UserMFAFactor`'s unique
`(user_id, type)` index — see its own doc comment). Without a check, a bare,
unelevated access token — the shape a stolen one has — could call
`AuthnEnrollTOTP` then `AuthnConfirmTOTP` with a secret it chose itself,
replacing a victim's active factor with no re-proof at all. `EnrollTOTP`
therefore takes the caller's whole `Principal` and refuses with
`ErrStepUpRequired` when an ACTIVE factor already exists and
`principal.AMR` does not already carry one (`hasSecondFactor`) — this is
enforced INSIDE the service rather than by wrapping the route in
`RequireStepUp`, because whether step-up is even required depends on
whether an active factor exists to protect, information only the service
has without an extra round trip. A brand-new enrollment (nothing active to
replace) proceeds exactly as before, regardless of AMR — see
docs/internal/05-identity-and-access.md line 125 (turning MFA on for the
first time) versus line 127 (changing it once it already exists): only the
latter is a step-up case. `AuthnRegenerateRecoveryCodes` has no such
first-time case (it requires an active factor as its own precondition), so
its handler wraps the whole operation in `RequireStepUp` directly instead.

### `RequireStepUp` has no password-re-entry fallback for an account with no MFA

An account with no second factor enrolled has nothing to step up WITH, so
`RequireStepUp` blocks a sensitive action unconditionally for it rather than
falling back to, say, re-entering a password. This is a stated, deliberate
gap, not an oversight — see Known limitations.

### Rate limiting is two layers, and they answer different questions

`go/ratelimit`'s `Limiter.Allow` gives the raw sliding-window counters
(`rateGuard.allow` in `ratelimit.go`) — it deliberately understands nothing
about "account", "progressive" or "lockout" (see its own AGENTS.md). This
module's `rateGuard` adds the one thing `go/ratelimit` does not: a
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
`clientIP`'s doc comment gives for ignoring `X-Forwarded-For`). A host not
actually serving over HTTPS anywhere must not pass it.

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
so `writeAppError` (`middleware.go`) is what emits it — one implementation
shared by `Middleware`, `RequireAuthenticated` and every `Handler` operation
below, per `docs/internal/11-cross-cutting.md`'s "the HTTP translation is the
caller's job, not the business-logic layer's" rule. Do not duplicate this
logic inside `handler.go`; call the shared `writeAppError`.

---

## Testing

```
go -C go/authn test ./... -race                    # unit tier, no Docker
golangci-lint run ./...                             # from inside go/authn
go -C go/authn test -tags=integration -race ./integration_test/...   # PostgreSQL, Docker required
```

All three are wired into CI: the unit tier and lint run through the shared
`go-module-ci` matrix in both `pr-check.yml` (every PR) and `pr-full.yml`
(`full-ci`-labeled PRs), and the PostgreSQL integration tier runs in
`pr-full.yml`'s `integration-tiers` matrix — `go/authn` is a row in both
matrices, and `Taskfile.yml`'s `INTEGRATION_DIRS` carries the same entry for
`task test:full`'s local loop.

`examples/reference-app` is this module's mandatory first consumer
(root CLAUDE.md): `cmd/server/server.go` wires the real `Module` into the
composed server ahead of `notes`' and `config`'s, and
`cmd/server/authn_e2e_test.go` drives all three sign-in channels (password,
social, phone+SMS) plus session revoke/refresh end to end through that real,
composed HTTP server — the proof this module's own package-level tests
cannot give, because they never assemble the actual middleware chain a host
wires (`authn.Middleware` then `tenancy.Middleware(NewPrincipalResolver())`,
see "The middleware chain is authn, then tenancy" above) around a real
`net/http` server.

Every new model gets `tenancytest.AssertNotTenantScoped` (identity data) or
`AssertIsolated` (tenant data). Shared fakes live in `internal/testutil`, which
deliberately does **not** import this package — a test file in `package authn`
cannot import anything that imports `authn`, so the membership fake there
satisfies `MembershipReader` structurally.

Concurrency is not optional to test here. The single-winner property of
`RefreshTokenRepository.Consume` is what replay detection rests on, and it is
exercised under `-race` by twenty goroutines racing one token at the unit
tier (`session_test.go`) and again by sixteen goroutines against a REAL
PostgreSQL server at the integration tier
(`integration_test/postgres_refresh_rotation_test.go`) — SQLite's coarse
table-level locking can pass the unit-tier version even with a broken CAS,
which is exactly why the same property gets a second, real-database proof.

`handler_test.go` exercises every operation through the actual composed
`Handler` (`httptest`, never calling `Service` methods directly), including
the round's mandatory deployment-mode-consistency suite: the same request
sequence run against a `Handler` wired with `NewConsoleSMSSender` and again
against one wired with `NewHTTPSMSSender` (an `httptest` server standing in
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
every test; `internal/safehttp/safehttp_test.go` separately proves the
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
| Access-token signing keys now live in `go/pki`'s database tables rather than purely in this process's memory (the pki-integration round's `KeySource` switch). | Accepted trade-off, not an oversight: docs/internal/22-pki.md's "authn's integration" section spells out both sides -- the round buys real rotation, multi-replica key consistency, and an expiry scan, at the cost that a database compromise plus a `dbkit` master-key compromise together are now enough to sign arbitrary tokens (mitigated by the `vault`/`kmsaws` `Signer` implementations round 4 ships, under which the key never enters this process's memory at all). |
| `Signer.EnsurePurpose` runs lazily on the FIRST `Issue` call of each process, not once at deployment-wide bootstrap. | `pkgcore.Module.Register` may perform no I/O (`Module.Register`'s own doc comment), so the earliest point a real `context.Context` and a certain need for the purpose exist is the first real token issuance. `EnsurePurpose` is idempotent past a purpose's first-ever key, so this costs nothing once the deployment has issued a single token; a process that only ever verifies (never issues) never calls it at all, and does not need to -- `Verifier` reads whatever key rows already exist in the database, created by whichever replica issued first. |
| A brand-new deployment's very first `EnsurePurpose` call races across replicas exactly like any other first-write race: two replicas both finding no active key and both attempting to create one can have the loser's `Create` refused by `pki_signing_keys`' partial unique index, surfacing as a token-issuance error on that replica's first request. | Inherited from `go/pki`'s own round-1 design (its `Service.EnsurePurpose` bootstrap path), not introduced by this round's call-site choice -- calling it from any other single point would have the identical race on a distributed deployment's very first boot. Not yet mitigated; a future round may want `EnsurePurpose` to treat "someone else just created the active key" as success rather than propagating the constraint violation. |
| Registration reports a duplicate identifier as a conflict, which makes it an account-enumeration oracle in a way sign-in deliberately is not. | Closing it means answering every registration with "check your inbox" and moving the conflict into an email, which needs the delivery and verification flows. |
| `sessions.ip_region` and `login_attempts.ip_region` ship empty. | Resolving an IP to a region needs a local GeoIP database whose licence has to clear the licence scanner first (`docs/internal/05-identity-and-access.md` says so explicitly). The columns exist now so no later table migration is needed. |
| The declared dynamic-config items are not yet read back at runtime; the values are injected through options with the same defaults. | The schema is declared, which is what a module owes the config module. The read-through binding lands with the block that needs a live value. |
| The frontend half of this module's API contract — the generated authn surface of `@speed/api-sdk` — has no runtime end-to-end consumer against a real server yet (a browser driving a live backend). | Runtime consumption is discharged in form since the auth-ui round: `@speed/auth-ui`'s `src/usage-example.test.tsx` compiles and executes the composed sign-in family over a real `@speed/api-client` — `createClient` with a memory access-token store and an injectable fetch whose stand-in answers genuine `Response` objects, bound through the same `bindRequestFn` seam a host's client binds — driving a password sign-in, a silent credential-less refresh (the retried request carries the fresh token), and a server-side session death whose refused refresh converges the snapshot to anonymous, six requests pinned in order. The generated half stays compile-consumed in-workspace by `@speed/auth-core`; `@speed/auth-ui`'s public `RegisterForm` callback (the generated `AuthnUser`) adds a second type-level consumer. What remains is the browser-and-real-server leg, landing with the reference-app shells and the e2e pipeline. |
| A brand-new account provisioned by an unmatched, trusted external identity (social or enterprise SSO) cannot sign in until something makes it an active member of the requested tenant. | Membership is `org`'s data and this module fails closed on it by design (see "Fail closed on membership"). The account and its identity are provisioned regardless — only the session is refused — so a later membership grant (or an `org`-round subscriber reacting to `authn.user.created`) lets the same sign-in succeed with no further action here. `examples/reference-app`'s `authn_e2e_test.go` sidesteps the same limitation the same honest way — register, grant, then sign in — for exactly this reason. |
| The reference app has no seed-data path, so `demoMemberships` (`examples/reference-app/cmd/server/server.go`) starts empty in production wiring: `task dev`'s server compiles, serves every operation and enforces every rule correctly, but no demo account can actually reach a tenant without `Taskfile.yml`'s `seed` task, which itself is a stub awaiting `org`+`billing`. | This module's own wiring is complete (reference-app first-consumer status, CI matrix rows, integration tier — see this file's "Testing" section); what remains is `org`'s membership data, out of this module's scope by design (see the row above). |
| `redocly.yaml`'s `rule/speed-tag-format` runs at `warn`, not `error`, because `examples/reference-app/internal/notes/api/openapi.yaml` (an earlier round, not this module's fragment) declares no per-operation `tags:` and so fails the module-tag convention once merged with this module's fragment. | Fixing notes' fragment is real but out of scope here — it also regenerates `@speed/api-sdk` (orval groups hooks by tag), a change this round has no reason to make. See `redocly.yaml`'s own comment. |
| Real SMS carrier adapters (Aliyun, Tencent Cloud, Twilio), QQ/Weibo/Alipay social providers, SAML, and WebAuthn/passkeys are not implemented. | Each needs credentials, a live account, or is explicitly deferred by `docs/internal/05-identity-and-access.md`. See this round's plan for the owning milestone of each. |
| `RequireStepUp` has no fallback for an account with no MFA factor enrolled — it blocks the sensitive action unconditionally rather than, say, accepting a re-entered password. | A password-re-entry fallback needs its own design decision (how long that proof stays valid, whether it composes with MFA) that this block did not make. |
| MFA (TOTP) is not enforced at LOGIN time — only `RequireStepUp`-gated sensitive actions require it. A password or SMS sign-in for an account WITH an enrolled factor still succeeds on the first factor alone. | Full second-factor-at-login is a larger design question (an interactive "enter your code now" challenge mid-flow) this block's scope did not include; the round's plan scoped MFA to enrollment, recovery and step-up. |
| Phone-login and TOTP/recovery-code lifetimes (`ConfigKeySMSCodeTTL`, `ConfigKeySMSCodeMaxAttempts`) are declared as dynamic-config schema but, like every other dynamic-config item in this module, are not yet read back at runtime — values are injected through options with matching defaults. | Same read-through gap `NewService`'s existing options already carry; the binding lands with whichever block wires this module to the live `config` module. |
| The `otpauth://` provisioning URI is rendered as a plain string; no QR image is generated server-side. | Deliberate — see `internal/totp`'s own doc comment. QR rendering is display logic and belongs on the frontend, which already owns every other rendering decision in this codebase. A QR-generation dependency was weighed and rejected for the same reason `pquerna/otp` was: every dependency added here lands in every consumer's build. |
