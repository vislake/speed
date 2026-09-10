/**
 * Display-timezone resolution for the account surfaces.
 *
 * The chain, highest tier first: the account's stored timezone preference
 * (GET /api/v1/authn/me/preferences -- empty means "not chosen"), the
 * device's own timezone, then UTC as the terminal fallback. This is the
 * same shape every locale chain in the stack observes: a stored choice
 * outranks the environment's guess, and a platform default closes the
 * chain so a renderer ALWAYS has an explicit zone -- never the engine's
 * silent process-local default, which would make the same account render
 * differently depending on which machine ran the code.
 *
 * Every Intl construction here is guarded. A stored value can be stale or
 * foreign (written against a newer tzdata, hand-edited), and
 * `Intl.DateTimeFormat` throws a RangeError on a zone it does not know:
 * an unguarded constructor would turn one bad value into a rendering
 * crash on a security-relevant surface (the sessions list a user reads to
 * find a stolen device). A value the engine rejects is skipped, exactly
 * like an empty one, and the chain continues -- never a crash, and never
 * a silent substitution of a different zone for the chain's lower tiers.
 */

/** The terminal tier: the platform default timezone, matching the backend
 * default (`authn.DefaultTimezone`), so an account with no stored choice
 * and an environment with no answer renders in a defined zone. */
export const FALLBACK_TIME_ZONE = 'UTC'

/**
 * Reports whether the engine's Intl accepts `zone` as a timezone name.
 * The probe constructor is the acceptance test: `Intl.DateTimeFormat`
 * validates the zone eagerly (RangeError on an unknown name), so a
 * constructed formatter proves the zone is usable.
 */
export function isSupportedTimeZone(zone: string): boolean {
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: zone })
    return true
  } catch {
    return false
  }
}

/**
 * The device's own IANA timezone, or null when the environment cannot
 * answer (a test renderer without Intl, an answer that is empty). The read
 * is guarded for the same reason the chain guards its constructors: a
 * resolution failure must degrade to the next tier, never crash a render.
 */
export function deviceTimeZone(): string | null {
  try {
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone
    return zone === '' ? null : (zone ?? null)
  } catch {
    return null
  }
}

/**
 * Resolves the display timezone through the three-tier chain: the stored
 * profile value when present and accepted by the engine, else the device
 * timezone when the environment answers, else UTC. The returned value is
 * always usable as an `Intl.DateTimeFormat` `timeZone` option -- the
 * chain's guards have rejected everything the engine would refuse.
 */
export function resolveTimeZone(
  profileTimeZone: string | null | undefined,
): string {
  const profile = profileTimeZone?.trim() ?? ''
  if (profile !== '' && isSupportedTimeZone(profile)) {
    return profile
  }
  const device = deviceTimeZone()
  if (device !== null && isSupportedTimeZone(device)) {
    return device
  }
  return FALLBACK_TIME_ZONE
}
