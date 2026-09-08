/**
 * admin-api.ts -- the typed access of the administration surface to
 * go/admin's operator-facing fragment: GET /api/v1/admin/tenants, the
 * tenant ledger (docs/internal/23-admin.md's D3) the platform
 * operator's surface reads.
 *
 * Why hand-written: admin's fragment ships a backend leg only, exactly
 * like org's, storage's and sharing's (the merged document
 * build/openapi/speed.yaml joins the fragments the frontend generated
 * hooks are orval'd from, and admin's is not among them), so no
 * generated operation exists for this host to call. The web reaches
 * the mounted route through the app's own api-client RequestFn -- the
 * same transport every generated call rides, the app never calls HTTP
 * directly -- answered by the module's own handler behind this app's
 * admin route guard (cmd/server/demo_admin.go's guardAdminRoute, which
 * evaluates every admin:* permission in rbac.SystemDomain). The path
 * is a module constant admin keeps unexported (adminRoutePath is this
 * app's own mirror in cmd/server/demo_admin.go), and the shapes are
 * the fragment's own (AdminTenant, AdminListTenantsResponse in
 * go/admin/api/openapi.yaml) -- the same hand-kept-shape relationship
 * the org-api calls have with go/org's fragment, kept in step by the
 * Go flow suite that drives the mounted route and the web suites that
 * drive this accessor.
 *
 * What a ledger row carries: tenantId (the identity key, never
 * rendered -- see admin-view.tsx's naming section for why), displayName
 * (the name an operator recorded for the tenant; the auto-registered
 * rows the org.node.created subscription lazily creates carry the
 * empty string by design, go/admin/tenant_service.go), status
 * (active/suspended), and the row's audit metadata. The administration
 * view renders identity, status and the recorded date from these.
 */

import type { RequestFn } from '@speed/api-client'

/** The path of go/admin's tenant ledger (adminRoutePath + '/tenants'
 * in cmd/server/demo_admin.go). */
export const ADMIN_TENANTS_PATH = '/api/v1/admin/tenants'

/** A ledger row's status vocabulary (AdminTenantStatus in
 * go/admin/api/openapi.yaml). */
export type AdminTenantStatus = 'active' | 'suspended'

/** One row of the operator-facing tenant ledger (AdminTenant in
 * go/admin/api/openapi.yaml). */
export interface AdminTenant {
  /** The tenant id -- the row's identity key, never rendered as a
   * name. */
  readonly tenantId: string
  /** The display name an operator recorded for the tenant; '' for the
   * rows the org.node.created subscription auto-registered. */
  readonly displayName: string
  /** The ledger's status opinion (active or suspended). */
  readonly status: AdminTenantStatus
  /** When the row was recorded. */
  readonly createdAt: string
  /** The operator who recorded the row. */
  readonly createdBy: string
  /** The operator's notes on the row. */
  readonly notes: string
}

/** GET /api/v1/admin/tenants' 200 answer (AdminListTenantsResponse in
 * go/admin/api/openapi.yaml). */
export interface AdminListTenantsResponse {
  readonly tenants?: AdminTenant[]
}

/**
 * Fetches the platform's tenant ledger through api. A non-2xx answer
 * rejects as the client's ApiError like every other request; the
 * administration view classifies the refusal exactly as the team
 * surface classifies its reads' (the rbac gate's 403 is an
 * authorization fact, anything else a load failure).
 */
export async function listAdminTenants(
  api: RequestFn,
): Promise<AdminListTenantsResponse> {
  return api<AdminListTenantsResponse>(ADMIN_TENANTS_PATH)
}
