/**
 * case-detail-view.tsx -- one case's page in the reference-app web
 * host: the case read through the generated cases_getCase hook over a
 * tenant-namespaced key, its photos rendered from their stored bytes.
 *
 * The photos are visible through the blob-URL pattern: each photo's
 * bytes are fetched through the api-client seam (the generated
 * cases_getPhotoContent operation -- JSON-carried base64, the only
 * transport this host's generated surface speaks), decoded into a Blob
 * of the media type the server's probe assigned, and rendered as an
 * object URL. Every URL this page creates is revoked when the photo it
 * renders leaves the page (unmount, or a refetch replacing the bytes),
 * so a session that opens and closes cases never leaks blob URLs. A
 * photo whose read fails renders its refusal's bilingual text (a case
 * another tenant once shared is not_found; a photo whose object was
 * cleaned up is photo_not_found -- both mapped in cases-errors.ts),
 * never a broken image.
 *
 * The page keeps its back control outside every state branch: a case
 * that cannot be found or fails to load is not a dead end, and the
 * person returns to the list (the host's answer to the onBack cue).
 * Created-at renders through the shared Intl formatter.
 */

import type { ReactElement } from 'react'
import { useEffect, useMemo, useState } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Typography from '@mui/material/Typography'
import {
  getCasesGetCaseQueryKey,
  getCasesGetPhotoContentQueryKey,
  useCasesGetCase,
  useCasesGetPhotoContent,
} from '@speed/api-sdk'
import type { CasesPhoto } from '@speed/api-sdk'
import { useCurrentTenant } from '@speed/auth-core'
import { useTranslation } from '@speed/i18n'
import { RouteGuard } from '@speed/layout-kit'
import { EmptyState } from '@speed/ui-kit'
import {
  casesErrorCodeOf,
  casesErrorTextKey,
} from '../cases-errors.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { useDateFormatter } from '../use-date-formatter.js'
import { CasesSurfaceHeading } from './case-surface-heading.js'

/** Decodes a base64 payload into its bytes (binary-safe: atob hands
 * back a binary string; the char-code loop keeps every byte). The
 * explicit ArrayBuffer-backed type keeps the result a valid BlobPart
 * under TypeScript 5.9's typed-array generics. */
function base64ToBytes(contentBase64: string): Uint8Array<ArrayBuffer> {
  const binary = atob(contentBase64)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i)
  }
  return bytes
}

/** One photo of the case: its bytes fetched through the photo-content
 * operation and rendered from a blob URL that this component owns and
 * revokes. */
function CasePhoto({
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

/** One case's page. */
export function CaseDetailView({
  caseId,
  onBack,
}: {
  readonly caseId: string
  /** The host's answer to the back control: navigate to the cases list. */
  readonly onBack: () => void
}): ReactElement {
  const { t, i18n } = useTranslation(REFERENCE_APP_NAMESPACE)
  const currentTenant = useCurrentTenant()
  const tenantId = currentTenant?.tenantId ?? null

  const caseKey = useMemo(
    () => ['tenant', tenantId, ...getCasesGetCaseQueryKey(caseId)],
    [tenantId, caseId],
  )
  const caseQuery = useCasesGetCase(caseId, {
    query: { queryKey: caseKey, enabled: tenantId !== null },
  })

  const formatDate = useDateFormatter(i18n.language)
  const record = caseQuery.data
  const readFailed = caseQuery.isError
  const gateStatus = record !== undefined ? 'allowed' : 'pending'

  return (
    <Box sx={{ p: { xs: 2, sm: 3 }, maxWidth: 720 }}>
      <Button
        variant="text"
        onClick={onBack}
        sx={{ marginBottom: 1, paddingLeft: 0 }}
      >
        {t('cases.detail.backToCases')}
      </Button>
      <CasesSurfaceHeading title={t('cases.detail.heading')} />
      {readFailed ? (
        <EmptyState variant="error" headingLevel="h2" sx={{ marginTop: 3 }} />
      ) : (
        <RouteGuard status={gateStatus}>
          {record === undefined ? null : (
            <>
              <Typography variant="h6" sx={{ marginTop: 2 }}>
                {record.patient_name}
              </Typography>
              <Typography variant="body2" color="text.secondary">
                {t('cases.detail.openedOn')} {formatDate(record.created_at)}
              </Typography>
              {record.patient_ref.length > 0 && (
                <Typography variant="body2" color="text.secondary">
                  {record.patient_ref}
                </Typography>
              )}
              <Typography variant="h6" sx={{ marginTop: 3, marginBottom: 1 }}>
                {t('cases.detail.photosHeading')}
              </Typography>
              {record.photos.length === 0 ? (
                <Typography variant="body2" color="text.secondary">
                  {t('cases.detail.noPhotos')}
                </Typography>
              ) : (
                <Box
                  sx={{
                    display: 'flex',
                    flexWrap: 'wrap',
                    gap: 2,
                  }}
                >
                  {record.photos.map((photo, position) => (
                    <CasePhoto
                      key={photo.object_id}
                      caseId={caseId}
                      photo={photo}
                      index={position + 1}
                    />
                  ))}
                </Box>
              )}
            </>
          )}
        </RouteGuard>
      )}
    </Box>
  )
}
