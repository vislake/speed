/**
 * tenant-name.ts -- the app's typed access to its own tenant-identity
 * answer: GET /api/reference-app/clinic-name (internal/app/clinic_name.go)
 * returns the org root name of the tenant the signed-in caller's access
 * token is scoped to.
 *
 * Why the app has its own identity answer at all: the demo's two
 * boot-configured tenants are named by the app's own static copy
 * (demo-tenants.ts -- no roster endpoint exists), and a tenant that did
 * not exist at boot -- the clinic a self-service registration provisions,
 * naming its org root after the name the registrant gave (self_service.go
 * and clinic_name.go) -- has no copy anywhere in the app. The frame's
 * tenant switcher and the work-area clinic line must still name it, and
 * this request is how they learn the name: through the app's own
 * api-client RequestFn (the same transport every generated call rides --
 * the app never calls HTTP directly), answered by the host's own route,
 * the same hand-kept-shape relationship the config endpoints have with
 * api-client's typed wrappers. The path is a host constant, not part of
 * any module's OpenAPI fragment, so it is spelled here and in
 * internal/app/clinic_name.go (ClinicNamePath) and kept in step by the Go
 * flow test that mounts the route and the web suites that drive it.
 *
 * The answer is deliberately one name: name is the org root name, or ''
 * when the tenant has no org tree to name it with (the web renders
 * nothing for an unnamed tenant rather than inventing an identifier).
 */

import type { RequestFn } from '@speed/api-client'

/** The path of the app's own clinic-name answer (clinicNamePath in
 * internal/app/clinic_name.go). */
export const CLINIC_NAME_PATH = '/api/reference-app/clinic-name'

/** The answer's wire shape. */
export interface ClinicNameResponse {
  /** The current tenant's org root name, or '' when the tenant has no
   * org tree to name it with. */
  readonly name?: string
}

/** Fetches the current tenant's clinic name through api. A non-2xx
 * answer rejects as the client's ApiError like every other request;
 * callers treat a rejection as "no name" (the unnamed-tenant render),
 * never as a crash. */
export function clinicNameRequest(
  api: RequestFn,
): Promise<ClinicNameResponse> {
  return api<ClinicNameResponse>(CLINIC_NAME_PATH)
}
