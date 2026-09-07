/**
 * share-view.tsx -- the block-C patient surface: the page a share link
 * opens, rendered for anyone holding the link, signed in or not. The
 * page carries one image -- the shared smile simulation -- loaded
 * straight from go/sharing's public access route
 * (/api/v1/sharing/access?token=..., the genuinely unauthenticated
 * route this host allowlists), so the bytes the browser decodes are the
 * bytes the share resolves, never a blob the page fetched under a
 * session: the block-B blob pattern cannot work here, because a patient
 * holds no session and the simulation-content route is an authenticated
 * surface. A granted load shows the image; a refused one -- an expired
 * or revoked link, a visitor over the route's rate budget, a simulation
 * object storage can no longer open -- shows one human message in the
 * page's own language, resolved from the refusal envelope's code
 * through the patient page's reachable-error whitelist
 * (share-errors.ts), never a broken frame and never a crash.
 *
 * How the refusal is learned: the browser's image load itself carries
 * no answer body a page can read, so when the image fails the page
 * asks the same route once more through the app's one RequestFn
 * (credential-less by declaration -- the visitor holds no session).
 * A refused answer carries the envelope and its code; an answer of raw
 * bytes (the request function cannot parse an image as JSON and
 * refuses it as client.protocol) means the share IS live and the
 * failed image load was transient, so the image is remounted for one
 * retry before any message is shown. The happy path is exactly one
 * request -- the image load itself -- so a live share is never
 * double-counted as two views by this page.
 */

import { useState } from 'react'
import type { ReactElement } from 'react'
import Box from '@mui/material/Box'
import Typography from '@mui/material/Typography'
import { useTranslation } from '@speed/i18n'
import { useAppServices } from '../app-services.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'
import { SHARE_ACCESS_PATH } from '../share-api.js'
import { shareErrorCodeOf, shareViewErrorTextKey } from '../share-errors.js'

/** The number of image-load retries the page grants a live share whose
 * load failed transiently before the transport message is shown. */
const MAX_IMAGE_RETRIES = 1

/** One shared simulation's phase: the image on its current attempt, or
 * the refusal message a failed load resolved to. */
type ShareViewPhase =
  | { readonly kind: 'image'; readonly attempt: number }
  | { readonly kind: 'refused'; readonly code: string }

/** The patient-facing page one share link opens (see the file header). */
export function ShareView({ token }: { readonly token: string }): ReactElement {
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const { api } = useAppServices()
  const [phase, setPhase] = useState<ShareViewPhase>({
    kind: 'image',
    attempt: 0,
  })

  const accessUrl = `${window.location.origin}${SHARE_ACCESS_PATH}?token=${encodeURIComponent(token)}`

  /** Diagnoses one failed image load: asks the access route why. The
   * answer drives the phase -- a refusal envelope shows its code's
   * text, while a live-share answer (raw bytes the request function
   * cannot parse, refused as client.protocol) remounts the image for
   * one retry. Guarded by the attempt that failed, so a stale answer
   * can never overwrite a newer phase. */
  const diagnose = (attempt: number): void => {
    api<unknown>(SHARE_ACCESS_PATH, {
      query: { token },
      omitAccessToken: true,
    }).then(
      () => {
        // Unreachable in practice: a granted access answers raw image
        // bytes, which this client refuses as client.protocol rather
        // than resolving. Kept as a defensive no-op so a future change
        // in that contract degrades to the transport message, never a
        // hang.
        setPhase((current) =>
          current.kind === 'image' && current.attempt === attempt
            ? { kind: 'refused', code: 'client.protocol' }
            : current,
        )
      },
      (error: unknown) => {
        const code = shareErrorCodeOf(error)
        setPhase((current) => {
          if (current.kind !== 'image' || current.attempt !== attempt) {
            return current
          }
          if (code === 'client.protocol' && attempt < MAX_IMAGE_RETRIES) {
            // The share is live -- the access route answered real
            // bytes -- so the failed image load was transient; retry.
            return { kind: 'image', attempt: attempt + 1 }
          }
          return { kind: 'refused', code }
        })
      },
    )
  }

  return (
    <Box
      component="main"
      sx={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        padding: { xs: 3, sm: 6 },
        maxWidth: 720,
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
      {phase.kind === 'refused' ? (
        <Typography
          variant="body1"
          role="alert"
          sx={{ marginTop: 4, maxWidth: 460 }}
        >
          {t(shareViewErrorTextKey(phase.code))}
        </Typography>
      ) : (
        <Box
          component="img"
          key={phase.attempt}
          src={accessUrl}
          alt={t('shareView.imageAlt')}
          onError={() => diagnose(phase.attempt)}
          sx={{
            display: 'block',
            marginTop: 4,
            maxWidth: '100%',
            maxHeight: 480,
            objectFit: 'contain',
            borderRadius: 1,
            border: '1px solid',
            borderColor: 'divider',
          }}
        />
      )}
    </Box>
  )
}
