/**
 * That a member can still sign in after the server has restarted.
 *
 * This is the regression gate for the defect that made the deployed app
 * unusable within minutes of being deployed: membership lived in an
 * in-process map that only boot-time seeding ever wrote, and seeding
 * deliberately skips an account a previous boot already created. So every
 * restart left the accounts in the database with no memberships anywhere,
 * and every sign-in answered authn.tenant_membership_required until the
 * database itself was wiped. On a host that stops an idle machine -- which
 * is the ordinary, recommended shape for a small deployment, and what the
 * reference app's own fly.toml configures -- that was a few minutes of
 * idleness, not an edge case. Fixed by reading org's real membership rows
 * at sign-in.
 *
 * The restart is real: the spec starts a SECOND server process against
 * the same database file and points the browser's API calls at it, which
 * is precisely the situation the defect could not survive -- a fresh
 * process, an existing database, seeding skipped. The page itself is
 * untouched, so the assertion stays a browser assertion: the same person
 * signs in on the same UI and the frame appears.
 *
 * The second process is addressed by rewriting the page's /api requests
 * rather than by restarting Playwright's own webServer, which a spec
 * cannot do. Its port and log file are its own, so nothing it writes
 * disturbs the servers the rest of the suite shares.
 */
import { spawn, type ChildProcess } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import { DEMO_PASSWORD } from '../playwright.config.js'
import { APP_TEXT, submitPasswordSignIn, visitSignIn } from './test-utils/journeys.js'

/** The reference-app Go module directory. */
const serverDir = fileURLToPath(new URL('../..', import.meta.url))

/** The port the restarted process listens on, away from the suite's own. */
const restartPort = process.env.E2E_RESTART_PORT ?? '8092'

test('a member signs in again after the server restarts against the same database', async ({
  page,
}) => {
  // One sign-in, and it happens AFTER the restart. Signing in first would
  // cost a second attempt against the same account's per-minute rate
  // limit while proving nothing this suite does not already prove: that
  // sign-in works against a first-boot server is what every other spec
  // asserts. What only this spec can say is that it still works against a
  // process that booted over an existing database.
  const databasePath = databasePathOfSuiteServer()
  const restarted = spawn(
    'go',
    ['run', './cmd/server'],
    {
      cwd: serverDir,
      env: {
        ...process.env,
        PORT: restartPort,
        APP_DB_PATH: databasePath,
        APP_DEPLOYMENT_MODE: 'standalone',
        APP_DEMO_USERS_PASSWORD: DEMO_PASSWORD,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    },
  )

  try {
    await waitForHealthy(restarted, restartPort)

    // Point the page's API calls at the restarted process. Everything
    // else about the page stays as it is, so what follows is the same
    // browser journey against a server that just booted over an existing
    // database -- the state seeding refuses to re-seed.
    await page.route('**/api/**', async (route) => {
      const url = new URL(route.request().url())
      url.protocol = 'http:'
      url.host = `localhost:${restartPort}`
      await route.continue({ url: url.toString() })
    })

    await visitSignIn(page)
    await submitPasswordSignIn(page, DEMO_OWNER.email, DEMO_OWNER.password)

    // The whole point: the frame, not authn.tenant_membership_required.
    await expect(page.getByRole('link', { name: APP_TEXT.navNotes })).toBeVisible()
  } finally {
    restarted.kill('SIGTERM')
  }
})

/**
 * The database file the suite's own server was started with, read from
 * the environment the configuration set for the whole run. Reading it
 * rather than recomputing it is what keeps the two processes on one file:
 * a path derived again here would be a different path, which is exactly
 * the mistake this suite made once already.
 */
function databasePathOfSuiteServer(): string {
  const path = process.env.E2E_DB_PATH
  if (path === undefined || path === '') {
    // Only reachable when the suite drives an external deployment
    // (E2E_BASE_URL), where there is no local database to restart over.
    test.skip(true, 'no local server to restart: the suite is driving an external deployment')
    return join(tmpdir(), 'unreachable')
  }
  return path
}

/** Waits for the restarted process to answer its health endpoint. */
async function waitForHealthy(child: ChildProcess, port: string): Promise<void> {
  const deadline = Date.now() + 300_000
  let lastError = ''
  child.stderr?.on('data', (chunk: Buffer) => {
    lastError = chunk.toString().slice(-500)
  })
  while (Date.now() < deadline) {
    if (child.exitCode !== null) {
      throw new Error(`e2e: the restarted server exited (${child.exitCode}): ${lastError}`)
    }
    const healthy = await fetch(`http://localhost:${port}/healthz`)
      .then((response) => response.ok)
      .catch(() => false)
    if (healthy) {
      return
    }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
  throw new Error(`e2e: the restarted server never became healthy: ${lastError}`)
}
