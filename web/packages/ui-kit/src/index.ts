/**
 * Public entry of @speed/ui-kit.
 *
 * Two kinds of things ship here: the theme factory (createAppTheme,
 * AppThemeProvider) that turns @speed/tokens into a MUI v9 theme, and the
 * seven controlled core components (PageHeader, EmptyState, ConfirmDialog,
 * FormField, FormLayout, DataTable, FileUploader) with their
 * ui-kit-namespace translations (UI_KIT_NAMESPACE + uiKitResources) for
 * the host to register — FileUploader under the package's one
 * interaction-local carve-out, its upload transport host-injected.
 * Everything else stays internal: helpers shared between components live
 * in src/internal/ and are deliberately not exported.
 */

export { UI_KIT_NAMESPACE, uiKitResources } from './resources.js'
export { createAppTheme, type AppTheme } from './theme/createAppTheme.js'
export {
  AppThemeProvider,
  type AppThemeProviderProps,
} from './theme/AppThemeProvider.js'
export {
  PageHeader,
  type PageHeaderBreadcrumb,
  type PageHeaderProps,
} from './components/PageHeader.js'
export {
  EmptyState,
  type EmptyStateProps,
  type EmptyStateVariant,
} from './components/EmptyState.js'
export {
  ConfirmDialog,
  type ConfirmDialogProps,
  type ConfirmDialogVariant,
} from './components/ConfirmDialog.js'
export {
  FormField,
  REQUIRED_ERROR_KEY,
  type FormFieldProps,
  type FormFieldRenderState,
} from './components/FormField.js'
export { FormLayout, type FormLayoutProps } from './components/FormLayout.js'
export {
  DataTable,
  type DataTableColumn,
  type DataTableFilter,
  type DataTablePagination,
  type DataTableProps,
  type DataTableSort,
  type DataTableSortDirection,
} from './components/DataTable.js'
export {
  FileUploader,
  type FileUploadContext,
  type FileUploadExecutor,
  type FileUploaderProps,
  type FileUploadQueueSummary,
} from './components/FileUploader.js'
