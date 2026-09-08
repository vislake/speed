/**
 * TeamView contract: the team surface gates on the roster read it
 * drives and renders what the server answers -- members, pending
 * invitations and the invite flow the add-a-colleague acceptance shape
 * clicks. The read is the real permission fetch: the demo server's
 * denyTeamRead switch answers the rbac gate's 403 the way the real
 * server does, so the gate's denied branch (the ui-kit no-permission
 * empty state, roster and invite control absent) and the invite
 * refusals are driven by genuine refusals, never stubbed locally. The
 * journeys drive a real client bound into the runtime seam, sign in
 * through the real session operation (the surface reads the current
 * tenant from the auth-core hooks, so an anonymous render can never
 * leave pending), and pin the observed requests -- the three-read
 * snapshot, the invite POST's body, and the invalidating refetch after
 * a send.
 *
 * The invite journey is the surface's acceptance shape end to end at
 * the unit tier: the opener reveals the address form, the send lands
 * the pending invitation, and the refetched list -- not a banner -- is
 * what shows the address standing under its Pending status, with the
 * session memory pairing (team-view.tsx's invited-address memory)
 * surviving a remount so the roster does not forget whom it invited.
 * A refused send keeps the form and renders the answer's code text; an
 * address the client's own rules refuse never reaches the network; and
 * a remount after navigating away collapses the form (a half-filled
 * invite field must not come back to life under a roster that has
 * since moved on).
 *
 * The roster naming is the defect this surface's walk-through found,
 * pinned here at the unit tier: the members read goes to the app's own
 * roster-with-identity answer (GET /api/reference-app/team-members,
 * which the demo server answers from its account directory), and a
 * member row renders the display identity the answer carried -- a
 * member who registered no display name is named by their email, a
 * member whose identity a suite scripts is named by it (display name
 * winning over email, the composition's own precedence), a member
 * whose account answered no identity renders the bundle's fallback
 * label -- and the raw user id is never what a row renders as a name,
 * which is the assertion that would have failed before this round.
 *
 * The gate's error classification earns the same three checks the
 * notes surface's own suite runs: a refused read falls the gate shut
 * to the no-permission suit; a 5xx read failure renders the read-error
 * suit, never the no-permission one; and a codeless failure (a raw
 * transport throw nothing normalized) renders the read-error suit too,
 * never a permanent pending.
 *
 * Built-in strings are asserted through the bundles they render from --
 * the app's own zh-CN/en-US fixtures and the ui-kit fixture (relative
 * imports, the notes-view precedent) -- never inline: the CJK scan
 * treats test files as English text like everything else. Served
 * addresses and user ids are server data, not copy, and travel in the
 * journeys verbatim.
 */

import { act } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { RequestFn } from '@speed/api-client'
import { beforeEach, describe, expect, it } from 'vitest'
import uiKitZhCN from '../../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import {
  DEMO_READER_IDENTIFIER,
  DEMO_READER_USER_ID,
  demoServer,
} from '../test-utils/demo-server.js'
import type { RealClientRig } from '../test-utils/real-client.js'
import {
  errorResponse,
  jsonResponse,
  makeRealClientRig,
  signInWithPassword,
} from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import {
  clearInvitedAddressMemory,
  TeamView,
} from './team-view.js'
import { TEAM_MEMBERS_PATH } from '../team-api.js'

/** A transport whose every call rejects with a raw, code-less error --
 * the shape a bug-shaped transport throw arrives in (see the notes
 * suite's own copy of the double). */
const UNNORMALIZED_TRANSPORT_FAILURE: RequestFn = () =>
  Promise.reject(new Error('[team-view.test] an un-normalized transport failure'))

/** The address the invite journeys send; the demo server accepts any
 * '@'-bearing address and lists the created invitation back. */
const INVITEE_EMAIL = 'new-hire@example.com'

/** The demo root node id every org answer binds an invitee to. */
const DEMO_ROOT_NODE_ID = 'node-root-1'

/** Renders the team surface over a signed-in rig (the surface reads
 * the current tenant from the auth-core hooks), with an optional api
 * override for the failure-shape journeys that must drive the reads
 * through something other than the rig's own client. */
function renderTeam(
  rig: RealClientRig,
  apiOverride?: RequestFn,
): ReturnType<typeof renderWithAppServices> {
  return renderWithAppServices(
    <TeamView />,
    { session: rig.session, api: apiOverride ?? rig.api },
  )
}

/** How many times the rig observed one org endpoint. */
function orgReads(rig: RealClientRig, path: string): number {
  return rig.calls.filter((call) => call.method === 'GET' && call.path === path)
    .length
}

/** The invite POSTs the rig observed, decoded. */
function invitePosts(rig: RealClientRig): unknown[] {
  return rig.calls
    .filter(
      (call) =>
        call.method === 'POST' && call.path === '/api/v1/org/invitations',
    )
    .map((call) => JSON.parse(call.body))
}

/** The invite POST bodies carry the address and the clinic's root node. */
function expectInviteBody(body: unknown, email: string): void {
  expect(body).toEqual({ email, nodeId: DEMO_ROOT_NODE_ID })
}

describe('TeamView', () => {
  beforeEach(() => {
    // The invited-address memory is module-scoped (see the view's own
    // doc comment): a journey's send records what it sent, and nothing
    // from an earlier test may name a row it never invited.
    clearInvitedAddressMemory()
  })

  it('renders the roster, the invite action and the empty pending list from the three-read snapshot', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderTeam(rig)

    await view.findByRole('heading', { name: zhCN.team.heading, level: 1 })
    // The section headings only render once the snapshot read answers
    // (the gate opens on the roster read), so they are waited on.
    await view.findByRole('heading', { name: zhCN.team.members.heading, level: 2 })
    expect(view.getByRole('heading', { name: zhCN.team.invitations.heading, level: 2 })).toBeInTheDocument()

    // The roster names people: the signed-in member's own row is the
    // bundle's "you" label (the principal's user id names it), and the
    // reader's row is named by the email the account registered with --
    // the demo directory's answer for the reader-shaped member. The raw
    // user id is never what a row renders as a name: the assertion that
    // failed before this round.
    expect(view.getByText(zhCN.team.members.youRow)).toBeInTheDocument()
    expect(view.getByText(DEMO_READER_IDENTIFIER)).toBeInTheDocument()
    expect(view.queryByText(DEMO_READER_USER_ID)).not.toBeInTheDocument()
    expect(view.getByText(zhCN.team.invitations.emptyTitle)).toBeInTheDocument()

    // The invite opener is drawn (the clinic has a root node to bind
    // an invitee to), and the whole surface came from one snapshot:
    // one read per endpoint -- the members half on the host's own
    // roster answer, never org's raw member list (the surface that
    // would have served user ids as names).
    expect(
      view.getByRole('button', { name: zhCN.team.invite.action }),
    ).toBeInTheDocument()
    expect(orgReads(rig, TEAM_MEMBERS_PATH)).toBe(1)
    expect(orgReads(rig, '/api/v1/org/invitations')).toBe(1)
    expect(orgReads(rig, '/api/v1/org/nodes')).toBe(1)
    expect(orgReads(rig, '/api/v1/org/members')).toBe(0)
  })

  it('a member who registered a display name is named by it, never by their email or user id', async () => {
    // The demo account directory knows the reader-shaped member by their
    // email only (the real reader registered without a display name); a
    // suite scripts the display name their registration carried, and the
    // naming ladder must render the name -- display name winning over
    // email, exactly the precedence the roster's composition decides.
    const rig = makeRealClientRig(
      demoServer({
        initialTeamIdentities: {
          [DEMO_READER_USER_ID]: { displayName: 'Demo Reader' },
        },
      }),
    )
    await signInWithPassword(rig)
    const view = renderTeam(rig)

    await view.findByRole('heading', { name: zhCN.team.members.heading, level: 2 })
    expect(view.getByText('Demo Reader')).toBeInTheDocument()
    expect(view.queryByText(DEMO_READER_IDENTIFIER)).not.toBeInTheDocument()
    expect(view.queryByText(DEMO_READER_USER_ID)).not.toBeInTheDocument()
  })

  it('a member whose account answers no identity renders the fallback label, never the raw id', async () => {
    // A roster row whose user the account directory cannot name -- the
    // mirror of a member whose account row the real composition could
    // not read. The surface renders the bundle's fallback label; the raw
    // user id must not surface as the name.
    const rig = makeRealClientRig(
      demoServer({
        initialTeamMembers: [
          {
            membershipId: 'membership-orphaned',
            userId: 'user-99',
            nodeId: DEMO_ROOT_NODE_ID,
            status: 'active',
            createdAt: '2026-09-01T00:00:00Z',
          },
        ],
      }),
    )
    await signInWithPassword(rig)
    const view = renderTeam(rig)

    await view.findByRole('heading', { name: zhCN.team.members.heading, level: 2 })
    expect(
      view.getByText(zhCN.team.members.identityUnknown),
    ).toBeInTheDocument()
    expect(view.queryByText('user-99')).not.toBeInTheDocument()
  })

  it('the invite flow: open the form, send an address, and the pending list shows the invitation standing', async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderTeam(rig)
    const user = userEvent.setup()

    await view.findByRole('heading', { name: zhCN.team.members.heading, level: 2 })
    await user.click(
      view.getByRole('button', { name: zhCN.team.invite.action }),
    )
    const address = view.getByLabelText(zhCN.team.invite.emailLabel)
    await user.type(address, INVITEE_EMAIL)
    await user.click(
      view.getByRole('button', { name: zhCN.team.invite.send }),
    )

    // The invitation is listed with the address the owner typed and
    // its Pending status -- the refetched server list is what shows
    // it, never a banner alone -- and the send's answer carried the
    // clinic's root node as the binding target.
    expect(await view.findByText(INVITEE_EMAIL)).toBeInTheDocument()
    expect(view.getByText(zhCN.team.status.pending)).toBeInTheDocument()
    expect(invitePosts(rig)).toHaveLength(1)
    expectInviteBody(invitePosts(rig)[0], INVITEE_EMAIL)

    // The send turned the form back over (its one submission never
    // repeats on a refetch) and the notice told the owner in the
    // status region.
    expect(view.getByRole('status')).toHaveTextContent(
      zhCN.team.invite.sentNotice,
    )
    expect(
      view.queryByLabelText(zhCN.team.invite.emailLabel),
    ).not.toBeInTheDocument()
  })

  it('remounting the surface keeps naming the pending invitation the session sent', async () => {
    const server = demoServer()
    const rig = makeRealClientRig(server)
    await signInWithPassword(rig)
    let view = renderTeam(rig)
    const user = userEvent.setup()

    await view.findByRole('heading', { name: zhCN.team.members.heading, level: 2 })
    await user.click(
      view.getByRole('button', { name: zhCN.team.invite.action }),
    )
    await user.type(view.getByLabelText(zhCN.team.invite.emailLabel), INVITEE_EMAIL)
    await user.click(
      view.getByRole('button', { name: zhCN.team.invite.send }),
    )
    await view.findByText(INVITEE_EMAIL)

    // Away and back: the invitation is still the server's pending row,
    // and this session still remembers whose address it is -- an owner
    // navigating away never comes back to a roster that forgot whom it
    // invited.
    view.unmount()
    view = renderTeam(rig)
    expect(await view.findByText(INVITEE_EMAIL)).toBeInTheDocument()
    expect(view.getByText(zhCN.team.status.pending)).toBeInTheDocument()

    // The remount collapsed the invite form: a half-filled field must
    // not come back to life under the roster.
    expect(
      view.getByRole('button', { name: zhCN.team.invite.action }),
    ).toBeInTheDocument()
  })

  it('a refused send keeps the form and renders the answer\'s code text', async () => {
    const rig = makeRealClientRig(
      demoServer({
        teamInviteRefusal: {
          status: 429,
          code: 'org.invitation_rate_limited',
        },
      }),
    )
    await signInWithPassword(rig)
    const view = renderTeam(rig)
    const user = userEvent.setup()

    await view.findByRole('heading', { name: zhCN.team.members.heading, level: 2 })
    await user.click(
      view.getByRole('button', { name: zhCN.team.invite.action }),
    )
    const address = view.getByLabelText(zhCN.team.invite.emailLabel)
    await user.type(address, INVITEE_EMAIL)
    await user.click(
      view.getByRole('button', { name: zhCN.team.invite.send }),
    )

    const alert = await view.findByRole('alert')
    expect(alert).toHaveTextContent(zhCN.team.invite.errors.rateLimited)
    expect(alert).not.toHaveTextContent(zhCN.team.invite.errors.unknown)
    // The address survived the refusal, ready to resubmit.
    expect(address).toHaveValue(INVITEE_EMAIL)
    expect(invitePosts(rig)).toHaveLength(1)
  })

  it("an address the client's own rules refuse never reaches the network", async () => {
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderTeam(rig)
    const user = userEvent.setup()

    await view.findByRole('heading', { name: zhCN.team.members.heading, level: 2 })
    await user.click(
      view.getByRole('button', { name: zhCN.team.invite.action }),
    )
    // Neither an empty form nor a string that cannot be an address is
    // submitted -- the form's own rules answer first, so no request
    // goes out that the server would only refuse.
    await user.click(
      view.getByRole('button', { name: zhCN.team.invite.send }),
    )
    expect(
      await view.findByText(zhCN.team.invite.emailRequired),
    ).toBeInTheDocument()
    expect(invitePosts(rig)).toHaveLength(0)

    await user.type(
      view.getByLabelText(zhCN.team.invite.emailLabel),
      'not-an-address',
    )
    await user.click(
      view.getByRole('button', { name: zhCN.team.invite.send }),
    )
    expect(
      await view.findByText(zhCN.team.invite.emailInvalid),
    ).toBeInTheDocument()
    expect(invitePosts(rig)).toHaveLength(0)
  })

  it('a refused read denies the gate: the no-permission empty state, no roster and no invite control', async () => {
    const rig = makeRealClientRig(demoServer({ denyTeamRead: true }))
    await signInWithPassword(rig)
    const view = renderTeam(rig)

    expect(
      await view.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(
      view.getByText(uiKitZhCN.emptyState.noPermission.description),
    ).toBeInTheDocument()
    expect(
      view.queryByRole('button', { name: zhCN.team.invite.action }),
    ).not.toBeInTheDocument()
    expect(
      view.queryByRole('button', { name: zhCN.team.invite.send }),
    ).not.toBeInTheDocument()
    expect(view.queryByRole('heading', { name: zhCN.team.members.heading, level: 2 })).not.toBeInTheDocument()
  })

  it('a 5xx read failure renders the read-error state, never the no-permission gate', async () => {
    const server = demoServer()
    const rig = makeRealClientRig((call) => {
      if (
        call.method === 'GET' &&
        call.path === TEAM_MEMBERS_PATH
      ) {
        return errorResponse(500, 'org.internal_error')
      }
      return server(call)
    })
    await signInWithPassword(rig)
    const view = renderTeam(rig)

    expect(
      await view.findByText(uiKitZhCN.emptyState.error.title),
    ).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.noPermission.title),
    ).not.toBeInTheDocument()
    expect(
      view.queryByRole('button', { name: zhCN.team.invite.action }),
    ).not.toBeInTheDocument()
  })

  it('a codeless read failure renders the read-error state, never a permanent pending', async () => {
    // The failure arrives with no code at all -- a raw transport throw
    // nothing normalized. The error state itself is the failure:
    // classifying by a code would find none and park the gate at
    // pending forever (no answer, no error branch).
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderTeam(rig, UNNORMALIZED_TRANSPORT_FAILURE)

    expect(
      await view.findByText(uiKitZhCN.emptyState.error.title),
    ).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.noPermission.title),
    ).not.toBeInTheDocument()
    expect(view.queryByRole('heading', { name: zhCN.team.members.heading, level: 2 })).not.toBeInTheDocument()
  })

  it('a resolved invitation leaves the pending list: the address pairing never outlives the server row', async () => {
    // The address the roster shows is paired with the SERVER's own
    // row, never a client-side ledger that outlives it: once the list
    // stops carrying the invitation (its acceptance resolved it -- the
    // journey scripts the half the browser cannot, exactly as the e2e
    // tier's org-invitation gate does), the row leaves the surface and
    // the address with it. A resolved invitation can never linger
    // under a fabricated pending label, which is what would send an
    // owner re-inviting a colleague who has already joined.
    let resolved = false
    const server = demoServer()
    const rig = makeRealClientRig(async (call) => {
      if (
        call.method === 'GET' &&
        call.path === '/api/v1/org/invitations' &&
        resolved
      ) {
        return jsonResponse(200, { invitations: [] })
      }
      return server(call)
    })
    await signInWithPassword(rig)
    const view = renderTeam(rig)
    const user = userEvent.setup()

    await view.findByRole('heading', { name: zhCN.team.members.heading, level: 2 })
    await user.click(
      view.getByRole('button', { name: zhCN.team.invite.action }),
    )
    await user.type(view.getByLabelText(zhCN.team.invite.emailLabel), INVITEE_EMAIL)
    await user.click(
      view.getByRole('button', { name: zhCN.team.invite.send }),
    )
    expect(await view.findByText(INVITEE_EMAIL)).toBeInTheDocument()

    // The invitee accepts; the next read of the same query (never a
    // fresh mount, so the refetch is exactly what a real settlement
    // would still hold) serves no pending row.
    resolved = true
    await act(async () => {
      await view.queryClient.refetchQueries()
    })

    expect(
      await view.findByText(zhCN.team.invitations.emptyTitle),
    ).toBeInTheDocument()
    expect(view.queryByText(INVITEE_EMAIL)).not.toBeInTheDocument()
  })
})
