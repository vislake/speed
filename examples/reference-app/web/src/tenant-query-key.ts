/**
 * tenant-query-key.ts -- the app's tenant-namespaced query keys: the one
 * place the ['tenant', tenantId, ...] convention is built.
 *
 * Every cached row this app reads under a signed-in principal is keyed
 * under the tenant that principal was acting in, so a tenant switch can
 * never serve the previous tenant's rows: user-menu.tsx evicts the
 * departing tenant's whole ['tenant', tenantId, ...] subtree on every
 * switch (TENANT_QUERY_PREFIX below names the prefix it removes), and
 * the bootstrap's shipped session-end eviction empties the whole cache
 * the moment the session ends.
 *
 * The tenant id is read from the auth-core hook -- the principal's own
 * claim, never local memory of a previous switch -- and a null tenant
 * (anonymous, or a principal carrying no tenant_id) still yields a key:
 * the reads pass `enabled: tenantId !== null`, so a null tenant's query
 * never fetches, failing closed rather than inventing a tenant. The
 * hook hands back both values a tenant-scoped read needs, so a surface
 * derives its key and its enablement from the one hook call.
 *
 * Call sites pass the bare key inline -- a generated `getXQueryKey(...)`
 * call or a literal tail -- so the key is a fresh-but-equal array per
 * render; the memo below only holds a caller that hands a stable array.
 * What a key IS here is its contents, not its reference: the generated
 * hooks hash query keys structurally (equal contents map to one cache
 * entry, so a fresh array neither refetches nor re-renders) and the
 * app's invalidation and eviction calls match structurally too, so no
 * consumer in this app can observe the difference.
 */

import { useMemo } from 'react'
import { useCurrentTenant } from '@speed/auth-core'

/** The tenant-scoped query-key prefix the app's data queries namespace
 * under (['tenant', tenantId, ...]); the eviction paths remove whole
 * prefixes. */
export const TENANT_QUERY_PREFIX = 'tenant'

/** The tenant-namespaced read a surface derives from one hook call: the
 * current tenant id (the `enabled` gate's operand) and the query key
 * built over it. */
export interface TenantQueryKey {
  /** The tenant the principal is acting in, or null while anonymous. */
  readonly tenantId: string | null
  /** ['tenant', tenantId, ...bareKey]. */
  readonly queryKey: readonly unknown[]
}

/**
 * The tenant-namespaced query key over one generated bare key: the
 * prefix, the current tenant id, then the bare key's own elements (see
 * the file header).
 */
export function useTenantQueryKey(
  bareKey: readonly unknown[],
): TenantQueryKey {
  const currentTenant = useCurrentTenant()
  const tenantId = currentTenant?.tenantId ?? null
  const queryKey = useMemo(
    () => [TENANT_QUERY_PREFIX, tenantId, ...bareKey],
    [tenantId, bareKey],
  )
  return { tenantId, queryKey }
}
