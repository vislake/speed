# reference-app end-to-end suite

The browser tier of the reference-app's tests: a real browser driving the
real composed UI against a real, freshly booted reference-app server.

```bash
# from examples/reference-app/web
pnpm test:e2e                     # boots both servers, runs the default tier
pnpm test:e2e:budget              # the verified gates the budget excludes
pnpm test:e2e:pending             # the gates for defects still open
pnpm test:e2e --headed            # watch it happen
pnpm exec playwright show-report  # the last run's report

# drive an already-running deployment: ONLY the @deployment gates run
E2E_BASE_URL=https://your-deployment.example pnpm test:e2e
```

`test:e2e` and `test:e2e:budget` together run every gate that is
expected to pass; neither alone does, and the reason is the sign-in
budget below rather than anything about the gates.

## Three tiers, and what each one means

| Tag | What it says | Where it runs |
|---|---|---|
| *(none)* | Verified, and it fits the budget | `pnpm test:e2e` |
| `@budget` | **Verified passing.** Out of the default run only because the suite has no sign-in left to spend | `pnpm test:e2e:budget` |
| `@pending` | The thing it checks is **still broken**, or its surface does not exist yet | `pnpm test:e2e:pending` |
| `@deployment` | Safe to run against a long-running deployment (needs no sign-in, or one) | any of the above with `E2E_BASE_URL` |

`@budget` and `@pending` were one tag once, and merging them cost the
suite the thing it is for: a gate held back because its defect is open
and a gate held back because the budget is full looked identical, so
nothing in the suite could say what was still broken. An acceptance
report cannot be honest on top of that.

**A gate that skips itself unless `E2E_BASE_URL` is set must also carry
`@deployment`.** Against a deployment the config narrows the run to
`@deployment`, and a command-line `--grep` narrows it *further* rather
than replacing that — the two intersect. So a deployment-only gate
without the tag is excluded at both ends: skipped locally by its own
guard, filtered out on the deployment for lacking the tag, and
`--grep @pending` there answers "No tests found". One gate lived like
that (the session address-source check) and had never run anywhere;
nothing catches this automatically, so it is a rule to hold when adding
a gate rather than something the suite enforces.

The budget is a hard ceiling, not untidiness, and it has **two**
dimensions — `go/authn`'s `ratelimit.go` is the authority for both:

| Limit | Value | What it binds |
|---|---|---|
| `limitLoginByAccount` | 5 / minute | one demo account |
| `limitLoginByIP` | 20 / minute | **the whole suite at once** |
| `limitRegisterByIP` | 10 / hour | the specs that register |

The per-account limit is the obvious one and the per-IP limit is the one
that actually caps the suite's size: every test signs in from the same
machine, so they all draw on one 20-per-minute pool, and the whole local
run finishes in about twenty-five seconds — inside a single window. The
default tier currently spends **17 of those 20**. Three sign-ins of
headroom, for any new gate, on any account.

That second dimension was missed when these tiers were first drawn (only
the per-account limit was counted), and it changes the answer: switching
a gate to a quieter demo account does **not** buy room, because the pool
it is drawing on is not per-account. A new gate either fits in the three
remaining slots or belongs in `@budget`, where the separate invocation
gives it a fresh 20.

Going over does not slow the suite down: it turns it red on `429` in
whichever test loses the race, a failure that says nothing about the
product.

**So ask the `@pending` tier for the block you mean, rather than running
it whole.** What is actually known, kept apart because the two halves
came from different runs: one measured run of the whole tier spent 18
sign-ins over 2.6 minutes and crossed the per-IP pool, reddening a test
on the budget rather than on its subject. A run of the same tier today
did NOT -- four of its eight gates fail early, at a surface that does
not exist yet, and never spend the sign-ins they would if they passed.
That is the shape of the hazard: the tier's cost grows as its gates
start passing, so the invocation that finally works is the one that
trips the budget, and its red will point at whichever test lost the
race. Ask for the block you mean:

```bash
E2E_RUN_TAGGED=1 pnpm exec playwright test \
  e2e/core-journey.pending.spec.ts --grep "block C"
```

This matters most to a round whose acceptance criterion is one of these
gates: running the whole tier to check one block produces a red that
says nothing, which is the same worthless answer as a default-tier green
that never selected the gate. A rule that says "run the gate" has to say
"run that gate".

What keeps `@budget` a real tier rather than a graveyard: each run boots
its own server, so a **separate invocation** starts with the rate-limit
counters at zero and a full budget of its own. That is why the answer is
two commands rather than a suite that quietly stops growing.

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
gates are not using. Then read "How a gate here has gone wrong" below
and check yours against it -- every item there is a real defect from
this suite, and the convenient locator is usually the wrong one.

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

## How a gate here has gone wrong

Every item below is a real defect found in THIS suite, and most of them
are one mistake: **the gate measured something adjacent to the property
instead of the property.** The last three are its relatives -- the right
question asked of the wrong population, a setting given to the wrong
process, and a convenient check trusted over an authoritative one -- and
they belong here because they produced the same result: an answer that
was not about the product.

They are written down for two reasons. The next person adding a gate
will reach for the same convenient locator. And several of these were a
step from accusing the product of a defect it did not have, which is the
most expensive thing an acceptance suite can do: a false accusation
spends a round's work on nothing and teaches everyone to discount the
next report.

**A locator that stands in for the element you mean.**
`getByRole('listitem').first()` clicked the frame's Home nav entry, not
the first case row -- the nav is a list, ahead of `main` in the DOM -- so
every gate using that helper sat on Home and never saw a case detail
page. `document.querySelectorAll('li')` collected the nav entries into
the "session rows". Scope to `main`, or to the section you mean.

**An accessible-name match is a SUBSTRING match.** `{ name: 'Account' }`
also matched "Linked social accounts" and failed on strict mode -- a
gate reporting a locator problem where a reader expects a product
problem. Page titles want `level: 1`.

**A check that is satisfied by absence.** "No private address appears"
is equally true of a list showing no address at all. "No nav link is on
the page" is equally true on a phone, where no nav link is ever on the
page -- so a "the frame is gone" check passed on the iPad project for a
reason unrelated to signing out, and would have kept passing if signing
out had stopped working. Assert the thing is there, THEN assert what it
must not be.

**A count that cannot tell two things from one thing twice.** "The
comparison shows two images" passes when the surface renders the
original twice -- the product's whole proposition rendered as a no-op,
and it looks right in a screenshot. Compare them.

**A latent false accusation, harmless only while a real failure masks
it.** The unscoped session rows would have flagged "Home" and "Account"
as rows with nothing to tell them apart -- but only once the genuine
User-Agent defect above it was fixed and stopped failing first. It would
have surfaced as a regression in the round that fixed the real thing,
and been blamed on it.

**`isVisible()` does not wait.** It is a point-in-time question, so a
helper that asks it first races the page's own render: a journey that
signed in and navigated immediately found neither the nav link nor the
menu button and failed with "no way to reach Notes" while both were
about to appear. A `.click()` auto-waits, which is why replacing one
with a check regressed it. Wait for whichever entrance the viewport
offers (`link.or(menu)`).

**`textContent` and `innerText` are wrong in opposite directions.**
`textContent` concatenates adjacent elements with no separator, so an
address arrived as `password223.70.82.202` and a word-boundary pattern
could not match it. `innerText` returns text as RENDERED, and MUI
uppercases button labels, so a tenant read back as `ACME DENTAL` and
matched no configured name. Ask what something renders as, innerText;
ask which value it is, textContent.

**A gate that depends on winning a race is not a gate.** "A generation
in flight must say so" passed or failed on whether it beat the fake
vendor's instant answer. The fix was to make the requirement observable
-- the fake now takes 300ms, which is closer to a real generation, not
further -- and not to weaken the assertion. That distinction matters,
because raising a stand-in's fidelity and tuning the evidence to fit an
assertion look identical in a diff.

**A gate written before its surface exists cannot be told apart, by
running it, from one that is merely plausible.** Blocks B, C and D were
all written ahead of delivery on purpose, and all three had holes: an
unscoped status region, a patient page satisfied by a logo, and a cost
check satisfied by the WORD "credits" with no figure anywhere. The only
defence available then is to read the gate as though it had already
passed and ask what it would have let through. That found two of them.

**A helper that documents an intention it does not have.**
`openCaseWithPhoto` promised to create a case "when the run has none"
and never created anything, so every block-B gate failed the moment
block B's surface landed. If a comment describes behaviour, the code has
to have it.

**The right question asked of the wrong population.** Twice, and both
times the question itself was correct. The clinic-name gate asked "can a
person tell which clinic they are working in" only of clinics configured
before the server booted -- so it stayed green while a self-service
clinic showed a raw tenant id on every surface. The four core-journey
gates ask "can a practice do the work" only of seeded demo accounts,
which boot-time seeding hands an entitlement and credits; a real
registrant gets neither, and a browser walk-through of the same journey
stopped at "This clinic's plan does not include smile simulation" while
all four were passing. When a gate signs in, ask whose account it is and
what that account was given.

**A switch handed to the wrong process.** The refund gate passed
FAKE_IMAGE_FAIL -- the fake PROVIDER's variable -- to the Go server, so
the server had no image provider configured at all and every generation
failed for an unrelated reason. It went green either way, because it
also never navigated to the surface it claimed to read: the probe that
found it printed the "ledger" and got the case detail page. Two wrong
explanations were offered before the probe, both plausible, both wrong.
When a gate spans processes, name which process each setting belongs to.

**My own check standing in for the authoritative one.** Three times in a
day. `grep -E '(passed|failed)' | tail -1` silently dropped "5 failed"
and reported 17 passed. A grep whose backslash the shell ate reported a
digit assertion missing when it was there. And a hand-written CJK regex
over e2e/ found nothing while the repository's own tools/scan_cjk.py
found thirteen -- browser snapshots in a gitignored directory, which the
scanner sees because it walks the tree rather than the index. Where an
authoritative tool exists, a quick check can locate but must not
conclude.

**A gate that runs nowhere.** One test skipped itself unless
`E2E_BASE_URL` was set and did not carry `@deployment`, so the
deployment run filtered it out and the local run skipped it. Excluded at
both ends, it looked like a gate and was a comment. See the tier rule
above.

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

Every file, because a partial list of what a suite contains invites the
same false sense of coverage a partial list of what it skips does. The
tier each spec sits in is the tag in its own `test(...)` call, not this
table -- read the file when it matters.

| Path | What it holds |
|---|---|
| `password-sign-in.spec.ts` | Signing in, being refused, the SMS channel, a single-tenant account's scope |
| `registration.spec.ts` | Creating an account, the policy refusals, the duplicate-address conflict |
| `self-service-signup.spec.ts` | A practice registering itself: it gets in, and can do an owner's work |
| `provisioning-recovery.spec.ts` | A registration whose clinic failed to open on the first try still gets in |
| `org-invitation-sign-in.spec.ts` | An invited colleague accepts and can then work in that organization |
| `session-lifecycle.spec.ts` | Signing out, the session-ended branch, signing back in, reloading |
| `restart-survival.spec.ts` | A member signs in against a process that booted over an existing database |
| `back-button.spec.ts` | Back moves the view and keeps the person signed in; no full reload |
| `authorization.spec.ts` | What a read-only member may do, and tenant isolation across a switch |
| `notes-surface.spec.ts` | The tenant-scoped business surface: read, write, the row that appears |
| `offline-save.spec.ts` | A save that fails offline keeps the text and blames the network |
| `account-surface.spec.ts` | Sessions, sign-in history, social bindings, two-step verification |
| `sessions-are-distinguishable.spec.ts` | Telling your own sign-ins apart, and the address each was made from |
| `current-clinic-is-visible.spec.ts` | Which clinic the work lands in, including one a registration just created |
| `tenant-landing.spec.ts` | Where a multi-clinic account lands, and that it lands somewhere |
| `home-is-self-consistent.spec.ts` | The home surface does not promise cards it has none of |
| `visible-controls.spec.ts` | Every control in the chrome is legible against what is behind it |
| `deployment-serves-the-app.spec.ts` | The address serves the app, mounts it, and loads without a failed asset |
| `offered-channels-work.spec.ts` | A channel the product offers either works or says why it cannot |
| `core-journey.pending.spec.ts` | The product's reason to exist, in four blocks: case and photo, generate and compare, share with the patient, what it cost |
| `test-utils/accounts.ts` | The seeded demo accounts and the grants each one carries |
| `test-utils/journeys.ts` | Shared journey steps and every en-US string the specs assert on |
| `test-utils/servers.ts` | Booting a server one spec owns, for the specs whose subject is a server's own lifecycle or configuration |
| `test-utils/invitations.ts` | Setting up a real invitation, including reading its token from the mail the server prints |
| `test-utils/fake-image-provider.mjs` | The stand-in for the vendor the smile simulation calls, with a refusal mode |

Specs are named for the journey they cover, never `e2e.spec.ts`, and
shared helpers live in `test-utils/` rather than being copied between
files (`.claude/skills/frontend-coding-standards/SKILL.md` §12).

Assertions quote the en-US bundles, and the browser context runs under
the en-US locale so the app's own language negotiation settles there from
`navigator.languages` — which exercises the negotiation path and keeps
every spec free of the CJK characters CI refuses outside `docs/internal`.

## Not covered yet, and one thing to read every green result against

**Nothing runs this suite automatically.** No workflow invokes Playwright
at all: `.github/workflows/e2e.yml` is a stub whose guard step exits 1,
and no other pipeline mentions `test:e2e`. So every green result from
this suite is *somebody having run it*, at one moment, against one tree
-- never a standing guarantee, and never a regression caught between
runs. A report should say which tier, which engines and which
environment it ran in, because "the suite is green" on its own does not
distinguish a full three-engine pass from a default-tier run that never
selected the gate in question. Wiring the pipeline (Go toolchain,
`playwright install --with-deps`, the trace artifact upload) belongs to
the roadmap's M4 e2e item.

That distinction has already cost something: a round's verify step ran
the default tier, went green, and had never executed the `@pending` gate
that was its own acceptance criterion -- the gate was not selected. A
tier's green says only what that tier selected.

**Open defects with a gate waiting on them** (`pnpm test:e2e:pending`):

| Gate | Waiting on |
|---|---|
| core-journey block D | `go/billing` has no HTTP surface, so a credits view has nothing to call |
| sessions-are-distinguishable | session rows still render raw User-Agent strings, so three sign-ins from one browser are three identical walls of text |
| current-clinic-is-visible (one of three) | a self-service clinic is shown as a raw tenant id and named on no surface |
| offered-channels-work | SMS cannot be configured in this demo, by decision |

**Not gated at all, and why:**

- **A generation that fails giving the credits back.** The fake vendor
  can now refuse (`FAKE_IMAGE_FAIL=1`), but the balance it should
  restore has no surface to read, so the gate waits on block D.
- **Team management and the admin console.** No browser surface exists.

The provisioning-recovery entry used to sit in this list, and what moved
it out is worth keeping: the recovery was real from the day `dcd091c`
landed, but nothing could make provisioning fail on purpose, so this
suite could only say the retry had not regressed -- never that it
converges. `APP_FAIL_SELF_SERVICE_PROVISION` closed that gap and
provisioning-recovery.spec.ts now asserts the convergence itself. The
lesson is about the two claims rather than the switch: "it did not
regress" and "it works" read alike in a report and are not the same
statement, and only one of them was available.
