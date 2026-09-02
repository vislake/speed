/**
 * DataTable: the fully controlled data-grid view for the ui-kit.
 *
 * Every piece of table state is owned by the host and flows through
 * props; the component only renders state back and fires change
 * callbacks. `rows` is by contract the set of rows the host wants shown
 * right now -- client-side hosts sort and slice, server-side hosts pass
 * the current page as the query returned it. The component never
 * re-sorts or re-slices: an implicit second sort would corrupt the
 * server-side page the host just fetched (sorting a 25-row page instead
 * of the full set), and slicing would hide rows the pagination labels
 * still count. Sorting and filtering therefore appear as state echo
 * plus callbacks -- the sort indicator (`sort`/`onSortChange`, click
 * cycle: new column ascends, the active column flips) and the filter
 * input (`filter`/`onFilterChange`) whose filtering itself stays with
 * the host, where the field-to-field logic lives.
 *
 * Loading shows a status row only while `rows` is empty; a table that
 * already has rows keeps showing them (the host is usually refetching
 * the next page), so the load state never blanks content mid-read.
 * An empty table renders the stock EmptyState placeholder (variant
 * 'empty', overridable via `emptyTitle`/`emptyDescription`/
 * `emptyAction`), which hosts swap for a variant of their own when the
 * emptiness means more than "no data yet".
 *
 * Selection is enabled by passing `onSelectionChange`; `selectedRowKeys`
 * then holds the keys (from `rowKey`, index-keyed by default -- pass an
 * id-based `rowKey` once a table can reorder) and the header checkbox
 * toggles exactly the rows currently rendered, leaving keys of other
 * pages untouched. `pagination` renders MUI's TablePagination footer
 * with the ui-kit namespace's bilingual labels; `count: -1` (unknown
 * total, infinite server scroll) switches the shown counter to the
 * no-total wording. All built-in text comes from the ui-kit namespace
 * (resource table in the README) and follows the active language.
 */

import { useCallback } from 'react'
import type { ReactNode } from 'react'
import Box from '@mui/material/Box'
import Checkbox from '@mui/material/Checkbox'
import CircularProgress from '@mui/material/CircularProgress'
import Table from '@mui/material/Table'
import TableBody from '@mui/material/TableBody'
import TableCell from '@mui/material/TableCell'
import TableContainer from '@mui/material/TableContainer'
import TableHead from '@mui/material/TableHead'
import TablePagination from '@mui/material/TablePagination'
import TableRow from '@mui/material/TableRow'
import TableSortLabel from '@mui/material/TableSortLabel'
import TextField from '@mui/material/TextField'
import type { SxProps, Theme } from '@mui/material/styles'
import { EmptyState } from './EmptyState.js'
import { useUiKitTranslation } from '../internal/translation.js'

/** One column definition: header plus a per-row cell renderer. */
export interface DataTableColumn<T> {
  /** Stable column identity; also the sort-state columnId. */
  readonly id: string
  /** Column header content. */
  readonly header: ReactNode
  /** Whether clicking the header offers sorting on this column. */
  readonly sortable?: boolean
  /** Cell text alignment; defaults to left. */
  readonly align?: 'left' | 'center' | 'right'
  /** Table-cell width hint. */
  readonly width?: number | string
  /** Renders one row's cell; rowIndex is the index within `rows`. */
  readonly cell: (row: T, rowIndex: number) => ReactNode
}

/** The controlled sort direction shown on the active column. */
export type DataTableSortDirection = 'asc' | 'desc'

/** The controlled sort state; null means "no active sort". */
export interface DataTableSort {
  readonly columnId: string
  readonly direction: DataTableSortDirection
}

/** The controlled filter input: the host owns value and filtering. */
export interface DataTableFilter {
  readonly value: string
  /** Fired on every input change; the host filters rows itself. */
  readonly onValueChange: (value: string) => void
}

/** The controlled pagination state (MUI conventions: page is 0-based). */
export interface DataTablePagination {
  /** The zero-based page currently shown. */
  readonly page: number
  /** Rows per page, as displayed in the footer selector. */
  readonly rowsPerPage: number
  /** Total rows across all pages; -1 when unknown. */
  readonly count: number
  /** Selector options; defaults to MUI's [10, 25, 100]. */
  readonly rowsPerPageOptions?: readonly number[]
  /** Fired when the host should move to another page. */
  readonly onPageChange: (page: number) => void
  /** Fired with the new rows-per-page value (page reset is the host's). */
  readonly onRowsPerPageChange: (rowsPerPage: number) => void
}

export interface DataTableProps<T> {
  /** The rows to render now (see header note: no implicit sorting or slicing). */
  readonly rows: readonly T[]
  readonly columns: readonly DataTableColumn<T>[]
  /**
   * Stable key per row: used as the React key and as the selection key.
   * Defaults to the row index -- fine for read-only tables, but any
   * table with selection (or rows that can reorder) must pass an
   * id-based key.
   */
  readonly rowKey?: (row: T, rowIndex: number) => string | number
  /** Renders a status row instead of content while rows are empty. */
  readonly loading?: boolean
  /** Table density; defaults to 'medium'. */
  readonly size?: 'small' | 'medium'
  /**
   * The current sort; give together with `onSortChange` to enable
   * sorting. null means sorting is on with no active column. The
   * component never reorders rows -- the host applies the sort.
   */
  readonly sort?: DataTableSort | null
  /** Fired when a sortable header is clicked (cycle: new column asc, active column flips). */
  readonly onSortChange?: (sort: DataTableSort) => void
  /** Renders the filter input above the table; filtering stays with the host. */
  readonly filter?: DataTableFilter
  /** Keys of the selected rows (selection keys are `rowKey` results). */
  readonly selectedRowKeys?: readonly (string | number)[]
  /** Presence of this callback turns row selection on. */
  readonly onSelectionChange?: (keys: readonly (string | number)[]) => void
  /** Renders the MUI TablePagination footer with these controlled values. */
  readonly pagination?: DataTablePagination
  /** Overrides the built-in empty title (rendered inside the stock EmptyState). */
  readonly emptyTitle?: ReactNode
  /** Overrides the built-in empty description. */
  readonly emptyDescription?: ReactNode
  /** Optional action inside the empty placeholder. */
  readonly emptyAction?: ReactNode
  /** Extra styling applied to the outermost box. */
  readonly sx?: SxProps<Theme>
}

function flip(direction: DataTableSortDirection): DataTableSortDirection {
  return direction === 'asc' ? 'desc' : 'asc'
}

/**
 * The controlled table view. See the header note for the ownership
 * contract -- every knob here is props in, callbacks out.
 */
export function DataTable<T>({
  rows,
  columns,
  rowKey,
  loading = false,
  size = 'medium',
  sort,
  onSortChange,
  filter,
  selectedRowKeys,
  onSelectionChange,
  pagination,
  emptyTitle,
  emptyDescription,
  emptyAction,
  sx,
}: DataTableProps<T>) {
  const { t } = useUiKitTranslation()

  const selectable = onSelectionChange !== undefined
  const sortingEnabled = onSortChange !== undefined && sort !== undefined
  const selected = new Set<string | number>(selectedRowKeys ?? [])

  const keyOf = (row: T, index: number): string | number =>
    rowKey === undefined ? index : rowKey(row, index)

  const pageKeys = rows.map(keyOf)
  const allSelected =
    pageKeys.length > 0 && pageKeys.every((key) => selected.has(key))
  const someSelected = !allSelected && pageKeys.some((key) => selected.has(key))

  const span = columns.length + (selectable ? 1 : 0)

  const toggleAll = () => {
    if (!selectable) return
    const current = selectedRowKeys ?? []
    const next = allSelected
      ? current.filter((key) => !pageKeys.includes(key))
      : [...current, ...pageKeys.filter((key) => !selected.has(key))]
    onSelectionChange(next)
  }

  const toggleRow = (key: string | number) => {
    if (!selectable) return
    const current = selectedRowKeys ?? []
    const next = selected.has(key)
      ? current.filter((existing) => existing !== key)
      : [...current, key]
    onSelectionChange(next)
  }

  // MUI v9 marks the indeterminate checkbox input with aria-checked="mixed"
  // and a data attribute but no longer sets the input.indeterminate DOM
  // property (its v5 behavior), so axe -- which requires the property to
  // match the aria value -- flags it. Restore the property so the DOM
  // state and the accessible state agree.
  const headerInputRef = useCallback(
    (instance: HTMLInputElement | null) => {
      if (instance !== null) {
        instance.indeterminate = someSelected
      }
    },
    [someSelected],
  )

  const handleSortClick = (columnId: string) => {
    if (!sortingEnabled) return
    const next =
      sort?.columnId === columnId
        ? { columnId, direction: flip(sort.direction) }
        : { columnId, direction: 'asc' as const }
    onSortChange(next)
  }

  const activeSortOf = (columnId: string): DataTableSortDirection | null =>
    sortingEnabled && sort !== null && sort !== undefined &&
    sort.columnId === columnId
      ? sort.direction
      : null

  return (
    <Box sx={{ width: '100%', ...sx }}>
      {filter !== undefined && (
        <Box
          sx={{
            display: 'flex',
            justifyContent: 'flex-end',
            marginBottom: 1.5,
          }}
        >
          <TextField
            size="small"
            value={filter.value}
            onChange={(event) => filter.onValueChange(event.target.value)}
            slotProps={{
              htmlInput: {
                'aria-label': t('dataTable.filterLabel'),
                placeholder: t('dataTable.filterLabel'),
              },
            }}
            sx={{ width: '20rem', maxWidth: '100%' }}
          />
        </Box>
      )}
      <TableContainer>
        <Table aria-label={t('dataTable.ariaLabel')} size={size}>
          <TableHead>
            <TableRow>
              {selectable && (
                <TableCell padding="checkbox">
                  <Checkbox
                    checked={allSelected}
                    indeterminate={someSelected}
                    onChange={toggleAll}
                    disabled={pageKeys.length === 0}
                    slotProps={{
                      input: {
                        'aria-label': t('dataTable.selectAllRows'),
                        ref: headerInputRef,
                      },
                    }}
                  />
                </TableCell>
              )}
              {columns.map((column) => {
                const activeDirection = activeSortOf(column.id)
                return (
                  <TableCell
                    key={column.id}
                    align={column.align}
                    sx={{ width: column.width }}
                    aria-sort={
                      activeDirection === 'asc'
                        ? 'ascending'
                        : activeDirection === 'desc'
                          ? 'descending'
                          : undefined
                    }
                  >
                    {sortingEnabled && column.sortable ? (
                      <TableSortLabel
                        active={activeDirection !== null}
                        direction={activeDirection ?? 'asc'}
                        hideSortIcon={activeDirection === null}
                        onClick={() => handleSortClick(column.id)}
                      >
                        {column.header}
                      </TableSortLabel>
                    ) : (
                      column.header
                    )}
                  </TableCell>
                )
              })}
            </TableRow>
          </TableHead>
          <TableBody>
            {rows.length === 0 && loading && (
              <TableRow>
                <TableCell colSpan={span} align="center" sx={{ border: 0 }}>
                  <Box
                    role="status"
                    sx={{
                      display: 'flex',
                      alignItems: 'center',
                      justifyContent: 'center',
                      gap: 1.5,
                      paddingY: 4,
                      color: 'text.secondary',
                    }}
                  >
                    <CircularProgress
                      size={22}
                      aria-label={t('dataTable.loading')}
                    />
                    {t('dataTable.loading')}
                  </Box>
                </TableCell>
              </TableRow>
            )}
            {rows.length === 0 && !loading && (
              <TableRow>
                <TableCell colSpan={span} sx={{ border: 0, padding: 0 }}>
                  <EmptyState
                    variant="empty"
                    title={emptyTitle}
                    description={emptyDescription}
                    action={emptyAction}
                    sx={{ paddingY: 4, paddingX: 2 }}
                  />
                </TableCell>
              </TableRow>
            )}
            {rows.map((row, index) => {
              const key = keyOf(row, index)
              return (
                <TableRow key={key} hover>
                  {selectable && (
                    <TableCell padding="checkbox">
                      <Checkbox
                        checked={selected.has(key)}
                        onChange={() => toggleRow(key)}
                        slotProps={{
                          input: {
                            'aria-label': t('dataTable.selectRow', {
                              row: index + 1,
                            }),
                          },
                        }}
                      />
                    </TableCell>
                  )}
                  {columns.map((column) => (
                    <TableCell
                      key={column.id}
                      align={column.align}
                      sx={{ width: column.width }}
                    >
                      {column.cell(row, index)}
                    </TableCell>
                  ))}
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </TableContainer>
      {pagination !== undefined && (
        <TablePagination
          component="div"
          count={pagination.count}
          page={pagination.page}
          rowsPerPage={pagination.rowsPerPage}
          rowsPerPageOptions={pagination.rowsPerPageOptions}
          onPageChange={(_event, page) => pagination.onPageChange(page)}
          onRowsPerPageChange={(event) =>
            pagination.onRowsPerPageChange(Number(event.target.value))
          }
          labelRowsPerPage={t('dataTable.rowsPerPage')}
          labelDisplayedRows={({ from, to, count: total }) =>
            total === -1
              ? t('dataTable.displayedRowsUnknown', { from, to })
              : t('dataTable.displayedRows', { from, to, count: total })
          }
        />
      )}
    </Box>
  )
}
