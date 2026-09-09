/**
 * simulation-share-action.tsx -- the clinic-side share surface: the
 * control that turns one completed simulation into a patient-facing
 * link. It renders beneath the before/after comparison of the
 * simulation it was given (the panel mounts one per newest succeeded
 * result): the clinic user clicks the share action and the app mints a
 * PAIR of shares through the owner-facing sharing route -- one for the
 * BEFORE photo the simulation was generated from and one for the
 * simulation's OUTPUT object -- because the link's patient page shows
 * the before/after comparison, not the result alone (the shape the e2e
 * share journeys hold the patient page to: two images, both genuinely
 * decoded, never the same image twice). The minted link -- an absolute
 * URL into the app's own /#/share/<before>/<after> fragment -- appears
 * in a read-only textbox the practice can see and copy, with the
 * earlier of the two shares' expiries shown beside it (the link is
 * whole only until its first half expires).
 *
 * Failure is all-or-nothing and compensated: a mint where either half
 * is refused shows that refusal's bilingual text (never a link for a
 * pair that is only half there), and the half that did succeed is
 * revoked again on the spot -- the pair's two shares are minted for one
 * link, and a link that never appears must not leave a live,
 * unhanded-out share behind. Every refusal resolves through the share
 * action's reachable-error whitelist (share-errors.ts) to bilingual
 * text -- including the rbac gate's permission_denied, the honest
 * answer for a practice member whose role holds no sharing:create --
 * never a raw code. The control keeps its own phase state keyed to the
 * simulation it was mounted for; a newer simulation replaces it by
 * remount.
 */

import { useState } from 'react'
import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Typography from '@mui/material/Typography'
import type { SmilesimSimulation } from '../app-api/index.js'
import { useTranslation } from '@speed/i18n'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { useAppServices } from '../app-services.js'
import { useDateFormatter } from '../use-date-formatter.js'
import {
  createPatientShare,
  revokePatientShare,
} from '../share-api.js'
import {
  shareActionErrorTextKey,
  shareErrorCodeOf,
} from '../share-errors.js'

/** The share fragment a patient link points at: the app's hash router
 * parses /#/share/<beforeToken>/<afterToken> into the patient page
 * (app.tsx). */
export const SHARE_FRAGMENT_PREFIX = '/share/'

/** The absolute URL of the share fragment under this page's own origin
 * and path -- the value the practice sends the patient. The fragment
 * carries the leading '#' (the hash-routed form the app's own
 * navigation uses), so opening the link lands on the SPA's share
 * fragment rather than on a server path. The two tokens name the pair's
 * two shares, before first, in the order the patient page renders
 * them. */
export function shareLinkUrl(beforeToken: string, afterToken: string): string {
  return new URL(
    `#${SHARE_FRAGMENT_PREFIX}${beforeToken}/${afterToken}`,
    window.location.href,
  ).href
}

/**
 * The clinic's share control for one completed simulation (see the file
 * header). simulation must be a succeeded entry carrying an output
 * object id and photoObjectId must be the storage object id of the
 * original photo the simulation was generated from -- the before half
 * of the pair -- which the panel mounts the control with.
 */
export function SimulationShareAction({
  simulation,
  photoObjectId,
}: {
  readonly simulation: SmilesimSimulation
  readonly photoObjectId: string
}): ReactElement {
  const { t, i18n } = useTranslation(REFERENCE_APP_NAMESPACE)
  const { api } = useAppServices()
  const formatDate = useDateFormatter(i18n.language)
  // The output object id the after half of the pair mints for. The
  // panel mounts this control only for a succeeded entry carrying one,
  // but the spec's wire type leaves the field pointer-shaped -- an
  // entry without it renders nothing rather than minting a share for an
  // empty ref.
  const outputObjectId = simulation.output_object_id ?? ''

  /** The control's phase: idle (the action offered), creating (the pair
   * mint in flight), ready (the link is on screen) or refused (the
   * mint's coded answer, retryable). */
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
    // The pair, minted together: one share per half of the comparison
    // the patient page renders. Two independent requests because
    // go/sharing's model is one resource per share -- the module's
    // public route answers a share's raw bytes (io.Copy over the
    // resolved content), so the patient page loads each half from its
    // own share's access URL.
    const beforeMint = createPatientShare(api, photoObjectId)
    const afterMint = createPatientShare(api, outputObjectId)
    void Promise.allSettled([beforeMint, afterMint]).then((settled) => {
      const refused = settled.find(
        (result): result is PromiseRejectedResult => result.status === 'rejected',
      )
      if (refused === undefined) {
        const before = (settled[0] as PromiseFulfilledResult<Awaited<typeof beforeMint>>).value
        const after = (settled[1] as PromiseFulfilledResult<Awaited<typeof afterMint>>).value
        setPhase({
          kind: 'ready',
          url: shareLinkUrl(before.token, after.token),
          // The pair's lifetime is its shorter half's: the link stays
          // whole only until the first of the two shares stops being
          // accessible, and both are minted for the same tenant default.
          expiresAt:
            before.share.expiresAt < after.share.expiresAt
              ? before.share.expiresAt
              : after.share.expiresAt,
        })
        return
      }
      // All-or-nothing with compensation: whichever half of the pair
      // did mint is revoked again, so a refused share action never
      // leaves a live, unhanded-out link behind (best-effort -- a
      // revoke that itself fails leaves the orphan to the module's
      // default-expiry sweep). The refusal the surface shows is the
      // mint's own: the first half to fail, the before before the
      // after.
      const code = shareErrorCodeOf(refused.reason)
      const revocations: Promise<void>[] = []
      for (const result of settled) {
        if (result.status === 'fulfilled') {
          revocations.push(
            revokePatientShare(api, result.value.share.id).catch(() => {
              // Best-effort compensation: the orphan share expires
              // through the module's own default-expiry sweep.
            }),
          )
        }
      }
      void Promise.all(revocations).then(() => {
        setPhase({ kind: 'refused', code })
      })
    })
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
