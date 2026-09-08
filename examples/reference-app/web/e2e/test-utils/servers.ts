/**
 * Booting a reference-app server that one spec owns.
 *
 * Two journeys need a server of their own rather than the one
 * playwright.config.ts starts for the whole run, and for the same
 * reason: what they check is a property OF a server's own lifecycle or
 * configuration, which cannot be observed on a server shared with every
 * other spec.
 *
 *   - restart-survival.spec.ts needs two processes in sequence over one
 *     database, because a restart is one process and then another.
 *   - provisioning-recovery.spec.ts needs a server booted with failure
 *     injection armed, which would make every other gate's registration
 *     fail its first attempt if it were set for the whole run.
 *
 * The boot-and-wait routine lives here, shared by the two journeys, so
 * its failure diagnosis is written once rather than copied between
 * specs -- two copies would drift in exactly the way that costs a day.
 */
import { spawn, type ChildProcess } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { DEMO_PASSWORD } from '../../playwright.config.js'

/** The reference-app Go module directory. */
const serverDir = fileURLToPath(new URL('../../..', import.meta.url))

/** A server a spec owns: the process, its output, and how to end it. */
export interface OwnedServer {
  readonly process: ChildProcess
  /** Everything the process has said so far, on both streams. */
  readonly said: () => string
  /** Stops it and waits for the process to actually be gone. */
  readonly stop: () => Promise<void>
}

/** What one spec-owned server needs to know about itself. */
export interface ServerOptions {
  /** The port it listens on -- one of the run's own, never a literal. */
  readonly port: string
  /** The SQLite file it opens. */
  readonly databasePath: string
  /** Anything beyond the standard four, e.g. failure injection. */
  readonly env?: Readonly<Record<string, string>>
}

/**
 * Boots a server and returns it once it answers its health endpoint,
 * starting another when one exits before becoming healthy.
 *
 * The retry is not papering over flakiness: a boot legitimately fails
 * while a previous process still holds the jobs-queue single-writer
 * registration, and the registration is released or goes stale shortly
 * after. Retrying is what a supervisor does, and it keeps a spec from
 * encoding a number that belongs to go/jobs. Every attempt's output is
 * kept, so a failure that is NOT the race says so in the report.
 */
export async function bootServer(options: ServerOptions): Promise<OwnedServer> {
  // A cold build compiles every go/* module the app imports, which is
  // minutes rather than seconds.
  const deadline = Date.now() + 300_000
  let attempts = 0
  let transcript = ''

  while (Date.now() < deadline) {
    attempts += 1
    const child = spawn('go', ['run', './cmd/server'], {
      cwd: serverDir,
      env: {
        ...process.env,
        PORT: options.port,
        APP_DB_PATH: options.databasePath,
        APP_DEPLOYMENT_MODE: 'standalone',
        APP_DEMO_USERS_PASSWORD: DEMO_PASSWORD,
        ...options.env,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    const said = capture(child)

    if (await waitForHealthy(child, options.port, deadline)) {
      return { process: child, said, stop: () => stop(child) }
    }

    transcript += `\n--- attempt ${attempts} ---\n${said()}`
    child.kill('SIGKILL')
    await new Promise((resolve) => setTimeout(resolve, 500))
  }

  throw new Error(
    `e2e: no reference-app server became healthy on port ${options.port} in ${attempts} attempts:${transcript.slice(-4000)}`,
  )
}

/**
 * Collects everything a process says on BOTH streams, returning a reader
 * for it.
 *
 * Both, and all of it: keeping only the last stderr chunk would let
 * `go run`'s own "exit status 1" epilogue overwrite the program's
 * explanation, and reading stderr alone would miss the explanation
 * whenever it went to stdout -- which is where this app's structured
 * logger writes, so that is the normal case rather than the exception.
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
 * doing so (false).
 */
async function waitForHealthy(
  child: ChildProcess,
  port: string,
  deadline: number,
): Promise<boolean> {
  while (Date.now() < deadline) {
    if (child.exitCode !== null || child.signalCode !== null) {
      return false
    }
    const healthy = await fetch(`http://127.0.0.1:${port}/healthz`)
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
 * than for the signal to have been sent. A next boot competes with this
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

/**
 * Boots a fake image provider a spec owns, and returns how to stop it.
 *
 * The run's shared provider answers successfully (playwright.config.ts's
 * third webServer entry), which is what every gate about a working
 * generation needs. A gate about a FAILED generation needs a provider
 * that refuses, and flipping the shared one would break the others -- so
 * it gets its own instance on its own port.
 */
export async function bootImageProvider(options: {
  readonly port: string
  readonly refuse: boolean
}): Promise<OwnedServer> {
  const deadline = Date.now() + 60_000
  const child = spawn('node', ['e2e/test-utils/fake-image-provider.mjs'], {
    cwd: fileURLToPath(new URL('../..', import.meta.url)),
    env: {
      ...process.env,
      PORT: options.port,
      ...(options.refuse ? { FAKE_IMAGE_FAIL: '1' } : {}),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  const said = capture(child)
  if (!(await waitForHealthy(child, options.port, deadline))) {
    throw new Error(`e2e: the fake image provider never became healthy: ${said()}`)
  }
  return { process: child, said, stop: () => stop(child) }
}

/**
 * Sends the page's /api calls to a spec's own server.
 *
 * Fetched by Playwright and handed back as this origin's own answer,
 * rather than continued to a different origin: `route.continue({ url })`
 * to another port is a CROSS-ORIGIN request as far as the page is
 * concerned, and the browser applies CORS to it. This app's server sends
 * no CORS headers -- it has never needed to, being same-origin in every
 * real deployment -- so a rewritten call would be blocked and the page
 * would render "No network connection", which reads exactly like the
 * defect a gate is looking for on an engine where nothing is wrong.
 */
export async function routeApiTo(
  page: import('@playwright/test').Page,
  port: string,
): Promise<void> {
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    url.protocol = 'http:'
    url.host = `127.0.0.1:${port}`
    const response = await route.fetch({ url: url.toString() })
    await route.fulfill({ response })
  })
}
