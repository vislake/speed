/**
 * simulation-share-action.tsx -- the block-C clinic surface: the control
 * that turns one completed simulation into a patient-facing link. It
 * renders beneath the before/after comparison of the simulation it was
 * given (the panel mounts one per newest succeeded result): the clinic
 * user clicks the share action, the app mints a share for the
 * simulation's OUTPUT object through the owner-facing sharing route
 * (createPatientShare over the app's one RequestFn -- go/sharing ships
 * no generated surface, so this call is this app's own typed seam,
 * share-api.ts), and the minted link appears in a read-only textbox the
 * practice can see and copy, with the expiry the server resolved shown
 * beside it. Every refusal resolves through the share action's
 * reachable-error whitelist (share-errors.ts) to bilingual text --
 * including the rbac gate's permission_denied, the honest answer for a
 * practice member whose role holds no sharing:create -- never a raw
 * code. The control keeps its own phase state keyed to the simulation
 * it was mounted for; a newer simulation replaces it by remount.
 */

import { useState } from 'react'
import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Typography from '@mui/material/Typography'
import type { SmilesimSimulation } from '@speed/api-sdk'
import { useTranslation } from '@speed/i18n'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { useAppServices } from '../app-services.js'
import { useDateFormatter } from '../use-date-formatter.js'
import { createPatientShare } from '../share-api.js'
import {
  shareActionErrorTextKey,
  shareErrorCodeOf,
} from '../share-errors.js'

/** The share fragment a patient link points at: the app's hash router
 * parses /#/share/<token> into the patient page (app.tsx). */
export const SHARE_FRAGMENT_PREFIX = '/share/'

/** The absolute URL of the share fragment under this page's own origin
 * and path -- the value the practice sends the patient. The fragment
 * carries the leading '#' (the hash-routed form the app's own
 * navigation uses), so opening the link lands on the SPA's share
 * fragment rather than on a server path. */
export function shareLinkUrl(token: string): string {
  return new URL(`#${SHARE_FRAGMENT_PREFIX}${token}`, window.location.href)
    .href
}

/**
 * The clinic's share control for one completed simulation (see the file
 * header). simulation must be a succeeded entry carrying an output
 * object id -- the panel only mounts it for one.
 */
export function SimulationShareAction({
  simulation,
}: {
  readonly simulation: SmilesimSimulation
}): ReactElement {
  const { t, i18n } = useTranslation(REFERENCE_APP_NAMESPACE)
  const { api } = useAppServices()
  const formatDate = useDateFormatter(i18n.language)
  // The output object id the share mints for. The panel mounts this
  // control only for a succeeded entry carrying one, but the spec's
  // wire type leaves the field pointer-shaped -- an entry without it
  // renders nothing rather than minting a share for an empty ref.
  const outputObjectId = simulation.output_object_id ?? ''

  /** The control's phase: idle (the action offered), creating (the mint
   * in flight), ready (the link is on screen) or refused (the mint's
   * coded answer, retryable). */
  const [phase, setPhase] = useState<
    | { readonly kind: 'idle' }
    | { readonly kind: 'creating' }
    | { readonly kind: 'ready'; readonly url: string; readonly expiresAt: string }
    | { readonly kind: 'refused'; readonly code: string }
  >({ kind: 'idle' })

  if (outputObjectId === '') {
    // Unreachable by the panel's own filter; the shape guard renders
    // nothing rather than a control that could mint for an empty ref.
    return <Box />
  }

  const mintShare = (): void => {
    if (phase.kind === 'creating') {
      return
    }
    setPhase({ kind: 'creating' })
    createPatientShare(api, outputObjectId).then(
      (created) => {
        setPhase({
          kind: 'ready',
          url: shareLinkUrl(created.token),
          expiresAt: created.share.expiresAt,
        })
      },
      (error: unknown) => {
        setPhase({ kind: 'refused', code: shareErrorCodeOf(error) })
      },
    )
  }

  return (
    <Box sx={{ marginTop: 2 }}>
      {phase.kind === 'ready' ? (
        <>
          {/* The copyable link itself: a read-only text input whose
          accessible name is the link label (the acceptance gate reads
          it as the surface's copyable link) and whose whole value
          selects on focus, so one click-and-copy hands the patient
          the URL. */}
          <input
            type="text"
            readOnly
            value={phase.url}
            aria-label={t('cases.share.linkLabel')}
            onFocus={(event) => event.currentTarget.select()}
            style={{ width: '100%', maxWidth: 420 }}
          />
          <Typography variant="caption" color="text.secondary">
            {t('cases.share.expiresOn', { date: formatDate(phase.expiresAt) })}
          </Typography>
        </>
      ) : (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 2 }}>
          <Button
            variant="outlined"
            onClick={mintShare}
            disabled={phase.kind === 'creating'}
          >
            {t('cases.share.action')}
          </Button>
          {phase.kind === 'refused' && (
            <Typography variant="body2" role="alert" color="error">
              {t(shareActionErrorTextKey(phase.code))}
            </Typography>
          )}
        </Box>
      )}
    </Box>
  )
}
