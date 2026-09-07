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
import { fileURLToPath } from 'node:url'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

/** This app's directory, resolved from this config file's own location. */
const appDir = fileURLToPath(new URL('.', import.meta.url))

/** The reference-app Go module directory (this app's parent). */
const serverDir = fileURLToPath(new URL('..', import.meta.url))

/**
 * Ports deliberately away from the defaults a developer's own `vite` and
 * `go run ./cmd/server` occupy (5173 and 8080), so a suite run never
 * collides with a session already in progress.
 */
const apiPort = process.env.E2E_API_PORT ?? '8091'
const webPort = process.env.E2E_WEB_PORT ?? '5191'

/** Where the browser half points. */
const baseURL = process.env.E2E_BASE_URL ?? `http://localhost:${webPort}`

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
  // The @pending gates describe surfaces that do not exist yet
  // (core-journey.pending.spec.ts: the upload / generate / compare /
  // share / cost journey the product is for). They are written ahead of
  // delivery on purpose -- each one is the acceptance criterion for a
  // block, checkable the day it lands -- but a suite that is permanently
  // red says nothing, so the default run leaves them out and they are
  // asked for by name instead:
  //
  //   pnpm test:e2e --grep @pending
  //
  // A block's gate moves out of @pending when its surface ships, which is
  // the moment it starts being a regression gate rather than a promise.
  //
  // The exclusion is conditional rather than absolute because a config
  // grepInvert OVERRIDES a command-line --grep: with it always on, asking
  // for the pending gates by name answered "No tests found", which is the
  // worst possible failure mode for a gate written to be run deliberately.
  // Asking for them means saying so:
  //
  //   E2E_INCLUDE_PENDING=1 pnpm test:e2e --grep @pending
  grepInvert: process.env.E2E_INCLUDE_PENDING === undefined ? /@pending/ : undefined,
  // One worker: the specs share one server, and that server's own
  // per-account rate limiting and progressive lockout (go/authn's
  // ratelimit.go) make concurrent sign-in attempts against the same demo
  // account answer 429 rather than what the spec is asserting. Serial
  // execution is the honest shape for a suite whose subject is a single
  // stateful deployment.
  workers: 1,
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['github'], ['list']] : [['list']],
  timeout: 60_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL,
    locale: 'en-US',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
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
          command: `sh -c 'go run ./cmd/server > "${SERVER_LOG_PATH}"'`,
          cwd: serverDir,
          url: `http://localhost:${apiPort}/healthz`,
          // A cold build of this app compiles every go/* module it
          // imports, which is minutes rather than seconds.
          timeout: 300_000,
          reuseExistingServer: !process.env.CI,
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
          },
        },
        {
          command: `vite --port ${webPort} --strictPort`,
          cwd: appDir,
          url: baseURL,
          timeout: 120_000,
          reuseExistingServer: !process.env.CI,
          stdout: 'pipe',
          stderr: 'pipe',
          env: { REFERENCE_APP_API_PROXY: `http://localhost:${apiPort}` },
        },
      ],
})
