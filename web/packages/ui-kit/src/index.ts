/**
 * Public entry of @speed/ui-kit.
 *
 * Two kinds of things ship here: the theme factory (createAppTheme,
 * AppThemeProvider) that turns @speed/tokens into a MUI v9 theme, and the
 * controlled core components (PageHeader, EmptyState, ConfirmDialog,
 * FormField, FormLayout, DataTable) with their ui-kit-namespace
 * translations (UI_KIT_NAMESPACE + uiKitResources) for the host to
 * register. Everything else stays internal: helpers shared between
 * components live in src/internal/ and are deliberately not exported.
 */

export { UI_KIT_NAMESPACE, uiKitResources } from './resources.js'
export { createAppTheme, type AppTheme } from './theme/createAppTheme.js'
export {
  AppThemeProvider,
  type AppThemeProviderProps,
} from './theme/AppThemeProvider.js'
