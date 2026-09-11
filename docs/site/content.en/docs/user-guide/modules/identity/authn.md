---
title: authn
weight: 1
description: "Authentication — who a caller is, never what they may do: accounts, password and SMS-code sign-in, Ed25519 access tokens with rotating keys, refresh rotation, sessions, MFA and step-up, social and enterprise sign-in."
---

# authn

authn is speed's authentication module: it decides **who a caller is —
never what they may do**. Accounts, sign-in, sessions, tokens and MFA
live here; every "may they?" question belongs to `rbac`, and the
membership facts authn re-verifies arrive through a host seam, with
`org` the canonical implementation behind it.

## What it is for

authn owns the identity-domain tables — `users`, `sessions`,
`refresh_tokens`, `login_attempts`, `user_identities`, verification
codes, MFA factors and recovery codes — plus the one tenant-domain
table `tenant_sso_configs`. None is tenant-scoped: a person belongs to
several tenants. The per-tenant half of the relationship is the
`memberships` row, and that lives in `org`.

- **Password sign-in** — argon2id, with the cost parameters inside
  each stored hash (PHC), so raising the cost is a configuration
  change, never a migration.
- **Access tokens** — Ed25519 EdDSA-signed, keys resolved on every
  `Issue`/`Verify` through a `KeySource` seam that `*pki.Service`
  satisfies structurally, no import edge in either direction.
- **Refresh rotation** — single-use tokens, stored hashed; a replay
  revokes the whole family and its session.
- **Phone-plus-SMS-code sign-in** — numbers encrypted at rest and
  blind-indexed, delivered over pkgcore's shared SMS seam.
- **Social sign-in** — Google, GitHub, WeChat, DingTalk and Feishu,
  plus per-tenant enterprise OIDC.
- **MFA** — TOTP on the standard library only (RFC 4226/6238
  vectors), recovery codes, step-up re-verification.

Not authorization — no roles, no permission checks, and tokens carry
no permissions, since a frozen permission would outlive its revocation
— and not organization: no tree, no memberships. SAML,
WebAuthn/passkeys, QQ/Weibo/Alipay and anomalous-login detection are
not implemented; each is recorded with its reason in the module's
`AGENTS.md`.

## When to choose it

Every deployment where people sign in: accounts, sessions with a
device list, a refresh story that survives token expiry, MFA for
sensitive operations, social or enterprise channels. Machine access —
a product whose callers are only API keys — uses integration's key
issuing, not accounts.

## Wiring and minimal use

Two options are **required** — no safe default exists for either — and
membership must fail closed, never default open:

```go
authn.RegisterPIISerializer(cipher) // call before opening the *gorm.DB

m, err := authn.NewModule(db,
    authn.WithKeySource(pkiMod.Service()),        // *pki.Service satisfies KeySource
    authn.WithBlindIndexKey(blindIndexKey),       // 32-byte key behind the email/phone blind indexes
    authn.WithMembershipReader(membershipReader), // nil refuses every membership question
)
if err != nil {
    return err // a missing required option is a startup error
}
```

`WithSMSSender` takes the SMS transport — console by default in
standalone; a distributed deployment must also declare
`the composition's deployment field`, or construction fails with
`ErrMissingDistributedSMSSender`. `WithFeatureGate` enforces the
module's eight declared feature flags (password login, SMS login, the
social channels, enterprise SSO) at request time; `*config.Service`
satisfies it structurally. TTLs,
`WithRevocationMode`, `WithSocialProviders` and the rest tune
defaults; a host that skips the registry builds the service with
`NewService(db, bus, kv, opts...)`.

The middleware order is fixed: `authn.Middleware(verifier)` first —
verification is optional (a bad token is a 401, an absent one stays
anonymous) and never injects tenant context — then
`tenancy.Middleware(authn.NewPrincipalResolver())`, which turns the
verified principal into tenant context. authn's own subtree mounts
straight from `authn.Middleware`'s output, never downstream of
`tenancy.Middleware`: most operations (registration, sign-in, refresh,
callbacks) happen before any tenant is known. Per-route enforcement is
`RequireAuthenticated` or the stricter `RequireStepUp`, never a global
wrapper.

## Core concepts and API essentials

- **The `Principal`** — user, current tenant, session, authentication
  methods. No roles, permissions or email claim — a bearer token lands
  in logs and proxies.
- **Membership fails closed** — a token is minted for a tenant only
  when the `MembershipReader` confirms an active membership, and every
  refresh re-verifies.
- **Sign-in does not answer what it refuses** — every failed password
  sign-in returns the same parameter-less `authn.invalid_credentials`;
  the reason goes on the `login_attempts` row, keyed by blind index,
  never the identifier.
- **Refresh is single-use and replay is theft** — two concurrent
  refreshes with one token are indistinguishable from a stolen token,
  so clients must serialise them; re-verification happens before the
  presented token is consumed.
- **Revocation is enforced, not ceremonial** — in immediate mode the
  middleware consults the session manager on every verifying request;
  a revocation check that could not run refuses.
- **No merging on email alone** — auto-link requires a verified
  provider email and a trusted provider; enterprise SSO adds active
  membership in the configuring tenant. WeChat keys on `unionid`,
  never `openid`.
- **Step-up is bounded by the token, not the session**, and a bare
  token cannot seize an already-active MFA factor.
- **Rate limiting** is sliding-window plus progressive lockout,
  failing closed on store errors.

## Boundaries and pitfalls

- Do not hang authn routes behind `tenancy.Middleware` or wrap the
  whole handler in `RequireAuthenticated` — both break its public
  operations.
- No existence-disclosing answers anywhere; the one recorded
  exception: registration reports a duplicate identifier as a
  conflict.
- `SearchUsers` is the one cross-tenant platform search and does **no
  authorization of its own** — gate it behind the caller's permission
  check.
- MFA is not enforced at login (only step-up-gated actions require
  it); `RequireStepUp` has no non-MFA fallback; declared
  dynamic-config items are injected through options.
- Coded errors: see the [error code
  index](/docs/user-guide/error-codes/#authn).

## Source

- [go/authn/AGENTS.md](https://github.com/vislake/speed/blob/main/go/authn/AGENTS.md) — the authoritative document (rules, seams, known limitations)
- HTTP fragment: [go/authn/api/openapi.yaml](https://github.com/vislake/speed/blob/main/go/authn/api/openapi.yaml)
- Related: the domain guide [Identity and access](/docs/user-guide/domains/identity-access/), the middleware's other half [tenancy](/docs/user-guide/modules/core/tenancy/), the key source [pki](/docs/user-guide/modules/services/pki/), and the group pages [rbac](/docs/user-guide/modules/identity/rbac/) and [org](/docs/user-guide/modules/identity/org/)
