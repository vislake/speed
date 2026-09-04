/**
 * user-menu.tsx -- the AppBar's right-hand cluster: the tenant
 * switcher over the demo's two tenants and auth-ui's SignOutButton,
 * both host-composed over the session from app services.
 *
 * The demo has no roster endpoint, so the roster is the app's own
 * static data over the two seeded demo tenants, its display names
 * app namespace keys. The roster lists tenants only; membership is
 * the server's own fact, never inferred here -- of the accounts the
 * seed registers, demo-owner and demo-reader hold membership in
 * both demo tenants and demo-acme-only@example.com in tenant-acme
 * alone, and the real server refuses a switch into a tenant the
 * signed-in account lacks (authn.tenant_membership_required). The
 * web rig's journeys, all owner journeys (owner@example.test),
 * never cross that refusal. The current tenant comes from the
 * auth-core hook
 * (the principal's own claim), never from local memory of a previous
 * switch, and a completed switch evicts the previous tenant's query
 * data -- the tenant-namespaced-query-key discipline: after switching,
 * this tenant's caches hold only this tenant's rows, and the notes
 * surface (whose keys are ['tenant', tenantId, ...]) re-fetches fresh
 * under the new tenant's access token.
 *
 * Which tenant rows were evicted is read before the switch, from the
 * hook snapshot of the tenant about to be left behind -- never from
 * the new tenant id, which names nothing that was cached.
 */

import type { ReactElement } from 'react'
import { Box } from '@mui/material'
import { useQueryClient } from '@tanstack/react-query'
import { SignOutButton } from '@speed/auth-ui'
import { useCurrentTenant } from '@speed/auth-core'
import { useTranslation } from '@speed/i18n'
import { TenantSwitcher } from '@speed/tenancy-ui'
import type { TenantOption } from '@speed/tenancy-ui'
import { useAppServices } from '../app-services.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'

interface DemoTenant {
  readonly id: string
  /** The app-namespace key carrying the tenant's display name (no
   * roster endpoint exists in the demo; names are host copy). */
  readonly nameKey: string
}

/** The demo's seeded tenants, in server order. */
export const DEMO_TENANTS: readonly DemoTenant[] = [
  { id: 'tenant-acme', nameKey: 'tenants.acme' },
  { id: 'tenant-globex', nameKey: 'tenants.globex' },
]

/** The tenant-scoped query-key prefix the app's data queries namespace
 * under (['tenant', tenantId, ...]); the eviction below removes whole
 * prefixes. */
export const TENANT_QUERY_PREFIX = 'tenant'

/** The user menu the AppShell mounts at the AppBar's end. */
export function UserMenu(): ReactElement {
  const { session } = useAppServices()
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const queryClient = useQueryClient()
  const currentTenant = useCurrentTenant()

  const tenants: readonly TenantOption[] = DEMO_TENANTS.map((tenant) => ({
    id: tenant.id,
    name: t(tenant.nameKey),
  }))

  const handleSwitched = (tenantId: string): void => {
    // Evict the tenant being left, captured before the switch commits:
    // the notes list (and any later tenant-scoped data) of the old
    // tenant must not survive into the new tenant's session.
    const leavingTenantId = currentTenant?.tenantId ?? null
    if (leavingTenantId !== null && leavingTenantId !== tenantId) {
      queryClient.removeQueries({
        queryKey: [TENANT_QUERY_PREFIX, leavingTenantId],
      })
    }
  }

  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
      <TenantSwitcher
        session={session}
        tenants={tenants}
        currentTenantId={currentTenant?.tenantId ?? null}
        onSwitched={handleSwitched}
      />
      <SignOutButton session={session} />
    </Box>
  )
}
