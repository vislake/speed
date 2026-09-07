/**
 * case-photo.tsx -- one photo of a case rendered from its stored bytes:
 * the bytes fetched through the cases photo-content operation,
 * base64-decoded, and shown as a blob URL this component owns and
 * revokes (the pattern the case page and the simulation comparison both
 * use, so both fetch the original through one shared query). A photo
 * whose read fails renders its refusal's bilingual text (mapped through
 * the cases reachable-error map), never a broken image.
 */

import { useEffect, useMemo, useState } from 'react'
import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Typography from '@mui/material/Typography'
import {
  getCasesGetPhotoContentQueryKey,
  useCasesGetPhotoContent,
} from '@speed/api-sdk'
import type { CasesPhoto } from '@speed/api-sdk'
import { useCurrentTenant } from '@speed/auth-core'
import { useTranslation } from '@speed/i18n'
import {
  casesErrorCodeOf,
  casesErrorTextKey,
} from '../cases-errors.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { base64ToBytes } from '../base64.js'

/** One photo of the case: its bytes fetched through the photo-content
 * operation and rendered from a blob URL that this component owns and
 * revokes. */
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
  const currentTenant = useCurrentTenant()
  const tenantId = currentTenant?.tenantId ?? null

  const contentKey = useMemo(
    () => [
      'tenant',
      tenantId,
      ...getCasesGetPhotoContentQueryKey(caseId, photo.object_id),
    ],
    [tenantId, caseId, photo.object_id],
  )
  const contentQuery = useCasesGetPhotoContent(caseId, photo.object_id, {
    query: { queryKey: contentKey, enabled: tenantId !== null },
  })

  // The blob URL the img renders: created when the bytes arrive,
  // revoked when they are replaced or the photo leaves the page. The
  // effect owns exactly the URL it created -- revoking the previous
  // URL is the cleanup of the previous effect run, so a session that
  // renders many photos never leaks.
  const [objectUrl, setObjectUrl] = useState<string | null>(null)
  useEffect(() => {
    const data = contentQuery.data
    if (data === undefined) {
      return
    }
    const bytes = base64ToBytes(data.content_base64)
    const url = URL.createObjectURL(
      new Blob([bytes], { type: data.media_type }),
    )
    setObjectUrl(url)
    return () => {
      URL.revokeObjectURL(url)
    }
  }, [contentQuery.data])

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
