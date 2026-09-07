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
| | **No `@pending` gate is waiting on a DEFECT any more** -- every one of those has seen its fix land. The two that carry the tag today are waiting on a *surface that does not exist*, which is the tag's second meaning | |
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

**Going over now costs time rather than a red.** `submitPasswordSignIn`
keeps a ledger of the run's attempts and waits until the next one is
inside both limits before making it (journeys.ts's `payTheLoginBudget`),
extending only that test's own timeout by what the wait cost. Before
that, crossing the budget turned the suite red on `429` in whichever
test lost the race -- a failure that said nothing about the product and
pointed at the wrong gate: the run that found this reported
`visible-controls.spec.ts`, which was not the spec that overspent.

Three things about that ledger are worth knowing before trusting it:

- **It is a file, not a variable.** A Playwright worker serves one
  project and restarts at every engine boundary, so an in-memory ledger
  reset three times per run while the server counted once -- the pacing
  then did nothing on engines two and three. Same shape as the
  fixed-port hazard below: state assumed shared, and not.
- **It replicates `go/ratelimit`'s real arithmetic**, which is a
  sliding-window *counter* over two epoch-aligned windows
  (`thisWindow + previousWindow * (1 - elapsedFraction) <= Rate`), not a
  rolling log. "Sixty seconds since the oldest attempt" is not the
  boundary and waiting it out still gets refused; after five attempts in
  one window the next is allowed only about a fifth of the way into the
  following one. Two wrong models of this cost a run each.
- **A refused attempt still counts.** `Allow` increments before it
  decides, so a `429` makes the next attempt worse. The ledger records
  every submission, not just the ones that worked.

It is deliberately not a retry: it waits *before* an attempt so the
attempt is legal, and never re-submits one the server refused. A helper
that retried past a refusal would stop this suite being able to tell a
regression from its own impatience.

### One engine per invocation, and why pacing cannot fix it

**Run each project separately.** All three engines in one invocation
share one server process, and therefore one set of rate-limit counters:

```bash
for p in chromium webkit ipad; do pnpm test:e2e --project=$p; done
```

The default tier is green on all three engines that way, and red when
asked for all three at once. The reason is `limitRegisterByIP`, and it
is the one limit the pacing above deliberately does not touch: **10 per
HOUR**. Each engine's registering specs spend about six, so three
engines in one invocation ask for roughly eighteen, and the third engine
is answered `authn.rate_limited` with `retry_after_seconds: 2532` --
forty-two minutes. There is no wait that makes that a good trade: a tier
that pauses for forty minutes is worse than one that says it cannot fit.
Separate invocations each boot their own server, so each starts with the
counters at zero, which is the same mechanism that makes `@budget` a
real tier rather than a graveyard.

So the two budgets need different answers, and conflating them was the
mistake:

| Limit | Window | Answer |
|---|---|---|
| `limitLoginByAccount`, `limitLoginByIP` | a minute | pace inside it (`payTheLoginBudget`) |
| `limitRegisterByIP` | an hour | do not pace -- one engine per invocation |

**The ledger does not see API-driven attempts.** `org-invitation-sign-in`
drives register and login as direct requests rather than through the
sign-in form, so those attempts spend the server's budget without ever
reaching `payTheLoginBudget`. Its failure looks different too -- a raw
`429` with the envelope in the message rather than the named refusal --
which is the tell that a budget failure came from a spec the pacing
cannot help.

**Asking for one block at a time is no longer necessary, and the reason
it used to be is worth keeping.** The hazard was that a tier's cost
grows as its gates start passing: the run that finally works is the one
that trips the budget, and its red points at whichever test lost the
race rather than at the gate that overspent. That is exactly what
happened -- one measured run of the whole tier spent 18 sign-ins over
2.6 minutes and crossed the per-IP pool, while an earlier run of the
same tier did not, only because four of its gates failed early at a
surface that did not exist yet and never spent the sign-ins they would
have if they passed. The pacing above absorbs that: the whole
core-journey file now runs on three engines in one invocation, 18 gates
in 3.7 minutes, most of the difference being waits. Asking for one block
is still the fast way to check one block:

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

**A point-in-time sample read as a settled state.** Six times: an
assertion, a wait, a helper, a balance read, a notice read and a
disclosure read. Five let a defect through; the sixth accused a round
that had done its work correctly of not having done it, which is the
expensive direction. The shapes were always the same -- a query that had
not answered, a navigation still in flight, an announcement a beat
behind its trigger.

Recording it did not stop it. This section carried the failure class for
most of a day and it happened twice more. What was missing was not the
knowledge but the convenient path: `await locator.innerText()` is one
call and always available, so it is what gets written. `readSettledText`
in test-utils/journeys.ts makes waiting equally short, and that -- not
another note -- is what a repeated mistake needs.

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

## The product brief, and which parts a browser can reach

The acceptance question is not "do the gates pass" but "can a dental
practice do what this product was described as doing". Checked against
the brief, item by item, because a suite that only reports on what it
covers cannot say what it is missing.

| Brief item | Reachable in a browser |
|---|---|
| Secure practice accounts | Yes -- registration, sign-in, sessions, MFA |
| Patient photo upload | Yes -- block A |
| AI smile transformation | Yes -- block B (locally; see the deployment boundary below) |
| Multiple smile / tooth-shade options | Yes -- three styles, three shades, a strength |
| Before/after comparison | Yes -- in the clinic and on the patient's page |
| Ability to regenerate | Yes -- changing the options and pressing Simulate starts an ordinary second generation |
| Mobile and desktop responsive | Yes -- every gate runs on chromium, webkit and an iPad viewport |
| Patient-facing shareable link | Yes -- block C |
| Credit / usage system | Yes -- block D, balance and ledger |
| Multi-location account support | Yes -- the tenant switcher, and the clinic named on every work surface |
| **Download the result** | **No surface** -- the practice can share a link, but nothing offers the image itself |
| **Team / user management** | **No surface** -- invitations work through the API only, so a practice cannot add a colleague by clicking |
| **Subscription billing** | **No surface** -- credits are visible, but nothing shows or changes a plan |
| **Usage analytics** | **No surface** |
| **Admin dashboard** | **No surface** |

The five with no surface are not gated, and deliberately so: a gate
against a surface nobody has designed would be inventing the design.
They are listed because "the core journey works" and "the product is
what it was described as" are different claims, and this suite can only
speak to the first.

Team management is the one worth singling out. An invitation is a real,
working flow (`org-invitation-sign-in.spec.ts` drives it), but its setup
steps go through the API because there is no team surface to click --
so a practice that hires a second dentist cannot add them, and the gate
that proves invitations work had to reach around the product to set
itself up. That gap is invisible from the gate's green result, which is
exactly why it is written here.

## Before a fix round relies on a gate, check the gate

A gate is only good if it fails with **the defect it guards**. Not with a
timeout, not with a locator complaint, and above all not by passing.
That is checkable in one command per gate, and it is worth doing before
somebody starts implementing against one, because whoever does is
reading the failure as the specification.

Doing it once across three open gates found two of them healthy and one
broken in both possible ways at the same time. The SMS gate read the
surface through `getByRole('main')`, which a sign-in page does not have
-- there is no app frame yet, which is the point of a sign-in page -- so
it timed out where an implementer needed the defect. Reading from the
body instead, it PASSED, because the notice it examined was an empty
live region: `auth-ui` keeps one on every surface so an announcement can
be placed into it without a container appearing, which is correct for a
screen reader and satisfies a plain `toBeVisible()` while holding "".

The wrong diagnosis that followed is the part worth remembering. "The
surface is completely silent after Send code" was alarming, plausible,
and false: polling shows the notice arrives a beat later, and the real
defect -- it claims the phone received a code that went to the server
log -- was one beat away. A round sent to hunt a missing notice would
have found nothing missing.

So the check is not only "does it fail", it is "does it fail saying the
thing I would want a stranger to read".

## How a finding gets recorded wrong

Separate from how a gate is written wrong, and with its own cost: a
misclassified finding is not merely an inaccurate record, it is a defect
that gets shelved permanently.

The SMS gate spent a long time filed as "cannot be configured in this
demo, so not applicable". That reading was mine and it was wrong. The
gate does not wait for SMS to work; it refuses one specific state --
the channel offered while the surface claims "Code sent to +8613800…"
about a phone that will never receive anything. Not offering the channel
passes. Offering it and saying the code went to the server log passes.
Only the lie fails, and removing the lie needs no SMS configuration at
all.

Filed as environmental, it would have become a red line nobody reads
again, pointing at something fixable that a person will actually walk
into. The rule that follows: before recording a finding as blocked by
the environment, check whether the gate's own acceptance conditions
include one the environment permits. If any of them does, the finding is
open, not exempt.

The same distinction applies to the deployment boundary above, and lands
the other way: the generation genuinely cannot be exercised on the
deployment without a vendor credential, and no condition of those gates
is satisfiable there. That one IS environmental, which is why it is
recorded as a limit on what a report may claim rather than as a defect
waiting on someone.

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

**Open defects with a gate waiting on them: none.** Every gate this
suite wrote against a defect has seen its fix land. What the list held,
and what closed each one -- kept because the shape of these findings is
the suite's own record of what it is for:

| Gate | Was waiting on | Closed by |
|---|---|---|
| core-journey block D | `go/billing` had no HTTP surface, so a credits view had nothing to call | the credits surface, then `a9a7e22` for the cost disclosure |
| sessions-are-distinguishable | session rows rendered raw User-Agent strings, so three sign-ins from one browser were three identical walls of text | `6d56d71` |
| current-clinic-is-visible (one of three) | a self-service clinic was shown as a raw tenant id and named on no surface | the clinic-naming round |
| offered-channels-work | an SMS tab was offered that the demo cannot configure | `ba061cd`, by closing the channel rather than configuring it |

That is a claim about this suite, not about the product: it says every
defect this suite found has been fixed, and says nothing about the
defects it never looked for. The two lists below -- what the deployment
cannot answer, and what is not gated at all -- are where that difference
lives.

**Unbuilt surfaces with a gate waiting on them** (`pnpm test:e2e:pending`):

| Gate | Waiting on |
|---|---|
| keep-the-result | no way to save the generated image -- the only way out of the product is a screenshot |
| add-a-colleague | no surface for a practice's people, so a colleague can only be invited through the API |

These are written ahead of the work, which is the shape that produced the
core journey: the acceptance criterion exists before the round does,
rather than being argued about after it. Both assert what a person must
be able to ACCOMPLISH and deliberately prescribe no layout, wording or
route -- `test-utils/cases.ts`'s `CASE_UI` and each gate's own name
constants are the one place to reconcile when a surface lands calling
things something else.

Two traps they were written around, both of which this suite has already
paid for once:

- **`add-a-colleague` does not match "Account".** That surface exists and
  carries a person's own sessions and bindings; a pattern loose enough to
  reach a team page that might live there is loose enough to land on the
  personal one and pass.
- **`keep-the-result` does not match "share".** A share control exists,
  and matching it would make the gate pass on exactly the thing it says
  is not enough -- a link that expires by design is not the clinic's own
  copy.

**What the deployment cannot answer.** The fly.io deployment has no
image-provider configuration (`flyctl secrets list` shows no
`APP_AI_GATEWAY_IMAGE_*` entry), so a generation there reaches for a
vendor that is not configured and fails. Everything up to that point is
verifiable on the deployment -- registration, sign-in, the clinic's own
name, a case with its photograph, the smile and shade choices -- and
everything from the generation onward is verifiable only locally, where
the suite boots its own fake provider. Setting that configuration needs
a real credential and is a decision outside this suite.

That boundary is worth stating whenever a result is reported, because
"verified" means different things on either side of it: the local runs
prove the product's own code, and the deployment runs prove it survives
being deployed. A claim that the core journey works end to end is only
as strong as the environment it was walked in, and today the middle of
that journey has been walked locally and not on the deployment.

**Not gated at all, and why:**

- **Subscription billing, usage analytics, the admin console.** No
  browser surface exists for any of them, and none has a gate yet.

Team management and downloading the result used to sit here, and what
moved them out is the point: a missing surface can be gated, and until it
is, its absence is invisible from every green result the suite produces.
`org-invitation-sign-in` is the sharpest case -- it proves the whole
invitation cycle and passes, while driving the API directly, because
there is no surface to click. Its green says the backend is sound and
says nothing about whether a dentist can add a colleague.

The refund entry used to sit here for the same reason the
provisioning-recovery one below did -- the vendor could refuse
(`FAKE_IMAGE_FAIL=1`) but the balance it should restore had no surface
to read. `go/billing`'s credits surface gave it one, and
refund-on-failed-generation.spec.ts now asserts the refunded ledger row
rather than a balance that returned to its old number, which is the
stronger question: a balance that came back is equally consistent with
the charge never having been taken.

The provisioning-recovery entry used to sit in this list, and what moved
it out is worth keeping: the recovery was real from the day `dcd091c`
landed, but nothing could make provisioning fail on purpose, so this
suite could only say the retry had not regressed -- never that it
converges. `APP_FAIL_SELF_SERVICE_PROVISION` closed that gap and
provisioning-recovery.spec.ts now asserts the convergence itself. The
lesson is about the two claims rather than the switch: "it did not
regress" and "it works" read alike in a report and are not the same
statement, and only one of them was available.
