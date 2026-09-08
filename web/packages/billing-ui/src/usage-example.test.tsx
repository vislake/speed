/**
 * usage-example.test.tsx -- the README quick start, compiled and run.
 *
 * The quick start wires the billing page for a signed-in viewer: the
 * bilingual i18n instance with both namespaces registered (the
 * billing-ui namespace for this package's strings, the ui-kit
 * namespace because the empty and error states speak
 * ui-kit-namespace keys), the AppThemeProvider tree and a
 * QueryClientProvider (the surface reads through the
 * @tanstack/react-query hooks generated into @speed/api-sdk -- the one
 * provider this package's harness carries beyond the theme tree), and
 * the host's billing page composing InvoicesSection. The access-token
 * store already holds the bearer token the reads ride -- the sign-in
 * that preceded the billing page is the auth-core story, out of this
 * package's scope -- and no refresh seam is bound, so a refused
 * request surfaces its own answer.
 *
 * Why a real client: the wiring the README documents -- the client
 * bound to the api-sdk runtime, the providers, the i18n instance and
 * the section -- is a composition no single-component suite exercises.
 * This file is that journey: it mounts the page and walks it through
 * the surface's story -- the newest-first list rendering the three
 * lifecycle states with the status vocabulary, one row expanded into
 * its document (the single-document read answers an invoice that
 * settled after the list was fetched, so the document shows the
 * current status while the collapsed row keeps the list's snapshot),
 * the row collapsed again, and the same page in the other supported
 * language -- over a real @speed/api-client whose scripted fetch
 * answers genuine Response objects, with the whole exchange pinned in
 * order at the end.
 */

import type { ReactElement } from 'react'
import { describe, expect, it } from 'vitest'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { switchLanguage } from '@speed/i18n'
import type { BillingInvoice } from '@speed/api-sdk'
import {
  jsonResponse,
  makeInvoice,
  makeRealClientRig,
} from '../test-utils/real-client.js'
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import { InvoicesSection } from './InvoicesSection.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

const LIST_PATH = '/api/v1/billing/invoices'
const ACCESS_TOKEN = 'access-1'

const OPEN_ID = 'a1b2c3d4-0001-4a1b-8c9d-0e1f2a3b4c5d'
const PAID_ID = 'a1b2c3d4-0002-4a1b-8c9d-0e1f2a3b4c5d'
const VOID_ID = 'a1b2c3d4-0003-4a1b-8c9d-0e1f2a3b4c5d'

/** The quick start's server story, newest first: the July document is
 * still open in the list, but its single-document read answers a
 * settled invoice -- the story the detail read exists for. */
const JULY_INVOICE = makeInvoice({
  id: OPEN_ID,
  subscriptionId: '7a2b3c4d-5e6f-4a1b-8c9d-0e1f2a3b4c5d',
  status: 'open',
  amountCents: 12000,
  currency: 'CNY',
  periodStart: '2026-07-01T00:00:00Z',
  periodEnd: '2026-07-31T23:59:59Z',
  createdAt: '2026-07-01T08:00:00Z',
  updatedAt: '2026-07-01T08:00:00Z',
})
const SETTLED_JULY_INVOICE: BillingInvoice = {
  ...JULY_INVOICE,
  status: 'paid',
  updatedAt: '2026-07-05T14:00:00Z',
}
const JUNE_INVOICE = makeInvoice({
  id: PAID_ID,
  subscriptionId: '8b3c4d5e-6f7a-4b2c-9d0e-1f2a3b4c5d6e',
  status: 'paid',
  amountCents: 9800,
  currency: 'USD',
  periodStart: '2026-06-01T00:00:00Z',
  periodEnd: '2026-06-30T23:59:59Z',
  createdAt: '2026-06-01T08:00:00Z',
  updatedAt: '2026-06-03T09:30:00Z',
})
const MAY_INVOICE = makeInvoice({
  id: VOID_ID,
  subscriptionId: '9c4d5e6f-7a8b-4c3d-8e0f-2a3b4c5d6e7f',
  status: 'void',
  amountCents: 6000,
  currency: 'CNY',
  periodStart: '2026-05-01T00:00:00Z',
  periodEnd: '2026-05-31T23:59:59Z',
  createdAt: '2026-05-01T08:00:00Z',
  updatedAt: '2026-05-01T08:00:00Z',
})

/** The expected label of one invoice's cycle in one language, computed
 * in the UTC calendar the components render in. */
function monthLabel(language: string, iso: string): string {
  return new Intl.DateTimeFormat(language, {
    year: 'numeric',
    month: 'long',
    timeZone: 'UTC',
  }).format(new Date(iso))
}

/** The expected currency string of one amount in one language. */
function moneyLabel(language: string, cents: number, currency: string): string {
  return new Intl.NumberFormat(language, {
    style: 'currency',
    currency,
  }).format(cents / 100)
}

/** The expected medium-date string of one timestamp in one language. */
function dateLabel(language: string, iso: string): string {
  return new Intl.DateTimeFormat(language, {
    dateStyle: 'medium',
    timeZone: 'UTC',
  }).format(new Date(iso))
}

/** The host's billing page in the quick start: one section, under host
 * headings. The surface reads the caller's tenant from the bound
 * client's access token -- no tenant prop exists anywhere. */
function BillingPage(): ReactElement {
  return (
    <div>
      <InvoicesSection />
    </div>
  )
}

describe('the README quick start, exercised over a real api-client', () => {
  it('walks the billing page from the newest-first list through one invoice document and back, in both languages', async () => {
    // The scripted server. The July document's read answers the
    // settled invoice -- the server-side story the journey pins.
    let releaseDocumentRead: () => void = () => undefined
    const documentReadGate = new Promise<void>((resolve) => {
      releaseDocumentRead = resolve
    })
    const rig = makeRealClientRig(async (call) => {
      switch (call.path) {
        case LIST_PATH:
          return jsonResponse(200, {
            invoices: [JULY_INVOICE, JUNE_INVOICE, MAY_INVOICE],
          })
        case `${LIST_PATH}/${OPEN_ID}`:
          await documentReadGate
          return jsonResponse(200, SETTLED_JULY_INVOICE)
        default:
          return jsonResponse(404, {
            code: 'billing.invoice_not_found',
          })
      }
    })
    // The access-token store already holds the bearer token: the
    // host's sign-in flow ran before this page mounted.
    rig.store.set(ACCESS_TOKEN)
    const { i18n } = renderWithProviders(<BillingPage />)

    // The newest-first list, in the quick start's language: each row
    // names its cycle, its status through the vocabulary and its
    // amount, newest document of the server's answer first.
    await waitFor(() => expect(rig.calls.length).toBe(1))
    const rows = await screen.findAllByRole('listitem')
    expect(rows).toHaveLength(3)
    for (const [index, invoice] of [
      JULY_INVOICE,
      JUNE_INVOICE,
      MAY_INVOICE,
    ].entries()) {
      const row = within(rows[index]!)
      expect(row.getByText(zhCN.invoices.status[invoice.status])).toBeTruthy()
      expect(
        row.getByText(monthLabel('zh-CN', invoice.periodStart)),
      ).toBeTruthy()
      expect(
        row.getByText(
          moneyLabel('zh-CN', invoice.amountCents, invoice.currency),
        ),
      ).toBeTruthy()
    }
    expect(
      screen.getByRole('heading', { name: zhCN.invoices.title }),
    ).toBeTruthy()

    // Open the July document: its single-document read is the journey's
    // one detail leg.
    const zhPeriod = monthLabel('zh-CN', JULY_INVOICE.periodStart)
    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', {
        name: zhCN.invoices.row.expandAriaWithPeriod.replace(
          '{{period}}',
          zhPeriod,
        ),
      }),
    )
    const region = await screen.findByRole('region', {
      name: zhCN.invoices.detail.regionAriaWithPeriod.replace(
        '{{period}}',
        zhPeriod,
      ),
    })
    // The document read is in flight while the region waits on it.
    expect(within(region).getByText(zhCN.invoices.detail.loading)).toBeTruthy()
    releaseDocumentRead()

    // The document renders the read's own answer -- the invoice
    // settled after the list was fetched: the document reads paid,
    // with a last-update row, while the collapsed row above it keeps
    // the list's open snapshot.
    await waitFor(() =>
      expect(
        within(region).queryByText(zhCN.invoices.detail.loading),
      ).not.toBeInTheDocument(),
    )
    expect(within(region).getByText(zhCN.invoices.status.paid)).toBeTruthy()
    expect(
      within(region).getByText(
        moneyLabel('zh-CN', SETTLED_JULY_INVOICE.amountCents, 'CNY'),
      ),
    ).toBeTruthy()
    expect(
      within(region).getByText(
        `${dateLabel('zh-CN', SETTLED_JULY_INVOICE.periodStart)} – ${dateLabel(
          'zh-CN',
          SETTLED_JULY_INVOICE.periodEnd,
        )}`,
      ),
    ).toBeTruthy()
    expect(within(region).getByText(zhCN.invoices.detail.updated)).toBeTruthy()
    expect(
      within(region).getByText(SETTLED_JULY_INVOICE.subscriptionId),
    ).toBeTruthy()
    expect(within(region).getByText(SETTLED_JULY_INVOICE.id)).toBeTruthy()
    const firstRow = within(rows[0] as HTMLElement)
    expect(firstRow.getByText(zhCN.invoices.status.open)).toBeTruthy()

    // Collapse the row: the document unmounts with the region.
    await user.click(
      screen.getByRole('button', {
        name: zhCN.invoices.row.collapseAriaWithPeriod.replace(
          '{{period}}',
          zhPeriod,
        ),
      }),
    )
    await waitFor(() =>
      expect(
        screen.queryByRole('region', {
          name: zhCN.invoices.detail.regionAriaWithPeriod.replace(
            '{{period}}',
            zhPeriod,
          ),
        }),
      ).not.toBeInTheDocument(),
    )

    // The same page in the other supported language: the vocabulary,
    // the cycle labels and the amounts re-render in English.
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    expect(
      screen.getByRole('heading', { name: enUS.invoices.title }),
    ).toBeTruthy()
    const enRows = screen.getAllByRole('listitem')
    for (const [index, invoice] of [
      JULY_INVOICE,
      JUNE_INVOICE,
      MAY_INVOICE,
    ].entries()) {
      const row = within(enRows[index]!)
      expect(row.getByText(enUS.invoices.status[invoice.status])).toBeTruthy()
      expect(
        row.getByText(monthLabel('en-US', invoice.periodStart)),
      ).toBeTruthy()
      expect(
        row.getByText(
          moneyLabel('en-US', invoice.amountCents, invoice.currency),
        ),
      ).toBeTruthy()
    }
    expect(screen.queryByText(zhCN.invoices.status.open)).toBeNull()

    // The whole exchange, in order: the list window, then the one
    // document read, each riding the bearer token the store holds; the
    // language switch re-renders from the cache and adds no request.
    expect(
      rig.calls.map((call) => `${call.method} ${call.path}`),
    ).toEqual([`GET ${LIST_PATH}`, `GET ${LIST_PATH}/${OPEN_ID}`])
    expect(rig.calls[0]?.query).toBe('?limit=50')
    expect(rig.calls[0]?.authorization).toBe(`Bearer ${ACCESS_TOKEN}`)
    expect(rig.calls[1]?.authorization).toBe(`Bearer ${ACCESS_TOKEN}`)

    await expectNoAxeViolations()
  })
})
