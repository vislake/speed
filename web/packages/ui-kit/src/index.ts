/**
 * Public entry of @speed/ui-kit.
 *
 * Two kinds of things ship here: the theme factory (createAppTheme,
 * AppThemeProvider) that turns @speed/tokens into a MUI v9 theme, and the
 * fully controlled components (PageHeader, EmptyState, ConfirmDialog,
 * FormField, FormLayout, DataTable, FileUploader, InlineError,
 * ListSkeleton, AsyncSection) with their ui-kit-namespace translations
 * (UI_KIT_NAMESPACE + uiKitResources) for the host to register —
 * FileUploader renders the queue the host owns as `rows` props while
 * uploads run in the host's own transport code. The cross-surface state
 * primitives ship beside them: InlineError + errorCodeOf (the
 * whole-attempt failure banner), ListSkeleton and AsyncSection (the
 * pending/error/empty/content branches of an async read) and
 * visuallyHiddenSx (the clip recipe for hidden live regions and file
 * inputs). Everything else stays internal: helpers shared between
 * components live in src/internal/ and are deliberately not exported.
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
  type EmptyStateHeadingLevel,
  type EmptyStateProps,
  type EmptyStateVariant,
} from './components/EmptyState.js'
export {
  ConfirmDialog,
  CONFIRM_ARM_LOCKOUT_MS,
  type ConfirmDialogProps,
  type ConfirmDialogVariant,
} from './components/ConfirmDialog.js'
export {
  FormField,
  REQUIRED_ERROR_KEY,
  type FormFieldProps,
  type FormFieldRenderState,
} from './components/FormField.js'
export {
  FormLayout,
  type FormLayoutColumns,
  type FormLayoutProps,
} from './components/FormLayout.js'
export {
  DataTable,
  type DataTableColumn,
  type DataTableColumnPriority,
  type DataTableFilter,
  type DataTablePagination,
  type DataTableProps,
  type DataTableSort,
  type DataTableSortDirection,
} from './components/DataTable.js'
export {
  FileUploader,
  type FileUploaderProps,
  type FileUploaderRow,
  type FileUploaderRowStatus,
} from './components/FileUploader.js'
export {
  InlineError,
  errorCodeOf,
  type InlineErrorProps,
} from './components/InlineError.js'
export {
  ListSkeleton,
  type ListSkeletonProps,
} from './components/ListSkeleton.js'
export {
  AsyncSection,
  type AsyncSectionEmptyState,
  type AsyncSectionErrorState,
  type AsyncSectionProps,
} from './components/AsyncSection.js'
export { visuallyHiddenSx } from './visually-hidden.js'
