/**
 * simulation-download-action.tsx -- the keep-the-result surface: the
 * control that hands one completed smile simulation's generated image
 * to the browser as a real file. A generation is a thing the clinic
 * bought -- credits left the ledger for it -- and without this control
 * the practice could look at the result, regenerate it, or turn it
 * into a temporary patient link, but could not keep the image itself;
 * this is the way out of the product that is not a screenshot.
 *
 * The bytes are read through the simulation-content operation -- the
 * same tenant-namespaced query the comparison's result image reads, so
 * the action's presence adds no second network read -- materialized by
 * use-content-object-url into a blob URL of the media type the probe
 * assigned (revoked when the bytes are replaced or the control leaves
 * the page), and offered as a real download link: an anchor whose
 * download attribute carries the filename and whose href is the blob
 * URL, so pressing it starts the browser's own download (the event,
 * the name and the bytes the e2e download gate observes) rather than a
 * navigation that opens the image in a tab.
 *
 * The control renders nothing until the bytes have actually arrived: a
 * read that has not settled offers no download of nothing, and a
 * refused read leaves the refusal's own text -- rendered where the
 * image belongs -- to speak, never a control that saves an error page.
 */

import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import {
  getSmilesimGetSimulationContentQueryKey,
  useSmilesimGetSimulationContent,
} from '../app-api/index.js'
import type { SmilesimSimulation } from '../app-api/index.js'
import { useTranslation } from '@speed/i18n'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { useTenantQueryKey } from '../tenant-query-key.js'
import { useContentObjectUrl } from '../use-content-object-url.js'

/** The image media types the storage probe can assign, each to the
 * extension its bytes should carry when saved to disk. A type outside
 * the map is still downloadable -- the file simply carries no
 * extension. */
const EXTENSION_BY_MEDIA_TYPE: Readonly<Record<string, string>> = {
  'image/png': 'png',
  'image/jpeg': 'jpg',
  'image/webp': 'webp',
}

/** The filename-hostile characters a patient name could carry (path
 * separators, the characters Windows refuses in a filename, control
 * characters), each replaced by a space; runs of spaces collapse. */
const FILENAME_HOSTILE = /[\\/:*?"<>|\u0000-\u001f]/g

/** The filename the saved result should carry: the patient it belongs
 * to -- so a folder of downloads stays identifiable without opening
 * any of the files -- followed by what the file is, plus the extension
 * the media type's bytes should carry. */
export function simulationDownloadFileName(
  patientName: string,
  mediaType: string,
): string {
  const patient = patientName
    .replace(FILENAME_HOSTILE, ' ')
    .replace(/\s+/g, ' ')
    .trim()
  const stem = `${patient.length > 0 ? `${patient} ` : ''}smile simulation`
  const extension = EXTENSION_BY_MEDIA_TYPE[mediaType]
  return extension === undefined ? stem : `${stem}.${extension}`
}

/**
 * The clinic's download control for one completed simulation (see the
 * file header). simulation must be a succeeded entry whose generated
 * content the tenant can read, photoObjectID the storage object id of
 * the photo the simulation was generated from (the address the
 * content read is keyed by), and patientName the name of the patient
 * the case belongs to -- the saved file's name carries it.
 */
export function SimulationDownloadAction({
  simulation,
  photoObjectID,
  patientName,
}: {
  readonly simulation: SmilesimSimulation
  readonly photoObjectID: string
  /** The patient the simulation belongs to; the saved file's name
   * carries it, so the file a clinic saves stays identifiable in a
   * folder full of downloads. */
  readonly patientName: string
}): ReactElement | null {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const { tenantId, queryKey: contentKey } = useTenantQueryKey(
    getSmilesimGetSimulationContentQueryKey(photoObjectID, simulation.job_id),
  )
  const contentQuery = useSmilesimGetSimulationContent(
    photoObjectID,
    simulation.job_id,
    { query: { queryKey: contentKey, enabled: tenantId !== null } },
  )

  const objectUrl = useContentObjectUrl(contentQuery.data)

  const data = contentQuery.data
  if (objectUrl === null || data === undefined) {
    return null
  }
  return (
    <Box sx={{ marginTop: 2 }}>
      <Button
        component="a"
        variant="outlined"
        href={objectUrl}
        download={simulationDownloadFileName(patientName, data.media_type)}
      >
        {t('cases.sim.downloadImage')}
      </Button>
    </Box>
  )
}
