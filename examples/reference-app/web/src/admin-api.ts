/**
 * admin-api.ts -- the typed access of the administration surface to
 * go/admin's operator-facing fragment: the tenant ledger and the
 * usage/billing dashboard the platform operator's two reads use.
 *
 * go/admin's fragment joins the merged document and the generated
 * @speed/api-sdk like every platform module's does; this host reaches
 * the mounted route through the app's own api-client RequestFn -- the
 * same transport every generated call rides, the app never calls HTTP
 * directly -- answered by the module's own handler behind this app's
 * admin route guard (internal/app/demo_admin.go's guardAdminRoute, which
 * evaluates every admin:* permission in rbac.SystemDomain). The path
 * is a module constant admin keeps unexported (adminRoutePath is this
 * app's own mirror in internal/app/demo_admin.go), and the shapes are
 * the fragment's own (AdminTenant, AdminListTenantsResponse,
 * AdminUsageSummaryRow, AdminUsageSummaryResponse in
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
 * in internal/app/demo_admin.go). */
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

// --- The usage/billing dashboard --------------------------------------------

/**
 * The path of go/admin's usage/billing dashboard (adminRoutePath +
 * '/usage-summary' in internal/app/demo_admin.go), served behind the
 * same admin route guard as the ledger above.
 */
export const ADMIN_USAGE_SUMMARY_PATH = '/api/v1/admin/usage-summary'

/**
 * One recorded usage aggregation of a dashboard row (AdminUsageFeatureSummary
 * in go/admin/api/openapi.yaml): a feature's summed quantity over one
 * calendar period. The feature keys the composed app records are go/
 * ai-gateway's usage dimensions (ai.chat_tokens, ai.image_count,
 * ai.image_steps -- the dimensions internal/app/server.go's
 * usage-recorder wiring reports), the vocabulary admin-usage-view renders
 * through the bundle.
 */
export interface AdminUsageFeatureSummary {
  readonly feature: string
  readonly periodStart: string
  readonly periodEnd: string
  readonly quantity: number
}

/** A tenant's credit-balance answer (AdminCreditBalance in
 * go/admin/api/openapi.yaml): the available spend and what in-flight
 * generations hold reserved. */
export interface AdminUsageCreditBalance {
  readonly available: number
  readonly reserved: number
}

/** A tenant's active subscription (AdminSubscription in
 * go/admin/api/openapi.yaml): the subscription row plus its plan
 * reference. The plan id is an opaque billing row id the dashboard
 * answer does not enrich with the plan's recorded name, so it stays a
 * key, never rendered copy (see admin-usage-view.tsx's naming
 * section). */
export interface AdminUsageSubscription {
  readonly id: string
  readonly planId: string
  readonly status: string
  readonly createdAt: string
}

/**
 * One tenant's row of the usage/billing dashboard (AdminUsageSummaryRow
 * in go/admin/api/openapi.yaml): the ledger's tenant identity plus the
 * dimensions a row carries only when the answering module was wired.
 * The composed app wires both metering and billing, so every row it
 * answers carries meteringSummaries (empty for a tenant with no
 * recorded usage, never absent) and creditBalance, while
 * activeSubscription is absent exactly when the tenant holds no active
 * subscription.
 */
export interface AdminUsageSummaryRow {
  /** The ledger row's tenant id -- the identity key, never rendered as
   * a name (the ledger rows' own rule). */
  readonly tenantId: string
  /** The display name the ledger recorded for the tenant; '' for the
   * auto-registered rows. */
  readonly displayName: string
  readonly meteringSummaries?: readonly AdminUsageFeatureSummary[]
  readonly creditBalance?: AdminUsageCreditBalance
  readonly activeSubscription?: AdminUsageSubscription
}

/** GET /api/v1/admin/usage-summary's 200 answer (AdminUsageSummaryResponse
 * in go/admin/api/openapi.yaml): one row per tenant in the ledger. */
export interface AdminUsageSummaryResponse {
  readonly rows?: AdminUsageSummaryRow[]
}

/**
 * Fetches the platform's usage/billing dashboard through api. A non-2xx
 * answer rejects as the client's ApiError like every other request; the
 * dashboard view classifies the refusal exactly as the ledger view
 * classifies its own read's.
 */
export async function fetchAdminUsageSummary(
  api: RequestFn,
): Promise<AdminUsageSummaryResponse> {
  return api<AdminUsageSummaryResponse>(ADMIN_USAGE_SUMMARY_PATH)
}
