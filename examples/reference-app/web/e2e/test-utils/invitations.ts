/**
 * The setup an invitation journey needs and the browser cannot do,
 * because the surfaces for it do not exist yet.
 *
 * Two things here are deliberately not browser steps:
 *
 *  1. Creating an invitation. go/org ships the operation, but this app
 *     has no team-management UI, so the journey's setup calls the
 *     operation directly. When that UI lands, this helper is what the
 *     spec stops needing.
 *  2. Reading the invitation token. It never appears in an API response
 *     -- it is a bearer credential handed to the invitee alone, through
 *     the message the server sends. In standalone mode the Mailer seam is
 *     the console mailer, whose whole purpose is to print that message,
 *     so the token comes out of the server's captured output the same way
 *     a developer running the app would read it.
 *
 * Both use Playwright's request fixture rather than a bare fetch, so the
 * calls carry the run's own base URL and proxy exactly like the page's.
 */
import { readFile } from 'node:fs/promises'
import { expect, type APIRequestContext } from '@playwright/test'
import { SERVER_LOG_PATH } from '../../playwright.config.js'

/** The header this app's demo subject resolver reads to identify a caller. */
const DEMO_SUBJECT_HEADER = 'X-Demo-User-Id'

/** A signed-in caller: the bearer token plus the user id it belongs to. */
export interface Caller {
  readonly accessToken: string
  readonly userId: string
}

/** Signs in through the API and returns what the org calls need. */
export async function signInThroughApi(
  request: APIRequestContext,
  email: string,
  password: string,
): Promise<Caller> {
  const response = await request.post('/api/v1/authn/login/password', {
    data: { identifier: email, password },
  })
  expect(response.status(), await bodyOf(response)).toBe(200)
  const body = (await response.json()) as {
    access_token?: string
    principal?: { user_id?: string }
  }
  const accessToken = body.access_token
  const userId = body.principal?.user_id
  if (accessToken === undefined || userId === undefined) {
    throw new Error('e2e: sign-in answered without an access token or user id')
  }
  return { accessToken, userId }
}

/** Registers an account through the API and returns nothing but its id. */
export async function registerThroughApi(
  request: APIRequestContext,
  email: string,
  password: string,
): Promise<string> {
  const response = await request.post('/api/v1/authn/register', {
    data: { email, password },
  })
  expect(response.status(), await bodyOf(response)).toBe(201)
  const body = (await response.json()) as { id?: string }
  if (body.id === undefined) {
    throw new Error('e2e: register answered without an id')
  }
  return body.id
}

/** The id of the caller's tenant's root organization node. */
export async function rootNodeId(
  request: APIRequestContext,
  caller: Caller,
): Promise<string> {
  const response = await request.get('/api/v1/org/nodes', {
    headers: orgHeaders(caller),
  })
  expect(response.status(), await bodyOf(response)).toBe(200)
  const body = (await response.json()) as { nodes?: { id: string; depth: number }[] }
  const nodes = body.nodes ?? []
  const root = nodes.find((node) => node.depth === 0) ?? nodes[0]
  if (root === undefined) {
    throw new Error('e2e: the tenant has no organization nodes to invite into')
  }
  return root.id
}

/**
 * Invites an address into the caller's tenant at the given node, then
 * returns the token the invitation message carried.
 */
export async function inviteAndReadToken(
  request: APIRequestContext,
  caller: Caller,
  email: string,
  nodeId: string,
): Promise<string> {
  const response = await request.post('/api/v1/org/invitations', {
    headers: orgHeaders(caller),
    data: { email, nodeId },
  })
  expect(response.status(), await bodyOf(response)).toBe(201)
  return await tokenFromServerOutput(email)
}

/**
 * Accepts an invitation with no tenant-scoped credential at all -- the
 * shape a genuinely fresh invitee is in, and the one the endpoint was
 * unable to serve until the tenantless-accept fix: the tenant is resolved
 * from the token itself.
 */
export async function acceptInvitationAsFreshUser(
  request: APIRequestContext,
  inviteeUserId: string,
  token: string,
): Promise<void> {
  const response = await request.post('/api/v1/org/invitations/accept', {
    // No Authorization header at all: a memberless invitee holds no
    // tenant-scoped token, and this is the state the endpoint must serve.
    // Only the caller identity this app's demo subject resolver reads.
    headers: { [DEMO_SUBJECT_HEADER]: inviteeUserId },
    data: { token },
  })
  expect(response.status(), await bodyOf(response)).toBe(200)
}

/** The headers org's caller-scoped operations need from this app. */
function orgHeaders(caller: Caller): Record<string, string> {
  return {
    Authorization: `Bearer ${caller.accessToken}`,
    [DEMO_SUBJECT_HEADER]: caller.userId,
  }
}

/**
 * Polls the server's captured output for the invitation message sent to
 * one address and returns the token on its accept link. The mail is
 * written after the request that triggered it answers, so a short poll
 * rather than a single read is what makes this deterministic.
 */
async function tokenFromServerOutput(email: string): Promise<string> {
  const deadline = Date.now() + 15_000
  let lastSeen = ''
  while (Date.now() < deadline) {
    const output = await readFile(SERVER_LOG_PATH, 'utf8').catch(() => '')
    if (output !== lastSeen) {
      lastSeen = output
      const token = extractToken(output, email)
      if (token !== undefined) {
        return token
      }
    }
    await new Promise((resolve) => setTimeout(resolve, 250))
  }
  throw new Error(`e2e: no invitation message for ${email} appeared in the server output`)
}

/**
 * Finds the accept link in the mail record addressed to one recipient and
 * returns its token parameter. The console mailer prints one record per
 * message, each opening with its own "[mail] to:" line, so the record for
 * this recipient is the text from that line up to the next one.
 */
function extractToken(output: string, email: string): string | undefined {
  const recipientIndex = output.lastIndexOf(`[mail] to: ${email}`)
  if (recipientIndex === -1) {
    return undefined
  }
  const nextRecord = output.indexOf('[mail] to: ', recipientIndex + 1)
  const record = output.slice(
    recipientIndex,
    nextRecord === -1 ? undefined : nextRecord,
  )
  const link = /https:\/\/\S+/.exec(record)?.[0]
  if (link === undefined) {
    return undefined
  }
  const token = new URL(link).searchParams.get('token')
  return token === null || token === '' ? undefined : token
}

/** The response body, for an assertion message that says what went wrong. */
async function bodyOf(response: { text: () => Promise<string> }): Promise<string> {
  return await response.text().catch(() => '<unreadable body>')
}
