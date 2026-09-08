/**
 * app-services.tsx -- the app's composition services, shared down the
 * tree through one context: the auth-core session every view that
 * drives a session operation receives as a prop (auth-ui's contract:
 * the view layer never attaches or reads hooks itself) and the one
 * RequestFn the config hooks read from.
 *
 * The api identity is load-bearing, not incidental: usePublicConfig
 * and useFeature (the ./react subpath of @speed/api-client) share one
 * fetch per RequestFn reference, so every consumer on this page --
 * the header brand, the sign-in brand and the home brand -- passing
 * the same context value renders off a single /api/config/public
 * request. The context therefore holds the client as the RequestFn
 * it structurally is, and the value object itself is memoized on
 * [session, api] so a re-render never mints a fresh reference that
 * would silently split the config cache.
 *
 * The brand name is the shell's one server-driven text: go/config
 * serves brand.site_name as a Public config value (data, never a
 * translation -- the UI falls back to the app namespace while the
 * value is loading or the fetch failed, then renders the server's
 * answer verbatim in whatever language the page speaks).
 */

import { useQuery } from '@tanstack/react-query'
import { createContext, useContext, useMemo } from 'react'
import type { ReactElement, ReactNode } from 'react'
import { usePublicConfig } from '@speed/api-client/react'
import type { RequestFn } from '@speed/api-client'
import { useCurrentTenant } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'
import { useTranslation } from '@speed/i18n'
import { demoTenantNameKey } from './demo-tenants.js'
import { REFERENCE_APP_NAMESPACE } from './resources.js'
import { clinicNameRequest } from './tenant-name.js'

/** The services the shell views compose. */
export interface AppServices {
  /** The session views drive sign-in, registration, tenant switch and
   * sign-out through; attached to the auth-core hooks by the host
   * bootstrap. */
  readonly session: AuthSession
  /** The one RequestFn the page's config reads go through (the client
   * the host bound into the api-sdk runtime seam). */
  readonly api: RequestFn
}

/**
 * The display name of the tenant the signed-in frame is currently in,
 * or null while the tenant has no known name.
 *
 * The demo's two boot-configured tenants are named by the app's own
 * static copy (demo-tenants.ts -- their ids are on the roster), and a
 * tenant that did not exist at boot -- the clinic a self-service
 * registration provisions -- is named by its org root, fetched from the
 * app's own tenant-identity answer (tenant-name.ts) under a
 * tenant-namespaced query key, so a tenant switch evicts the departing
 * tenant's name with its other ['tenant', tenantId] data and the new
 * tenant's name loads under its own key. Null while the answer is
 * loading or absent: the switcher and the work-area clinic line render
 * nothing rather than inventing a name -- the same out-of-set
 * treatment demo-tenants.ts gives a tenant id that is not on the
 * roster.
 */
export function useCurrentTenantName(): string | null {
  const { api } = useAppServices()
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const currentTenant = useCurrentTenant()
  const tenantId = currentTenant?.tenantId ?? null
  const nameKey = demoTenantNameKey(tenantId)
  // The demo roster's copy answers first; the server answer is fetched
  // only for a tenant the copy cannot name. The query key shares the
  // ['tenant', tenantId, ...] prefix the app's tenant-scoped data keys
  // use, so user-menu's switch-time eviction of the departing tenant
  // (TENANT_QUERY_PREFIX) removes this row with the notes list's.
  const nameQuery = useQuery({
    queryKey: ['tenant', tenantId, 'clinic-name'],
    queryFn: () => clinicNameRequest(api),
    enabled: tenantId !== null && nameKey === null,
  })
  if (nameKey !== null) {
    return t(nameKey)
  }
  const name = nameQuery.data?.name
  return name !== undefined && name.length > 0 ? name : null
}

/** The Public config key carrying the brand name a server serves. */
export const BRAND_SITE_NAME_CONFIG_KEY = 'brand.site_name'

const AppServicesContext = createContext<AppServices | null>(null)

export interface AppServicesProviderProps extends AppServices {
  readonly children: ReactNode
}

export function AppServicesProvider({
  session,
  api,
  children,
}: AppServicesProviderProps): ReactElement {
  const value = useMemo<AppServices>(
    () => ({ session, api }),
    [session, api],
  )
  return (
    <AppServicesContext.Provider value={value}>
      {children}
    </AppServicesContext.Provider>
  )
}

/** The services of the enclosing provider; throws when none is set --
 * the app shell never renders outside its provider, so reaching the
 * throw means a unit was mounted without the harness. */
export function useAppServices(): AppServices {
  const services = useContext(AppServicesContext)
  if (services === null) {
    throw new Error(
      'useAppServices must be used inside an AppServicesProvider',
    )
  }
  return services
}

/**
 * The brand name for the current page, driven by go/config's Public
 * brand.site_name value: the server's answer verbatim when it is a
 * non-empty string, the app namespace's fallback while loading, on
 * error, or when the value is not a string.
 *
 * The config access is null-guarded twice over, because this hook
 * renders in the AppBar on every page: the Public-config wire shape is
 * a hand-maintained seam (config-fetcher.ts's PublicConfigResponse --
 * no OpenAPI fragment exists for either config endpoint), and a 200
 * whose body is not that shape -- a `{}` document, a non-object answer
 * -- must read as "no brand configured" and render the fallback, never
 * throw during render. `data?.config` alone would not be enough: a
 * truthy `data` with no `config` member is exactly what a
 * shape-mismatched 200 hands back, and reading `[key]` off the missing
 * member is a render-time TypeError on the page's most-mounted surface
 * (a shape-mismatched config answer must not white-screen the app --
 * no error boundary exists below this hook).
 */
export function useBrandName(): string {
  const { api } = useAppServices()
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const { data } = usePublicConfig(api)
  const value = data?.config?.[BRAND_SITE_NAME_CONFIG_KEY]
  if (typeof value === 'string' && value.length > 0) {
    return value
  }
  return t('brand.fallback')
}
