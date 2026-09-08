/**
 * InvoicesSection behaviour, driven over the real-client rig: the
 * section never sees a mock fetch -- the responder answers genuine
 * Response objects to a real @speed/api-client (bound through the same
 * bindRequestFn seam the generated hooks use) and every invoice answer
 * is the shape the server answers, assertions on what the user sees.
 *
 * The suite pins the surface contract: the list query is one
 * newest-first window (the frozen in-range limit rides as a query
 * parameter) with the caller's bearer token; rows render the status
 * vocabulary's own text per status and the amount through Intl in the
 * surface's language; an unresolved load keeps the loading skeleton,
 * an empty answer renders the empty state with the header hidden, a
 * failed load renders the error state with a retry that refetches.
 * Expanding a row issues the single-document read for that invoice and
 * renders the region from the get's own fresh answer (a settled status
 * shows in the document while the collapsed row keeps the list's
 * snapshot); a refused document read (billing.invoice_not_found, a
 * dead-session authn code) renders the whitelisted code text with a
 * retry, and a code outside the whitelist renders the unknown
 * fallback. Every scenario ends with an axe pass.
 */

import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { BillingInvoice } from '@speed/api-sdk'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import {
  errorResponse,
  jsonResponse,
  makeInvoice,
  makeRealClientRig,
} from '../test-utils/real-client.js'
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import { InvoicesSection } from './InvoicesSection.js'

const LIST_PATH = '/api/v1/billing/invoices'
const ACCESS_TOKEN = 'access-1'

const OPEN_ID = 'a1b2c3d4-0001-4a1b-8c9d-0e1f2a3b4c5d'
const PAID_ID = 'a1b2c3d4-0002-4a1b-8c9d-0e1f2a3b4c5d'
const VOID_ID = 'a1b2c3d4-0003-4a1b-8c9d-0e1f2a3b4c5d'

/** The fixtures of the three lifecycle states, each a full calendar
 * month cycle in the UTC calendar the documents render in. */
function invoiceOf(overrides: Partial<BillingInvoice>): BillingInvoice {
  return makeInvoice(overrides)
}

const JULY_INVOICE = invoiceOf({
  id: OPEN_ID,
  status: 'open',
  amountCents: 12000,
  currency: 'CNY',
  periodStart: '2026-07-01T00:00:00Z',
  periodEnd: '2026-07-31T23:59:59Z',
  createdAt: '2026-07-01T08:00:00Z',
  updatedAt: '2026-07-01T08:00:00Z',
})
const JUNE_INVOICE = invoiceOf({
  id: PAID_ID,
  status: 'paid',
  amountCents: 9800,
  currency: 'USD',
  periodStart: '2026-06-01T00:00:00Z',
  periodEnd: '2026-06-30T23:59:59Z',
  createdAt: '2026-06-01T08:00:00Z',
  updatedAt: '2026-06-03T09:30:00Z',
})
const MAY_INVOICE = invoiceOf({
  id: VOID_ID,
  status: 'void',
  amountCents: 6000,
  currency: 'CNY',
  periodStart: '2026-05-01T00:00:00Z',
  periodEnd: '2026-05-31T23:59:59Z',
  createdAt: '2026-05-01T08:00:00Z',
  updatedAt: '2026-05-01T08:00:00Z',
})

/** The expected label of one invoice's cycle, computed in the same UTC
 * calendar the component renders in. */
function zhMonthLabel(iso: string): string {
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric',
    month: 'long',
    timeZone: 'UTC',
  }).format(new Date(iso))
}

/** The expected medium-date string of one server timestamp, computed in
 * the same UTC calendar the component renders in. */
function zhDateLabel(iso: string): string {
  return new Intl.DateTimeFormat('zh-CN', {
    dateStyle: 'medium',
    timeZone: 'UTC',
  }).format(new Date(iso))
}

/** The expected currency string of one amount, computed in zh-CN. */
function zhMoneyLabel(cents: number, currency: string): string {
  return new Intl.NumberFormat('zh-CN', {
    style: 'currency',
    currency,
  }).format(cents / 100)
}

/** The zh-CN period label of one invoice, as the row renders it. */
function periodOf(invoice: BillingInvoice): string {
  return zhMonthLabel(invoice.periodStart)
}

/** The zh-CN expand/collapse control label of one invoice's row. */
function expandAriaOf(invoice: BillingInvoice): string {
  return zhCN.invoices.row.expandAriaWithPeriod.replace(
    '{{period}}',
    periodOf(invoice),
  )
}

function collapseAriaOf(invoice: BillingInvoice): string {
  return zhCN.invoices.row.collapseAriaWithPeriod.replace(
    '{{period}}',
    periodOf(invoice),
  )
}

/** The zh-CN region name of one invoice's detail region. */
function regionAriaOf(invoice: BillingInvoice): string {
  return zhCN.invoices.detail.regionAriaWithPeriod.replace(
    '{{period}}',
    periodOf(invoice),
  )
}

/** Mounts the section over a rig whose responder answers the list from
 * `invoices` and the single-document read from `getInvoice`. */
async function renderListed(
  invoices: readonly BillingInvoice[],
  getInvoice: (invoice: BillingInvoice) => Response | Promise<Response>,
): Promise<{ calls: ReturnType<typeof makeRealClientRig>['calls'] }> {
  const rig = makeRealClientRig(async (call) => {
    if (call.path === LIST_PATH) {
      return jsonResponse(200, { invoices })
    }
    const invoice = invoices.find((candidate) =>
      call.path.startsWith(`${LIST_PATH}/${candidate.id}`),
    )
    if (invoice !== undefined) {
      return getInvoice(invoice)
    }
    return errorResponse(404, 'billing.invoice_not_found')
  })
  rig.store.set(ACCESS_TOKEN)
  renderWithProviders(<InvoicesSection />)
  // Settled: the first answer arrived and the loading skeleton has
  // unmounted (an empty answer and a failed answer both leave it).
  await waitFor(() => expect(rig.calls.length).toBeGreaterThan(0))
  await waitFor(() =>
    expect(screen.queryByRole('status')).not.toBeInTheDocument(),
  )
  return { calls: rig.calls }
}

describe('InvoicesSection', () => {
  it('render the newest-first answer with the status vocabulary and one expand control per row', async () => {
    const { calls } = await renderListed(
      [JULY_INVOICE, JUNE_INVOICE, MAY_INVOICE],
      () => jsonResponse(200, {}),
    )

    // The rows render the answer's own order -- newest first is the
    // server's ordering key, never re-sorted here.
    const rows = screen.getAllByRole('listitem')
    expect(rows).toHaveLength(3)
    const expectedOrder = [JULY_INVOICE, JUNE_INVOICE, MAY_INVOICE]
    for (const [index, invoice] of expectedOrder.entries()) {
      const row = within(rows[index]!)
      // The status vocabulary: each lifecycle state renders its own
      // text, and the three states read distinctly.
      expect(
        row.getByText(zhCN.invoices.status[invoice.status]),
      ).toBeTruthy()
      expect(row.getByText(periodOf(invoice))).toBeTruthy()
      expect(
        row.getByText(zhMoneyLabel(invoice.amountCents, invoice.currency)),
      ).toBeTruthy()
      // The issued date meta of the answer.
      expect(
        row.getByText(
          zhCN.invoices.issuedWithDate.replace(
            '{{date}}',
            zhDateLabel(invoice.createdAt),
          ),
        ),
      ).toBeTruthy()
      const expand = row.getByRole('button', {
        name: expandAriaOf(invoice),
      })
      expect(expand).toHaveAttribute('aria-expanded', 'false')
    }
    // The rows' amounts tell the three fixtures apart; the CNY and USD
    // currencies format each in their own symbol.
    expect(rows[0]!.textContent).toContain(
      zhMoneyLabel(12000, 'CNY'),
    )
    expect(rows[1]!.textContent).toContain(
      zhMoneyLabel(9800, 'USD'),
    )

    // One list window rode the caller's bearer token with the frozen
    // in-range limit as its query parameter, and no document read has
    // happened: nothing is expanded.
    expect(calls).toEqual([
      {
        method: 'GET',
        path: LIST_PATH,
        query: '?limit=50',
        authorization: `Bearer ${ACCESS_TOKEN}`,
      },
    ])

    await expectNoAxeViolations()
  })

  it('fall back to the invoice id for a row whose period does not parse', async () => {
    const broken = makeInvoice({
      id: OPEN_ID,
      status: 'open',
      periodStart: 'not-a-timestamp',
      periodEnd: '2026-07-31T23:59:59Z',
      createdAt: '2026-07-01T08:00:00Z',
      updatedAt: '2026-07-01T08:00:00Z',
    })
    await renderListed([broken], () => jsonResponse(200, {}))

    // The row names itself by the document id -- the one label that
    // never lies about which invoice the row is.
    expect(screen.getByText(OPEN_ID)).toBeTruthy()
    expect(screen.getByText(zhCN.invoices.status.open)).toBeTruthy()
    await expectNoAxeViolations()
  })

  it('keep the loading skeleton while the first load is in flight', async () => {
    let release: () => void = () => undefined
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    const rig = makeRealClientRig(async (call) => {
      if (call.path === LIST_PATH) {
        await gate
        return jsonResponse(200, { invoices: [JULY_INVOICE] })
      }
      return errorResponse(404, 'billing.invoice_not_found')
    })
    rig.store.set(ACCESS_TOKEN)
    renderWithProviders(<InvoicesSection />)

    // The loading announcement is a role="status" container with the
    // skeleton shapes inside, never a fake heading.
    const status = await screen.findByRole('status')
    expect(status).toHaveAttribute(
      'aria-label',
      zhCN.invoices.loading,
    )
    expect(status).toHaveAttribute('aria-busy', 'true')
    // The header shows while the answer is unresolved, so the section
    // never flashes empty.
    expect(
      screen.getByRole('heading', { name: zhCN.invoices.title }),
    ).toBeTruthy()

    release()
    await waitFor(() =>
      expect(screen.queryByRole('status')).not.toBeInTheDocument(),
    )
    expect(screen.getByText(periodOf(JULY_INVOICE))).toBeTruthy()
    await expectNoAxeViolations()
  })

  it('render the empty state for an answer listing no invoices, header hidden', async () => {
    await renderListed([], () => jsonResponse(200, {}))

    expect(
      screen.queryByRole('heading', { name: zhCN.invoices.title }),
    ).not.toBeInTheDocument()
    expect(screen.getByText(zhCN.invoices.empty.title)).toBeTruthy()
    expect(
      screen.getByText(zhCN.invoices.empty.description),
    ).toBeTruthy()
    await expectNoAxeViolations()
  })

  it('render the error state for a failed load, with a retry that refetches', async () => {
    let attempts = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.path === LIST_PATH) {
        attempts += 1
        if (attempts === 1) {
          return errorResponse(500, 'billing.internal_error')
        }
        return jsonResponse(200, { invoices: [JULY_INVOICE] })
      }
      return errorResponse(404, 'billing.invoice_not_found')
    })
    rig.store.set(ACCESS_TOKEN)
    renderWithProviders(<InvoicesSection />)

    // A failed load renders the error state -- the header hidden, the
    // EmptyState's title standing in at its level -- never a partial
    // list and never the empty state's "no invoices" claim.
    await waitFor(() =>
      expect(screen.getByText(zhCN.invoices.error.title)).toBeTruthy(),
    )
    expect(
      screen.queryByRole('heading', { name: zhCN.invoices.title }),
    ).not.toBeInTheDocument()
    expect(
      screen.queryByText(zhCN.invoices.empty.title),
    ).not.toBeInTheDocument()

    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: zhCN.invoices.retry }))
    await screen.findByText(periodOf(JULY_INVOICE))
    expect(rig.calls).toHaveLength(2)
    expect(rig.calls[1]?.query).toBe('?limit=50')
    await expectNoAxeViolations()
  })

  it('re-read the expanded row as a document and collapse it again', async () => {
    // The document read answers a settled invoice: the list's snapshot
    // still shows open, the get's fresh answer shows paid with a
    // last-update time. The list row keeps its snapshot; the document
    // shows the current answer.
    let release: () => void = () => undefined
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    const settled = makeInvoice({
      id: OPEN_ID,
      status: 'paid',
      amountCents: 12000,
      currency: 'CNY',
      periodStart: '2026-07-01T00:00:00Z',
      periodEnd: '2026-07-31T23:59:59Z',
      createdAt: '2026-07-01T08:00:00Z',
      updatedAt: '2026-07-05T14:00:00Z',
    })
    const rig = makeRealClientRig(async (call) => {
      if (call.path === LIST_PATH) {
        return jsonResponse(200, { invoices: [JULY_INVOICE] })
      }
      if (call.path === `${LIST_PATH}/${OPEN_ID}`) {
        await gate
        return jsonResponse(200, settled)
      }
      return errorResponse(404, 'billing.invoice_not_found')
    })
    rig.store.set(ACCESS_TOKEN)
    renderWithProviders(<InvoicesSection />)
    await screen.findByText(periodOf(JULY_INVOICE))

    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: expandAriaOf(JULY_INVOICE) }),
    )

    // The document read is in flight: the region announces loading
    // while no document content has arrived.
    const region = await screen.findByRole('region', {
      name: regionAriaOf(JULY_INVOICE),
    })
    expect(within(region).getByText(zhCN.invoices.detail.loading)).toBeTruthy()

    release()
    // The document renders the get's own answer: the settled status,
    // the amount, both cycle bounds, the issue and last-update dates,
    // and the two document ids.
    await waitFor(() =>
      expect(
        within(region).queryByText(zhCN.invoices.detail.loading),
      ).not.toBeInTheDocument(),
    )
    const document = within(region)
    expect(document.getByText(zhCN.invoices.status.paid)).toBeTruthy()
    expect(
      document.getByText(zhMoneyLabel(12000, 'CNY')),
    ).toBeTruthy()
    const zhRange = `${zhDateLabel(settled.periodStart)} – ${zhDateLabel(
      settled.periodEnd,
    )}`
    expect(document.getByText(zhRange)).toBeTruthy()
    expect(document.getByText(zhCN.invoices.detail.issued)).toBeTruthy()
    expect(document.getByText(zhDateLabel(settled.createdAt))).toBeTruthy()
    expect(document.getByText(zhCN.invoices.detail.updated)).toBeTruthy()
    expect(document.getByText(zhDateLabel(settled.updatedAt))).toBeTruthy()
    expect(
      document.getByText(settled.subscriptionId),
    ).toBeTruthy()
    expect(document.getByText(settled.id)).toBeTruthy()

    // The collapsed row still carries the list's own snapshot: the same
    // invoice reads open in the row while the document reads paid.
    const row = within(
      screen.getAllByRole('listitem')[0] as HTMLElement,
    )
    expect(row.getByText(zhCN.invoices.status.open)).toBeTruthy()

    // Collapse the row: the region unmounts and the control flips back
    // to its expand label.
    await user.click(
      screen.getByRole('button', { name: collapseAriaOf(JULY_INVOICE) }),
    )
    await waitFor(() =>
      expect(
        screen.queryByRole('region', { name: regionAriaOf(JULY_INVOICE) }),
      ).not.toBeInTheDocument(),
    )
    expect(
      screen.getByRole('button', { name: expandAriaOf(JULY_INVOICE) }),
    ).toBeTruthy()

    // The whole exchange: one list window, one document read, both
    // riding the caller's bearer token.
    expect(
      rig.calls.map((call) => `${call.method} ${call.path}`),
    ).toEqual([`GET ${LIST_PATH}`, `GET ${LIST_PATH}/${OPEN_ID}`])
    expect(rig.calls[1]?.authorization).toBe(`Bearer ${ACCESS_TOKEN}`)
    await expectNoAxeViolations()
  })

  it('render an untouchable invoice document without a last-update row', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.path === LIST_PATH) {
        return jsonResponse(200, { invoices: [JULY_INVOICE] })
      }
      if (call.path === `${LIST_PATH}/${OPEN_ID}`) {
        return jsonResponse(200, JULY_INVOICE)
      }
      return errorResponse(404, 'billing.invoice_not_found')
    })
    rig.store.set(ACCESS_TOKEN)
    renderWithProviders(<InvoicesSection />)
    await screen.findByText(periodOf(JULY_INVOICE))

    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: expandAriaOf(JULY_INVOICE) }),
    )
    const document = within(
      await screen.findByRole('region', {
        name: regionAriaOf(JULY_INVOICE),
      }),
    )
    // updatedAt equals createdAt by fixture: no last-update row.
    expect(document.queryByText(zhCN.invoices.detail.updated)).toBeNull()
    expect(
      document.getByText(zhCN.invoices.detail.invoiceId),
    ).toBeTruthy()
    await expectNoAxeViolations()
  })

  it('render a refused document read as its whitelisted code text, with a retry that refetches', async () => {
    let attempts = 0
    const rig = makeRealClientRig(async (call) => {
      if (call.path === LIST_PATH) {
        return jsonResponse(200, { invoices: [JULY_INVOICE] })
      }
      if (call.path === `${LIST_PATH}/${OPEN_ID}`) {
        attempts += 1
        if (attempts === 1) {
          // An id that names no invoice of the caller's tenant -- the
          // server answers the same code whether the id never existed
          // or belongs to another tenant, so the copy says nothing
          // about either.
          return errorResponse(404, 'billing.invoice_not_found')
        }
        return jsonResponse(200, JULY_INVOICE)
      }
      return errorResponse(404, 'billing.invoice_not_found')
    })
    rig.store.set(ACCESS_TOKEN)
    renderWithProviders(<InvoicesSection />)
    await screen.findByText(periodOf(JULY_INVOICE))

    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: expandAriaOf(JULY_INVOICE) }),
    )
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(zhCN.errors.billing.invoice_not_found)

    const region = screen.getByRole('region', {
      name: regionAriaOf(JULY_INVOICE),
    })
    await user.click(
      within(region).getByRole('button', { name: zhCN.invoices.retry }),
    )
    await waitFor(() =>
      expect(
        within(region).getByText(zhCN.invoices.status.open),
      ).toBeTruthy(),
    )
    await expectNoAxeViolations()
  })

  it('render a dead-session document read as its session-lifecycle code text', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.path === LIST_PATH) {
        return jsonResponse(200, { invoices: [JULY_INVOICE] })
      }
      if (call.path === `${LIST_PATH}/${OPEN_ID}`) {
        // The caller's session died between the list and the document
        // read: the protected read answers with the authn chain's own
        // code, resolved to its whitelisted text.
        return errorResponse(401, 'authn.token_expired')
      }
      return errorResponse(404, 'billing.invoice_not_found')
    })
    rig.store.set(ACCESS_TOKEN)
    renderWithProviders(<InvoicesSection />)
    await screen.findByText(periodOf(JULY_INVOICE))

    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: expandAriaOf(JULY_INVOICE) }),
    )
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(zhCN.errors.authn.token_expired)
    await expectNoAxeViolations()
  })

  it('render the unknown fallback for a document read code outside the whitelist', async () => {
    const rig = makeRealClientRig(async (call) => {
      if (call.path === LIST_PATH) {
        return jsonResponse(200, { invoices: [JULY_INVOICE] })
      }
      if (call.path === `${LIST_PATH}/${OPEN_ID}`) {
        // A 500 internal-error envelope: like every internal-error
        // answer, it is deliberately outside the whitelist.
        return errorResponse(500, 'billing.internal_error')
      }
      return errorResponse(404, 'billing.invoice_not_found')
    })
    rig.store.set(ACCESS_TOKEN)
    renderWithProviders(<InvoicesSection />)
    await screen.findByText(periodOf(JULY_INVOICE))

    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: expandAriaOf(JULY_INVOICE) }),
    )
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(zhCN.errors.unknown)
    expect(alert.textContent).not.toContain('internal_error')
    await expectNoAxeViolations()
  })
})
