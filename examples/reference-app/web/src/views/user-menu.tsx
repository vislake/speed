/**
 * user-menu.tsx -- the AppBar's right-hand cluster: the tenant
 * switcher over the demo's two tenants and auth-ui's SignOutButton,
 * both host-composed over the session from app services.
 *
 * The demo has no roster endpoint, so the roster is the app's own
 * static data over the two seeded demo tenants, its display names
 * app namespace keys -- plus, when the session's current tenant is
 * not one of them (a self-registered account's own clinic, which
 * registration provisions -- cmd/server/self_service.go), one extra
 * row naming that tenant by its id, so the switcher's trigger is
 * never left in its no-current-tenant state while signed in. The
 * roster lists tenants only; membership is
 * the server's own fact, never inferred here -- of the accounts the
 * seed registers, demo-owner and demo-reader hold membership in
 * both demo tenants and demo-acme-only@example.com in tenant-acme
 * alone, and the real server refuses a switch into a tenant the
 * signed-in account lacks (authn.tenant_membership_required). The
 * web rig's journeys never cross that refusal: the owner day
 * (owner@example.test) is the one that switches tenants, while the
 * reader day (app-journey.test.tsx) stays on notes, where its grant
 * asymmetry lives. The current tenant comes from the auth-core hook
 * (the principal's own claim), never from local memory of a previous
 * switch, and a completed switch evicts the rows the departing access
 * token fetched: the previous tenant's tenant-namespaced query data
 * (the notes surface, whose keys are ['tenant', tenantId, ...]) and
 * the identity-domain rows the account surface reads (the sessions,
 * login-history and identities lists, whose bare spec-path keys carry
 * no tenant segment -- evictIdentityDomainQueries below) -- the rule
 * being that no cached row fetched under one access token survives
 * into the next one's session. After switching, this tenant's caches
 * hold only this tenant's rows, and the notes surface re-fetches
 * fresh under the new tenant's access token.
 *
 * A completed switch also revalidates the Public config the frame's
 * brand and the home feature cards render from (the config hook's
 * refresh -- the host-switched-tenants-same-origin case, where the
 * server's per-tenant answer cannot be keyed client-side):
 * reference-app-web.md P2-refapp-14.
 *
 * Which tenant rows were evicted is read before the switch, from the
 * hook snapshot of the tenant about to be left behind -- never from
 * the new tenant id, which names nothing that was cached.
 */

import type { ReactElement } from 'react'
import { Box } from '@mui/material'
import { useQueryClient } from '@tanstack/react-query'
import type { QueryClient } from '@tanstack/react-query'
import { usePublicConfig } from '@speed/api-client/react'
import {
  getAuthnListIdentitiesQueryKey,
  getAuthnListLoginHistoryQueryKey,
  getAuthnListSessionsQueryKey,
} from '@speed/api-sdk'
import { SignOutButton } from '@speed/auth-ui'
import { useCurrentTenant } from '@speed/auth-core'
import { useTranslation } from '@speed/i18n'
import { TenantSwitcher } from '@speed/tenancy-ui'
import type { TenantOption } from '@speed/tenancy-ui'
import { useAppServices } from '../app-services.js'
import { DEMO_TENANTS } from '../demo-tenants.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'

/** The tenant-scoped query-key prefix the app's data queries namespace
 * under (['tenant', tenantId, ...]); the eviction below removes whole
 * prefixes. */
export const TENANT_QUERY_PREFIX = 'tenant'

/**
 * Evicts the identity-domain queries of the account surface: the
 * sessions, login-history and identities lists read through the
 * generated hooks of @speed/api-sdk, whose keys are bare spec paths
 * (['/api/v1/authn/sessions'], ['/api/v1/authn/login-history', ...]
 * and ['/api/v1/authn/identities']) -- nothing in the generated
 * package is tenant-namespaced, so no ['tenant', tenantId] removal can
 * reach them, yet every one is answered under the caller's access
 * token. The transitions that rotate that token must therefore evict
 * them by key: a tenant switch (this module's own handleSwitched,
 * below) and a session end (main.tsx's evictQueriesOnSessionEnd, which
 * removes the whole cache). Exported for the session-end side of that
 * story to name the same keys in tests.
 */
export function evictIdentityDomainQueries(queryClient: QueryClient): void {
  for (const queryKey of [
    getAuthnListSessionsQueryKey(),
    getAuthnListLoginHistoryQueryKey(),
    getAuthnListIdentitiesQueryKey(),
  ]) {
    // Partial-key removal: each generated key is a prefix of every real
    // key under it (the login-history key carries the query's {limit}
    // params as further elements), so removing the prefix removes the
    // rows whatever shape the actual query key took.
    queryClient.removeQueries({ queryKey })
  }
}

/** The user menu the AppShell mounts at the AppBar's end. */
export function UserMenu(): ReactElement {
  const { session, api } = useAppServices()
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const queryClient = useQueryClient()
  const currentTenant = useCurrentTenant()
  // Subscribes this menu to the page's shared Public-config cache (one
  // fetch serves every consumer of this api; see app-services.tsx), so
  // the switch handler below can reach the cache's sole revalidation
  // lever -- refresh -- the host answer to the host-resolved brand and
  // feature answers.
  const publicConfig = usePublicConfig(api)

  const tenants: TenantOption[] = DEMO_TENANTS.map((tenant) => ({
    id: tenant.id,
    name: t(tenant.nameKey),
  }))
  // A self-registered account's own clinic (registration provisions the
  // registrant a tenant of its own -- cmd/server/self_service.go) is not
  // among the seeded demo tenants this roster knows, yet the switcher's
  // trigger must name the tenant the session actually runs in: a
  // current-tenant id absent from the options would leave the trigger
  // in its no-current-tenant disabled state while signed in. The clinic
  // is merged in as one extra row, labelled by its tenant id -- the
  // identifier the host can honestly show for a tenant whose name no
  // roster endpoint serves (the same identifier-as-display fallback
  // org's own auto-created root naming uses server-side); the row a
  // self-service account owns is disabled like any current row, and the
  // demo rows above stay switchable only as far as the server's own
  // membership answers let a switch through.
  const currentTenantId = currentTenant?.tenantId ?? null
  if (
    currentTenantId !== null &&
    !tenants.some((tenant) => tenant.id === currentTenantId)
  ) {
    tenants.push({ id: currentTenantId, name: currentTenantId })
  }

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
    // The identity-domain rows were answered under the access token the
    // switch just rotated: evict them too, so the account surface can
    // never serve rows fetched under the departing token
    // (reference-app-web.md P1-apisdk-1).
    evictIdentityDomainQueries(queryClient)
    // The switch committed (the new tenant is the principal's claim
    // now), and the server resolves this page's Public config by host:
    // the brand and the home feature cards must answer the tenant the
    // page is actually in, so the shared config cache is re-asked
    // (reference-app-web.md P2-refapp-14). Stale-while-revalidate: the
    // previous answer keeps rendering until the new one lands.
    publicConfig.refresh()
  }

  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
      <TenantSwitcher
        session={session}
        tenants={tenants}
        currentTenantId={currentTenantId}
        onSwitched={handleSwitched}
      />
      <SignOutButton session={session} />
    </Box>
  )
}
