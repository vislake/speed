/**
 * use-preferred-time-zone.ts -- the display timezone every date-rendering
 * surface of this app formats with: the signed-in account's stored
 * timezone preference (GET /api/v1/authn/me/preferences, empty meaning
 * "not chosen"), else the device's own zone, else UTC -- the same
 * three-tier chain @speed/account-ui's surfaces observe, resolved here
 * for the app's own views (the package's internal helper is not part of
 * its public surface, and same-tier packages never import each other's
 * internals).
 *
 * The value is always usable as an Intl `timeZone` option: a stored
 * value the engine rejects (stale tzdata, a hand-edited row) is skipped
 * by the guarded probe, never thrown into a render. The preference read
 * is an ordinary generated-hooks query shared across the views by
 * react-query; before it settles -- and for an anonymous caller the
 * read 401s straight into react-query's error state -- the chain
 * degrades to the device zone exactly as "not chosen" does.
 */
import {
  getAuthnGetPreferencesQueryKey,
  useAuthnGetPreferences,
} from '@speed/api-sdk'

/** The terminal tier: the platform default timezone, matching the
 * backend's own default, so a viewer with no stored choice and no
 * resolvable device zone still renders in a defined zone. */
export const FALLBACK_TIME_ZONE = 'UTC'

/**
 * How long the profile preference read counts as fresh, for the app's
 * two consumers (this hook and the language sync). The pair changes
 * only through the settings surface, which invalidates the query key
 * after a successful PATCH (and invalidation refetches regardless of
 * staleness), so a per-view-mount refetch would only repeat a read
 * whose answer this session already holds -- and the whole cache is
 * emptied at session end (the app's shipped session-end strategy), so
 * a new sign-in always reads its own account's row fresh. A
 * session-scale window keeps the display data stable without ever
 * serving a previous account's row.
 */
export const PROFILE_PREFERENCES_STALE_MS = 5 * 60 * 1000

/** Reports whether the engine accepts `zone` as a timezone name (the
 * probe constructor validates eagerly -- RangeError on an unknown
 * name). */
function isSupportedTimeZone(zone: string): boolean {
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: zone })
    return true
  } catch {
    return false
  }
}

/** The device's IANA timezone, or null when the environment cannot
 * answer. Guarded: a resolution failure degrades to the next tier. */
function deviceTimeZone(): string | null {
  try {
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone
    return zone === '' ? null : (zone ?? null)
  } catch {
    return null
  }
}

/** The display timezone for the signed-in frame (see the header for the
 * chain); safe to call from any view, anonymous ones included. */
export function usePreferredTimeZone(): string {
  const { data } = useAuthnGetPreferences({
    query: {
      queryKey: getAuthnGetPreferencesQueryKey(),
      staleTime: PROFILE_PREFERENCES_STALE_MS,
    },
  })
  const profile = data?.timezone?.trim() ?? ''
  if (profile !== '' && isSupportedTimeZone(profile)) {
    return profile
  }
  const device = deviceTimeZone()
  if (device !== null && isSupportedTimeZone(device)) {
    return device
  }
  return FALLBACK_TIME_ZONE
}
