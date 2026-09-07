/**
 * AppThemeProvider contract: provides the merged-token theme through MUI
 * context, merges the MUI locale of the active i18n language onto it,
 * follows 'languageChanged' (MUI built-in texts switch with the app
 * language), rebuilds when an override layer changes identity, and
 * renders the theme-aware baseline alongside the children.
 */

import { act, render } from '@testing-library/react'
import { useEffect } from 'react'
import { useTheme } from '@mui/material/styles'
import type { Theme } from '@mui/material/styles'
import { enUS, zhCN } from '@mui/material/locale'
import { describe, expect, it } from 'vitest'
import { createI18n, I18nextProvider, switchLanguage } from '@speed/i18n'
import { defaultTokens } from '@speed/tokens'
import type { TokensOverride } from '@speed/tokens'
import { AppThemeProvider } from './AppThemeProvider.js'
import { createUiKitI18n, renderWithProviders } from '../../test-utils/render.js'

/** Read the theme MUI context hands the tree at render time. */
function ThemeProbe({ onTheme }: { onTheme: (theme: Theme) => void }) {
  onTheme(useTheme())
  return null
}

function paginationLabel(theme: Theme | undefined): string | undefined {
  return theme?.components?.MuiTablePagination?.defaultProps?.labelRowsPerPage as
    | string
    | undefined
}

describe('AppThemeProvider', () => {
  it('provides the merged-token theme and applies the active MUI locale', () => {
    let seen: Theme | undefined
    const { i18n } = renderWithProviders(<ThemeProbe onTheme={(t) => (seen = t)} />)
    expect(i18n.language).toBe('zh-CN')
    expect(seen?.palette.primary.main).toBe(defaultTokens.color.semantic.primary.main)
    expect(paginationLabel(seen)).toBe(
      zhCN.components?.MuiTablePagination?.defaultProps?.labelRowsPerPage,
    )
  })

  it('re-merges the MUI locale when the language switches', async () => {
    let seen: Theme | undefined
    const { i18n } = renderWithProviders(<ThemeProbe onTheme={(t) => (seen = t)} />)
    const zhLabel = zhCN.components?.MuiTablePagination?.defaultProps?.labelRowsPerPage
    expect(paginationLabel(seen)).toBe(zhLabel)
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    expect(i18n.language).toBe('en-US')
    const enLabel = enUS.components?.MuiTablePagination?.defaultProps?.labelRowsPerPage
    expect(paginationLabel(seen)).toBe(enLabel)
    expect(paginationLabel(seen)).not.toBe(zhLabel)
  })

  it('applies project and tenant override layers, tenant winning per key', () => {
    const project: TokensOverride = {
      color: {
        semantic: {
          primary: { main: '#111111' },
          secondary: { main: '#222222' },
        },
      },
    }
    const tenant: TokensOverride = {
      color: { semantic: { primary: { main: '#333333' } } },
    }
    let seen: Theme | undefined
    renderWithProviders(<ThemeProbe onTheme={(t) => (seen = t)} />, {
      projectTokens: project,
      tenantOverrides: tenant,
    })
    expect(seen?.palette.primary.main).toBe('#333333')
    expect(seen?.palette.secondary.main).toBe('#222222')
    // Untouched branches keep the defaults.
    expect(seen?.palette.error.main).toBe(defaultTokens.color.semantic.error.main)
  })

  it('rebuilds the theme when an override layer changes identity', () => {
    const i18n = createUiKitI18n('zh-CN')
    const seen: Theme[] = []
    const first: TokensOverride = { color: { semantic: { primary: { main: '#AAAAAA' } } } }
    const tree = (projectTokens: TokensOverride | undefined) => (
      <I18nextProvider i18n={i18n}>
        <AppThemeProvider i18n={i18n} projectTokens={projectTokens}>
          <ThemeProbe onTheme={(t) => seen.push(t)} />
        </AppThemeProvider>
      </I18nextProvider>
    )
    const { rerender } = render(tree(first))
    expect(seen[0]?.palette.primary.main).toBe('#AAAAAA')
    const second: TokensOverride = { color: { semantic: { primary: { main: '#BBBBBB' } } } }
    rerender(tree(second))
    expect(seen[1]?.palette.primary.main).toBe('#BBBBBB')
  })

  it('stops following language changes after unmount', async () => {
    const i18n = createUiKitI18n('zh-CN')
    const { unmount } = renderWithProviders(<ThemeProbe onTheme={() => undefined} />, {
      i18n,
    })
    unmount()
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    expect(i18n.language).toBe('en-US')
  })

  it('picks up a language a child switches to inside its own mount effect (P1-4 regression)', async () => {
    // PRE-FIX (P1-4): AppThemeProvider snapshots i18n.language into state
    // at first render and registers its languageChanged listener in an
    // effect. React runs child effects before parent effects, so a child
    // that switches language inside its own mount effect emits the event
    // before the provider's listener exists -- the MUI built-in texts stay
    // on the snapshot language permanently. POST-FIX: (re)attaching the
    // listener re-adopts the instance's live language in the same effect,
    // so a switch that fired before the listener existed is recovered at
    // attach time instead of being lost.
    const i18n = createUiKitI18n('zh-CN')
    let seen: Theme | undefined
    function SwitchingChild() {
      useEffect(() => {
        void switchLanguage(i18n, 'en-US')
      }, [])
      return null
    }
    render(
      <I18nextProvider i18n={i18n}>
        <AppThemeProvider i18n={i18n}>
          <ThemeProbe onTheme={(t) => (seen = t)} />
          <SwitchingChild />
        </AppThemeProvider>
      </I18nextProvider>,
    )
    await act(async () => {})
    expect(i18n.language).toBe('en-US')
    const enLabel = enUS.components?.MuiTablePagination?.defaultProps?.labelRowsPerPage
    expect(paginationLabel(seen)).toBe(enLabel)
    const zhLabel = zhCN.components?.MuiTablePagination?.defaultProps?.labelRowsPerPage
    expect(paginationLabel(seen)).not.toBe(zhLabel)
  })

  it('adopts the language of a swapped-in i18n instance without waiting for an event (P1-4 regression)', () => {
    // PRE-FIX (P1-4): the effect re-subscribes to a new i18n instance when
    // the prop changes identity, but the language state still holds the
    // first instance's snapshot -- a new instance never emits
    // languageChanged for the language it was created on, so MUI built-in
    // texts stay in the old instance's language forever. POST-FIX: the
    // re-attach re-adopts the new instance's live language.
    const first = createUiKitI18n('zh-CN')
    const second = createUiKitI18n('en-US')
    const seen: Theme[] = []
    const tree = (i18n: ReturnType<typeof createUiKitI18n>) => (
      <I18nextProvider i18n={i18n}>
        <AppThemeProvider i18n={i18n}>
          <ThemeProbe onTheme={(t) => seen.push(t)} />
        </AppThemeProvider>
      </I18nextProvider>
    )
    const { rerender } = render(tree(first))
    const zhLabel = zhCN.components?.MuiTablePagination?.defaultProps?.labelRowsPerPage
    expect(paginationLabel(seen[0])).toBe(zhLabel)
    rerender(tree(second))
    expect(second.language).toBe('en-US')
    const enLabel = enUS.components?.MuiTablePagination?.defaultProps?.labelRowsPerPage
    expect(paginationLabel(seen.at(-1))).toBe(enLabel)
    expect(paginationLabel(seen.at(-1))).not.toBe(zhLabel)
  })

  it('renders children inside the theme context', () => {
    const { getByText } = renderWithProviders(<div>child content</div>)
    expect(getByText('child content')).toBeInTheDocument()
  })

  it('renders with a language outside the MUI locale table, using the en-US MUI locale fallback', () => {
    // createI18n's supported set is host-chosen and may include languages
    // the MUI locale table does not map; rendering must never throw for
    // one of them.
    const i18n = createI18n({
      supportedLanguages: ['zh-CN', 'en-US', 'fr-FR'],
      defaultLanguage: 'fr-FR',
      storage: null,
      urlParameterName: null,
      navigatorLanguages: [],
    })
    let seen: Theme | undefined
    expect(() =>
      render(
        <I18nextProvider i18n={i18n}>
          <AppThemeProvider i18n={i18n}>
            <ThemeProbe onTheme={(t) => (seen = t)} />
          </AppThemeProvider>
        </I18nextProvider>,
      ),
    ).not.toThrow()
    expect(i18n.language).toBe('fr-FR')
    expect(paginationLabel(seen)).toBe(
      enUS.components?.MuiTablePagination?.defaultProps?.labelRowsPerPage,
    )
  })

  it('re-merges with the fallback MUI locale when switching into a language outside the table', async () => {
    const i18n = createI18n({
      supportedLanguages: ['zh-CN', 'en-US', 'fr-FR'],
      defaultLanguage: 'zh-CN',
      storage: null,
      urlParameterName: null,
      navigatorLanguages: [],
    })
    let seen: Theme | undefined
    render(
      <I18nextProvider i18n={i18n}>
        <AppThemeProvider i18n={i18n}>
          <ThemeProbe onTheme={(t) => (seen = t)} />
        </AppThemeProvider>
      </I18nextProvider>,
    )
    const zhLabel = zhCN.components?.MuiTablePagination?.defaultProps?.labelRowsPerPage
    expect(paginationLabel(seen)).toBe(zhLabel)
    await act(async () => {
      await switchLanguage(i18n, 'fr-FR')
    })
    expect(i18n.language).toBe('fr-FR')
    const enLabel = enUS.components?.MuiTablePagination?.defaultProps?.labelRowsPerPage
    expect(paginationLabel(seen)).toBe(enLabel)
    expect(paginationLabel(seen)).not.toBe(zhLabel)
  })
})
