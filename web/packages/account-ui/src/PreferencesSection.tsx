/**
 * PreferencesSection: the language-and-timezone surface of the account
 * page.
 *
 * Two selects, both editing the account's own stored preferences through
 * GET/PATCH /api/v1/authn/me/preferences:
 *
 *  - Language: the options are the i18n instance's OWN supported set
 *    (readSupportedLanguages), so a host whose instance speaks a wider or
 *    narrower set gets exactly the choices its catalogs can render;
 *    option labels are the language names in the CURRENT UI language
 *    (Intl.DisplayNames, falling back to the raw tag when the engine
 *    cannot name it). Choosing one PATCHes the account first and, only on
 *    success, runs the persisting switchLanguage -- the settings switch
 *    lands the decision on the device (the manual-choice slot) and the
 *    account together, which is the one place both are meant to move.
 *    Clearing the preference (the "not chosen" choice, the empty string
 *    the server stores as no choice) PATCHes the account alone: the
 *    empty string is not a language the instance can switch to, so the
 *    device keeps the language it is showing.
 *  - Timezone: the options are the engine's IANA list
 *    (Intl.supportedValuesOf('timeZone'), guarded) plus an explicit
 *    "not chosen" choice whose value is the empty string -- the stored
 *    clearing value, rendered per this module's API contract (an empty
 *    field IS "not chosen yet", so a 200 without fields is a valid state,
 *    not an error). The stored value is guaranteed present in the option
 *    list: a value the engine's list omits (a zone removed from newer
 *    tzdata, a value written by another engine) is added as its own
 *    option, because a select whose value matches no option would silently
 *    display the wrong one.
 *
 * Both writes are immediate (no submit button) and PATCH-first: the
 * server is asked before anything moves locally, so a refused write
 * leaves the controls reading the stored values -- the natural revert --
 * and renders the two-line failure alert: the save line plus the
 * resolved code text (authn.invalid_locale / authn.invalid_timezone when
 * the server refuses a value, the transport codes when the request never
 * landed). Both selects are disabled while a write is in flight, so a
 * change can never race its own predecessor.
 *
 * The section takes no props -- whose preferences these are comes from
 * the caller's bound client and its access token -- and follows the
 * account surfaces' settled-state paradigm: an unresolved load keeps a
 * loading placeholder (one announceable status row), a settled load with
 * no data renders the ui-kit EmptyState error variant with a retry, and
 * the settled-with-data state always renders both controls (both fields
 * absent means both are "not chosen", which each control displays as
 * such).
 */

import { useMemo, useState } from 'react'
import Alert from '@mui/material/Alert'
import Box from '@mui/material/Box'
import MenuItem from '@mui/material/MenuItem'
import Skeleton from '@mui/material/Skeleton'
import TextField from '@mui/material/TextField'
import Typography from '@mui/material/Typography'
import { useQueryClient } from '@tanstack/react-query'
import {
  getAuthnGetPreferencesQueryKey,
  useAuthnGetPreferences,
  useAuthnUpdatePreferences,
} from '@speed/api-sdk'
import { readSupportedLanguages, switchLanguage } from '@speed/i18n'
import { AsyncSection, errorCodeOf } from '@speed/ui-kit'
import { useAccountUiErrorText } from './internal/error-text.js'

import { useAccountUiTranslation } from './internal/translation.js'

/** The language name to show for a tag, in the current UI language; the
 * raw tag is the fallback for an engine without DisplayNames (guarded --
 * an old engine throws on the constructor, and a missing name must never
 * crash a select). */
function languageDisplayName(tag: string, uiLanguage: string): string {
  try {
    const names = new Intl.DisplayNames([uiLanguage], { type: 'language' })
    return names.of(tag) ?? tag
  } catch {
    return tag
  }
}

/** The engine's IANA timezone list, sorted, or [] when the environment
 * cannot produce one (Intl.supportedValuesOf is newer than the rest of
 * Intl, so its absence is a real deployment shape): the select then
 * offers the "not chosen" choice plus -- via the stored-value guard in
 * the renderer -- whatever value the account already holds, never a
 * fabricated list. */
function supportedTimeZones(): readonly string[] {
  try {
    const values = Intl.supportedValuesOf('timeZone')
    return [...values].sort()
  } catch {
    return []
  }
}

export function PreferencesSection() {
  const { t, i18n } = useAccountUiTranslation()
  const errorText = useAccountUiErrorText()
  const queryClient = useQueryClient()
  const { data, isPending, refetch } = useAuthnGetPreferences()
  const updatePreferences = useAuthnUpdatePreferences()
  const [failureCode, setFailureCode] = useState<string | null>(null)

  const languageOptions = useMemo(
    () => readSupportedLanguages(i18n),
    [i18n],
  )
  const timeZoneOptions = useMemo(() => supportedTimeZones(), [])

  const pending = isPending && data === undefined
  const saving = updatePreferences.isPending

  const storedLocale = data?.locale ?? ''
  const storedTimezone = data?.timezone ?? ''

  // The stored timezone is always selectable, even when the engine's list
  // omits it (see the header): the option list is the engine's, plus the
  // stored value when absent from it.
  const timeZoneChoices = useMemo(() => {
    if (storedTimezone !== '' && !timeZoneOptions.includes(storedTimezone)) {
      return [storedTimezone, ...timeZoneOptions]
    }
    return timeZoneOptions
  }, [storedTimezone, timeZoneOptions])

  /** PATCH-first write: the server answers before anything local moves,
   * so a refusal needs no revert (the controls read the stored values)
   * and only the failure alert renders. */
  async function saveLocale(locale: string): Promise<void> {
    setFailureCode(null)
    try {
      await updatePreferences.mutateAsync({ data: { locale } })
      // The empty string IS the clearing value, not a language: a
      // successful clear leaves the device on its current language, so
      // only a real choice is switched to (switchLanguage refuses the
      // empty string, which must not read as a failed save).
      if (locale !== '') {
        // The account accepted the choice; land it on the device too (the
        // persisting form -- this is the manual language-switch UI). A
        // storage failure inside switchLanguage is a console warning, never
        // a rejection: the switch itself still happens.
        await switchLanguage(i18n, locale)
      }
      await queryClient.invalidateQueries({
        queryKey: getAuthnGetPreferencesQueryKey(),
      })
    } catch (error) {
      setFailureCode(errorCodeOf(error))
    }
  }

  async function saveTimezone(timezone: string): Promise<void> {
    setFailureCode(null)
    try {
      await updatePreferences.mutateAsync({ data: { timezone } })
      await queryClient.invalidateQueries({
        queryKey: getAuthnGetPreferencesQueryKey(),
      })
    } catch (error) {
      setFailureCode(errorCodeOf(error))
    }
  }

  return (
    <Box>
      <Typography variant="h5" component="h2" sx={{ mb: 2 }}>
        {t('preferences.title')}
      </Typography>

      <AsyncSection
        pending={pending}
        payload={data}
        empty={false}
        loading={
          <Box
            role="status"
            aria-label={t('preferences.loading')}
            aria-busy="true"
          >
            <Skeleton variant="text" width="42%" />
            <Skeleton variant="text" width="58%" />
          </Box>
        }
        errorState={{
          title: t('preferences.error.title'),
          description: t('preferences.error.description'),
          retryLabel: t('preferences.retry'),
          onRetry: () => void refetch(),
        }}
      >
        {() => (
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2.5 }}>
            {failureCode !== null && (
              <Alert severity="error">
                {/* The two-line failure alert: what happened, then the
                    server's own code text (the whitelist resolves
                    authn.invalid_locale / authn.invalid_timezone and the
                    transport codes; anything else reads the generic line). */}
                <Typography variant="body2">
                  {t('preferences.saveFailed')}
                </Typography>
                <Typography variant="body2">
                  {errorText(failureCode)}
                </Typography>
              </Alert>
            )}

            <TextField
              select
              fullWidth
              // displayEmpty rides the Select slot (MUI 9's slotProps
              // architecture): the "not chosen" choice carries the empty
              // string, and without this the control renders blank instead
              // of the option's own label.
              slotProps={{ select: { displayEmpty: true } }}
              label={t('preferences.language.label')}
              value={storedLocale}
              disabled={saving}
              onChange={(event) => {
                void saveLocale(event.target.value)
              }}
              helperText={t('preferences.language.helper')}
            >
              {/* The "not chosen" choice first: an empty stored locale is
               * the account's real initial state, and the chain's device
               * and default tiers are what actually apply until one is
               * chosen. */}
              <MenuItem value="">
                {t('preferences.language.notChosen')}
              </MenuItem>
              {languageOptions.map((option) => (
                <MenuItem key={option} value={option}>
                  {languageDisplayName(option, i18n.language)}
                </MenuItem>
              ))}
            </TextField>

            <TextField
              select
              fullWidth
              slotProps={{ select: { displayEmpty: true } }}
              label={t('preferences.timezone.label')}
              value={storedTimezone}
              disabled={saving}
              onChange={(event) => {
                void saveTimezone(event.target.value)
              }}
              helperText={t('preferences.timezone.helper')}
            >
              <MenuItem value="">
                {t('preferences.timezone.notChosen')}
              </MenuItem>
              {timeZoneChoices.map((option) => (
                <MenuItem key={option} value={option}>
                  {option}
                </MenuItem>
              ))}
            </TextField>
          </Box>
        )}
      </AsyncSection>
    </Box>
  )
}
