/**
 * CreditsView contract: the credits surface renders the balance and
 * the ledger the billing reads serve, gated like every other surface
 * of this host -- the two GETs are the real reads (the demo server's
 * billingDeny switch answers the rbac gate's 403 the way the real
 * server does, so the denied branch is driven by a genuine refusal,
 * never stubbed locally), and every failure of a read lands in the
 * error empty state, never the no-permission one.
 *
 * The consumption and refund journeys are driven through the real
 * client bound into the runtime seam exactly as a host composes it:
 * the journeys sign in through the real session operation, run a
 * simulation through the generated smilesim operations (the same
 * reserve/confirm/refund journey the block-B surface drives, only
 * here the job's terminal outcome is observed directly through the
 * job-status operation), and then mount the view against the SAME
 * responder -- whose billing ledger settled the job the way the real
 * server's settleCredit would: a succeeded generation reads back as
 * the balance reduced by its confirmed deduct row (regression (a) of
 * the block-D round), a dead_letter generation as its deduct row's
 * refunded state with the balance restored (regression (b)) -- so a
 * consumption and a refund that happened before the view opened are
 * what the view renders, never scripted rows hand-placed under it.
 *
 * Built-in strings are asserted through the bundles they render from
 * -- the app's own zh-CN/en-US fixtures and the ui-kit fixture
 * (relative imports, the notes-view precedent) -- never inline: the
 * CJK scan treats test files as English text like everything else.
 * Dates render through the same Intl formatting the view uses.
 */

import { waitFor } from '@testing-library/react'
import { smilesimGetJob, smilesimSimulate } from '@speed/api-sdk'
import type {
  BillingCreditTransaction,
  SmilesimJobRef,
  SmilesimSimulationOptionsSmileStyle,
  SmilesimSimulationOptionsToothShade,
} from '@speed/api-sdk'
import { describe, expect, it } from 'vitest'
import uiKitZhCN from '../../../../../web/packages/ui-kit/src/locales/zh-CN.json' with { type: 'json' }
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import {
  DEMO_CREDIT_SEED_REASON,
  DEMO_SIMULATION_CREDIT_REASON,
  demoServer,
} from '../test-utils/demo-server.js'
import type { RealClientRig } from '../test-utils/real-client.js'
import {
  errorResponse,
  makeRealClientRig,
  signInWithPassword,
} from '../test-utils/real-client.js'
import type { RenderWithProvidersOptions } from '../test-utils/render.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { CreditsView } from './credits-view.js'

/** The fixed demo epoch the demo server's ledger rows carry. */
const DEMO_CREATED_AT = '2026-09-04T00:00:00Z'

/** A deduct row in its confirmed state: one generation's permanent
 * spend, the shape the ledger serves after a job succeeded. */
const CONFIRMED_DEDUCT: BillingCreditTransaction = {
  id: 'sim-1',
  type: 'deduct',
  status: 'confirmed',
  amount: 10,
  reason: 'smilesim:simulate',
  createdAt: DEMO_CREATED_AT,
}

/** A grant row: the demo's boot-time seed, the ledger's other usual
 * occupant. */
const SEED_GRANT: BillingCreditTransaction = {
  id: 'demo-seed',
  type: 'grant',
  status: 'confirmed',
  amount: 1000,
  reason: 'demo:seed',
  createdAt: DEMO_CREATED_AT,
}

/** The balance of a tenant that has consumed one confirmed
 * generation. */
const AFTER_CONSUMPTION_BALANCE = {
  available: 990,
  reserved: 0,
  updatedAt: DEMO_CREATED_AT,
}

/** Renders the credits surface over a signed-in rig (the surface reads
 * the current tenant from the auth-core hooks). */
function renderCredits(
  rig: RealClientRig,
  options: RenderWithProvidersOptions = {},
) {
  return renderWithAppServices(
    <CreditsView />,
    { session: rig.session, api: rig.api },
    options,
  )
}

function balanceGets(rig: RealClientRig): number {
  return rig.calls.filter(
    (call) => call.method === 'GET' && call.path === '/api/v1/billing/credits/balance',
  ).length
}

function transactionGets(rig: RealClientRig): number {
  return rig.calls.filter(
    (call) =>
      call.method === 'GET' &&
      call.path === '/api/v1/billing/credits/transactions',
  ).length
}

/** Runs one generation to its terminal outcome through the generated
 * operations over the bound client: the simulate answer's job id, then
 * the job-status polls the deterministic responder needs to reach its
 * terminal state (pending -> running -> terminal). The fixture settles
 * the billing ledger on the same terminal outcome the way the real
 * server's settleCredit does. */
async function runSimulationToTerminal(): Promise<void> {
  const jobRef: SmilesimJobRef = await smilesimSimulate({
    photo_object_id: 'photo-1',
    options: {
      smile_style: 'natural' as SmilesimSimulationOptionsSmileStyle,
      tooth_shade: 'natural' as SmilesimSimulationOptionsToothShade,
      strength: 1,
    },
  })
  // Poll to terminal: the demo responder's deterministic progression
  // reaches its outcome on the second job-status read.
  await smilesimGetJob(jobRef.job_id)
  await smilesimGetJob(jobRef.job_id)
}

/** The created-at cell text the view's own Intl formatting produces. */
function createdAtText(language: string, value: string): string {
  return new Intl.DateTimeFormat(language, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(value))
}

describe('CreditsView', () => {
  it('renders the clinic the balance belongs to under the heading', async () => {
    // The balance is the current clinic's own: the surface must name
    // the clinic it is showing, the same acceptance shape as the notes
    // surface (current-clinic-is-visible) -- a balance that does not
    // say whose it is cannot tell a tenant switch from a loss.
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const view = renderCredits(rig)
    await view.findByRole('heading', { level: 1 })
    expect(
      await view.findByText(
        zhCN.clinic.currentClinic.replace('{{name}}', zhCN.tenants.acme),
      ),
    ).toBeInTheDocument()
  })

  it('renders the balance and the ledger rows after a real simulation consumption (regression a)', async () => {
    // The view renders the balance and the transactions from the real
    // composed reads AFTER a real consumption -- the generated simulate
    // and job-status calls above reserved and settled one generation
    // against the same responder the view then reads, so the confirmed
    // deduct row and the reduced balance are the ledger's own answer,
    // not a scripted state.
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    await runSimulationToTerminal()
    const view = renderCredits(rig)
    const heading = await view.findByRole('heading', { level: 1 })
    expect(heading).toHaveTextContent(zhCN.credits.heading)

    // The available number, the figure the fragment names as what the
    // view renders: 1000 seeded minus the 10 the succeeded generation
    // permanently spent.
    expect(
      await view.findByText(
        zhCN.credits.availableLine_other.replace('{{value}}', '990'),
      ),
    ).toBeInTheDocument()
    // The ledger rows, newest first: the generation's confirmed deduct
    // above the seed grant, its amount signed negative. The meta line
    // is the row's date; the reason the row carries must not render --
    // go/billing's machine annotation (the same value audit Changes
    // rows copy verbatim) holds no meaning the translated label above
    // does not already carry, so the row shows label, date and amount,
    // never the token.
    const rows = view.getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent(zhCN.credits.rows.deductConfirmed)
    expect(rows[0]).toHaveTextContent('-10')
    expect(rows[0]).toHaveTextContent(createdAtText('zh-CN', DEMO_CREATED_AT))
    expect(rows[0]).not.toHaveTextContent(DEMO_SIMULATION_CREDIT_REASON)
    expect(rows[1]).toHaveTextContent(zhCN.credits.rows.grant)
    expect(rows[1]).toHaveTextContent('+1,000')
    expect(rows[1]).not.toHaveTextContent(DEMO_CREDIT_SEED_REASON)

    // Both reads travelled the composed stack exactly once each.
    await waitFor(() => expect(balanceGets(rig)).toBe(1))
    await waitFor(() => expect(transactionGets(rig)).toBe(1))
  })

  it('renders a failed generation as its deduct row refunded, balance restored (regression b)', async () => {
    // The FAKE_IMAGE_FAIL shape: the demo responder's simulateJobOutcome
    // option scripts the job the way a provider refusal ends it --
    // dead_letter after the reservation opened. The view must show the
    // refund: the reservation's row at status refunded (never gone,
    // never a silent balance-only change) and the balance back at the
    // full seed -- the property the billing fragment exists to serve.
    const rig = makeRealClientRig(demoServer({ simulateJobOutcome: 'dead_letter' }))
    await signInWithPassword(rig)
    await runSimulationToTerminal()
    const view = renderCredits(rig)

    // The failed generation's row reads refunded, its amount still the
    // reservation's own (the reversal is the status, never a second
    // row), newest first above the seed grant.
    await view.findByText(zhCN.credits.rows.deductRefunded)
    const rows = view.getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent(zhCN.credits.rows.deductRefunded)
    expect(rows[0]).toHaveTextContent('-10')
    // The balance is restored: nothing was permanently spent.
    expect(
      await view.findByText(
        zhCN.credits.availableLine_other.replace('{{value}}', '1,000'),
      ),
    ).toBeInTheDocument()
    // No reserved line: the released reservation left the reserved
    // bucket empty.
    expect(
      view.queryByText(zhCN.credits.reservedLine_other, { exact: false }),
    ).not.toBeInTheDocument()
  })

  it('renders a pending reservation as in-progress while reserved, then settled after confirmation', async () => {
    // A generation still running shows its deduct row pending -- the
    // reservation half of the two-phase shape -- and the balance's
    // reserved bucket carries its amount, held out of available the
    // way the real server's reservation holds it (the demo mirror's
    // own ledger regressions pin the same numbers).
    const rig = makeRealClientRig(demoServer())
    await signInWithPassword(rig)
    const jobRef = await smilesimSimulate({
      photo_object_id: 'photo-1',
      options: {
        smile_style: 'natural' as SmilesimSimulationOptionsSmileStyle,
        tooth_shade: 'natural' as SmilesimSimulationOptionsToothShade,
        strength: 1,
      },
    })
    const pendingView = renderCredits(rig)
    expect(
      await pendingView.findByText(zhCN.credits.rows.deductPending),
    ).toBeInTheDocument()
    expect(
      await pendingView.findByText(
        zhCN.credits.reservedLine_other.replace('{{value}}', '10'),
      ),
    ).toBeInTheDocument()
    expect(
      pendingView.queryByText(zhCN.credits.rows.deductConfirmed),
    ).not.toBeInTheDocument()
    // The pending view leaves the document before the second mount --
    // unmounting keeps the later queries from matching its DOM (the
    // reload the settled mount stands in for replaced the whole page).
    pendingView.unmount()

    // The job runs to success: a later read of the same responder
    // settles the same row in place -- confirmed, reserved bucket
    // empty -- exactly what a reload after the job's completion shows.
    await smilesimGetJob(jobRef.job_id)
    await smilesimGetJob(jobRef.job_id)
    const settledView = renderCredits(rig)
    expect(
      await settledView.findByText(zhCN.credits.rows.deductConfirmed),
    ).toBeInTheDocument()
    expect(
      settledView.queryByText(zhCN.credits.rows.deductPending),
    ).not.toBeInTheDocument()
    expect(
      settledView.queryByText(zhCN.credits.reservedLine_other, {
        exact: false,
      }),
    ).not.toBeInTheDocument()
  })

  it('gates on the reads: the rbac refusal renders the no-permission state, never the data (a caller without billing:credit:read)', async () => {
    // The demo route guard's genuine 403 (billingDeny), the same shape
    // the real server gives a caller without billing:credit:read (the
    // block-D read of the demo subject's reader-shaped caller). A
    // refused read means no surface: no balance, no rows.
    const rig = makeRealClientRig(demoServer({ billingDeny: true }))
    await signInWithPassword(rig)
    const view = renderCredits(rig)
    await view.findByText(uiKitZhCN.emptyState.noPermission.title)
    expect(
      view.getByText(uiKitZhCN.emptyState.noPermission.description),
    ).toBeInTheDocument()
    expect(view.queryByText(zhCN.credits.balance)).not.toBeInTheDocument()
  })

  it('renders a failed read (server error) as the load-failure state, never the no-permission one', async () => {
    // A 500 with the billing envelope is not an authorization fact: a
    // down or failing server renders the error empty state in its own
    // suit, never the no-permission one -- a user told they are
    // forbidden while the server is failing reads like a
    // misconfiguration to the operator who must fix it. The 500 is the
    // real handler's own envelope answer (billing.internal_error),
    // scripted by wrapping the demo responder for the billing paths
    // only -- the sign-in leg above them still travels the demo
    // server, exactly as the journey against a failing module would.
    const server = demoServer({
      creditHistory: {
        balance: AFTER_CONSUMPTION_BALANCE,
        transactions: [CONFIRMED_DEDUCT, SEED_GRANT],
      },
    })
    const rig = makeRealClientRig(async (call) => {
      if (call.path.startsWith('/api/v1/billing/')) {
        return errorResponse(500, 'billing.internal_error')
      }
      return server(call)
    })
    await signInWithPassword(rig)
    const view = renderCredits(rig)
    await view.findByText(uiKitZhCN.emptyState.error.title)
    expect(
      view.getByText(uiKitZhCN.emptyState.error.description),
    ).toBeInTheDocument()
    expect(
      view.queryByText(uiKitZhCN.emptyState.noPermission.title),
    ).not.toBeInTheDocument()
  })

  it('renders bilingual copy, switching language live', async () => {
    // The surface renders every built-in string from the bundle of the
    // active language, never a hardcoded text: render under the en-US
    // instance and switch to zh-CN, asserting the balance line and row
    // vocabulary change language with it.
    const rig = makeRealClientRig(
      demoServer({
        creditHistory: {
          balance: AFTER_CONSUMPTION_BALANCE,
          transactions: [CONFIRMED_DEDUCT, SEED_GRANT],
        },
      }),
    )
    await signInWithPassword(rig)
    const view = renderCredits(rig, { language: 'en-US' })
    expect(
      await view.findByText(
        enUS.credits.availableLine_other.replace('{{value}}', '990'),
      ),
    ).toBeInTheDocument()
    expect(await view.findByText(enUS.credits.rows.deductConfirmed)).toBeInTheDocument()
    await view.i18n.changeLanguage('zh-CN')
    expect(
      await view.findByText(
        zhCN.credits.availableLine_other.replace('{{value}}', '990'),
      ),
    ).toBeInTheDocument()
    expect(await view.findByText(zhCN.credits.rows.deductConfirmed)).toBeInTheDocument()
  })
})
