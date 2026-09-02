/**
 * MUI locale linkage: translate a canonical language tag to the MUI
 * localization object createTheme consumes (createTheme(theme, zhCN)).
 * Isolated in its own module so the main entry never imports @mui/material
 * -- consumers without MUI get a working @speed/i18n instance without
 * pulling the MUI tree.
 */

import { zhCN, enUS, type Localization } from '@mui/material/locale'

/** The MUI localization object type (re-exported for host convenience). */
export type MuiLocale = Localization

/**
 * The MUI localization for a canonical language tag. Throws on unknown
 * tags: a silently wrong locale (English text inside a Chinese UI) is
 * worse than an immediate error at theme-assembly time.
 */
export function muiLocaleFor(language: string): MuiLocale {
  switch (language) {
    case 'zh-CN':
      return zhCN
    case 'en-US':
      return enUS
    default:
      throw new Error(
        `[speed-i18n] no MUI localization for language "${language}"; ` +
          'supported: zh-CN, en-US. Add the mapping here when the platform ' +
          'starts shipping another language.',
      )
  }
}
