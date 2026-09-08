/**
 * reference-app end-to-end test configuration.
 *
 * This is the browser-level tier of the reference-app's test pyramid
 * (.claude/skills/frontend-coding-standards/SKILL.md §12): the specs under
 * e2e/ drive a real browser against a real, freshly booted reference-app
 * server, so they cover exactly the gap every tier below them structurally
 * cannot reach -- an answer the SERVER actually sends, rendered by the
 * component tree the browser actually mounts.
 *
 * That gap is not hypothetical. The unit and component suites under src/
 * and in web/packages/* drive their requests through scripted fetch
 * doubles, and a double answers whatever shape its author believed the
 * server sends. When the two drifted -- @speed/api-client demanding a
 * traceId field on the error envelope that no backend writer has ever
 * emitted -- every scripted suite kept passing while every real error in
 * the deployed app degraded to the generic "something went wrong" text
 * (fixed in f23079d). The specs here would have failed on the very first
 * wrong-password assertion, which is why the error-text assertions in
 * password-sign-in.spec.ts and registration.spec.ts are written as
 * regression gates rather than incidental checks.
 *
 * TOPOLOGY
 *
 * Two servers, both started by Playwright and both disposable:
 *
 *   1. the reference-app Go server, in standalone deployment mode over a
 *      SQLite file this config places in the OS temp directory under a
 *      run-unique name. The freshness matters: demo-account seeding is
 *      deliberately skipped for an account a previous boot already
 *      created (cmd/server/demo_users.go's own "leaving it unseeded so it
 *      fails closed" branch), so a reused database would hand the suite
 *      accounts with no memberships.
 *   2. the app's own vite dev server (vite.config.ts), which serves the
 *      browser page and proxies /api to the Go server above.
 *
 * Set E2E_BASE_URL to point the suite at an already-running deployment
 * instead (a fly.io instance, a staging host); both webServer entries
 * then stay down and only the browser half runs. Everything else --
 * ports, the demo passphrase, the database path -- is overridable through
 * the environment for the same reason.
 *
 * LANGUAGE
 *
 * The browser context runs under the en-US locale, so the app's own
 * language negotiation (@speed/i18n's createI18n: URL parameter, stored
 * choice, profile language, navigator languages, then the zh-CN default)
 * settles on en-US from navigator.languages alone -- no ?lang= parameter
 * in any spec's URL, and the negotiation path itself gets exercised. The
 * assertions therefore quote the en-US bundles, which also keeps every
 * spec free of the CJK characters CI refuses outside docs/internal
 * (root CLAUDE.md's language rule).
 */
import { defineConfig, devices } from '@playwright/test'
import { randomInt } from 'node:crypto'
import { fileURLToPath } from 'node:url'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

/** This app's directory, resolved from this config file's own location. */
const appDir = fileURLToPath(new URL('.', import.meta.url))

/** The reference-app Go module directory (this app's parent). */
const serverDir = fileURLToPath(new URL('..', import.meta.url))

/**
 * The four ports one run needs: the Go server, the vite server, the
 * second Go process restart-survival.spec.ts starts over the same
 * database, and -- since the block-B gates -- the throwaway
 * OpenAI-compatible images provider (fake-image-provider.mjs) the Go
 * server's smile-simulation pipeline reaches. Picked at random per run
 * rather than fixed, and set through the environment for the same
 * reason the database path below is -- the runner chooses, every worker
 * inherits.
 *
 * Fixed ports were the original shape (8091 and 5191, chosen only to stay
 * clear of a developer's own 5173 and 8080) and they were wrong for a
 * reason the repository's own working style makes routine rather than
 * exotic: several git worktrees of this repository run this same suite at
 * the same time, one per concurrent round. Worktrees isolate the CODE.
 * They do not isolate the machine's TCP ports, its filesystem or its
 * process table -- so two runs asked for 5191, the second lost, and
 * `reuseExistingServer` (below) quietly handed it the FIRST run's server:
 * a suite in one worktree driving another worktree's code. That is worse
 * than a collision that fails, because a green result means nothing and
 * says nothing about it.
 *
 * A random base cannot make a collision impossible -- roughly one run in
 * six hundred with eight running at once -- so it is paired with the
 * settings that make one loud: `--strictPort` on vite and
 * `reuseExistingServer: false` everywhere, which together turn a taken
 * port into a startup failure naming the port instead of a silent
 * adoption. Override any of the three explicitly when a run needs a known
 * port (E2E_API_PORT, E2E_WEB_PORT, E2E_RESTART_PORT).
 */
const portBase = 8100 + randomInt(0, 18_000) * 3

/**
 * Reads one of the three ports, choosing it on this run's behalf when
 * nobody has, and writing the choice back so every worker process reads
 * the same answer this one just made. Returns a string rather than
 * leaving `process.env`'s own `string | undefined` to every call site.
 */
function runPort(variable: string, chosen: number): string {
  const port = process.env[variable] ?? String(chosen)
  process.env[variable] = port
  return port
}

const apiPort = runPort('E2E_API_PORT', portBase)
const webPort = runPort('E2E_WEB_PORT', portBase + 1)
const imageProviderPort = runPort('E2E_IMAGE_PROVIDER_PORT', portBase + 2)

/**
 * The port restart-survival.spec.ts starts its second server on, exported
 * so the spec reads the run's own choice rather than defaulting to one of
 * its own -- the identical reason SERVER_LOG_PATH below is exported.
 */
export const RESTART_API_PORT = runPort('E2E_RESTART_PORT', portBase + 3)

/**
 * The port provisioning-recovery.spec.ts boots its own server on: a
 * server armed with APP_FAIL_SELF_SERVICE_PROVISION, which cannot be the
 * run's shared one because arming it there would fail the first
 * provisioning attempt of every other gate's registration too.
 */
export const INJECT_API_PORT = runPort('E2E_INJECT_PORT', portBase + 4)

/**
 * The port refund-on-failed-generation.spec.ts boots its own REFUSING
 * image provider on.
 *
 * The run's shared provider answers successfully, which every other gate
 * needs; a gate about what happens when a generation fails needs the
 * opposite, and cannot have it by flipping the shared one. So it starts
 * a second instance with FAKE_IMAGE_FAIL=1 and points its own server at
 * that instead.
 */
export const REFUSING_IMAGE_PORT = runPort('E2E_REFUSING_IMAGE_PORT', portBase + 5)

/**
 * The port refund-on-failed-generation.spec.ts boots ITS server on.
 *
 * Its own, not shared with provisioning-recovery: both specs own a
 * server, they run in the same tier one after the other, and a boot onto
 * a port a previous spec's server still holds does not fail -- bootServer
 * waits for a healthy /healthz and the incumbent answers it. So the
 * refund gate silently adopted the recovery gate's server, which has no
 * image provider configured, and its generation was refused before it
 * could be enqueued: "Something went wrong. Try again later." on the
 * panel, passing when run alone and failing in the tier.
 *
 * The identical mistake this suite fixed at the run level hours earlier
 * (fixed ports plus reuseExistingServer, one worktree adopting
 * another's server) reappeared between two of its own specs. A port a
 * second thing might want is a port that needs its own name.
 */
export const REFUND_API_PORT = runPort('E2E_REFUND_PORT', portBase + 6)

/**
 * The loopback address every local URL in this file names.
 *
 * `127.0.0.1` rather than `localhost`, and not a style preference: the
 * name resolves to both `::1` and `127.0.0.1` on a dual-stack host, a
 * server may bind only one of them, and the two halves of this suite
 * disagree about which to try -- Playwright's own health check reaches a
 * server over either, while Chromium picked IPv4 and answered
 * ERR_CONNECTION_REFUSED for a server listening on IPv6 alone. So the
 * health check passed, the suite ran, and every spec failed on a
 * navigation. Naming the address leaves nothing to resolve.
 */
const loopback = '127.0.0.1'

/** Where the browser half points. */
const baseURL = process.env.E2E_BASE_URL ?? `http://${loopback}:${webPort}`

/** True when the suite drives an already-running deployment. */
const external = process.env.E2E_BASE_URL !== undefined

/**
 * A run-unique SQLite path in the OS temp directory. Unique per run
 * rather than deleted-and-recreated so a crashed run never leaves the
 * next one arguing with a half-written file, and outside the repository
 * so no run can dirty a working tree.
 */
// Through the environment, not a module constant: this file is imported
// by the runner AND by every worker process, so a value computed at
// import time would differ per process -- the server would write one
// database and a spec would look for another. The runner evaluates this
// first and workers inherit the variable it set, so all of them name the
// same file.
process.env.E2E_DB_PATH ??= join(
  tmpdir(),
  `reference-app-e2e-${Date.now()}-${process.pid}.db`,
)
const databasePath = process.env.E2E_DB_PATH

/**
 * Where this run records the sign-ins it has spent, so a gate can pace
 * itself inside go/authn's login budget (e2e/test-utils/journeys.ts).
 *
 * A file rather than a module-level array, and for a reason this suite
 * has now paid for twice: a Playwright worker serves ONE project, and
 * switching project restarts it. A ledger held in memory therefore
 * resets at every engine boundary while the server's own rate limiter
 * counts the whole run -- so the pacing did nothing on the second and
 * third engines and the refusal came back, which is exactly the "state I
 * assumed was shared and is not" shape that the fixed-port hazard was.
 *
 * Through the environment for the same reason the database path is: the
 * runner evaluates this first and every worker inherits the name, so all
 * of them append to one file.
 */
process.env.E2E_LOGIN_LEDGER ??= join(
  tmpdir(),
  `reference-app-e2e-logins-${Date.now()}-${process.pid}.json`,
)
/** The run's sign-in ledger, shared by every worker. */
export const LOGIN_LEDGER_PATH = process.env.E2E_LOGIN_LEDGER

/**
 * Where the server's own output is captured for the one journey that
 * needs to read it: the invitation flow.
 *
 * An invitation token never appears in an API response -- it is a bearer
 * credential handed to the invitee alone, through the message the server
 * sends (go/org's own fragment says so), and this app has no
 * team-management UI to click through instead. In standalone mode the
 * Mailer seam resolves to the console mailer, whose entire purpose is to
 * print the mail a developer would otherwise have to intercept, so the
 * suite reads it the same way that developer would: from the server's
 * output. It is the cross-process form of what the app's own Go flow test
 * does with a capturing mailer.
 *
 * Nothing else reads this file, and no product code knows it exists.
 *
 * Through the environment for the same reason the database path is: the
 * writer is the server the runner started, the reader is a spec in a
 * worker process, and a per-import value would leave them looking at two
 * different files.
 */
process.env.E2E_SERVER_LOG_PATH ??= join(
  tmpdir(),
  `reference-app-e2e-${Date.now()}-${process.pid}.log`,
)
export const SERVER_LOG_PATH = process.env.E2E_SERVER_LOG_PATH

/**
 * The passphrase that gates demo-account seeding
 * (cmd/server/demo_users.go's APP_DEMO_USERS_PASSWORD). NOT a secret: it
 * exists only so a throwaway server boots with the three demo accounts
 * the specs sign in as, and it is regenerated on nobody's behalf -- the
 * same shape as the repository's other documented, non-secret
 * development defaults (examples/reference-app/.env.example). It must
 * satisfy authn's real password policy (12 characters minimum), because
 * the seed registers these accounts through the real register route.
 */
export const DEMO_PASSWORD = process.env.E2E_DEMO_PASSWORD ?? 'e2e-demo-password-2026'

export default defineConfig({
  testDir: './e2e',
  // TWO TIERS SIT OUTSIDE THE DEFAULT RUN, FOR TWO DIFFERENT REASONS
  //
  //   @pending -- the thing it checks is still broken, or the surface it
  //   drives does not exist yet (core-journey.pending.spec.ts: the upload
  //   / generate / compare / share / cost journey the product is for;
  //   sessions-are-distinguishable and offered-channels-work each carry
  //   their own open acceptance blocker, and current-clinic-is-visible
  //   carries ONE @pending test beside its two @budget ones -- the
  //   clinic a self-service registration creates is still shown as a
  //   raw tenant id and named on no surface, which is the same missing
  //   capability its file header points at rather than a UI omission).
  //   Written ahead of the fix on purpose -- each one is an acceptance
  //   criterion, checkable the day it lands -- but a suite that is
  //   permanently red says nothing, so they are asked for by name:
  //
  //     pnpm test:e2e:pending
  //
  //   @budget -- VERIFIED PASSING, and out of the default run only
  //   because the suite has no sign-in left to spend on it
  //   (self-service-signup's gate moved here the round its acceptance
  //   blocker closed -- ef8b97a -- its own file header says why):
  //
  //     pnpm test:e2e:budget
  //
  // Keeping these two apart is not bookkeeping. They were one tag, and
  // the conflation made the suite unreadable in the way that matters
  // most: a gate excluded because its defect is open and a gate excluded
  // because the budget is full looked identical, so nothing in the suite
  // could tell anyone what was still broken. An acceptance report cannot
  // be honest on top of that.
  //
  // The budget is a real ceiling, not a tidiness preference, and it has
  // TWO dimensions (go/authn's ratelimit.go): five sign-ins per account
  // per minute, and twenty per IP per minute. The second is the one that
  // caps the suite -- every test signs in from the same machine, so they
  // all draw on one twenty-per-minute pool, and the whole local run
  // finishes inside a single window. The default tier spends seventeen
  // of those twenty.
  //
  // Only the per-account limit was counted when these tiers were drawn,
  // and the difference matters: moving a gate to a quieter demo account
  // buys NO room, because the binding pool is not per-account. A new
  // gate either fits in the three remaining slots or belongs in @budget,
  // whose separate invocation gets a fresh twenty. Going over does not
  // slow the suite down -- it turns it red on 429 in whichever test
  // happens to lose, a failure that says nothing about the product.
  //
  // What makes @budget a real tier rather than a graveyard: each run
  // boots its own server, so a SEPARATE invocation starts with the
  // rate-limit counters at zero and its own full budget. The two
  // commands together run every gate in this suite; one command cannot.
  //
  // The exclusion is conditional rather than absolute because a config
  // grepInvert OVERRIDES a command-line --grep: with it always on, asking
  // for a tier by name answered "No tests found", which is the worst
  // possible failure mode for a gate written to be run deliberately. So
  // selecting by tag says so, which is what the two scripts above do:
  //
  //   E2E_RUN_TAGGED=1 pnpm test:e2e --grep @budget
  grepInvert:
    process.env.E2E_RUN_TAGGED === undefined ? /@pending|@budget/ : undefined,
  // Against a REAL DEPLOYMENT, only the specs tagged @deployment run.
  //
  // The rest are not merely slower there, they are wrong there. This
  // suite signs in 38 times across 11 files, nearly always as the same
  // account, which is free against the local server -- a fresh process
  // per run, so go/authn's in-memory rate-limit counters start at zero
  // and the whole suite finishes in twenty seconds. A deployment is a
  // long-running process: the counters accumulate, the network adds
  // latency, and the suite locks its own account out. Every later test
  // then fails on 429 rather than on its subject, which produces a wall
  // of red that says nothing about the product.
  //
  // So deployment mode is a deliberately small set: the gates that need
  // no sign-in, or one. Reusing a signed-in session across tests -- the
  // usual answer -- is impossible here by design: this product keeps the
  // access token in memory and the refresh token in a closure, writing
  // neither to storage, so there is no storageState to save. That is the
  // right security decision and this suite works within it rather than
  // asking for it to be weakened.
  grep: external ? /@deployment/ : undefined,
  // One worker: the specs share one server, and that server's own
  // per-account rate limiting and progressive lockout (go/authn's
  // ratelimit.go) make concurrent sign-in attempts against the same demo
  // account answer 429 rather than what the spec is asserting. Serial
  // execution is the honest shape for a suite whose subject is a single
  // stateful deployment.
  workers: 1,
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  // NO RETRIES, ANYWHERE -- and this is a decision rather than a
  // default. It used to be `process.env.CI ? 1 : 0`, which would have
  // made a CI run strictly weaker than a local one: a gate failing its
  // first attempt and passing its second reports green, and nobody sees
  // the first.
  //
  // Every flake this suite has had was a DEFECT IN THE GATE, not noise:
  // a decode state read one `evaluateAll` after the element count
  // reached two (expectBeforeAndAfter's own note), an `isVisible()`
  // asked before the frame rendered (openSurface), five more
  // point-in-time reads that `readSettledText` replaced, and a refusal
  // gate locking out its own account across engines. A single retry
  // would have hidden all of them, and the one that mattered most --
  // the disclosure gate reddening on work that had already been done --
  // would have been hidden intermittently, which is worse than either
  // outcome.
  //
  // This file's own comments say it twice already: "a false red is not
  // a cheap failure", and "once a gate is known to flake, its red stops
  // being read". A retry count is the mechanism that makes both true.
  //
  // If the e2e pipeline lands (M4) and a genuinely environmental flake
  // appears -- a browser download hiccup, a runner slow enough to pass
  // a 60s test timeout -- the answer is to fix the gate or raise that
  // timeout. Should retries ever be truly necessary, the reasoning goes
  // here, in writing, next to what it costs.
  retries: 0,
  reporter: process.env.CI ? [['github'], ['list']] : [['list']],
  timeout: 60_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL,
    locale: 'en-US',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  // Chromium is the default because it is the fastest to run and the
  // engine most of the specs were written against. WebKit is here for a
  // reason a dental practice makes concrete: the machines at a front desk
  // and the iPad a dentist shows a patient their simulation on are Safari,
  // and Safari is the engine most likely to disagree with the others. It
  // is opt-in rather than always-on so the everyday run stays quick:
  //
  //   pnpm test:e2e --project=webkit
  //
  // Install it once with `pnpm exec playwright install webkit`.
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'webkit', use: { ...devices['Desktop Safari'] } },
    { name: 'ipad', use: { ...devices['iPad (gen 7)'] } },
  ],
  webServer: external
    ? undefined
    : [
        {
          // `go run` rather than a prebuilt binary: the Go build cache
          // makes the second run cheap, and one command keeps the
          // toolchain's own module resolution (go.work) intact. The
          // redirection is what makes the console mailer's output
          // readable to the invitation journey (SERVER_LOG_PATH above);
          // stdout stays out of the test report either way.
          // Only stdout is redirected: stderr stays on the process, so a
          // server that fails to boot still says so in the test report.
          //
          // THE COST OF THAT, since it is now worth knowing: everything
          // Kernel.Bootstrap says goes to stderr and is therefore NOT in
          // this file. pkgcore announces its resolved seam composition
          // and warns about implementations that do not survive a
          // restart through slog.Default() -- it cannot do otherwise,
          // being the module every other one sits on, so it can never
          // import observability -- and this app runs buildServer, which
          // is where Bootstrap happens, deliberately BEFORE obs.Init
          // (main.go says why). So those lines are Go's default text
          // format on stderr while everything a gate reads here is the
          // structured logger's stdout.
          //
          // Measured, not assumed: a probe boot answers
          // "pkgcore: bootstrapped seam composition eventbus=... " plus
          // two restart warnings on stderr, and zero occurrences on
          // stdout. So a gate that wants to assert on a boot-time line
          // -- which composition the process actually resolved, say --
          // cannot use the shared server at all; it needs its own
          // through test-utils/servers.ts, whose bootServer captures
          // BOTH streams for exactly this reason. Left as it is rather
          // than redirecting stderr too, because that would take a
          // failed boot out of the test report, which is the one thing
          // this redirection was careful to keep.
          command: `sh -c 'go run ./cmd/server > "${SERVER_LOG_PATH}"'`,
          cwd: serverDir,
          url: `http://${loopback}:${apiPort}/healthz`,
          // A cold build of this app compiles every go/* module it
          // imports, which is minutes rather than seconds.
          timeout: 300_000,
          // Never adopt a server this run did not start, on CI or off it.
          // The ports above are this run's own, so there is nothing
          // legitimate to reuse -- and reuse is exactly how a run in one
          // worktree ended up driving another worktree's server. Off, a
          // taken port is a startup failure that names it.
          reuseExistingServer: false,
          // The server's stdout is its OpenTelemetry span export plus its
          // structured log, several hundred lines per test -- piping it
          // buries the test report it is supposed to accompany. Failures
          // still surface: stderr stays piped, and a failed expectation
          // carries its own trace, screenshot and page snapshot.
          stdout: 'ignore',
          stderr: 'pipe',
          env: {
            PORT: apiPort,
            APP_DB_PATH: databasePath,
            APP_DEPLOYMENT_MODE: 'standalone',
            APP_DEMO_USERS_PASSWORD: DEMO_PASSWORD,
            // The smile-simulation pipeline's provider: the block-B
            // gates generate a real simulation, and the only way a
            // freshly booted server can complete one is against this
            // run's own throwaway OpenAI-compatible images endpoint
            // (the entry below). The key is deliberately a fixed
            // non-secret value -- it authenticates nothing, exactly like
            // the demo passphrase above.
            APP_AI_GATEWAY_IMAGE_BASE_URL: `http://${loopback}:${imageProviderPort}`,
            APP_AI_GATEWAY_IMAGE_API_KEY: 'e2e-image-key',
          },
        },
        {
          // --host pins the dev server to the one address baseURL names,
          // so the browser cannot be refused by a server that bound the
          // other half of the dual stack; --strictPort makes a taken port
          // a failure rather than a silent move to the next one.
          command: `vite --port ${webPort} --strictPort --host ${loopback}`,
          cwd: appDir,
          url: baseURL,
          timeout: 120_000,
          reuseExistingServer: false,
          stdout: 'pipe',
          stderr: 'pipe',
          env: { REFERENCE_APP_API_PROXY: `http://${loopback}:${apiPort}` },
        },
        {
          // The throwaway OpenAI-compatible images endpoint: answers
          // every /images/edits with a fixed, deterministic image, so a
          // generation the block-B gates start completes on the real
          // composed stack with no live provider anywhere (see that
          // script's own header).
          command: `node e2e/test-utils/fake-image-provider.mjs`,
          cwd: appDir,
          url: `http://${loopback}:${imageProviderPort}/healthz`,
          timeout: 30_000,
          reuseExistingServer: false,
          stdout: 'pipe',
          stderr: 'pipe',
          env: { PORT: imageProviderPort },
        },
      ],
})
