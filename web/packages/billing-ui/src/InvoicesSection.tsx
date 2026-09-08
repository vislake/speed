/**
 * InvoicesSection: the billing-documents surface of a speed host's
 * billing page.
 *
 * Renders the caller's tenant's invoices, newest page of the server's
 * answer first, read through the generated list hook with the page size
 * frozen at 50 -- within the spec's 1..100 limit, sized to render the
 * recent documents without scrolling machinery, and deliberately not
 * paginated: the billing read surface serves one newest-first window,
 * and a keyset-paginated read over a whole history is not part of the
 * spec (go/billing/AGENTS.md records the deferral). Each row shows the
 * document's status through the lifecycle's typed status vocabulary --
 * open (awaiting payment), paid (settled in full), void (canceled
 * before payment) -- as a chip, the billing cycle it covers and the
 * amount in its own currency, and the issue date. Times, periods and
 * amounts render through Intl in the surface's current language, never
 * hand-formatted.
 *
 * Rows are expandable: expanding a row mounts the row's detail, a
 * region that re-reads the single document through billing_getInvoice
 * and renders the full answer as a document -- the status, amount,
 * both cycle bounds, issue and last-update times (the latter only when
 * the row was touched after issue), and the subscription and invoice
 * ids. The list row stays the list query's snapshot; the detail is the
 * get's own fresh answer, so a status settled after the list was
 * fetched shows its current value in the document. The region's fetch
 * fails (an id that no longer names a tenant invoice answers
 * billing.invoice_not_found) with the code-level banner and a retry;
 * the invoice id fallback in the region's own label and the row's
 * expand button keeps every control nameable while the document loads.
 *
 * The section takes no props: whose invoices these are comes from the
 * caller's bound client and its access token, never from a prop (the
 * billing operations carry no tenant concept -- the tenant travels in
 * the token). An unresolved load -- the first load in flight, or
 * parked by react-query's default networkMode 'online' while the
 * device is offline -- keeps the loading branch, header included. Only
 * a settled query leaves it: an answer listing zero invoices hides the
 * header and renders the ui-kit EmptyState empty variant, while a load
 * that failed -- and a successful answer that omits the invoices key,
 * as unreadable as an error and never grounds for a "no invoices"
 * claim -- render the error variant with a retry button. In every
 * settled state the EmptyState title stands in for the hidden h2
 * header at its own level, so the heading order never skips a level
 * and the no-invoices text is only ever asserted for an answer that
 * genuinely listed none.
 */

import { useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Chip from '@mui/material/Chip'
import CircularProgress from '@mui/material/CircularProgress'
import IconButton from '@mui/material/IconButton'
import Skeleton from '@mui/material/Skeleton'
import Typography from '@mui/material/Typography'
import {
  useBillingGetInvoice,
  useBillingListInvoices,
} from '@speed/api-sdk'
import type { BillingInvoice, BillingInvoiceStatus } from '@speed/api-sdk'
import { EmptyState } from '@speed/ui-kit'
import { createInvoiceFormatters } from './internal/invoice-format.js'
import { errorCodeOf, InlineError } from './internal/inline-error.js'
import { useBillingUiTranslation } from './internal/translation.js'

/**
 * The frozen page size of this surface: one window of the server's
 * newest-first invoice listing, within the spec's 1..100 limit, without
 * pagination (the billing page shows the recent documents; the read
 * surface serves no keyset cursor).
 */
const INVOICE_LIST_LIMIT = 50

/** The status vocabulary meta of the three lifecycle states: each maps
 * to its bundle label and chip look. Keyed by the spec's closed status
 * vocabulary, so the exhaustive Record is a compile-time assertion that
 * every status the type names has a presentation; a status value that
 * is none of the three (a future lifecycle state the answer already
 * carries) has no meta and renders no chip, never a raw value. */
const STATUS_META: Record<
  BillingInvoiceStatus,
  {
    readonly labelKey:
      | 'invoices.status.open'
      | 'invoices.status.paid'
      | 'invoices.status.void'
    readonly color?: 'primary' | 'success'
    readonly muted?: true
  }
> = {
  open: { labelKey: 'invoices.status.open', color: 'primary' },
  paid: { labelKey: 'invoices.status.paid', color: 'success' },
  void: { labelKey: 'invoices.status.void', muted: true },
}

/** The status chip of one lifecycle state. Color never carries the
 * meaning alone: the chip's label is the status's own text. */
function StatusChip({ status }: { readonly status: BillingInvoiceStatus }) {
  const { t } = useBillingUiTranslation()
  const meta = STATUS_META[status]
  if (meta === undefined) {
    return null
  }
  return (
    <Chip
      size="small"
      variant="outlined"
      color={meta.color}
      label={t(meta.labelKey)}
      sx={meta.muted === true ? { color: 'text.disabled' } : undefined}
    />
  )
}

/** The row-end expansion glyph, hand-drawn on the packages' 24-grid
 * pattern: a chevron pointing down when the row is collapsed, up when
 * the detail is open (the open state rotates the same path 180deg),
 * stroke currentColor, no fill. */
function ExpandChevronIcon({ size = 20 }: { readonly size?: number }) {
  return (
    <svg
      aria-hidden="true"
      focusable="false"
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.7}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="m6 9 6 6 6-6" />
    </svg>
  )
}

/** The pending-state placeholder, read aloud as one loading
 * announcement. */
function InvoiceListSkeleton({ label }: { readonly label: string }) {
  const widths = [
    { primary: '18%', secondary: '22%' },
    { primary: '14%', secondary: '26%' },
    { primary: '22%', secondary: '18%' },
  ]
  return (
    <Box role="status" aria-label={label} aria-busy="true">
      {widths.map((width, index) => (
        <Box
          key={String(index)}
          sx={{
            py: 1.5,
            ...(index > 0
              ? { borderTop: '1px solid', borderColor: 'divider' }
              : {}),
          }}
        >
          <Box
            sx={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'space-between',
            }}
          >
            <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5 }}>
              <Skeleton variant="rounded" width={64} height={24} />
              <Skeleton variant="text" width={width.primary} />
            </Box>
            <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5 }}>
              <Skeleton variant="text" width={width.secondary} />
              <Skeleton variant="circular" width={20} height={20} />
            </Box>
          </Box>
          <Skeleton variant="text" width="30%" />
        </Box>
      ))}
    </Box>
  )
}

/** One invoice as a document: the get query's own fresh answer,
 * rendered field by field. Mounted only while its row is expanded. */
function InvoiceDetail({ invoiceId }: { readonly invoiceId: string }) {
  const { t, i18n } = useBillingUiTranslation()
  const fmt = useMemo(
    () => createInvoiceFormatters(i18n.language),
    [i18n.language],
  )
  const { data, error, isPending, refetch } = useBillingGetInvoice(invoiceId)
  const failureCode =
    data === undefined && !isPending ? errorCodeOf(error) : null

  if (isPending && data === undefined) {
    return (
      <Box
        role="status"
        aria-busy="true"
        sx={{ display: 'flex', alignItems: 'center', gap: 1.5, py: 0.5 }}
      >
        <CircularProgress size={16} thickness={5} aria-hidden="true" />
        <Typography variant="body2" color="text.secondary">
          {t('invoices.detail.loading')}
        </Typography>
      </Box>
    )
  }
  if (data === undefined) {
    return (
      <Box>
        <InlineError code={failureCode} />
        <Button
          size="small"
          sx={{ mt: 1 }}
          onClick={() => void refetch()}
        >
          {t('invoices.retry')}
        </Button>
      </Box>
    )
  }

  const issued = fmt.date(data.createdAt)
  const periodRange = fmt.periodRange(data.periodStart, data.periodEnd)
  const updated =
    data.updatedAt !== data.createdAt ? fmt.date(data.updatedAt) : null
  const statusMeta = STATUS_META[data.status]

  return (
    <Box component="dl" sx={{ m: 0 }}>
      {statusMeta !== undefined && (
        <DetailTerm label={t('invoices.detail.status')}>
          <StatusChip status={data.status} />
        </DetailTerm>
      )}
      <DetailTerm label={t('invoices.detail.amount')}>
        <Typography variant="body2" sx={{ fontWeight: 600 }}>
          {fmt.amount(data.amountCents, data.currency)}
        </Typography>
      </DetailTerm>
      {periodRange !== null && (
        <DetailTerm label={t('invoices.detail.period')}>
          {periodRange}
        </DetailTerm>
      )}
      {issued !== null && (
        <DetailTerm label={t('invoices.detail.issued')}>
          {issued}
        </DetailTerm>
      )}
      {updated !== null && (
        <DetailTerm label={t('invoices.detail.updated')}>
          {updated}
        </DetailTerm>
      )}
      <DetailTerm label={t('invoices.detail.subscriptionId')}>
        {data.subscriptionId}
      </DetailTerm>
      <DetailTerm label={t('invoices.detail.invoiceId')}>
        {data.id}
      </DetailTerm>
    </Box>
  )
}

/** One label/value pair of the invoice document. */
function DetailTerm({
  label,
  children,
}: {
  readonly label: string
  readonly children: ReactNode
}) {
  return (
    <Box
      sx={{
        display: 'flex',
        justifyContent: 'space-between',
        alignItems: 'baseline',
        gap: 2,
        py: 0.5,
      }}
    >
      <Box
        component="dt"
        sx={{ typography: 'body2', color: 'text.secondary', flexShrink: 0 }}
      >
        {label}
      </Box>
      <Box
        component="dd"
        sx={{
          m: 0,
          typography: 'body2',
          minWidth: 0,
          textAlign: 'right',
          overflowWrap: 'anywhere',
        }}
      >
        {children}
      </Box>
    </Box>
  )
}

/** One invoice row of the list: the summary line, and the expandable
 * document region that re-reads the invoice through billing_getInvoice. */
function InvoiceListItem({
  invoice,
  index,
}: {
  readonly invoice: BillingInvoice
  readonly index: number
}) {
  const { t, i18n } = useBillingUiTranslation()
  const fmt = useMemo(
    () => createInvoiceFormatters(i18n.language),
    [i18n.language],
  )
  const [expanded, setExpanded] = useState(false)
  // The region's id is the document's own: one region per invoice, and
  // its name stays stable across expansions.
  const regionId = `invoice-details-${invoice.id}`
  // The period label names the row and its controls; an unparseable
  // period (a malformed answer) falls back to the document's id, which
  // never lies about which invoice the row is.
  const period = fmt.periodLabel(invoice.periodStart, invoice.periodEnd)
  const primaryLabel = period ?? invoice.id
  const issued = fmt.date(invoice.createdAt)

  return (
    <Box
      component="li"
      sx={{
        py: 1.5,
        minWidth: 0,
        ...(index > 0
          ? { borderTop: '1px solid', borderColor: 'divider' }
          : {}),
      }}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5 }}>
        <StatusChip status={invoice.status} />
        <Typography
          variant="body1"
          noWrap
          sx={{ minWidth: 0, fontWeight: 500 }}
        >
          {primaryLabel}
        </Typography>
        <Box
          sx={{
            marginLeft: 'auto',
            flexShrink: 0,
            display: 'flex',
            alignItems: 'center',
            gap: 1,
          }}
        >
          <Typography
            variant="body1"
            noWrap
            sx={{ minWidth: 0, fontWeight: 600 }}
          >
            {fmt.amount(invoice.amountCents, invoice.currency)}
          </Typography>
          <IconButton
            aria-label={t(
              expanded
                ? 'invoices.row.collapseAriaWithPeriod'
                : 'invoices.row.expandAriaWithPeriod',
              {
                period: primaryLabel,
                // The label is an aria-label, not HTML: the period
                // string must reach the accessibility tree verbatim
                // (i18next's default value escaping would embed a
                // literal `&#x2F;` for a date's slashes).
                interpolation: { escapeValue: false },
              },
            )}
            aria-expanded={expanded}
            aria-controls={expanded ? regionId : undefined}
            size="small"
            onClick={() => setExpanded((open) => !open)}
            sx={{ color: 'text.secondary' }}
          >
            <Box
              sx={{
                display: 'flex',
                transition: 'transform 150ms ease',
                transform: expanded ? 'rotate(180deg)' : undefined,
              }}
            >
              <ExpandChevronIcon />
            </Box>
          </IconButton>
        </Box>
      </Box>

      {issued !== null && (
        <Typography variant="body2" color="text.secondary">
          {t('invoices.issuedWithDate', { date: issued })}
        </Typography>
      )}

      {expanded && (
        <Box
          id={regionId}
          role="region"
          aria-label={t('invoices.detail.regionAriaWithPeriod', {
            period: primaryLabel,
            // An aria-label, not HTML: see the expand button above.
            interpolation: { escapeValue: false },
          })}
          sx={{
            mt: 1.5,
            px: 2,
            py: 1.5,
            bgcolor: 'action.hover',
            borderRadius: 1,
          }}
        >
          <InvoiceDetail invoiceId={invoice.id} />
        </Box>
      )}
    </Box>
  )
}

export function InvoicesSection() {
  const { t } = useBillingUiTranslation()
  const { data, isPending, refetch } = useBillingListInvoices({
    limit: INVOICE_LIST_LIMIT,
  })

  const invoices = data?.invoices
  // The answer is unresolved whenever there is no data yet: the first
  // load in flight, or parked by react-query's default networkMode
  // 'online' while the device is offline -- such a fetch sits at
  // fetchStatus 'paused', where isFetching (and isLoading, its
  // isPending-and-isFetching conjunction) is false and isError stays
  // false too, so a pending test derived from isLoading would miss
  // every branch and read an unresolved load as an empty answer.
  // isPending alone tracks "no answer yet" across the in-flight and the
  // parked states.
  const pending = isPending && invoices === undefined
  const hasRows = invoices !== undefined && invoices.length > 0

  return (
    <Box>
      {(pending || hasRows) && (
        <Box sx={{ mb: 2 }}>
          <Typography variant="h5" component="h2">
            {t('invoices.title')}
          </Typography>
        </Box>
      )}

      {pending ? (
        <InvoiceListSkeleton label={t('invoices.loading')} />
      ) : invoices === undefined ? (
        // This guard tests the absent list field with the loading
        // branch already excluded above: pending is false here, so no
        // invoices means the query settled without delivering a list.
        // Two shapes settle that way: a load that failed with no data
        // (isError), and a successful answer whose body omits the
        // invoices key -- a 200 without the list reads as an error, not
        // as "no invoices" (nothing re-arms the loading branch for it:
        // isPending is false and stays false), and the Retry is the
        // exit.
        <EmptyState
          variant="error"
          title={t('invoices.error.title')}
          description={t('invoices.error.description')}
          // The retry is the exit for both shapes this state covers. A
          // refetch of a data-less failed load moves the query back to
          // the pending state, so the section re-enters the loading
          // branch above -- that loading announcement is the retry's
          // progress feedback; a refetch of a settled field-less answer
          // keeps the query's own data, so this state holds until the
          // refetched answer changes it. Either way react-query dedupes
          // the per-query fetches, so a click can never overlap a
          // request already in flight.
          action={
            <Button onClick={() => void refetch()}>
              {t('invoices.retry')}
            </Button>
          }
          // showHeader is false here (see its definition above): this
          // EmptyState's title stands in for the hidden h2 section
          // header, so it must render at that same level or the page's
          // heading order skips straight from h1 to h6.
          headingLevel="h2"
        />
      ) : invoices.length === 0 ? (
        <EmptyState
          variant="empty"
          title={t('invoices.empty.title')}
          description={t('invoices.empty.description')}
          headingLevel="h2"
        />
      ) : (
        // The rows are one real list: a screen-reader user hears each
        // invoice as one item of a numbered set with a boundary between
        // rows, never a flat div stack. role="list" keeps the list
        // semantics under WebKit, which strips them from a
        // list-style-none ul.
        <Box component="ul" role="list" sx={{ m: 0, p: 0, listStyle: 'none' }}>
          {invoices.map((invoice, index) => (
            <InvoiceListItem
              key={invoice.id}
              invoice={invoice}
              index={index}
            />
          ))}
        </Box>
      )}
    </Box>
  )
}
