---
title: Identity and access
weight: 1
description: Sign-in, sessions and permissions in your speed-based product — the authn, rbac and org modules, and the middleware order that makes them work.
---

# Identity and access

This domain is who your users are, how they sign in, and what they may
do: the `authn` module owns authentication (passwords, sessions, MFA,
social and enterprise sign-in), `rbac` owns authorization (deny by
default, exact `resource:action` grants), and `org` supplies the
organization tree and memberships that give grants their scope. `pki`
sits underneath as the signing-key source authn's access tokens are
verified against.

```mermaid
flowchart LR
    U[Browser / app] -->|credentials| A[authn\nsign-in endpoints]
    A -->|Ed25519-signed access token| M[authn.Middleware\nverifies optionally]
    M -->|principal| T[tenancy.Middleware\nresolves tenant]
    T -->|tenant context| R[rbac gate\nRequirePermission]
    R -->|Subject{TenantID, UserID}| B[Your handler]
    O[org] -.->|memberships & node paths| R
    P[pki] -.->|keys| A
```

The middleware order is load-bearing and is the composition this
codebase pins with real tests: `authn.Middleware` verifies a presented
token if there is one (a bad token is a 401; an absent one stays
anonymous) and never guesses a tenant, and the layer after it —
`tenancy.Middleware(authn.NewPrincipalResolver())` — turns a verified
principal into the tenant context every tenant-scoped repository
requires. Your routes must never sit downstream of a tenant-guessing
middleware, and the tenant never comes from a request header.

## Minimal integration steps

1. **Wire the modules into your kernel.** Add `authn`, `rbac` and
   `org` (plus `pki` as authn's key source) to the module set your
   `Kernel.Bootstrap` runs. The `dbkit.MigrationRegistry` applies each
   module's own migrations; the app's boot-time comments in
   `examples/reference-app/cmd/server/server.go` walk the exact wiring
   order.
2. **Give authn its mandatory seams.** `authn.NewModule` validates
   options eagerly: a `KeySource` (pki's `Service` satisfies it) and a
   blind-index key are required — there is no safe default for either —
   and a `MembershipReader` answers "is this user a member of this
   tenant" at sign-in; absent means refuse, never allow.
3. **Mount the chain in order.** Route authn's own subtree straight
   from `authn.Middleware`'s output — never through `tenancy.Middleware`,
   because sign-in happens before any tenant exists. Protect everything
   else with `tenancy.Middleware(authn.NewPrincipalResolver())`
   downstream.
4. **Gate your routes on permissions.** rbac attaches after
   `Kernel.Bootstrap` (its `Attach` freezes every module's declared
   permission vocabulary — granting anything else is refused). Protect
   an operation with `rbac.RequirePermission("notes", "write")` or its
   `*Func` variant; org exports the four permissions its own routes
   declare (`PermissionRead`, `PermissionManage`,
   `PermissionInviteMember`, `PermissionRemoveMember`) for you to gate
   on the same way.
5. **Drive identity data through org's flow.** Register a user, then
   invite them into a tenant node through `org`'s invitation flow; the
   accepted membership is what your `MembershipReader` and rbac's
   subject resolution see.

## Boundaries worth knowing

- Access tokens are short-lived and Ed25519-signed; refresh tokens are
  single-use, and replaying one rotates the whole token family and
  revokes the session — clients must serialise refreshes.
- Social/enterprise sign-in binds by verified email from a trusted
  provider, never by matching email alone; a last-login-method
  constraint keeps a social-only account from shedding its channel.
- Every existence-disclosing answer (user exists, email taken, provider
  bound) is suppressed: enumeration learns nothing.
- Login, registration and step-up sit behind sliding-window rate limits
  with progressive lockout, and step-up verification survives exactly
  one access token.

## Next steps

The complete API surface of each module — options, handlers, error
codes — lives in its per-module page: `authn`, `rbac`, `org` and `pki`
(in the module reference). The error codes this domain answers with are
in the [error code index](../../error-codes/).

## Source

- [authn AGENTS.md](https://github.com/vislake/speed/blob/main/go/authn/AGENTS.md)
- [rbac AGENTS.md](https://github.com/vislake/speed/blob/main/go/rbac/AGENTS.md)
- [org AGENTS.md](https://github.com/vislake/speed/blob/main/go/org/AGENTS.md)
- [pki AGENTS.md](https://github.com/vislake/speed/blob/main/go/pki/AGENTS.md)
