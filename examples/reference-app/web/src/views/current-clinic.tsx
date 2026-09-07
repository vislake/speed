/**
 * current-clinic.tsx -- the clinic-context line every surface that
 * does or reads work renders at page-title level, directly under its
 * h1: the one place in the main content area that always says which
 * clinic the person is working in.
 *
 * Why this exists (reference-app acceptance, current-clinic-is-visible):
 * the tenant switcher names the current clinic only in the chrome, and
 * a chrome-only mention is scannable past -- the acceptance story is a
 * practice manager who switches clinic, sees the record list go empty
 * and reads that as data loss, because nothing in the work area ever
 * said the context changed. A person writing a patient record needs to
 * know where that record will land from where they are writing, not
 * from a header they have stopped looking at. So the home and notes
 * surfaces both render this line under their page heading, and the
 * line is driven by the principal's own claim (auth-core's
 * useCurrentTenant) -- when a switch commits, the session notifies and
 * the line re-renders with the new clinic's name, the old name gone.
 *
 * The component renders nothing until a tenant is known (an anonymous
 * render, or a session mid-transition), and nothing when the tenant id
 * is not on the app's demo roster: an unknown tenant is named nowhere,
 * never guessed. The display names are the same app-namespace keys the
 * chrome's switcher uses (demo-tenants.ts holds the one roster), so
 * the name in the work area and the name on the trigger can never
 * drift apart.
 */

import type { ReactElement } from 'react'
import Typography from '@mui/material/Typography'
import { useCurrentTenant } from '@speed/auth-core'
import { useTranslation } from '@speed/i18n'
import { demoTenantNameKey } from '../demo-tenants.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'

/**
 * The clinic-context line: which clinic the signed-in session operates
 * in, in the clinic's own display name. Null until a tenant id exists.
 */
export function CurrentClinicLine(): ReactElement | null {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const currentTenant = useCurrentTenant()
  const nameKey = demoTenantNameKey(currentTenant?.tenantId ?? null)
  if (nameKey === null) {
    return null
  }
  return (
    <Typography
      component="p"
      variant="subtitle1"
      sx={{ marginTop: 0.5, color: 'text.secondary' }}
    >
      {t('clinic.currentClinic', { name: t(nameKey) })}
    </Typography>
  )
}
