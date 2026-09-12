/**
 * admin-usage-view.tsx -- the platform's usage/billing dashboard, the
 * second surface of this app's administration area (offered by the
 * platform frame's nav next to the tenant ledger): every tenant in
 * go/admin's ledger, one section each, read through the generated
 * @speed/api-sdk hook for the module's own operator-facing route (GET
 * /api/v1/admin/usage-summary, mounted behind the same admin route
 * guard as the ledger, internal/app/demo/demo_admin.go's
 * guardAdminRoute, which evaluates every admin:* permission in
 * rbac.SystemDomain).
 *
 * WHAT ONE SECTION SHOWS
 *
 * A tenant's section renders the dimensions the dashboard answer
 * carries: the recorded metering summaries (a feature's summed
 * quantity over one calendar period -- the feature keys this app's
 * flows record are ai-gateway's usage dimensions, named through the
 * bundle below), the credit balance (the available spend and, when
 * something is reserved, the reserved line), and the subscription
 * state (the status word and the subscribed-since date -- or the
 * no-subscription line when the answer carries none). Numbers and
 * dates render through Intl in the surface language, never
 * hand-formatted. The composed app wires both metering and billing, so
 * a served row always carries meteringSummaries and creditBalance; a
 * tenant with no recorded usage yet shows the no-usage line rather
 * than an empty gap, and one with no active subscription the
 * no-subscription line.
 *
 * WHO REACHES IT, AND HOW THE GATE WORKS
 *
 * The frame offers this entry only to a signed-in principal whose
 * tenant claim is the system pseudo-tenant -- the platform-staff frame
 * (app.tsx, demo-tenants.ts's SYSTEM_PSEUDO_TENANT_ID). A clinic owner
 * is never offered the entry at all; a caller who reaches this
 * fragment anyway -- a direct #/admin/usage visit, a stale bookmark --
 * meets the surface's own gate, which is the dashboard READ itself,
 * the ledger view's shape: the read failure is classified error-first,
 * only the rbac gate's own 403 code (rbac.permission_denied, the
 * answer this app's admin route guard gives a caller whose real user
 * id holds no admin grant under the system domain) is an authorization
 * fact and maps to 'denied', and every other failed read renders the
 * load-failure suit, never the no-permission one.
 *
 * NAMING THE TENANT SECTIONS
 *
 * Each section's heading renders the best name the composition can
 * honestly answer -- and never the tenant id: the same naming ladder
 * the ledger rows use. The ledger's auto-registered rows carry an
 * empty displayName by go/admin design, so a section rendered as
 * stored would show `tenant-64307885-...` -- the raw-identifier defect
 * the ledger view closes. The ladder is therefore: the display name an
 * operator recorded on the ledger row itself; else, for a tenant this
 * app's demo roster knows, the roster copy's name (demo-tenants.ts
 * names the two boot-configured demo tenants, the same copy the frame
 * chrome and the clinic line render); else the surface's "unnamed
 * tenant" fallback label. What the app cannot name stays unnamed. The
 * subscription's plan id gets the same treatment for the same reason:
 * it is an opaque billing row id the dashboard answer does not enrich
 * with the plan's recorded name, so it is a row key, never rendered
 * copy -- the section names the subscription's state and its
 * subscribed-since date, which is what the answer can honestly say.
 *
 * The status word renders through the bundle for the vocabulary the
 * read answers (active -- SubscriptionService.Active reads only rows
 * in that state) and as its raw value for anything a future server
 * adds, the ledger view's treatment of unknown tokens.
 */

import { useMemo } from 'react'
import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Typography from '@mui/material/Typography'
import { useAdminGetUsageSummary } from '@speed/api-sdk'
import type { AdminSubscription } from '@speed/api-sdk'
import { useTranslation } from '@speed/i18n'
import { demoTenantNameKey } from '../demo-tenants.js'
import { gatedRead } from '../gated-read.js'
import { GatedContent } from '../gated-read.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { useTenantQueryKey } from '../tenant-query-key.js'
import { useDateFormatter } from '../use-date-formatter.js'

/** A metering summary's bundle key by the feature key the composed app
 * records (go/ai-gateway's usage dimensions). A feature outside the
 * vocabulary (a future addition) renders as its raw value rather than
 * an untranslated guess. */
const USAGE_FEATURE_TEXT_KEYS: Readonly<Record<string, string>> = {
  'ai.chat_tokens': 'admin.usage.features.chatTokens',
  'ai.image_count': 'admin.usage.features.imageCount',
  'ai.image_steps': 'admin.usage.features.imageSteps',
}

/** A subscription status's bundle key by the server's vocabulary value.
 * Statuses outside the vocabulary (a future addition) render as their
 * raw value. */
const SUBSCRIPTION_STATUS_TEXT_KEYS: Readonly<Record<string, string>> = {
  active: 'admin.usage.subscriptionStatusActive',
}

/** The best name one tenant section can render: the display name an
 * operator recorded on the ledger row, else the demo roster's copy for
 * a tenant this app knows, else the fallback label -- never the raw
 * tenant id (see the file header's naming section). */
function tenantSectionName(
  t: (key: string, options?: Record<string, unknown>) => string,
  row: { readonly displayName: string; readonly tenantId: string },
): string {
  if (row.displayName.trim() !== '') {
    return row.displayName
  }
  const nameKey = demoTenantNameKey(row.tenantId)
  return nameKey === null ? t('admin.tenants.identityUnknown') : t(nameKey)
}

/**
 * The usage/billing dashboard: heading and intro, then the gated
 * content -- one section per tenant in the platform's ledger.
 */
export function AdminUsageView(): ReactElement {
  const { t, i18n } = useTranslation(REFERENCE_APP_NAMESPACE)
  // The dashboard read under a tenant-namespaced query key, the ledger
  // view's shape: the rows are platform data, but they were fetched
  // under THIS principal's access token, and the app's eviction
  // discipline keys every cached row it may have to drop on a tenant
  // switch or a session end under ['tenant', tenantId, ...] -- so the
  // platform read shares the prefix, exactly like the ledger row does.
  const { tenantId, queryKey: summaryKey } = useTenantQueryKey([
    'admin',
    'usage',
  ])
  const summaryQuery = useAdminGetUsageSummary({
    query: { queryKey: summaryKey, enabled: tenantId !== null },
  })

  // The gate is the dashboard read itself (gated-read.tsx's
  // error-first classification and why it must be so).
  const gate = gatedRead(summaryQuery)

  const formatNumber = useMemo(() => {
    const formatter = new Intl.NumberFormat(i18n.language)
    return (value: number): string => formatter.format(value)
  }, [i18n.language])
  const formatDate = useDateFormatter(i18n.language)

  const balanceLine = (key: string, amount: number): string =>
    t(key, { count: amount, value: formatNumber(amount) })
  const subscriptionLine = (subscription: AdminSubscription): string => {
    const statusKey = SUBSCRIPTION_STATUS_TEXT_KEYS[subscription.status]
    const statusText =
      statusKey === undefined ? subscription.status : t(statusKey)
    const date = formatDate(subscription.createdAt)
    return date === ''
      ? t('admin.usage.subscriptionStatusOnly', { status: statusText })
      : t('admin.usage.subscriptionLine', { status: statusText, date })
  }
  const periodCaption = (from: string, to: string): string => {
    const start = formatDate(from)
    const end = formatDate(to)
    return start === '' || end === '' ? '' : `${start} – ${end}`
  }

  const rows = summaryQuery.data?.rows ?? []

  return (
    <Box sx={{ p: { xs: 2, sm: 3 }, maxWidth: 720 }}>
      <Typography component="h1" variant="h4" sx={{ fontWeight: 600 }}>
        {t('admin.usage.heading')}
      </Typography>
      <Typography
        variant="body1"
        color="text.secondary"
        sx={{ marginTop: 1, marginBottom: 3 }}
      >
        {t('admin.usage.intro')}
      </Typography>
      <GatedContent gate={gate}>
        {rows.length === 0 ? (
          <Box>
            <Typography component="h2" variant="h6" sx={{ fontWeight: 600 }}>
              {t('admin.usage.emptyTitle')}
            </Typography>
            <Typography variant="body2" color="text.secondary">
              {t('admin.usage.emptyDescription')}
            </Typography>
          </Box>
        ) : (
          <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
            {rows.map((row) => (
              <Box
                component="li"
                key={row.tenantId}
                sx={{
                  paddingY: 1.5,
                  borderBottom: '1px solid',
                  borderColor: 'divider',
                  ':last-of-type': { borderBottom: 'none' },
                }}
              >
                <Typography
                  component="h2"
                  variant="h6"
                  sx={{ fontWeight: 600 }}
                >
                  {tenantSectionName(t, row)}
                </Typography>
                {row.meteringSummaries !== undefined &&
                  (row.meteringSummaries.length === 0 ? (
                    <Typography variant="body2" color="text.secondary">
                      {t('admin.usage.noUsage')}
                    </Typography>
                  ) : (
                    <Box
                      component="ul"
                      sx={{ margin: 0, padding: 0, listStyle: 'none' }}
                    >
                      {row.meteringSummaries.map((summary) => {
                        const featureKey =
                          USAGE_FEATURE_TEXT_KEYS[summary.feature]
                        const caption = periodCaption(
                          summary.periodStart,
                          summary.periodEnd,
                        )
                        return (
                          <Box
                            component="li"
                            key={`${summary.feature}-${summary.periodStart}`}
                            sx={{
                              display: 'flex',
                              justifyContent: 'space-between',
                              gap: 2,
                              alignItems: 'baseline',
                              paddingY: 0.5,
                            }}
                          >
                            <Box sx={{ minWidth: 0 }}>
                              <Typography variant="body2">
                                {featureKey === undefined
                                  ? summary.feature
                                  : t(featureKey)}
                              </Typography>
                              {caption !== '' && (
                                <Typography
                                  variant="caption"
                                  color="text.secondary"
                                >
                                  {caption}
                                </Typography>
                              )}
                            </Box>
                            <Typography
                              variant="body2"
                              sx={{ whiteSpace: 'nowrap' }}
                            >
                              {formatNumber(summary.quantity)}
                            </Typography>
                          </Box>
                        )
                      })}
                    </Box>
                  ))}
                {row.creditBalance !== undefined && (
                  <Box sx={{ marginTop: 0.5 }}>
                    <Typography variant="body1" sx={{ fontWeight: 600 }}>
                      {balanceLine(
                        'admin.usage.availableLine',
                        row.creditBalance.available,
                      )}
                    </Typography>
                    {row.creditBalance.reserved > 0 && (
                      <Typography variant="body2" color="text.secondary">
                        {balanceLine(
                          'admin.usage.reservedLine',
                          row.creditBalance.reserved,
                        )}
                      </Typography>
                    )}
                  </Box>
                )}
                <Typography
                  variant="body2"
                  color="text.secondary"
                  sx={{ marginTop: 0.5 }}
                >
                  {row.activeSubscription !== undefined
                    ? subscriptionLine(row.activeSubscription)
                    : t('admin.usage.subscriptionNone')}
                </Typography>
              </Box>
            ))}
          </Box>
        )}
      </GatedContent>
    </Box>
  )
}
