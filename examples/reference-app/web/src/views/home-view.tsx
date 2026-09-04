/**
 * home-view.tsx -- the first real-data surface of the app: everything
 * on it renders from the server's Public config answer, never from a
 * local copy. The heading is the served brand name (the same
 * useBrandName every surface reads), the intro is app copy, and each
 * feature card exists only while the flag that gates it is on -- the
 * card list is data-driven over go/config's feature list, resolved
 * per key through useFeature's composition on the shared Public
 * config cache (one fetch serves the header brand, this view's brand
 * and both flag checks).
 *
 * The two demo flags exercise both shapes the module ships: a plain
 * flag (ai.smile_preview) and a dependent one (ai.premium_upsell,
 * enabled only while its dependency is), so a viewer here sees the
 * feature set the server actually resolved for the tenant.
 */

import type { ReactElement } from 'react'
import { Box, Card, CardContent, Typography } from '@mui/material'
import { useFeature } from '@speed/api-client/react'
import { useTranslation } from '@speed/i18n'
import { useAppServices, useBrandName } from '../app-services.js'
import { REFERENCE_APP_NAMESPACE } from '../resources.js'

/** The demo flag keys, as the reference app's own notes module declares
 * them -- a hand-kept host copy of internal/notes/module.go's exported
 * FeatureFlagSmilePreview / FeatureFlagPremiumUpsell constants, reused
 * by the suites that script feature lists. No cross-language import
 * exists to pin the mirror, so drift is fail-closed by direction only:
 * a server-side rename would hide the card silently (useFeature answers
 * false), surfacing only against the real server on the M4 leg. */
export const FEATURE_SMILE_PREVIEW = 'ai.smile_preview'
export const FEATURE_PREMIUM_UPSELL = 'ai.premium_upsell'

interface HomeFeatureCard {
  readonly featureKey: string
  readonly titleKey: string
  readonly descriptionKey: string
}

/** The demo's feature cards: each keyed to one server flag, every text
 * slot a key of the app namespace. */
const FEATURE_CARDS: readonly HomeFeatureCard[] = [
  {
    featureKey: FEATURE_SMILE_PREVIEW,
    titleKey: 'features.smilePreview.title',
    descriptionKey: 'features.smilePreview.description',
  },
  {
    featureKey: FEATURE_PREMIUM_UPSELL,
    titleKey: 'features.premiumUpsell.title',
    descriptionKey: 'features.premiumUpsell.description',
  },
]

/** The signed-in landing surface: brand and features from config. */
export function HomeView(): ReactElement {
  const { api } = useAppServices()
  const { t } = useTranslation(REFERENCE_APP_NAMESPACE)
  const brand = useBrandName()
  // Each check reads the shared per-api config cache; the hooks
  // compose instead of fetching twice (useFeature's own contract).
  const smilePreviewOn = useFeature(api, FEATURE_SMILE_PREVIEW)
  const premiumUpsellOn = useFeature(api, FEATURE_PREMIUM_UPSELL)
  const enabled = FEATURE_CARDS.filter((card) => {
    switch (card.featureKey) {
      case FEATURE_SMILE_PREVIEW:
        return smilePreviewOn
      case FEATURE_PREMIUM_UPSELL:
        return premiumUpsellOn
      default:
        return false
    }
  })

  return (
    <Box sx={{ p: 3, maxWidth: 720 }}>
      <Typography component="h1" variant="h4" sx={{ fontWeight: 600 }}>
        {brand}
      </Typography>
      <Typography variant="body1" sx={{ mt: 1, color: 'text.secondary' }}>
        {t('home.intro')}
      </Typography>
      {enabled.length > 0 && (
        <>
          <Typography component="h2" variant="h6" sx={{ mt: 4 }}>
            {t('features.heading')}
          </Typography>
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, mt: 2 }}>
            {enabled.map((card) => (
              <Card key={card.featureKey} variant="outlined">
                <CardContent>
                  <Typography variant="subtitle1" sx={{ fontWeight: 600 }}>
                    {t(card.titleKey)}
                  </Typography>
                  <Typography
                    variant="body2"
                    sx={{ mt: 0.5, color: 'text.secondary' }}
                  >
                    {t(card.descriptionKey)}
                  </Typography>
                </CardContent>
              </Card>
            ))}
          </Box>
        </>
      )}
    </Box>
  )
}
