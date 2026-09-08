/**
 * case-detail-view.tsx -- one case's page in the reference-app web
 * host: the case read through the generated cases_getCase hook over a
 * tenant-namespaced key, its photos rendered from their stored bytes,
 * and each photo carrying its smile-simulation workbench
 * (photo-simulation-panel.tsx: option pickers, the async generation
 * with its honest status, and the before/after comparison once a
 * generation completes).
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
 * this tenant cannot see answers not_found; a photo whose object was
 * cleaned up answers photo_not_found -- both mapped in
 * cases-errors.ts), never a broken image.
 *
 * The page keeps its back control outside every state branch: a case
 * that cannot be found or fails to load is not a dead end, and the
 * person returns to the list (the host's answer to the onBack cue).
 * Created-at renders through the shared Intl formatter.
 */

import type { ReactElement } from 'react'
import { useMemo } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Typography from '@mui/material/Typography'
import {
  getCasesGetCaseQueryKey,
  useCasesGetCase,
} from '@speed/api-sdk'
import { useCurrentTenant } from '@speed/auth-core'
import { useTranslation } from '@speed/i18n'
import { RouteGuard } from '@speed/layout-kit'
import { EmptyState } from '@speed/ui-kit'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { useDateFormatter } from '../use-date-formatter.js'
import { CasePhoto } from './case-photo.js'
import { PhotoSimulationPanel } from './photo-simulation-panel.js'
import { CasesSurfaceHeading } from './case-surface-heading.js'

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
              <Typography
                variant="h6"
                sx={{ marginTop: 3, marginBottom: 1 }}
              >
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
                    flexDirection: 'column',
                    gap: 3,
                  }}
                >
                  {record.photos.map((photo, position) => (
                    <Box key={photo.object_id}>
                      <CasePhoto
                        caseId={caseId}
                        photo={photo}
                        index={position + 1}
                      />
                      <PhotoSimulationPanel
                        caseId={caseId}
                        photo={photo}
                        index={position + 1}
                        patientName={record.patient_name}
                      />
                    </Box>
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
