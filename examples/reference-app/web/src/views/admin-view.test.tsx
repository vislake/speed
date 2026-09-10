/**
 * AdminView contract: the administration surface gates on the ledger
 * read it drives and renders what the server answers -- the platform's
 * tenant ledger from go/admin's own operator-facing route (admin-api.ts
 * -- GET /api/v1/admin/tenants, mounted behind this app's admin route
 * guard, internal/app/demo/demo_admin.go). The demo server serves the ledger
 * only to the platform-staff shape (a principal scoped to the system
 * pseudo-tenant, SYSTEM_PSEUDO_TENANT_ID) and answers the rbac gate's
 * 403 to every other principal, the way the real guard answers a
 * caller whose user id holds no admin grant under the system domain --
 * so the gate's denied branch is driven by a genuine refusal, never
 * stubbed locally, and a clinic-shaped session can never read the
 * platform ledger through this surface.
 *
 * NAMING IS THE TRAP THIS SURFACE EXISTS TO CLEAR, PINNED HERE AT THE
 * UNIT TIER
 *
 * The ledger's auto-registered rows carry an empty displayName by
 * go/admin design, so a row rendered as stored would show a raw
 * tenant id -- the raw-identifier shape the team roster's naming
 * ladder exists to prevent. The rows therefore render the naming
 * ladder the view's own doc comment records: a row an operator
 * recorded a display name on is named by it, a tenant this app's demo
 * roster knows is named by the roster copy, and a row with no name
 * source renders the bundle's fallback label -- and the raw tenant id
 * is never what a row renders as a name.
 *
 * The gate's error classification earns the same three checks the
 * notes and team surfaces' own suites run: a refused read falls the
 * gate shut to the no-permission suit; a 5xx read failure renders the
 * read-error suit, never the no-permission one; and a codeless
 * failure renders the read-error suit too, never a permanent pending.
 *
 * Built-in strings are asserted through the bundles they render from
 * -- the app's own zh-CN fixture and the ui-kit fixture (relative
 * imports, the team-view precedent) -- never inline: the CJK scan
 * treats test files as English text like everything else. Served
 * tenant ids and recorded stamps are server data, not copy, and
 * travel in the journeys verbatim.
 */

import type { RequestFn } from '@speed/api-client'
import { describe, expect, it } from 'vitest'
import uiKitZhCN from '../../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { ADMIN_TENANTS_PATH } from '../admin-api.js'
import { SYSTEM_PSEUDO_TENANT_ID } from '../demo-tenants.js'
import type { DemoAdminTenant } from '../test-utils/demo-server.js'
import { demoServer } from '../test-utils/demo-server.js'
import type { RealClientRig } from '../test-utils/real-client.js'
import {
  errorResponse,
  makeRealClientRig,
  signInWithPassword,
} from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { AdminView } from './admin-view.js'

/** A transport whose every call rejects with a raw, code-less error --
 * the shape a bug-shaped transport throw arrives in (the team suite's
 * own copy of the double). */
const UNNORMALIZED_TRANSPORT_FAILURE: RequestFn = () =>
  Promise.reject(new Error('[admin-view.test] an un-normalized transport failure'))

/** The recorded-at stamp every served ledger row carries -- the same
 * fixed demo epoch the fixture's other answers use. */
const LEDGER_CREATED_AT = '2026-09-04T00:00:00Z'

/** The ledger as the real server answers it for the two demo tenants:
 * the auto-registered rows (org root creation lazily registers the
 * row with a blank display name -- go/admin/tenant_service.go). */
const DEMO_TENANT_ROWS: readonly DemoAdminTenant[] = [
  {
    tenantId: 'tenant-acme',
    displayName: '',
    status: 'active',
    createdAt: LEDGER_CREATED_AT,
  },
  {
    tenantId: 'tenant-globex',
    displayName: '',
    status: 'suspended',
    createdAt: LEDGER_CREATED_AT,
  },
]

/** A tenant id of the ledger's own shape -- full raw identifiers of
 * the kind the gate's naming rule forbids a rendered row to carry. */
const RAW_LEDGER_TENANT_ID = 'tenant-64307885-8a11-4b23-9c45-6d7e8f90a1b2'

/** Renders the administration surface over a signed-in rig (the
 * surface reads the current tenant from the auth-core hooks), with an
 * optional api override for the failure-shape journeys that must
 * drive the read through something other than the rig's own client. */
function renderAdmin(
  rig: RealClientRig,
  apiOverride?: RequestFn,
): ReturnType<typeof renderWithAppServices> {
  return renderWithAppServices(
    <AdminView />,
    { session: rig.session, api: apiOverride ?? rig.api },
  )
}

describe('AdminView', () => {
  it('renders the platform\'s tenant ledger from the served rows, each named like a person would read it', async () => {
    // The staff-shaped rig: the configured account signed into the
    // system pseudo-tenant (the demo-server's mirror of the platform
    // staff account, whose admin:* grants live under
    // rbac.SystemDomain), served the two demo rows plus one row an
    // operator recorded a name on.
    const rig = makeRealClientRig(
      demoServer({
        tenantId: SYSTEM_PSEUDO_TENANT_ID,
        initialAdminTenants: [
          ...DEMO_TENANT_ROWS,
          {
            tenantId: 'tenant-7f9e4d2b',
            displayName: 'Mayfair Dental',
            status: 'active',
            createdAt: LEDGER_CREATED_AT,
          },
        ],
      }),
    )
    await signInWithPassword(rig)
    const view = renderAdmin(rig)

    await view.findByRole('heading', { name: zhCN.admin.heading, level: 1 })
    // The ledger rows: the demo tenants the ledger auto-registered
    // (blank display name) are named by the app's demo roster copy --
    // never by their raw tenant ids -- and the operator-named row is
    // named by the recorded display name.
    await view.findByText(zhCN.tenants.acme)
    expect(view.getByText(zhCN.tenants.globex)).toBeInTheDocument()
    expect(view.getByText('Mayfair Dental')).toBeInTheDocument()
    // Statuses render through the bundle: the suspended row is named
    // by its status text, not by the raw status token (the two active
    // rows each render the active text, so the query is plural).
    expect(view.getByText(zhCN.admin.tenants.statusSuspended)).toBeInTheDocument()
    expect(
      view.getAllByText(zhCN.admin.tenants.statusActive).length,
    ).toBeGreaterThan(0)
    expect(view.queryByText('suspended')).not.toBeInTheDocument()
    // The read was one request to the module's own route.
    const ledgerReads = rig.calls.filter(
      (call) => call.method === 'GET' && call.path === ADMIN_TENANTS_PATH,
    )
    expect(ledgerReads).toHaveLength(1)
  })

  it('a row with no name source renders the fallback label; the raw tenant id never reaches the page', async () => {
    // The gate's trap row: a ledger row whose tenant this app has no
    // name for -- the mirror of a self-service clinic the ledger
    // auto-registered after the app's demo roster was written. The
    // identity position renders the bundle's fallback label; the raw
    // tenant id must not surface anywhere on the page.
    const rig = makeRealClientRig(
      demoServer({
        tenantId: SYSTEM_PSEUDO_TENANT_ID,
        initialAdminTenants: [
          { tenantId: RAW_LEDGER_TENANT_ID, displayName: '', status: 'active', createdAt: LEDGER_CREATED_AT },
        ],
      }),
    )
    await signInWithPassword(rig)
    const view = renderAdmin(rig)

    await view.findByText(zhCN.admin.tenants.identityUnknown)
    const text = view.container.textContent ?? ''
    expect(text).not.toContain(RAW_LEDGER_TENANT_ID)
    expect(text).not.toMatch(
      /tenant-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i,
    )
  })

  it('an empty ledger renders its empty state', async () => {
    const rig = makeRealClientRig(
      demoServer({ tenantId: SYSTEM_PSEUDO_TENANT_ID }),
    )
    await signInWithPassword(rig)
    const view = renderAdmin(rig)

    expect(
      await view.findByText(zhCN.admin.tenants.emptyTitle),
    ).toBeInTheDocument()
    expect(
      view.getByText(zhCN.admin.tenants.emptyDescription),
    ).toBeInTheDocument()
  })

  it('a refused read denies the gate: the no-permission empty state, no ledger', async () => {
    // A clinic-shaped session (the sign-in's default tenant is a
    // customer one) renders the surface only when a caller reaches the
    // fragment directly -- the nav never offers it -- and the read is
    // answered with the admin route guard's genuine 403, the same
    // answer the real server gives a caller without the admin grant.
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderAdmin(rig)

    expect(
      await view.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(
      view.getByText(uiKitZhCN.emptyState.noPermission.description),
    ).toBeInTheDocument()
    expect(
      view.queryByText(zhCN.admin.tenants.emptyTitle),
    ).not.toBeInTheDocument()
    expect(view.queryByText(zhCN.tenants.acme)).not.toBeInTheDocument()
  })

  it('a 5xx read failure renders the read-error state, never the no-permission gate', async () => {
    // The staff-shaped read failing with a coded 5xx -- a scripted
    // load failure, the shape the surface renders its error state
    // for. A down server is not a permission problem, so the
    // no-permission suit must not appear.
    const server = demoServer({ tenantId: SYSTEM_PSEUDO_TENANT_ID })
    const rig = makeRealClientRig((call) => {
      if (call.method === 'GET' && call.path === ADMIN_TENANTS_PATH) {
        return errorResponse(500, 'admin.usage_modules_not_wired')
      }
      return server(call)
    })
    await signInWithPassword(rig)
    const view = renderAdmin(rig)

    expect(
      await view.findByText(uiKitZhCN.emptyState.error.title),
    ).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.noPermission.title),
    ).not.toBeInTheDocument()
    expect(
      view.queryByText(zhCN.admin.tenants.emptyTitle),
    ).not.toBeInTheDocument()
  })

  it('a codeless read failure renders the read-error state, never a permanent pending', async () => {
    // The failure arrives with no code at all -- a raw transport throw
    // nothing normalized. The error state itself is the failure:
    // classifying by a code would find none and park the gate at
    // pending forever (no answer, no error branch).
    const rig = makeRealClientRig(
      demoServer({ tenantId: SYSTEM_PSEUDO_TENANT_ID }),
    )
    await signInWithPassword(rig)
    const view = renderAdmin(rig, UNNORMALIZED_TRANSPORT_FAILURE)

    expect(
      await view.findByText(uiKitZhCN.emptyState.error.title),
    ).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.noPermission.title),
    ).not.toBeInTheDocument()
    expect(view.queryByText(zhCN.admin.heading)).toBeInTheDocument()
  })
})
