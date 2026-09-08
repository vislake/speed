/**
 * current-clinic.tsx -- the clinic-context line every surface that
 * does or reads work renders at page-title level, directly under its
 * h1: the one place in the main content area that always says which
 * clinic the person is working in.
 *
 * Why this exists: the tenant switcher names the current clinic only
 * in the chrome, and a chrome-only mention is scannable past -- a
 * person who switches clinic, sees the record list go empty and has
 * nothing in the work area telling them the context changed can read
 * that as data loss. A person writing a patient record needs to know
 * where that record will land from where they are writing, not from a
 * header they have stopped looking at (the property
 * e2e/current-clinic-is-visible.spec.ts gates). So the surfaces render
 * this line under their page heading, and the line is driven by the
 * principal's own claim (auth-core's useCurrentTenant) -- when a
 * switch commits, the session notifies and the line re-renders with
 * the new clinic's name, the old name gone.
 *
 * The name comes from useCurrentTenantName (app-services.tsx): the
 * demo roster's copy for the boot-configured tenants, the tenant's own
 * org root name -- fetched from the app's tenant-identity answer,
 * tenant-name.ts -- for a clinic that did not exist at boot, the same
 * name the chrome's switcher shows, so the name in the work area and
 * the name on the trigger can never drift apart. The component renders
 * nothing until a tenant is known (an anonymous render, or a session
 * mid-transition), and nothing while a runtime clinic's name is still
 * loading or absent: an unknown tenant is named nowhere, never guessed.
 */

import type { ReactElement } from 'react'
import Typography from '@mui/material/Typography'
import { useTranslation } from '@speed/i18n'
import { useCurrentTenantName } from '../app-services.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'

/**
 * The clinic-context line: which clinic the signed-in session operates
 * in, in the clinic's own display name. Null until a tenant exists and
 * its name is known.
 */
export function CurrentClinicLine(): ReactElement | null {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const name = useCurrentTenantName()
  if (name === null) {
    return null
  }
  return (
    <Typography
      component="p"
      variant="subtitle1"
      sx={{ marginTop: 0.5, color: 'text.secondary' }}
    >
      {t('clinic.currentClinic', { name })}
    </Typography>
  )
}
