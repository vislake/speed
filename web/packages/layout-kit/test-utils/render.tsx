/**
 * Shared DOM-test harness for layout-kit packages' tests.
 *
 * renderWithProviders mounts a unit under the tree a real host builds:
 * I18nextProvider (both the layout-kit and ui-kit namespaces'
 * translations need it -- RouteGuard's default deniedFallback reuses
 * ui-kit's EmptyState) around ui-kit's own AppThemeProvider, the same
 * theme runtime every real host composes rather than a second,
 * hand-rolled one. AppShell only reads `theme.breakpoints` /
 * `theme.zIndex`, both MUI-identical to the speed token defaults (see
 * the package AGENTS.md's tokens adjudication), so this is a faithful
 * stand-in for what a real host renders even though this package takes
 * no *direct* dependency on @speed/tokens itself.
 *
 * The i18n instance is created per call with a deterministic
 * configuration (no storage, no URL, no navigator) and both namespaces
 * registered -- a fresh instance per call keeps registerNamespace's
 * double-registration guard from firing across tests.
 */

import { render } from '@testing-library/react'
import type { RenderResult } from '@testing-library/react'
import type { ReactElement } from 'react'
import {
  createI18n,
  I18nextProvider,
  registerNamespace,
  type I18nInstance,
} from '@speed/i18n'
import { AppThemeProvider, UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import { LAYOUT_KIT_NAMESPACE, layoutKitResources } from '../src/resources.js'

export interface RenderWithProvidersOptions {
  /** Start language of the fresh instance; defaults to zh-CN. */
  readonly language?: string
  /**
   * Reuse an existing instance instead of creating one -- the caller
   * owns its creation AND namespace registration (a second registration
   * on the same instance throws by design).
   */
  readonly i18n?: I18nInstance
}

export interface RenderWithProvidersResult extends RenderResult {
  /** The instance the tree renders with; language-switch tests act on it. */
  readonly i18n: I18nInstance
}

export const TEST_LANGUAGES = ['zh-CN', 'en-US'] as const

export function createLayoutKitI18n(language: string = 'zh-CN'): I18nInstance {
  const instance = createI18n({
    supportedLanguages: TEST_LANGUAGES,
    defaultLanguage: language,
    storage: null,
    urlParameterName: null,
    navigatorLanguages: [],
  })
  registerNamespace(instance, LAYOUT_KIT_NAMESPACE, layoutKitResources)
  registerNamespace(instance, UI_KIT_NAMESPACE, uiKitResources)
  return instance
}

export function renderWithProviders(
  ui: ReactElement,
  options: RenderWithProvidersOptions = {},
): RenderWithProvidersResult {
  const { language = 'zh-CN', i18n } = options
  const instance = i18n ?? createLayoutKitI18n(language)
  const result = render(
    <I18nextProvider i18n={instance}>
      <AppThemeProvider i18n={instance}>{ui}</AppThemeProvider>
    </I18nextProvider>,
  )
  return { ...result, i18n: instance }
}
