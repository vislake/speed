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
> recovery codes and step-up re-verification, and progressive/sliding-window
> rate limiting on every login, registration, code-send, code-verify and
> step-up path. The HTTP surface lands in a later block of the same round and
> extends this file rather than replacing it. Judge what exists by the tree,
> not by this note.

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
| `WithSigningKeys`, `WithBlindIndexKey` | **Required.** No safe default exists for either. |
| `WithMembershipReader` | The seam through which membership is asked. Absent means "refuse", not "allow". |
| `WithClock`, `WithIssuer`, `WithAccessTokenTTL`, `WithRefreshTokenTTL`, `WithSessionTTL`, `WithRevocationMode`, `WithPasswordParams`, `WithPasswordPolicy` | Everything else. A nil or non-positive value leaves the default in place. |
| `WithSMSSender`, `WithDeploymentMode`, `WithSMSCodeTTL`, `WithSMSCodeMaxAttempts` | The phone-login transport and its lifetime/attempt budget. See "A distributed deployment must wire an `SMSSender`" below for what `WithDeploymentMode` is for. |

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
| `Service.EnrollTOTP`, `Service.ConfirmTOTP` | Start enrollment (returns a secret and an `otpauth://` provisioning URI); confirm with a real code (returns ten recovery codes, shown once). |
| `Service.VerifyStepUp` | Re-verifies the CURRENT session with a TOTP code or a recovery code and mints a freshly enriched access token, reusing the existing refresh token. |
| `Service.RegenerateRecoveryCodes` | Replaces a user's whole recovery-code batch; requires an active TOTP factor. |
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
| Registration reports a duplicate identifier as a conflict, which makes it an account-enumeration oracle in a way sign-in deliberately is not. | Closing it means answering every registration with "check your inbox" and moving the conflict into an email, which needs the delivery and verification flows. |
| `sessions.ip_region` and `login_attempts.ip_region` ship empty. | Resolving an IP to a region needs a local GeoIP database whose licence has to clear the licence scanner first (`docs/internal/05-identity-and-access.md` says so explicitly). The columns exist now so no later table migration is needed. |
| The declared dynamic-config items are not yet read back at runtime; the values are injected through options with the same defaults. | The schema is declared, which is what a module owes the config module. The read-through binding lands with the block that needs a live value. |
| `Module.OpenAPISpec()` returns nil. | The HTTP surface is spec-first: the fragment appears together with the generated server interface and the handler that implements it. Returning a fragment before those exist would advertise endpoints nothing serves. |
| A brand-new account provisioned by an unmatched, trusted external identity (social or enterprise SSO) cannot sign in until something makes it an active member of the requested tenant. | Membership is `org`'s data and this module fails closed on it by design (see "Fail closed on membership"). The account and its identity are provisioned regardless — only the session is refused — so a later membership grant (or an `org`-round subscriber reacting to `authn.user.created`) lets the same sign-in succeed with no further action here. |
| Real SMS carrier adapters (Aliyun, Tencent Cloud, Twilio), QQ/Weibo/Alipay social providers, SAML, and WebAuthn/passkeys are not implemented. | Each needs credentials, a live account, or is explicitly deferred by `docs/internal/05-identity-and-access.md`. See this round's plan for the owning milestone of each. |
| `RequireStepUp` has no fallback for an account with no MFA factor enrolled — it blocks the sensitive action unconditionally rather than, say, accepting a re-entered password. | A password-re-entry fallback needs its own design decision (how long that proof stays valid, whether it composes with MFA) that this block did not make. |
| MFA (TOTP) is not enforced at LOGIN time — only `RequireStepUp`-gated sensitive actions require it. A password or SMS sign-in for an account WITH an enrolled factor still succeeds on the first factor alone. | Full second-factor-at-login is a larger design question (an interactive "enter your code now" challenge mid-flow) this block's scope did not include; the round's plan scoped MFA to enrollment, recovery and step-up. |
| Phone-login and TOTP/recovery-code lifetimes (`ConfigKeySMSCodeTTL`, `ConfigKeySMSCodeMaxAttempts`) are declared as dynamic-config schema but, like every other dynamic-config item in this module, are not yet read back at runtime — values are injected through options with matching defaults. | Same read-through gap `NewService`'s existing options already carry; the binding lands with whichever block wires this module to the live `config` module. |
| The `otpauth://` provisioning URI is rendered as a plain string; no QR image is generated server-side. | Deliberate — see `internal/totp`'s own doc comment. QR rendering is display logic and belongs on the frontend, which already owns every other rendering decision in this codebase. A QR-generation dependency was weighed and rejected for the same reason `pquerna/otp` was: every dependency added here lands in every consumer's build. |
