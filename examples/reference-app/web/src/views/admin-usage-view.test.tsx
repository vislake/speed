/**
 * AdminUsageView contract: the platform's usage/billing dashboard gates
 * on the dashboard read it drives and renders what the server answers
 * -- one section per tenant in go/admin's ledger, from the module's own
 * operator-facing route (admin-api.ts -- GET /api/v1/admin/usage-summary,
 * mounted behind this app's admin route guard like the tenant ledger).
 * The demo server serves the dashboard only to the platform-staff shape
 * (a principal scoped to the system pseudo-tenant,
 * SYSTEM_PSEUDO_TENANT_ID) and answers the rbac gate's 403 to every
 * other principal, the way the real guard answers a caller whose user
 * id holds no admin grant under the system domain -- so the gate's
 * denied branch is driven by a genuine refusal, never stubbed locally,
 * and a clinic-shaped session can never read the platform dashboard
 * through this surface.
 *
 * THE SERVED DATA THE SUITES PIN
 *
 * The fixture mirrors the real composed answer's shape: a row carries
 * the tenant identity (displayName empty for the auto-registered demo
 * tenants), the recorded metering summaries (a feature's summed
 * quantity over one calendar period -- ai.chat_tokens the feature this
 * app's own recording path reports), the credit balance and the active
 * subscription, and a tenant with no recorded usage yet answers an
 * empty-but-present meteringSummaries list. Names render through the
 * naming ladder the ledger rows use -- the demo roster's copy for a
 * tenant this app knows, the fallback label for a row it cannot name,
 * the raw tenant id never reaching the page. The subscription's status
 * word and the balance lines render from the bundles; dates and numbers
 * render through the same Intl formatting the view uses, so a pinned
 * line's expected text is built in this suite with the identical
 * formatter options (the cases suite's precedent).
 *
 * The gate's error classification earns the same checks the ledger
 * surface's own suite runs: a refused read falls the gate shut to the
 * no-permission suit; a 5xx read failure renders the read-error suit,
 * never the no-permission one; and a codeless failure renders the
 * read-error suit too, never a permanent pending.
 *
 * Built-in strings are asserted through the bundles they render from --
 * the app's own zh-CN/en-US fixtures and the ui-kit fixture (relative
 * imports, the ledger suite's precedent) -- never inline: the CJK scan
 * treats test files as English text like everything else. Served
 * tenant ids and recorded stamps are server data, not copy, and travel
 * in the journeys verbatim.
 */

import type { RequestFn } from '@speed/api-client'
import type { AdminUsageSummaryRow } from '@speed/api-sdk'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { describe, expect, it } from 'vitest'
import { act } from '@testing-library/react'
import { switchLanguage } from '@speed/i18n'
import uiKitZhCN from '../../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import { SYSTEM_PSEUDO_TENANT_ID } from '../demo-tenants.js'
import { demoServer } from '../test-utils/demo-server.js'
import type { RealClientRig } from '../test-utils/real-client.js'
import {
  errorResponse,
  makeRealClientRig,
  signInWithPassword,
} from '../test-utils/real-client.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { AdminUsageView } from './admin-usage-view.js'

/** The path of go/admin's usage/billing dashboard the generated hook
 * reads (adminGetUsageSummary's own URL). */
const ADMIN_USAGE_SUMMARY_PATH = '/api/v1/admin/usage-summary'

/** A transport whose every call rejects with a raw, code-less error --
 * the shape a bug-shaped transport throw arrives in (the ledger suite's
 * own copy of the double). */
const UNNORMALIZED_TRANSPORT_FAILURE: RequestFn = () =>
  Promise.reject(new Error('[admin-usage-view.test] an un-normalized transport failure'))

/** The recorded-at stamp every served row's subscription carries -- the
 * same fixed demo epoch the fixture's other answers use. */
const SUBSCRIPTION_CREATED_AT = '2026-09-04T00:00:00Z'

/** The calendar period the served usage summary aggregates -- the
 * metering bucket shape, [start, end). */
const PERIOD_START = '2026-09-01T00:00:00Z'
const PERIOD_END = '2026-10-01T00:00:00Z'

/** A tenant id of the ledger's own shape -- full raw identifiers the
 * naming ladder's trap is written about; the assertion that no rendered
 * text ever carries one is what pins the ladder's fallback. */
const RAW_LEDGER_TENANT_ID = 'tenant-64307885-8a11-4b23-9c45-6d7e8f90a1b2'

/** The dashboard as the real composed app answers it for the two demo
 * tenants (see the file header): tenant-acme holds one recorded usage
 * summary, a balance with something reserved and an active
 * subscription; tenant-globex holds an empty-but-present usage list, a
 * full balance and no subscription. */
const DEMO_USAGE_ROWS: readonly AdminUsageSummaryRow[] = [
  {
    tenantId: 'tenant-acme',
    displayName: '',
    meteringSummaries: [
      {
        feature: 'ai.chat_tokens',
        periodStart: PERIOD_START,
        periodEnd: PERIOD_END,
        quantity: 20,
      },
    ],
    creditBalance: { available: 990, reserved: 10 },
    activeSubscription: {
      id: 'sub-1',
      planId: 'plan-demo',
      status: 'active',
      createdAt: SUBSCRIPTION_CREATED_AT,
    },
  },
  {
    tenantId: 'tenant-globex',
    displayName: '',
    meteringSummaries: [],
    creditBalance: { available: 1000, reserved: 0 },
  },
]

/** The date text the view's own Intl formatting produces for one
 * instant in one language (dateStyle medium + timeStyle short -- the
 * identical options use-date-formatter.ts applies), so a pinned line
 * can be built exactly as the view renders it. */
function formattedDate(language: string, value: string): string {
  return new Intl.DateTimeFormat(language, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(value))
}

/** A bundle template rendered with its placeholders substituted -- the
 * way the view's own t() interpolation renders it, derived here from
 * the fixture rather than spelled out inline (test files are English
 * text like everything else; an inline zh literal would both violate
 * the CJK rule and drift from the resource). */
function renderedText(
  template: string,
  params: Readonly<Record<string, string>>,
): string {
  let text = template
  for (const [name, value] of Object.entries(params)) {
    text = text.replace(`{{${name}}}`, value)
  }
  return text
}

/** The plural-suffix choice i18next makes for a non-one count -- the
 * balance lines' other-form fixture, whose zh value equals its one-form
 * and whose en value is the plural. */
function balanceText(
  otherTemplate: string,
  value: string,
): string {
  return renderedText(otherTemplate, { value })
}

/** Renders the dashboard surface over a signed-in rig (the surface
 * reads the current tenant from the auth-core hooks). An api override
 * also rebinds the api-sdk runtime seam (last bind wins) -- the
 * generated dashboard hook reads the transport through that seam, so a
 * failure-shape journey driving the read through something other than
 * the rig's own client must replace both the context value and the
 * binding. */
function renderUsage(
  rig: RealClientRig,
  apiOverride?: RequestFn,
): ReturnType<typeof renderWithAppServices> {
  bindRequestFn(apiOverride ?? rig.api)
  return renderWithAppServices(
    <AdminUsageView />,
    { session: rig.session, api: apiOverride ?? rig.api },
  )
}

describe('AdminUsageView', () => {
  it('renders every tenant section from the served rows: usage, balance and subscription state, each named like a person would read it', async () => {
    // The staff-shaped rig: the configured account signed into the
    // system pseudo-tenant (the demo-server's mirror of the platform
    // staff account, whose admin:* grants live under rbac.SystemDomain).
    const rig = makeRealClientRig(
      demoServer({
        tenantId: SYSTEM_PSEUDO_TENANT_ID,
        initialUsageSummary: [
          ...DEMO_USAGE_ROWS,
          // The naming ladder's trap row: a tenant this app has no name
          // for -- the mirror of a self-service clinic the ledger
          // auto-registered after the app's demo roster was written.
          {
            tenantId: RAW_LEDGER_TENANT_ID,
            displayName: '',
            meteringSummaries: [],
            creditBalance: { available: 500, reserved: 0 },
          },
        ],
      }),
    )
    await signInWithPassword(rig)
    const view = renderUsage(rig)

    await view.findByRole('heading', { name: zhCN.admin.usage.heading, level: 1 })
    // The tenant sections: the demo tenants (blank display name) are
    // named by the app's demo roster copy -- never by their raw tenant
    // ids -- and the unknown tenant by the fallback label.
    await view.findByRole('heading', { name: zhCN.tenants.acme, level: 2 })
    expect(
      view.getByRole('heading', { name: zhCN.tenants.globex, level: 2 }),
    ).toBeInTheDocument()
    expect(
      view.getByRole('heading', {
        name: zhCN.admin.tenants.identityUnknown,
        level: 2,
      }),
    ).toBeInTheDocument()

    // The recorded usage row renders the feature's bundle name and its
    // summed quantity -- the real metering data of the served row.
    expect(
      view.getByText(zhCN.admin.usage.features.chatTokens),
    ).toBeInTheDocument()
    expect(view.getByText('20')).toBeInTheDocument()

    // The credit balance: the available line, and the reserved line
    // only when something is reserved.
    expect(
      view.getByText(
        balanceText(
          zhCN.admin.usage.availableLine_other,
          '990',
        ),
      ),
    ).toBeInTheDocument()
    expect(
      view.getByText(
        balanceText(zhCN.admin.usage.reservedLine_other, '10'),
      ),
    ).toBeInTheDocument()
    // The four-digit balance renders through Intl's own grouping.
    expect(
      view.getByText(
        balanceText(zhCN.admin.usage.availableLine_other, '1,000'),
      ),
    ).toBeInTheDocument()
    expect(
      view.queryByText(
        balanceText(zhCN.admin.usage.reservedLine_other, '0'),
      ),
    ).not.toBeInTheDocument()

    // The subscription state: tenant-acme's active subscription names
    // its status and subscribed-since date through the bundle --
    // statuses never render as raw tokens -- and tenant-globex's absent
    // subscription renders the no-subscription line.
    expect(
      view.getByText(
        renderedText(zhCN.admin.usage.subscriptionLine, {
          status: zhCN.admin.usage.subscriptionStatusActive,
          date: formattedDate('zh-CN', SUBSCRIPTION_CREATED_AT),
        }),
      ),
    ).toBeInTheDocument()
    // The two tenants without an active subscription (globex and the
    // unknown row) each render the no-subscription line.
    expect(
      view.getAllByText(zhCN.admin.usage.subscriptionNone).length,
    ).toBeGreaterThanOrEqual(2)

    // The tenants with no recorded usage (globex and the unknown row)
    // each show the no-usage line, and no raw tenant id ever reaches
    // the page.
    expect(view.getAllByText(zhCN.admin.usage.noUsage).length).toBe(2)
    const text = view.container.textContent ?? ''
    expect(text).not.toContain(RAW_LEDGER_TENANT_ID)
    expect(text).not.toMatch(
      /tenant-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i,
    )
    // The read was one request to the module's own route.
    const summaryReads = rig.calls.filter(
      (call) =>
        call.method === 'GET' && call.path === ADMIN_USAGE_SUMMARY_PATH,
    )
    expect(summaryReads).toHaveLength(1)
  })

  it('speaks the active language: the same served rows render the en-US feature name, balance and subscription lines after a switch', async () => {
    const rig = makeRealClientRig(
      demoServer({
        tenantId: SYSTEM_PSEUDO_TENANT_ID,
        initialUsageSummary: DEMO_USAGE_ROWS,
      }),
    )
    await signInWithPassword(rig)
    const view = renderUsage(rig)
    await view.findByText(zhCN.admin.usage.features.chatTokens)

    await act(async () => {
      await switchLanguage(view.i18n, 'en-US')
    })

    expect(
      view.getByRole('heading', { name: enUS.admin.usage.heading, level: 1 }),
    ).toBeInTheDocument()
    expect(
      view.getByText(enUS.admin.usage.features.chatTokens),
    ).toBeInTheDocument()
    expect(view.getByText('20')).toBeInTheDocument()
    expect(
      view.getByText(
        balanceText(enUS.admin.usage.availableLine_other, '990'),
      ),
    ).toBeInTheDocument()
    expect(
      view.getByText(
        balanceText(enUS.admin.usage.reservedLine_other, '10'),
      ),
    ).toBeInTheDocument()
    expect(
      view.getByText(
        renderedText(enUS.admin.usage.subscriptionLine, {
          status: enUS.admin.usage.subscriptionStatusActive,
          date: formattedDate('en-US', SUBSCRIPTION_CREATED_AT),
        }),
      ),
    ).toBeInTheDocument()
  })

  it('an empty dashboard answer renders its empty state', async () => {
    const rig = makeRealClientRig(
      demoServer({ tenantId: SYSTEM_PSEUDO_TENANT_ID }),
    )
    await signInWithPassword(rig)
    const view = renderUsage(rig)

    expect(
      await view.findByText(zhCN.admin.usage.emptyTitle),
    ).toBeInTheDocument()
    expect(
      view.getByText(zhCN.admin.usage.emptyDescription),
    ).toBeInTheDocument()
    expect(
      view.queryByRole('heading', { name: zhCN.tenants.acme, level: 2 }),
    ).not.toBeInTheDocument()
  })

  it('a refused read denies the gate: the no-permission empty state, no dashboard content', async () => {
    // A clinic-shaped session (the sign-in's default tenant is a
    // customer one) renders the surface only when a caller reaches the
    // fragment directly -- the nav never offers it -- and the read is
    // answered with the admin route guard's genuine 403, the same
    // answer the real server gives a caller without the admin grant.
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderUsage(rig)

    expect(
      await view.findByText(uiKitZhCN.emptyState.noPermission.title),
    ).toBeInTheDocument()
    expect(
      view.getByText(uiKitZhCN.emptyState.noPermission.description),
    ).toBeInTheDocument()
    expect(view.queryByText(zhCN.admin.usage.emptyTitle)).not.toBeInTheDocument()
    expect(
      view.queryByRole('heading', { name: zhCN.tenants.acme, level: 2 }),
    ).not.toBeInTheDocument()
  })

  it('a 5xx read failure renders the read-error state, never the no-permission gate', async () => {
    // The staff-shaped read failing with a coded 5xx -- a scripted
    // load failure, the shape the surface renders its error state
    // for. A down server is not a permission problem, so the
    // no-permission suit must not appear.
    const server = demoServer({ tenantId: SYSTEM_PSEUDO_TENANT_ID })
    const rig = makeRealClientRig((call) => {
      if (call.method === 'GET' && call.path === ADMIN_USAGE_SUMMARY_PATH) {
        return errorResponse(500, 'admin.internal')
      }
      return server(call)
    })
    await signInWithPassword(rig)
    const view = renderUsage(rig)

    expect(
      await view.findByText(uiKitZhCN.emptyState.error.title),
    ).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.noPermission.title),
    ).not.toBeInTheDocument()
    expect(view.queryByText(zhCN.admin.usage.emptyTitle)).not.toBeInTheDocument()
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
    const view = renderUsage(rig, UNNORMALIZED_TRANSPORT_FAILURE)

    expect(
      await view.findByText(uiKitZhCN.emptyState.error.title),
    ).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.noPermission.title),
    ).not.toBeInTheDocument()
    expect(view.queryByText(zhCN.admin.usage.heading)).toBeInTheDocument()
  })
})
