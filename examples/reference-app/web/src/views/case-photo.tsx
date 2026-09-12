/**
 * case-photo.tsx -- one photo of a case rendered from its stored bytes:
 * the bytes fetched through the cases photo-content operation and shown
 * as the blob URL use-content-object-url owns and revokes (the hook the
 * case page and the simulation comparison both read the original
 * through, so both share one query). A photo whose read fails renders
 * its refusal's bilingual text (mapped through the cases
 * reachable-error map), never a broken image.
 */

import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Typography from '@mui/material/Typography'
import {
  getCasesGetPhotoContentQueryKey,
  useCasesGetPhotoContent,
} from '../app-api/index.js'
import type { CasesPhoto } from '../app-api/index.js'
import { useTranslation } from '@speed/i18n'
import {
  casesErrorCodeOf,
  casesErrorTextKey,
} from '../cases-errors.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { useTenantQueryKey } from '../tenant-query-key.js'
import { useContentObjectUrl } from '../use-content-object-url.js'

/** One photo of the case: its bytes fetched through the photo-content
 * operation and rendered from a blob URL owned and revoked by
 * use-content-object-url. */
export function CasePhoto({
  caseId,
  photo,
  index,
}: {
  readonly caseId: string
  readonly photo: CasesPhoto
  /** The 1-based position for the photo's accessible name. */
  readonly index: number
}): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const { tenantId, queryKey: contentKey } = useTenantQueryKey(
    getCasesGetPhotoContentQueryKey(caseId, photo.object_id),
  )
  const contentQuery = useCasesGetPhotoContent(caseId, photo.object_id, {
    query: { queryKey: contentKey, enabled: tenantId !== null },
  })

  const objectUrl = useContentObjectUrl(contentQuery.data)

  const alt = t('cases.detail.photoAlt', { index })

  if (contentQuery.isError) {
    return (
      <Typography variant="body2" color="text.secondary">
        {t(casesErrorTextKey(casesErrorCodeOf(contentQuery.error)))}
      </Typography>
    )
  }
  if (objectUrl === null) {
    return <Box sx={{ height: 180 }} />
  }
  return (
    <Box
      component="img"
      src={objectUrl}
      alt={alt}
      sx={{
        display: 'block',
        maxWidth: 320,
        maxHeight: 240,
        objectFit: 'contain',
        borderRadius: 1,
        border: '1px solid',
        borderColor: 'divider',
      }}
    />
  )
}
