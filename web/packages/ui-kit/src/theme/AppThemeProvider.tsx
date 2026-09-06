/**
 * AppThemeProvider: composes the MUI theme runtime for an app.
 *
 * It owns three concerns:
 *
 * 1. Theme assembly -- createAppTheme(projectTokens, tenantOverrides)
 *    merges the three token layers; the provider memoizes the result so
 *    the merged tree and the locale-free theme are built once per
 *    override set (hosts that rebuild override objects every render
 *    should memoize them, or pass module-level constants).
 * 2. MUI locale linkage -- the provider merges the MUI locale matching
 *    the active i18n language onto the base theme (createTheme's second
 *    argument), and subscribes to 'languageChanged' so MUI built-in
 *    texts (table pagination labels, date picker strings, ...) follow
 *    every language switch. The locale merge lands after the token
 *    mapping, so MUI component defaults never overwrite theme tokens.
 * 3. Global reset -- CssBaseline renders the theme-aware baseline
 *    (background color from theme.palette.background.default, no body
 *    margin) once, under the same theme context as the children.
 *
 * The provider deliberately does NOT render I18nextProvider: namespace
 * registration and the provider tree belong to the host bootstrap, and
 * this component neither registers nor owns any namespace. It takes the
 * app's i18n instance as a prop and reads only its language events.
 */

import { useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { createTheme, ThemeProvider } from '@mui/material/styles'
import CssBaseline from '@mui/material/CssBaseline'
import { enUS } from '@mui/material/locale'
import type { Localization } from '@mui/material/locale'
import { muiLocaleFor } from '@speed/i18n/mui-locale'
import type { I18nInstance } from '@speed/i18n'
import type { TokensOverride } from '@speed/tokens'
import { createAppTheme } from './createAppTheme.js'

/**
 * The MUI localization for the active language, or the en-US MUI locale
 * when the language has no entry in the MUI locale table. muiLocaleFor
 * deliberately throws on unknown tags for direct callers who want a loud
 * failure at theme-assembly time; a render path must never throw, because
 * createI18n's supported set is host-chosen and can include languages
 * beyond the two MUI mappings this package ships. MUI's own built-in
 * texts (table pagination labels, tooltips, ...) then fall back to
 * English -- a documented default -- while the app's translations keep
 * rendering in the active language.
 */
function muiLocaleForOrDefault(language: string): Localization {
  try {
    return muiLocaleFor(language)
  } catch {
    return enUS
  }
}

export interface AppThemeProviderProps {
  /** The app's i18n instance (createI18n result). MUI built-in texts follow its language. */
  readonly i18n: I18nInstance
  /** Project-layer token overrides, applied over the speed defaults. */
  readonly projectTokens?: TokensOverride
  /** Tenant-layer token overrides, applied over the project layer. */
  readonly tenantOverrides?: TokensOverride
  readonly children?: ReactNode
}

/**
 * Wrap an app's tree with the speed theme: MUI ThemeProvider under the
 * theme built from the merged tokens, locale-aware (language switches
 * re-merge the MUI locale), plus the theme-aware CssBaseline.
 */
export function AppThemeProvider({
  i18n,
  projectTokens,
  tenantOverrides,
  children,
}: AppThemeProviderProps) {
  const base = useMemo(
    () => createAppTheme(projectTokens, tenantOverrides),
    [projectTokens, tenantOverrides],
  )
  const [language, setLanguage] = useState(i18n.language)
  useEffect(() => {
    const onLanguageChanged = (next: string) => setLanguage(next)
    i18n.on('languageChanged', onLanguageChanged)
    return () => {
      i18n.off('languageChanged', onLanguageChanged)
    }
  }, [i18n])
  // MUI's built-in texts follow the active language. A language outside
  // the MUI locale table must never throw mid-render: createI18n supports
  // host-chosen canonical sets that can exceed the MUI mappings, so such a
  // language falls back to the en-US MUI locale (see muiLocaleForOrDefault)
  // instead of white-screening the tree.
  const theme = useMemo(
    () => createTheme(base.theme, muiLocaleForOrDefault(language)),
    [base, language],
  )

  return (
    <ThemeProvider theme={theme}>
      <CssBaseline />
      {children}
    </ThemeProvider>
  )
}
