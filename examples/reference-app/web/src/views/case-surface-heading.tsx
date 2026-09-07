/**
 * case-surface-heading.tsx -- the cases pages' level-one heading,
 * shared by the list, create and detail pages so the current clinic's
 * name reaches the title level wherever a person works cases.
 *
 * The clinic is the work context the cases pages answer to, and the
 * acceptance chain's current-clinic gate names it inside the main
 * landmark: the heading composes the clinic's display name (the same
 * host roster the tenant switcher reads -- useCurrentTenantName) with
 * the page's own title, e.g. "Acme Dental · Cases", and falls back to
 * the bare title when the current tenant is not on the demo roster (a
 * real deployment's tenant has no name in this host's static copy).
 */

import type { ReactElement } from 'react'
import Typography from '@mui/material/Typography'
import { useTranslation } from '@speed/i18n'
import { useCurrentTenantName } from '../app-services.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'

/** The cases pages' h1: the current clinic's name at the title level,
 * composed with the page's own title (the list page passes the bare
 * "Cases" title -- the list is never "My Cases"). */
export function CasesSurfaceHeading({ title }: { readonly title: string }): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const clinic = useCurrentTenantName()
  const text =
    clinic !== null
      ? t('cases.headingWithTitle', { clinic, title })
      : title
  return (
    <Typography component="h1" variant="h4" sx={{ fontWeight: 600 }}>
      {text}
    </Typography>
  )
}
