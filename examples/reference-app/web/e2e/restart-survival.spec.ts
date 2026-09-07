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
 * first process's own exit. bootServer retries for that reason --
 * the same thing a process supervisor does, rather than an assumption
 * about how long the window is.
 */
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from '@playwright/test'
import { DEMO_OWNER } from './test-utils/accounts.js'
import { RESTART_API_PORT } from '../playwright.config.js'
import { bootServer, routeApiTo, type OwnedServer } from './test-utils/servers.js'
import {
  expectSignedIn,
  submitPasswordSignIn,
  visitSignIn,
} from './test-utils/journeys.js'

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
  const seeding = await bootServer({ port: restartPort, databasePath })
  await seeding.stop()

  // The boot under test: a fresh process over the file the first one left,
  // which is the state seeding refuses to re-seed.
  const restarted: OwnedServer = await bootServer({ port: restartPort, databasePath })

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
    await routeApiTo(page, restartPort)

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
    await restarted.stop()
  }
})
