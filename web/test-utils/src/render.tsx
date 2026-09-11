/**
 * The render-primitive layer shared by the packages' and the app's DOM
 * harnesses. Which provider tree a unit renders under is package policy
 * (a package's own test-utils/render.tsx assembles its tree: the theme
 * provider, a query client where the package reads react-query hooks,
 * the page-heading context a page-shaped surface needs); what every
 * harness builds that tree from is here:
 *
 * - TEST_LANGUAGES: the bilingual pair every suite switches between.
 * - createTestI18n: a fresh instance per call with a deterministic
 *   configuration -- no storage, no URL, no navigator -- that the
 *   caller then registers its namespaces on. A fresh instance per call
 *   keeps registerNamespace's double-registration guard from firing
 *   across tests.
 * - createTestQueryClient: a fresh client that retries nothing, so an
 *   operation a test scripts to fail surfaces its error on the first
 *   attempt, not after react-query default retries, and a mutation
 *   never outlives its test.
 * - RenderWithProvidersOptions / RenderWithProvidersResult: the option
 *   and result shapes the harnesses extend (the shared options are the
 *   language and a caller-owned instance; the shared result carries the
 *   instance the tree renders with, so language-switch tests act on it
 *   and the provider's 'languageChanged' subscription re-renders the
 *   tree).
 */

import type { RenderResult } from '@testing-library/react'
import { QueryClient } from '@tanstack/react-query'
import { createI18n } from '@speed/i18n'
import type { I18nInstance } from '@speed/i18n'

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

/** Fresh bilingual instance with no namespaces yet: register them on the
 * returned instance (see the header; a fresh instance per call). */
export function createTestI18n(language: string = 'zh-CN'): I18nInstance {
  return createI18n({
    supportedLanguages: TEST_LANGUAGES,
    defaultLanguage: language,
    storage: null,
    urlParameterName: null,
    navigatorLanguages: [],
  })
}

/** Fresh client that never retries: see the header. */
export function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
}
