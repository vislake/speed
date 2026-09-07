# reference-app end-to-end suite

The browser tier of the reference-app's tests: a real browser driving the
real composed UI against a real, freshly booted reference-app server.

```bash
# from examples/reference-app/web
pnpm test:e2e                     # boots both servers, runs every journey
pnpm test:e2e --headed            # watch it happen
pnpm exec playwright show-report  # the last run's report

# the gates for defects still open, asked for by name
E2E_INCLUDE_PENDING=1 pnpm test:e2e --grep @pending

# drive an already-running deployment: ONLY the @deployment gates run
E2E_BASE_URL=https://your-deployment.example pnpm test:e2e
```

## Two environments, and why the suite is not the same in both

Against the local servers this file's config boots, every gate runs.
Against a real deployment only the ones tagged `@deployment` do, and
that restriction is not a convenience -- the others are *wrong* there.

The suite signs in 38 times across its files, nearly always as the same
account. Locally that is free: the config starts a fresh server per run,
so `go/authn`'s in-memory rate-limit counters begin at zero and the whole
suite finishes in about twenty seconds. A deployment is a long-running
process. The counters accumulate, the network adds latency, and partway
through a run the suite locks its own account out -- after which every
remaining test fails on `429` rather than on its subject, producing a
wall of red that says nothing about the product. That happened, and the
results had to be thrown away.

So `@deployment` marks the gates that need no sign-in, or one, and the
deployment-mode sign-ins are spread across the three seeded accounts so
none of them approaches its own per-minute budget.

The usual fix -- sign in once and reuse the session across tests through
Playwright's `storageState` -- **cannot work on this product, by
design**: the access token lives in memory and the refresh token in a
closure, and neither is ever written to storage (`@speed/api-client`'s
memory store, `@speed/auth-core`'s session). That is the correct security
decision, so the suite works within it instead of asking for it to be
weakened. The cost is real and worth stating plainly: reverifying a
login-heavy journey after a deployment is a local exercise, and an
acceptance report should say which environment it was run in rather than
claiming "verified" without qualification.

**Adding a gate:** if it signs in more than once, leave it untagged --
it belongs to the local tier. If it must run against a deployment, keep
it to one sign-in and pick an account the neighbouring `@deployment`
gates are not using.

The first run compiles the Go server, which takes minutes; later runs
reuse the build cache and the whole suite finishes in seconds.

## A worktree does not isolate the machine

Several git worktrees of this repository routinely run this same suite at
the same time, one per concurrent round. A worktree isolates the **code**.
It does not isolate the machine's TCP ports, its filesystem or its process
table, and that gap produced the worst failure this suite has had.

The config used to name two fixed ports and set
`reuseExistingServer: !CI`. Two runs asked for the same port, the second
lost -- and instead of failing, Playwright saw a healthy server there and
adopted it. A suite in one worktree then drove **another worktree's
server**: another round's code, another round's database. It surfaced as a
wall of red (that server had bound IPv6 only, and Chromium resolved the
name to IPv4), which was luck. The same mechanism can just as easily
produce green, and a green measured against code you are not testing is
worse than any red.

So the suite now picks its three ports at random per run and never reuses
a server it did not start. A collision is still possible and is now loud:
`--strictPort` plus `reuseExistingServer: false` make a taken port a
startup failure that names the port, never a silent adoption. Name a port
explicitly (`E2E_API_PORT`, `E2E_WEB_PORT`, `E2E_RESTART_PORT`) only when
a run needs a known one. Every local URL says `127.0.0.1` rather than
`localhost` for the other half of that failure: the name resolves to both
stacks, a server may bind one, and Playwright's health check and the
browser do not have to pick the same one.

The general lesson, worth carrying past this suite: **shared machine state
in a test harness fails silently in the direction of a pass.** A fixed
port, a fixed temp path, a fixed container name or a fixed database file
all have this shape. This suite's run-unique SQLite path and log file were
already built that way; the ports were the one place it had not been
applied.

## Why this tier exists

Every tier below this one drives its requests through a scripted fetch
double, and a double answers whatever shape its author believed the
server sends. When the two drifted, nothing below this tier could tell:

- `@speed/api-client` required a `traceId` field on the error envelope.
  No backend writer has ever emitted one — every module's `writeError`
  and every OpenAPI fragment documents `{code, params}` — so every real
  error degraded to a synthetic `client.http.<status>` code that no
  reachable-error whitelist maps, and the deployed app rendered its
  generic "something went wrong" text in place of every specific
  message. Fixed in `f23079d`; three specs here are its regression gate
  (a refused sign-in, a too-short password, a refused write).
- Tenant isolation is a property of the server's own scoping. A
  component test asserting a list can only assert the list it scripted.
  `authorization.spec.ts` writes in one tenant and reads in the other.

That is the rule of thumb for what belongs here: a behaviour that only
exists when a real server answers. Component behaviour, rendering and
copy belong in the vitest suites under `src/`, which are faster and more
precise about it.

## What the suite knows about the server

Three facts shape the specs, and all three are the server behaving
correctly rather than obstacles to route around:

**Sign-in is rate-limited.** `go/authn`'s `ratelimit.go` allows five
sign-ins per account per minute (twenty per IP, ten registrations per IP
per hour) with progressive lockout. So the suite spends sign-ins
deliberately: one per spec file where it can, spread across the three
seeded demo accounts — the owner where a write is needed, the reader for
the account surface, the single-tenant account for the session-lifecycle
journey. There is no retry-on-429 helper, on purpose: a suite that
retried past a rate limit would stop being able to tell a real
regression from its own impatience.

**A fresh database per run is mandatory, not hygiene.** Demo-account
seeding deliberately skips an account a previous boot already created
(`cmd/server/demo_users.go`), so a reused database hands the suite
accounts with no memberships. `playwright.config.ts` puts the SQLite file
in the OS temp directory under a run-unique name.

**Which tenant an account lands in is not fixed.** Sign-in with no tenant
named resolves the account's first tenant from the host's
`MembershipReader`, and this host's seeding grants tenants while
iterating a Go map — randomized per boot and per account. So a spec reads
the tenant the frame actually landed in (`readCurrentTenant`) and
switches deliberately when it needs the other one (`switchTenant`).
Reported as a finding: a returning member of a multi-location practice
should land somewhere predictable, which is a host-side ordering
decision.

## Layout

| Path | What it holds |
|---|---|
| `password-sign-in.spec.ts` | Signing in, being refused, the SMS channel, a single-tenant account's scope |
| `registration.spec.ts` | Creating an account, the policy refusals, the duplicate-address conflict |
| `notes-surface.spec.ts` | The tenant-scoped business surface: read, write, the row that appears |
| `authorization.spec.ts` | What a read-only member may do, and tenant isolation across a switch |
| `session-lifecycle.spec.ts` | Signing out, the session-ended branch, signing back in, reloading |
| `account-surface.spec.ts` | Sessions, sign-in history, social bindings, two-step verification |
| `test-utils/accounts.ts` | The seeded demo accounts and the grants each one carries |
| `test-utils/journeys.ts` | Shared journey steps and every en-US string the specs assert on |

Specs are named for the journey they cover, never `e2e.spec.ts`, and
shared helpers live in `test-utils/` rather than being copied between
files (`.claude/skills/frontend-coding-standards/SKILL.md` §12).

Assertions quote the en-US bundles, and the browser context runs under
the en-US locale so the app's own language negotiation settles there from
`navigator.languages` — which exercises the negotiation path and keeps
every spec free of the CJK characters CI refuses outside `docs/internal`.

## Not covered yet

- **Demo accounts surviving a process restart** and **a really-invited
  user signing in to the invited tenant**: both wait on the sign-in
  membership fix, which is what makes them pass at all. They are the two
  gates this suite is missing on purpose rather than by oversight.
- **The CI pipeline.** `.github/workflows/e2e.yml` is still its stub
  guard; wiring it (Go toolchain, `playwright install --with-deps`, the
  artifact upload for traces) is the next step and belongs to whoever
  owns that pipeline's round.
- **Everything the product design calls for that has no UI yet** —
  patient photo upload, the AI simulation and its before/after view,
  share links, billing, team management, the admin console. The backends
  for most of them exist; the browser surfaces do not, so there is
  nothing for a browser to drive.
