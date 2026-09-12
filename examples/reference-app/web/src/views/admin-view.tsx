/**
 * admin-view.tsx -- the administration surface of the reference-app
 * web host: the platform's tenant ledger, the one operator task every
 * other one starts from, read through the generated @speed/api-sdk hook
 * for go/admin's own operator-facing route (GET /api/v1/admin/tenants,
 * mounted in this app behind guardAdminRoute,
 * internal/app/demo/demo_admin.go, which evaluates every admin:*
 * permission in rbac.SystemDomain).
 *
 * WHO REACHES IT, AND HOW THE GATE WORKS
 *
 * The frame offers the Administration entry only to a signed-in
 * principal whose tenant claim is the system pseudo-tenant -- the
 * platform-staff frame (app.tsx, demo-tenants.ts's
 * SYSTEM_PSEUDO_TENANT_ID). A clinic owner is never offered the entry
 * at all; a caller who reaches this fragment anyway -- a direct
 * #/admin visit, a stale bookmark -- meets the surface's own gate,
 * which is the ledger READ itself, the notes/team shape: the read
 * failure is classified error-first, only the rbac gate's own 403
 * code (rbac.permission_denied, the answer this app's admin route
 * guard gives a caller whose real user id holds no admin grant under
 * the system domain) is an authorization fact and maps to 'denied',
 * and every other failed read renders the load-failure suit, never
 * the no-permission one.
 *
 * NAMING THE ROWS
 *
 * Each row's identity position renders the best name the composition
 * can honestly answer -- and never the tenant id. The ledger's
 * auto-registered rows carry an empty displayName by go/admin design
 * (tenant_service.go's lazily created rows record no name), so a row
 * rendered as stored would show a raw tenant id -- the raw-identifier
 * shape a naming ladder exists to prevent, exactly as on the team
 * roster. The naming ladder is therefore: the display name an operator
 * recorded on the
 * row itself (the manual-CRUD rows carry one, and it is the server's
 * own word for the tenant); else, for a tenant this app's demo roster
 * knows, the roster copy's name (demo-tenants.ts names the two
 * boot-configured demo tenants, the same copy the frame chrome and
 * the clinic line render); else the surface's "unnamed tenant"
 * fallback label. What the app cannot name stays unnamed -- a clinic
 * name resolved from org is deliberately NOT attempted here, because
 * org's tree is tenant-scoped and answers only the caller's own
 * tenant: no request a browser can make names another tenant's org
 * root, and go/admin's ledger is the only cross-tenant tenant answer
 * this host mounts. The tenant id stays on the row as its identity
 * key -- the row key and nothing else compares against it -- and
 * never reaches the rendered page.
 *
 * Statuses render through the bundle for the ledger's known
 * vocabulary (active, suspended) and as their raw value for anything
 * a future server adds, the team surface's treatment of unknown
 * tokens.
 */

import { useMemo } from 'react'
import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Typography from '@mui/material/Typography'
import { useAdminListTenants } from '@speed/api-sdk'
import type { AdminTenant } from '@speed/api-sdk'
import { useTranslation } from '@speed/i18n'
import type { RouteGuardStatus } from '@speed/layout-kit'
import { RouteGuard } from '@speed/layout-kit'
import type { DataTableColumn } from '@speed/ui-kit'
import { DataTable, EmptyState } from '@speed/ui-kit'
import { demoTenantNameKey } from '../demo-tenants.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { useTenantQueryKey } from '../tenant-query-key.js'
import { usePreferredTimeZone } from '../use-preferred-time-zone.js'

/** The read gate's own refusal code: the rbac layer's 403, the only
 * ledger-read answer that is an authorization fact (the answer this
 * app's admin route guard gives a caller without the admin grant). */
const ADMIN_READ_DENIED_CODE = 'rbac.permission_denied'

/** A ledger status's bundle key by the server's vocabulary value.
 * Statuses outside the vocabulary (a future addition) render as their
 * raw value rather than an untranslated guess. */
const TENANT_STATUS_TEXT_KEYS: Readonly<Record<string, string>> = {
  active: 'admin.tenants.statusActive',
  suspended: 'admin.tenants.statusSuspended',
}

/** The code of an ApiError-shaped failure, or null for a failure that
 * carries none (a bug-shaped throw, an un-normalized answer). */
function apiErrorCodeOf(error: unknown): string | null {
  if (typeof error !== 'object' || error === null) {
    return null
  }
  const code = (error as { code?: unknown }).code
  return typeof code === 'string' && code.length > 0 ? code : null
}

/**
 * The administration surface: heading and intro, then the gated
 * content -- the platform's tenant ledger.
 */
export function AdminView(): ReactElement {
  const { t, i18n } = useTranslation(REFERENCE_APP_NAMESPACE)

  // The ledger read under a tenant-namespaced query key, the notes and
  // team shape: the row is platform data, but it was fetched under
  // THIS principal's access token, and the app's eviction discipline
  // keys every cached row it may have to drop on a tenant switch or a
  // session end under ['tenant', tenantId, ...] (TENANT_QUERY_PREFIX,
  // the assembly's session-end eviction) -- so the platform ledger
  // shares the prefix, exactly like the clinic-name row does.
  const { tenantId, queryKey: tenantsKey } = useTenantQueryKey(['admin'])
  const tenantsQuery = useAdminListTenants(undefined, {
    query: { queryKey: tenantsKey, enabled: tenantId !== null },
  })

  // The read failure, classified error-first exactly like the team
  // surface's: only the rbac gate's own refusal is an authorization
  // fact; every other failed read is a load failure and renders the
  // error suit, never the no-permission one.
  const listReadFailed = tenantsQuery.isError
  const listErrorCode = listReadFailed ? apiErrorCodeOf(tenantsQuery.error) : null
  const gateDenied = listErrorCode === ADMIN_READ_DENIED_CODE
  const gateStatus: RouteGuardStatus = gateDenied
    ? 'denied'
    : tenantsQuery.data !== undefined
      ? 'allowed'
      : 'pending'

  const timeZone = usePreferredTimeZone()
  const formatDate = useMemo(() => {
    const formatter = new Intl.DateTimeFormat(i18n.language, {
      dateStyle: 'medium',
      timeStyle: 'short',
      timeZone,
    })
    return (value: string | undefined): string => {
      if (value === undefined) {
        return ''
      }
      const date = new Date(value)
      return Number.isNaN(date.getTime()) ? '' : formatter.format(date)
    }
  }, [i18n.language, timeZone])

  const tenantColumns: readonly DataTableColumn<AdminTenant>[] = useMemo(
    () => [
      {
        id: 'tenant',
        header: t('admin.tenants.tenantColumn'),
        cell: (row) => {
          // The naming ladder of one ledger row (see the file header's
          // naming section): the operator's recorded display name
          // first, then the demo roster's copy for a tenant the app
          // knows, then the unnamed fallback. The raw tenant id is
          // never rendered as a name.
          if (row.displayName.trim() !== '') {
            return row.displayName
          }
          const nameKey = demoTenantNameKey(row.tenantId)
          return nameKey === null
            ? t('admin.tenants.identityUnknown')
            : t(nameKey)
        },
      },
      {
        id: 'status',
        header: t('admin.tenants.statusColumn'),
        cell: (row) => {
          // The known status vocabulary renders through the bundle; an
          // unknown status renders as the raw value -- an opaque
          // server reference, deliberately untranslated (the team
          // surface's treatment of unknown tokens).
          const key = TENANT_STATUS_TEXT_KEYS[row.status]
          return key === undefined ? row.status : t(key)
        },
      },
      {
        id: 'created',
        header: t('admin.tenants.createdColumn'),
        cell: (row) => formatDate(row.createdAt),
      },
    ],
    [formatDate, t],
  )

  return (
    <Box sx={{ p: { xs: 2, sm: 3 }, maxWidth: 720 }}>
      <Typography component="h1" variant="h4" sx={{ fontWeight: 600 }}>
        {t('admin.heading')}
      </Typography>
      <Typography
        variant="body1"
        color="text.secondary"
        sx={{ marginTop: 1, marginBottom: 3 }}
      >
        {t('admin.intro')}
      </Typography>
      {listReadFailed && !gateDenied ? (
        // The read failed for a reason other than authorization -- the
        // error empty state in its own suit, never the no-permission
        // one (a down server is not a permission problem).
        <EmptyState variant="error" headingLevel="h2" />
      ) : (
        <RouteGuard
          status={gateStatus}
          deniedFallback={
            <EmptyState variant="noPermission" headingLevel="h2" />
          }
        >
          <DataTable
            rows={tenantsQuery.data?.tenants ?? []}
            columns={tenantColumns}
            rowKey={(row) => row.tenantId}
            loading={tenantsQuery.isFetching}
            emptyTitle={t('admin.tenants.emptyTitle')}
            emptyDescription={t('admin.tenants.emptyDescription')}
          />
        </RouteGuard>
      )}
    </Box>
  )
}
