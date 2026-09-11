/**
 * share-view.tsx -- the patient-facing surface a share link opens,
 * rendered for anyone holding the link, signed in or not. The page
 * carries the shared before/after pair side by side -- the original
 * photo and the smile simulation the clinic generated from it -- each
 * image loaded straight from go/sharing's public access route
 * (/api/v1/sharing/access?token=..., the genuinely unauthenticated
 * route this host allowlists) under its own half's share token, so the
 * bytes the browser decodes are the bytes each share resolves, never a
 * blob the page fetched under a session: the blob pattern the
 * authenticated case pages use cannot work here, because a patient
 * holds no session and the simulation-content route is an
 * authenticated surface. The two halves of the link are the two shares
 * the clinic's share action minted (views/simulation-share-action.tsx)
 * -- one resource per share is go/sharing's model -- and the e2e
 * journeys hold this page to the same pair the clinic's own comparison
 * shows: two images, both genuinely decoded, never the same image
 * twice.
 *
 * A refused link -- an expired or revoked share, a visitor over the
 * route's rate budget, a resource storage can no longer open -- shows
 * one human message in the page's own language, resolved from the
 * refusal envelope's code through the patient page's reachable-error
 * whitelist (share-errors.ts), never a broken frame and never a crash.
 *
 * How a refusal is learned: the browser's image load itself carries no
 * answer body a page can read, so when an image fails the page asks
 * that half's own route once more through the app's one RequestFn
 * (credential-less by declaration -- the visitor holds no session). A
 * refused answer carries the envelope and its code; an answer of raw
 * bytes (the request function cannot parse an image as JSON and
 * refuses it as client.protocol) means that share IS live and the
 * failed image load was transient, so the image is remounted for one
 * retry before any message is shown. The happy path is exactly two
 * requests -- the two image loads themselves -- so a live link is
 * never double-counted as four views by this page.
 */

import { useState } from 'react'
import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Typography from '@mui/material/Typography'
import { useTranslation } from '@speed/i18n'
import { useAppServices } from '../app-services.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { shareErrorCodeOf, shareViewErrorTextKey } from '../share-errors.js'

/** The number of image-load retries the page grants a live share whose
 * load failed transiently before the transport message is shown. */
const MAX_IMAGE_RETRIES = 1

/** GET /api/v1/sharing/access -- the module's public access route
 * (sharing.PathAccess, go/sharing/module.go), hand-kept here because
 * this page reaches it in the one way no generated operation can: as
 * an `<img>` src the browser navigates under its own credentials-free
 * rules, and as the credential-less diagnosis probe below. The probe
 * is registered residue of the app's RequestFn surface: the generated
 * mutator always attaches the session's access token when one exists
 * (@speed/api-sdk/runtime has no per-operation credential-less
 * channel), while this route must stay genuinely credential-less -- a
 * present-but-invalid bearer is refused even on this public route
 * (go/authn/middleware.go) -- and the page's whole point is that a
 * visitor holding no session, or one that died mid-visit, still gets
 * an honest answer. */
const SHARE_ACCESS_PATH = '/api/v1/sharing/access'

/** One half of the shared pair's phase: the image on its current
 * attempt, or the refusal message its failed load resolved to. */
type SidePhase =
  | { readonly kind: 'image'; readonly attempt: number }
  | { readonly kind: 'refused'; readonly code: string }

/** The pair's phases, one entry per half, keyed by the share's role in
 * the comparison. */
interface PairPhases {
  readonly before: SidePhase
  readonly after: SidePhase
}

type PairSide = keyof PairPhases

/** The patient-facing page one share link opens (see the file header).
 * beforeToken and afterToken are the two halves' share tokens, in the
 * order the clinic's share action put them in the link. */
export function ShareView({
  beforeToken,
  afterToken,
}: {
  readonly beforeToken: string
  readonly afterToken: string
}): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const { api } = useAppServices()
  const [phases, setPhases] = useState<PairPhases>({
    before: { kind: 'image', attempt: 0 },
    after: { kind: 'image', attempt: 0 },
  })

  const accessUrl = (token: string): string =>
    `${window.location.origin}${SHARE_ACCESS_PATH}?token=${encodeURIComponent(token)}`

  /** Diagnoses one failed image load: asks that half's access route
   * why. The answer drives that half's phase -- a refusal envelope
   * shows its code's text, while a live-share answer (raw bytes the
   * request function cannot parse, refused as client.protocol)
   * remounts the image for one retry. Guarded by the side and attempt
   * that failed, so a stale answer can never overwrite a newer phase. */
  const diagnose = (side: PairSide, attempt: number): void => {
    const token = side === 'before' ? beforeToken : afterToken
    api<unknown>(SHARE_ACCESS_PATH, {
      query: { token },
      omitAccessToken: true,
    }).then(
      () => {
        // Unreachable in practice: a granted access answers raw image
        // bytes, which this client refuses as client.protocol rather
        // than resolving. The branch stays as a defensive no-op, so a
        // change in that contract degrades to the transport message,
        // never a hang.
        setPhases((current) => {
          const phase = current[side]
          if (phase.kind !== 'image' || phase.attempt !== attempt) {
            return current
          }
          return { ...current, [side]: { kind: 'refused', code: 'client.protocol' } }
        })
      },
      (error: unknown) => {
        const code = shareErrorCodeOf(error)
        setPhases((current) => {
          const phase = current[side]
          if (phase.kind !== 'image' || phase.attempt !== attempt) {
            return current
          }
          if (code === 'client.protocol' && attempt < MAX_IMAGE_RETRIES) {
            // The share is live -- the access route answered real
            // bytes -- so the failed image load was transient; retry.
            return { ...current, [side]: { kind: 'image', attempt: attempt + 1 } }
          }
          return { ...current, [side]: { kind: 'refused', code } }
        })
      },
    )
  }

  // The page refuses as a whole: the link is one pair, and a half that
  // is gone makes the pair incomplete -- the first half to refuse names
  // the message, and a link whose halves died together (the ordinary
  // expiry) shows the same honest text either side would.
  const refusedCode =
    phases.before.kind === 'refused'
      ? phases.before.code
      : phases.after.kind === 'refused'
        ? phases.after.code
        : null

  const beforeImage = (phase: SidePhase): ReactElement => (
    <Box
      component="img"
      key={`before-${phase.kind === 'image' ? phase.attempt : ''}`}
      src={accessUrl(beforeToken)}
      alt={t('shareView.beforeImageAlt')}
      onError={() => {
        if (phase.kind === 'image') {
          diagnose('before', phase.attempt)
        }
      }}
      sx={{
        display: 'block',
        maxWidth: '100%',
        maxHeight: 420,
        objectFit: 'contain',
        borderRadius: 1,
        border: '1px solid',
        borderColor: 'divider',
      }}
    />
  )

  const afterImage = (phase: SidePhase): ReactElement => (
    <Box
      component="img"
      key={`after-${phase.kind === 'image' ? phase.attempt : ''}`}
      src={accessUrl(afterToken)}
      alt={t('shareView.afterImageAlt')}
      onError={() => {
        if (phase.kind === 'image') {
          diagnose('after', phase.attempt)
        }
      }}
      sx={{
        display: 'block',
        maxWidth: '100%',
        maxHeight: 420,
        objectFit: 'contain',
        borderRadius: 1,
        border: '1px solid',
        borderColor: 'divider',
      }}
    />
  )

  return (
    <Box
      component="main"
      sx={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        padding: { xs: 3, sm: 6 },
        maxWidth: 900,
        margin: '0 auto',
        textAlign: 'center',
      }}
    >
      <Typography variant="h4" component="h1">
        {t('shareView.heading')}
      </Typography>
      <Typography variant="body1" color="text.secondary" sx={{ marginTop: 1 }}>
        {t('shareView.intro')}
      </Typography>
      {refusedCode !== null ? (
        <Typography
          variant="body1"
          role="alert"
          sx={{ marginTop: 4, maxWidth: 460 }}
        >
          {t(shareViewErrorTextKey(refusedCode))}
        </Typography>
      ) : (
        <Box
          sx={{
            display: 'flex',
            gap: { xs: 2, sm: 4 },
            flexWrap: 'wrap',
            justifyContent: 'center',
            alignItems: 'flex-start',
            marginTop: 4,
          }}
        >
          <Box sx={{ maxWidth: 380 }}>{beforeImage(phases.before)}</Box>
          <Box sx={{ maxWidth: 380 }}>{afterImage(phases.after)}</Box>
        </Box>
      )}
    </Box>
  )
}
