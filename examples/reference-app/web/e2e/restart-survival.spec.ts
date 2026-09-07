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
 * THE RESTART IS SEQUENTIAL, WHICH IS THE WHOLE POINT
 *
 * The spec owns two server processes and one database file: the first boot
 * creates the file and seeds it, the spec then stops that process and
 * waits for it to be gone, and a second process boots over the file the
 * first left behind. That is what a restart is -- one process, then
 * another, over surviving state.
 *
 * It used to be written differently, and the difference mattered: a second
 * server was started while the suite's own was still running, and the
 * page's API calls were pointed at it. That is a concurrent second
 * process, not a restart, and it only resembled one because nothing in
 * the app minded. Something does now -- go/jobs grew a single-writer
 * registration, so a second queue on one database is refused outright
 * (jobs.queue_writer_active) -- and the refusal is correct: two
 * dispatchers on one jobs table would each claim the same work. The gate
 * had been passing on a shape that was never the thing it claimed to
 * check, and the guard is what exposed it.
 *
 * Because that registration is released on shutdown and stolen once its
 * heartbeat goes stale, the second boot may briefly lose a race with the
 * first process's own exit. bootUntilHealthy retries for that reason --
 * the same thing a process supervisor does, rather than an assumption
 * about how long the window is.
 */
import { spawn, type ChildProcess } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import { DEMO_PASSWORD, RESTART_API_PORT } from '../playwright.config.js'
import {
  expectSignedIn,
  submitPasswordSignIn,
  visitSignIn,
} from './test-utils/journeys.js'

/** The reference-app Go module directory. */
const serverDir = fileURLToPath(new URL('../..', import.meta.url))

/**
 * The port both of this spec's processes listen on -- one port, because
 * they run one after the other. It is this run's own third port, read
 * from the configuration rather than defaulted to here: several worktrees
 * of this repository run this suite at once, and a port named in the
 * source is a port they all ask for.
 */
const restartPort = RESTART_API_PORT

/**
 * This spec's own database, separate from the suite's. The suite's server
 * keeps its jobs-queue registration alive for as long as it runs, so a
 * second process could never boot over that file while the suite is up --
 * and it should not have to: what this gate needs is a file that outlives
 * a process, not that particular file.
 */
const databasePath = join(
  tmpdir(),
  `reference-app-e2e-restart-${Date.now()}-${process.pid}.db`,
)

test('a member signs in again after the server restarts against the same database', async ({
  page,
}) => {
  // The boot that seeds: an empty file, so demo seeding really runs and
  // the three accounts exist with their memberships.
  const seeding = await bootUntilHealthy()
  await stop(seeding)

  // The boot under test: a fresh process over the file the first one left,
  // which is the state seeding refuses to re-seed.
  const restarted = await bootUntilHealthy()

  try {
    // Point the page's API calls at the restarted process, so what
    // follows is the same browser journey a person makes against a
    // server that just booted over an existing database. The page itself
    // is untouched.
    //
    // The calls are FETCHED by Playwright and handed back as this
    // origin's own answer, rather than continued to a different origin.
    //
    // `route.continue({ url })` to another port is a CROSS-ORIGIN request
    // as far as the page is concerned, and the browser applies CORS to
    // it: this app's server sends no CORS headers (it has never needed
    // to, being same-origin in every real deployment), so WebKit blocked
    // every rewritten call and the page rendered "No network
    // connection." The gate then failed on the sign-in surface with the
    // frame never appearing -- which reads exactly like the membership
    // defect it exists to catch, on an engine where nothing was wrong.
    // Chromium happened to allow it, so the suite looked fine.
    //
    // route.fetch performs the request from Playwright's own stack, with
    // no origin and no preflight involved, and fulfill returns the real
    // response to the page. The server under test is unchanged, and so
    // is what the browser believes it is talking to.
    await page.route('**/api/**', async (route) => {
      const url = new URL(route.request().url())
      url.protocol = 'http:'
      url.host = `127.0.0.1:${restartPort}`
      const response = await route.fetch({ url: url.toString() })
      await route.fulfill({ response })
    })

    // One sign-in, and it happens after the restart. Signing in before it
    // would spend a second attempt against this account's per-minute rate
    // limit while proving nothing the rest of the suite does not already
    // prove. What only this spec can say is that sign-in still works
    // against a process that booted over an existing database.
    await visitSignIn(page)
    await submitPasswordSignIn(page, DEMO_OWNER.email, DEMO_OWNER.password)

    // The whole point: the frame, not authn.tenant_membership_required.
    await expectSignedIn(page)
  } finally {
    await stop(restarted)
  }
})

/** Starts one reference-app server over this spec's database and port. */
function boot(): ChildProcess {
  return spawn('go', ['run', './cmd/server'], {
    cwd: serverDir,
    env: {
      ...process.env,
      PORT: restartPort,
      APP_DB_PATH: databasePath,
      APP_DEPLOYMENT_MODE: 'standalone',
      APP_DEMO_USERS_PASSWORD: DEMO_PASSWORD,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
}

/**
 * Boots a server and returns it once it answers its health endpoint,
 * starting another when one exits before becoming healthy.
 *
 * The retry is not papering over flakiness: a boot legitimately fails
 * while the previous process still holds the jobs-queue single-writer
 * registration, and the registration is released or goes stale shortly
 * after. Retrying is what a supervisor does, and it keeps this spec from
 * encoding a number that belongs to go/jobs. Every attempt's output is
 * kept, so a failure that is NOT the race says so in the report.
 */
async function bootUntilHealthy(): Promise<ChildProcess> {
  const deadline = Date.now() + 300_000
  let attempts = 0
  let transcript = ''
  while (Date.now() < deadline) {
    attempts += 1
    const child = boot()
    const said = capture(child)
    const healthy = await waitForHealthy(child, deadline)
    if (healthy) {
      return child
    }
    transcript += `\n--- attempt ${attempts} ---\n${said()}`
    child.kill('SIGKILL')
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
  throw new Error(
    `e2e: no reference-app server became healthy on port ${restartPort} in ${attempts} attempts:${transcript.slice(-4000)}`,
  )
}

/**
 * Collects everything a process says on BOTH streams, returning a reader
 * for it.
 *
 * Both, and all of it: two separate omissions used to hide the reason a
 * restart failed. Keeping only the last stderr chunk let `go run`'s own
 * "exit status 1" epilogue overwrite the program's explanation, and
 * reading stderr alone missed the explanation entirely whenever it went
 * to stdout -- which is where this app's structured logger writes, so
 * that was the normal case rather than the exception. The report said a
 * server had exited without ever saying why.
 */
function capture(child: ChildProcess): () => string {
  let said = ''
  const collect = (chunk: Buffer): void => {
    said += chunk.toString()
  }
  child.stderr?.on('data', collect)
  child.stdout?.on('data', collect)
  return () => said.trim()
}

/**
 * Waits until the process answers /healthz (true) or exits without ever
 * doing so (false). A cold build compiles every go/* module the app
 * imports, which is minutes rather than seconds, so the deadline is the
 * caller's whole budget.
 */
async function waitForHealthy(child: ChildProcess, deadline: number): Promise<boolean> {
  while (Date.now() < deadline) {
    if (child.exitCode !== null || child.signalCode !== null) {
      return false
    }
    const healthy = await fetch(`http://127.0.0.1:${restartPort}/healthz`)
      .then((response) => response.ok)
      .catch(() => false)
    if (healthy) {
      return true
    }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
  return false
}

/**
 * Stops a server and waits for the process to actually be gone, rather
 * than for the signal to have been sent. The next boot competes with this
 * one for a port and for the jobs-queue registration, so "asked it to
 * stop" is not the state the next step needs.
 */
async function stop(child: ChildProcess): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) {
    return
  }
  const exited = new Promise<void>((resolve) => {
    child.once('exit', () => resolve())
  })
  child.kill('SIGTERM')
  const gaveUp = new Promise<void>((resolve) => setTimeout(resolve, 15_000))
  await Promise.race([exited, gaveUp])
  if (child.exitCode === null && child.signalCode === null) {
    child.kill('SIGKILL')
    await exited
  }
}
