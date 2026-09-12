/**
 * credits-view.tsx -- one clinic's credit balance and the recent window
 * of its append-only credit ledger, read through the generated billing
 * operations over tenant-namespaced query keys -- the same read
 * discipline as the notes and cases surfaces.
 *
 * The balance the view renders is the tenant's own, from the same
 * CreditService.Balance read the billing module's service code uses
 * (the fragment's own header): the available number is what a clinic
 * can spend right now, the reserved line what in-flight generations
 * have set aside. The ledger rows below are the only account of every
 * movement, newest first: a grant row is a top-up (the demo's
 * boot-time "demo:seed" grant), a deduct row is one generation's
 * two-phase reservation -- "pending" while its job runs, "confirmed"
 * once the spend is permanent, and "refunded" once a failed job's
 * reservation was released back to available. A failed generation is
 * therefore observable here as its deduct row's refunded state and the
 * balance restored -- never a row that silently vanishes. The reason
 * a row's record carries is never displayed: go/billing's contract
 * shapes it as a machine-readable annotation (ASCII-alphanumeric
 * segments joined by ':', '_' or '-', never free text -- the very
 * value the module copies into audit Changes rows), so it holds no
 * meaning a translated row label does not already carry. The meta
 * line under each label is the row's date alone.
 *
 * The gate shape mirrors notes-view.tsx's exactly: the read error is
 * classified first, because the error state is where every failure of
 * these reads lands, whatever it carried. Only the rbac read gate's
 * own refusal (rbac.permission_denied -- the demo route guard answers
 * a caller without billing:credit:read with it) is an authorization
 * fact and maps to 'denied'; every other failed read -- a coded
 * transport answer or server 5xx, or a codeless refusal -- is a load
 * failure and renders the ui-kit error empty state in its own suit,
 * never the no-permission one. Both queries fail together under every
 * failure mode that exists (the gate refuses the whole mount, the
 * handler 500s per route), so the surface gates on the two reads as
 * one: allowed only once both have data and no error stands.
 *
 * Times render through Intl in the surface language (never
 * hand-formatted); an unparseable value renders as an empty cell
 * rather than reaching Intl and throwing (use-date-formatter.ts).
 */

import { useMemo } from 'react'
import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Typography from '@mui/material/Typography'
import type { BillingCreditTransaction } from '@speed/api-sdk'
import {
  getBillingGetCreditBalanceQueryKey,
  getBillingListCreditTransactionsQueryKey,
  useBillingGetCreditBalance,
  useBillingListCreditTransactions,
} from '@speed/api-sdk'
import { useTranslation } from '@speed/i18n'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { gatedRead } from '../gated-read.js'
import { GatedContent } from '../gated-read.js'
import { useTenantQueryKey } from '../tenant-query-key.js'
import { useDateFormatter } from '../use-date-formatter.js'
import { CurrentClinicLine } from './current-clinic.js'

/**
 * The reachable codes of the credits surface's two reads, each mapped
 * to the app-namespace key carrying its current-language text. The
 * server answers of the billing fragment's two GETs (go/billing's own
 * error index): billing.internal_error, the handler-level envelope for
 * an unexpected failure, plus the demo route guard's rbac refusal
 * above. billing.invalid_limit and billing.invalid_request are
 * deliberately absent: this surface never sends a limit (the server's
 * 1-100 default window is the recent rows a credit view needs), so
 * neither refusal is reachable here. An answer that is not on the list
 * (a future server code, a client.http.<status> transport answer)
 * resolves to the unknown fallback, so the surface never shows a raw
 * key or another language's text -- the identical discipline the notes
 * and smile-simulation whitelists follow. Exported for the surface's
 * whitelist to be deep-imported by the codes-alignment suite. */
export const CREDITS_ERROR_TEXT_KEYS: Readonly<Record<string, string>> = {
  'rbac.permission_denied': 'credits.errors.permissionDenied',
  'billing.internal_error': 'credits.errors.internalError',
  'client.network': 'credits.errors.client',
  'client.timeout': 'credits.errors.client',
  'client.protocol': 'credits.errors.client',
}

/** The ledger vocabulary, type and status together, to the bundle key
 * describing one row (the rows' meanings come from the billing
 * fragment's own wire vocabulary: grant and expire rows are
 * single-phase and always confirmed, a deduct row's status is the
 * reservation lifecycle itself). A combination outside the map is
 * served under the unknown key rather than crashing or rendering a raw
 * type word. */
const ROW_TEXT_KEY: Readonly<Record<string, string>> = {
  'grant.confirmed': 'credits.rows.grant',
  'expire.confirmed': 'credits.rows.expired',
  'deduct.pending': 'credits.rows.deductPending',
  'deduct.confirmed': 'credits.rows.deductConfirmed',
  'deduct.refunded': 'credits.rows.deductRefunded',
}

/** The signed amount a row's movement displays: grants add to the
 * balance, confirmed deducts and expires take from it. A refunded
 * deduct row keeps its original reservation amount and its row text
 * says "refunded" -- the reversal is the status, never a second row,
 * and showing nothing would hide what the reservation had been. */
function signedAmount(
  formatNumber: (value: number) => string,
  row: BillingCreditTransaction,
): string {
  const sign = row.type === 'grant' ? '+' : '-'
  return `${sign}${formatNumber(row.amount)}`
}

/** The credit surface: the balance over the two reads, gated like the
 * notes list. */
export function CreditsView(): ReactElement {
  const { t, i18n } = useTranslation(REFERENCE_APP_NAMESPACE)

  const formatNumber = useMemo(() => {
    const formatter = new Intl.NumberFormat(i18n.language)
    return (value: number): string => formatter.format(value)
  }, [i18n.language])
  const formatDate = useDateFormatter(i18n.language)

  // The tenant-namespaced keys over the generated bare keys, the same
  // shape every surface of this host uses. A null tenant disables both
  // queries and the gate stays pending, failing closed rather than
  // inventing a tenant.
  const { tenantId, queryKey: balanceKey } = useTenantQueryKey(
    getBillingGetCreditBalanceQueryKey(),
  )
  const { queryKey: transactionsKey } = useTenantQueryKey(
    getBillingListCreditTransactionsQueryKey(),
  )
  const balanceQuery = useBillingGetCreditBalance({
    query: { queryKey: balanceKey, enabled: tenantId !== null },
  })
  const transactionsQuery = useBillingListCreditTransactions(undefined, {
    query: {
      queryKey: transactionsKey,
      enabled: tenantId !== null,
    },
  })

  // The gate stands on both reads: the balance leg's failure decides a
  // refusal, and the surface is 'allowed' only once both have answered
  // (gated-read.tsx's error-first ordering and why it must be so).
  const gate = gatedRead(balanceQuery, transactionsQuery)

  const balance = balanceQuery.data
  const rows = transactionsQuery.data?.transactions ?? []
  const updatedLine = useMemo(
    () =>
      balance?.updatedAt === undefined
        ? ''
        : t('credits.updatedLine', { date: formatDate(balance.updatedAt) }),
    [balance?.updatedAt, formatDate, t],
  )

  return (
    <Box sx={{ p: { xs: 2, sm: 3 }, maxWidth: 720 }}>
      <Typography component="h1" variant="h4" sx={{ fontWeight: 600 }}>
        {t('credits.heading')}
      </Typography>
      <CurrentClinicLine />
      <Typography
        variant="body1"
        color="text.secondary"
        sx={{ marginTop: 1, marginBottom: 3 }}
      >
        {t('credits.intro')}
      </Typography>
      <GatedContent gate={gate}>
        <Typography component="h2" variant="h6">
          {t('credits.balance')}
        </Typography>
        <Typography variant="h3" sx={{ fontWeight: 600 }}>
          {t('credits.availableLine', {
            count: balance?.available ?? 0,
            value: formatNumber(balance?.available ?? 0),
          })}
        </Typography>
        {(balance?.reserved ?? 0) > 0 && (
          <Typography variant="body2" color="text.secondary">
            {t('credits.reservedLine', {
              count: balance?.reserved ?? 0,
              value: formatNumber(balance?.reserved ?? 0),
            })}
          </Typography>
        )}
        {updatedLine !== '' && (
          <Typography variant="caption" color="text.secondary">
            {updatedLine}
          </Typography>
        )}

        <Typography
          component="h2"
          variant="h6"
          sx={{ marginTop: 3, marginBottom: 1 }}
        >
          {t('credits.activityHeading')}
        </Typography>
        {rows.length === 0 ? (
          <Typography variant="body2" color="text.secondary">
            {t('credits.empty')}
          </Typography>
        ) : (
          <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
            {rows.map((row) => (
              <Box
                component="li"
                key={`${row.id}-${row.createdAt}`}
                sx={{
                  display: 'flex',
                  justifyContent: 'space-between',
                  gap: 2,
                  alignItems: 'baseline',
                  paddingY: 1,
                  borderBottom: '1px solid',
                  borderColor: 'divider',
                }}
              >
                <Box sx={{ minWidth: 0 }}>
                  <Typography variant="body2">
                    {t(ROW_TEXT_KEY[`${row.type}.${row.status}`] ??
                      'credits.rows.unknown')}
                  </Typography>
                  {/* The meta line is the row's date alone. The
                   * ledger's reason field never renders: go/billing's
                   * contract makes it a machine annotation (the value
                   * audit Changes rows copy verbatim), and the
                   * translated label above is what the row means to
                   * the reader -- a token beside it would say the
                   * same thing in machine text. */}
                  <Typography variant="caption" color="text.secondary">
                    {formatDate(row.createdAt)}
                  </Typography>
                </Box>
                <Typography variant="body2" sx={{ whiteSpace: 'nowrap' }}>
                  {signedAmount(formatNumber, row)}
                </Typography>
              </Box>
            ))}
          </Box>
        )}
      </GatedContent>
    </Box>
  )
}
