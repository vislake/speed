/**
 * Shared DOM-test harness for layout-kit packages' tests.
 *
 * renderWithProviders mounts a unit under the tree a real host builds:
 * I18nextProvider (both the layout-kit and ui-kit namespaces'
 * translations need it -- RouteGuard's default deniedFallback reuses
 * ui-kit's EmptyState) around ui-kit's own AppThemeProvider, the same
 * theme runtime every real host composes rather than a second,
 * hand-rolled one. AppShell only reads `theme.breakpoints` /
 * `theme.zIndex`, both MUI-identical to the speed token defaults, so
 * this is a faithful stand-in for what a real host renders even though
 * this package takes no *direct* dependency on @speed/tokens itself.
 *
 * The provider tree is passed as the RTL `wrapper` option, not wrapped
 * around `ui` by hand: RTL re-wraps a `rerender(ui)` call in the very
 * same wrapper it rendered with, so a rerendered unit stays inside the
 * providers and React reconciles it in place. A hand-wrapped tree
 * instead gets REPLACED by the bare rerendered `ui` (the wrapper was
 * applied outside RTL's knowledge), which unmounts the providers and
 * remounts the unit -- a fresh instance whose refs and state reset on
 * the first rerender. RouteGuard's transition tests depend on rerenders
 * that preserve the instance (its effect ref is what dedupes the
 * denied-transition firing), so the scaffold has to keep the tree shape
 * stable across rerenders.
 *
 * The i18n instance is created per call through the shared
 * createTestI18n (see @speed/test-utils/render's header), with both
 * namespaces registered.
 */

import { render } from '@testing-library/react'
import type { ReactElement, ReactNode } from 'react'
import { I18nextProvider, registerNamespace } from '@speed/i18n'
import type { I18nInstance } from '@speed/i18n'
import { AppThemeProvider, UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import { createTestI18n } from '@speed/test-utils/render'
import type { RenderWithProvidersOptions } from '@speed/test-utils/render'
import type { RenderWithProvidersResult } from '@speed/test-utils/render'
import { LAYOUT_KIT_NAMESPACE, layoutKitResources } from '../src/resources.js'

export type { RenderWithProvidersOptions, RenderWithProvidersResult }

export function createLayoutKitI18n(language: string = 'zh-CN'): I18nInstance {
  const instance = createTestI18n(language)
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

  function Wrapper({ children }: { readonly children: ReactNode }): ReactElement {
    return (
      <I18nextProvider i18n={instance}>
        <AppThemeProvider i18n={instance}>{children}</AppThemeProvider>
      </I18nextProvider>
    )
  }

  const result = render(ui, { wrapper: Wrapper })
  return { ...result, i18n: instance }
}
