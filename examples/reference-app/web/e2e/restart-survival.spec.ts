/**
 * That a member can still sign in after the server has restarted.
 *
 * Membership lives in org's real rows, read at sign-in, so an account
 * seeded by one boot is still a member after the next -- a property
 * worth pinning because it is exactly what an in-process membership map
 * would break: seeding skips an account a previous boot already
 * created, and a host that stops an idle machine (the ordinary,
 * recommended shape for a small deployment, and what the reference
 * app's own fly.toml configures) would leave every sign-in refused
 * with the uniform 401 authn.invalid_credentials within minutes of
 * idleness.
 *
 * THE RESTART IS SEQUENTIAL, WHICH IS THE WHOLE POINT
 *
 * The spec owns two server processes and one database file: the first
 * boot creates the file and seeds it, the spec then stops that process
 * and waits for it to be gone, and a second process boots over the file
 * the first left behind. That is what a restart is -- one process, then
 * another, over surviving state. A second server started while the
 * first still runs would be a concurrent second process, not a restart,
 * and the app refuses one: go/jobs' queue registration is single-writer,
 * so a second queue on one database is refused outright
 * (jobs.queue_writer_active) -- correctly, since two dispatchers on one
 * jobs table would each claim the same work.
 *
 * Because that registration is released on shutdown and stolen once its
 * heartbeat goes stale, the second boot may briefly lose a race with
 * the first process's own exit. bootServer retries for that reason --
 * the same thing a process supervisor does, rather than an assumption
 * about how long the window is.
 */
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { DEMO_OWNER } from './test-utils/accounts.js'
import { RESTART_API_PORT } from '../playwright.config.js'
import { bootServer, routeApiTo, test, type OwnedServer } from './test-utils/servers.js'
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
    // routeApiTo fetches through Playwright's own stack and fulfills
    // with the real answer, rather than continuing the request to
    // another port: `route.continue({ url })` would make a CROSS-ORIGIN
    // request as far as the page is concerned, and the browser applies
    // CORS to it. This app's server sends no CORS headers (it has never
    // needed to, being same-origin in every real deployment), so a
    // rewritten call is blocked and the page renders "No network
    // connection." -- a failure that reads exactly like the membership
    // defect this gate exists to catch. The server under test is
    // unchanged, and so is what the browser believes it is talking to.
    await routeApiTo(page, restartPort)

    // One sign-in, and it happens after the restart. Signing in before it
    // would spend a second attempt against this account's per-minute rate
    // limit while proving nothing the rest of the suite does not already
    // prove. What only this spec can say is that sign-in still works
    // against a process that booted over an existing database.
    await visitSignIn(page)
    await submitPasswordSignIn(page, DEMO_OWNER.email, DEMO_OWNER.password)

    // The whole point: the frame, not the uniform 401
    // authn.invalid_credentials a missing membership draws.
    await expectSignedIn(page)
  } finally {
    await restarted.stop()
  }
})
