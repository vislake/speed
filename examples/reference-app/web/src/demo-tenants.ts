/**
 * demo-tenants.ts -- the app's demo roster, shared by every surface
 * that must name the tenant the session runs under: the frame chrome
 * (user-menu.tsx, whose TenantSwitcher shows the current tenant's name)
 * and the main-content clinic line (views/current-clinic.tsx, which
 * names where a person's work is about to land -- the reference-app
 * acceptance gate that no surface a person does or reads work on may
 * leave the clinic unnamed outside the chrome).
 *
 * The demo has no roster endpoint: the server seeds exactly two
 * tenants and the accounts the web journeys use hold membership in
 * both (internal/app/demo_users.go), so the roster is the app's own
 * static data with app-namespace display names -- the same
 * hand-maintained mirror the chrome always used, moved here so the
 * two consumers can never drift apart. The static copy covers the
 * two boot-configured tenants only; a tenant that did not exist at
 * boot -- the clinic a self-service registration provisions -- is
 * named by its org root through the app's own tenant-identity
 * answer (tenant-name.ts / useCurrentTenantName in app-services),
 * the route internal/app/clinic_name.go mounts. Which of the two demo
 * tenants an account lands in when it signs in with no tenant named
 * is the membership answer's first row (authn resolves the account's
 * first tenant from the host's MembershipReader, whose enumeration
 * org orders by tenant id), so a surface names the CURRENT tenant
 * from the principal's own claim, never from a guess.
 */

/** One demo tenant: the id the switch operation is called with, and
 * the app-namespace key carrying its display name. */
export interface DemoTenant {
  readonly id: string
  /** The app-namespace key carrying the tenant's display name (the
   * demo roster's names are host copy; off-roster tenants are named
   * by the server answer useCurrentTenantName fetches). */
  readonly nameKey: string
}

/** The demo's seeded tenants, in server order. */
export const DEMO_TENANTS: readonly DemoTenant[] = [
  { id: 'tenant-acme', nameKey: 'tenants.acme' },
  { id: 'tenant-globex', nameKey: 'tenants.globex' },
]

/** The app-namespace key naming a demo tenant id, or null when the id
 * is not on the roster (a tenant this app's demo does not configure --
 * the caller renders nothing rather than inventing a name). */
export function demoTenantNameKey(tenantId: string | null): string | null {
  const tenant = DEMO_TENANTS.find((entry) => entry.id === tenantId)
  return tenant?.nameKey ?? null
}

/**
 * The id of the system pseudo-tenant: rbac.SystemDomain's value
 * ("system", pinned by go/rbac's own suite in subject_test.go). The
 * demo's platform-staff seed (internal/app/demo_admin.go) grants
 * demo-platform-staff@example.com membership in this pseudo-tenant
 * ALONE -- never in any customer tenant -- so its access token's tenant
 * claim always resolves here, and the signed-in frame scoped to this
 * tenant is the platform frame, not a clinic's.
 *
 * Not on DEMO_TENANTS by design: it is not a clinic to switch into, and
 * the switcher roster lists tenants only. The frame's administration
 * entry gates on this claim (app.tsx) the way the server's admin
 * subject resolver evaluates every admin route in this domain
 * (internal/app/demo_admin.go's adminSubjectResolver) -- the web side
 * cannot import the Go constant, so the mirror lives here, and the
 * demo server's own staff-shaped answers key off the same value
 * (test-utils/demo-server.ts).
 */
export const SYSTEM_PSEUDO_TENANT_ID = 'system'
