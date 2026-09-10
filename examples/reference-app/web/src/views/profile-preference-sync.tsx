/**
 * profile-preference-sync.tsx -- the host's bridge from the signed-in
 * account's stored language preference to the i18n instance, mounted
 * once inside the app frame.
 *
 * The frontend negotiation chain's profile tier is a creation-time
 * input createI18n cannot have when the host bootstraps the page (the
 * profile arrives from /me/preferences AFTER sign-in), so the host
 * applies it here through the chain's one live-instance API --
 * switchLanguage in its NON-persisting form (`switchLanguage(i18n,
 * locale, null)`): the persisted manual slot belongs to the user's own
 * language-switch choices, and writing the profile into it would
 * permanently shadow later profile changes in that browser (the
 * @speed/i18n rule).
 *
 * The sync applies the profile ONLY when the higher chain tiers have
 * decided nothing: a `?lang=` URL override or a non-empty manual slot
 * is the person's explicit current choice and must outrank the stored
 * profile exactly as it does at creation. The applied value is
 * remembered in a ref, so a re-render or a query refetch never re-runs
 * the switch -- the instance is the state, and a switch the user makes
 * afterwards (the settings surface) must not be fought by this effect.
 */
import { useEffect, useRef } from 'react'
import type { ReactElement } from 'react'
import {
  useTranslation,
  switchLanguage,
  readSupportedLanguages,
  SPEED_LOCALE_STORAGE_KEY,
} from '@speed/i18n'
import { useAuthState } from '@speed/auth-core'
import {
  getAuthnGetPreferencesQueryKey,
  useAuthnGetPreferences,
} from '@speed/api-sdk'
import { PROFILE_PREFERENCES_STALE_MS } from '../use-preferred-time-zone.js'

/** Whether the URL's own language parameter names a choice. The app
 * leaves the parameter name at the package default ('lang'). */
function urlDecidesLanguage(): boolean {
  try {
    const value = new URLSearchParams(window.location.search).get('lang')
    return value !== null && value.trim() !== ''
  } catch {
    return false
  }
}

/** Whether the manual-choice slot holds a decision. Guarded: a storage
 * that denies reads (embedded contexts) decides nothing. */
function manualSlotDecidesLanguage(): boolean {
  try {
    const stored = window.localStorage.getItem(SPEED_LOCALE_STORAGE_KEY)
    return stored !== null && stored.trim() !== ''
  } catch {
    return false
  }
}

/** Applies the stored profile language to the live instance once (see
 * the header). Renders nothing. */
export function ProfilePreferenceSync(): ReactElement | null {
  const { i18n } = useTranslation()
  // The preferences read is an authenticated endpoint: gate it on a
  // signed-in principal so the sign-in screen never fires a request
  // that could only 401, and the read starts the moment the session
  // does.
  const authenticated = useAuthState().principal != null
  const { data } = useAuthnGetPreferences({
    query: {
      queryKey: getAuthnGetPreferencesQueryKey(),
      enabled: authenticated,
      // The session-scale freshness window the views share (see
      // use-preferred-time-zone.ts): one read per session, refreshed by
      // the settings surface's own invalidation.
      staleTime: PROFILE_PREFERENCES_STALE_MS,
    },
  })
  const applied = useRef<string | null>(null)

  const profileLocale = data?.locale?.trim() ?? ''
  useEffect(() => {
    if (profileLocale === '' || applied.current === profileLocale) {
      return
    }
    // A stored value this instance cannot speak (a deployment whose
    // catalogs and instance sets differ) is nothing to switch to:
    // switching would reject, and the device choice already stands.
    if (!readSupportedLanguages(i18n).includes(profileLocale)) {
      applied.current = profileLocale
      return
    }
    if (urlDecidesLanguage() || manualSlotDecidesLanguage()) {
      return
    }
    applied.current = profileLocale
    // Non-persisting: see the header. Nothing is persisted, so there is
    // no storage failure to absorb; the instance reports a refused
    // switch itself.
    void switchLanguage(i18n, profileLocale, null)
  }, [i18n, profileLocale])

  return null
}
