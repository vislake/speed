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
 * The empty phase's announcements follow the FileUploader live-region
 * twin: a role="status" region is mounted (empty and visually silent)
 * for the whole empty phase, and its text states the phase's current
 * state -- the loading text while loading, and the empty-result wording
 * (`dataTable.noData`, the ui-kit's own, deliberately not the
 * host-overridable EmptyState title: the stock placeholder beneath is
 * the empty result's visual, and duplicating a host-supplied title node
 * inside the live region would present the same text twice) once the
 * phase is not loading. The region therefore never falls silent between
 * states: a load that ends with zero rows replaces the loading text
 * instead of trailing off into an emptiness that could still mean
 * loading. A region that only appeared together with its text could
 * never announce any of this -- live regions speak about content changes
 * that follow their own existence, not text that mounts with them -- so
 * the empty phase's first committed frame is empty whatever transition
 * entered the phase: a phase that begins already loading (rows emptied
 * and loading flipped in the same host commit) shows the region empty
 * for one commit, the loading text filling it on the next, and an empty
 * phase entered not loading spends that same first frame empty before
 * the empty-result wording fills it -- the same fill-an-existing-region
 * shape, guaranteed for every route into the empty phase rather than
 * only the refresh of a phase that began not loading.
 *
 * The stock placeholder's title is a real heading element whose correct
 * level only the host knows (see EmptyState's own heading-level note):
 * `emptyHeadingLevel` forwards the level that continues the page's
 * heading order at the point this table sits, exactly as hosts already
 * do for EmptyState instances they render themselves. It defaults to
 * EmptyState's own 'h6' floor, so a host that does not pass it keeps
 * byte-identical behavior.
 *
 * Horizontal overflow is a stated, tested contract, not an accident of
 * implementation: the table is always wrapped in MUI's `TableContainer`,
 * whose default styling gives the wrapper `overflow-x: auto` -- more or
 * wider columns than the host's container scroll horizontally inside
 * that wrapper rather than overflowing the page. This holds regardless
 * of column count or width and needs no prop; see DataTable.test.tsx's
 * "horizontal-scroll container" test for the proof (a refactor that
 * drops `TableContainer` would fail it).
 *
 * Column priority (opt-in, additive) is the reflow alternative to that
 * scroll fallback: a column's `priority` ('high' | 'medium' | 'low')
 * hides it below that tier's breakpoint on the same shared scale
 * (@speed/tokens' breakpoints.values: xs:0/sm:600/md:900/lg:1200/xl:1536)
 * every other responsive surface in this repo reads from -- 'low' clears
 * at `lg`, 'medium' at `md`, 'high' at `sm`, so a narrowing viewport
 * sheds the least important columns first and keeps shedding until only
 * the always-visible ones (no `priority` set) remain, even at `xs`. A
 * column with no `priority` renders with no visibility override at all,
 * so a host that sets no column's priority is byte-for-byte unaffected --
 * the TableContainer scroll fallback above still applies to it. The
 * hiding is pure CSS (a breakpoint-keyed `display` value on
 * the cell), never a JS layout decision: unlike AppShell's drawer-variant
 * switch (@speed/layout-kit), a hidden table cell has no interactive
 * state to preserve across the switch, so there is nothing a `sx`
 * breakpoint value can't do that a `useMediaQuery` re-render would do
 * better, and CSS keeps every rendered breakpoint available to print/
 * devtools inspection rather than only the one JS measured at mount.
 * See DataTable.test.tsx's priority tests for what this proves and does
 * not (jsdom evaluates neither real layout nor `@media`, so the proof is
 * that the right declarations were wired in, not that a real viewport
 * renders correctly).
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

import { useCallback, useEffect, useState } from 'react'
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
import type { EmptyStateHeadingLevel } from './EmptyState.js'
import { useUiKitTranslation } from '../internal/translation.js'
import { visuallyHiddenSx } from '../visually-hidden.js'

/**
 * Opt-in column visibility priority (see the header note for the full
 * cascade). Higher priority survives to narrower viewports: 'low' is the
 * first to be hidden as the viewport shrinks, 'high' the last, and a
 * column with no priority is never hidden.
 */
export type DataTableColumnPriority = 'high' | 'medium' | 'low'

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
  /**
   * Opt-in responsive visibility: hides this column below the tier's
   * breakpoint as the viewport narrows (see DataTableColumnPriority and
   * the component header note for the exact cascade). Omitting it keeps
   * the column always visible -- the exact behavior, unaffected by this
   * prop existing.
   */
  readonly priority?: DataTableColumnPriority
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
  /**
   * The heading level of the stock empty placeholder's title (rendered
   * through the built-in EmptyState). Set it to whatever level continues
   * the real page's own heading order at the point this table sits --
   * see EmptyState's headingLevel note for the full reasoning. Defaults
   * to EmptyState's own 'h6' floor: a host that does not pass it keeps
   * byte-identical behavior.
   */
  readonly emptyHeadingLevel?: EmptyStateHeadingLevel
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
 * The breakpoint each priority tier stays visible from, on the shared
 * scale (@speed/tokens' breakpoints.values: sm:600/md:900/lg:1200). 'low'
 * clears the highest bar (visible only from `lg` up) so it is the first
 * dropped as the viewport narrows; 'high' clears the lowest bar (visible
 * from `sm` up) so it is the last dropped, hidden only below `sm`.
 */
const PRIORITY_VISIBLE_FROM: Record<DataTableColumnPriority, 'sm' | 'md' | 'lg'> = {
  high: 'sm',
  medium: 'md',
  low: 'lg',
}

/**
 * Renders a column's opt-in priority into a breakpoint-keyed `display`
 * sx value: `none` below the tier's breakpoint, `table-cell` (a
 * TableCell's own default display, restored explicitly since MUI's
 * breakpoint object only ever adds rules, never restores a browser
 * default) from it up. Returns undefined for a column with no priority,
 * so spreading its result into a cell's sx is a no-op -- the exact prior
 * sx shape, unaffected by this feature.
 */
function columnVisibilitySx(
  priority: DataTableColumnPriority | undefined,
): { display: Partial<Record<'xs' | 'sm' | 'md' | 'lg', string>> } | undefined {
  if (priority === undefined) return undefined
  const visibleFrom = PRIORITY_VISIBLE_FROM[priority]
  return { display: { xs: 'none', [visibleFrom]: 'table-cell' } }
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
  emptyHeadingLevel,
  sx,
}: DataTableProps<T>) {
  const { t } = useUiKitTranslation()

  const emptyPhase = rows.length === 0

  // The loading live region must never mount together with its text: a
  // role="status" region announces only content changes that follow its
  // own insertion, so text born in the same commit as the region would
  // be silent -- and the empty phase can begin already loading (rows
  // emptied and loading flipped in the same host commit), not only via a
  // refresh of a phase that began not loading.
  // The loading text below is therefore gated on `statusRegionCommitted`,
  // which lags the empty phase by one commit: the effect after the
  // phase's first commit arms it, so the region's first committed frame
  // is empty whatever transition entered the phase, and the text fills
  // an existing region a commit later. Leaving the empty phase disarms
  // it, so the next phase's region also spends its own first committed
  // frame empty, whatever loading state that phase begins with.
  const [statusRegionCommitted, setStatusRegionCommitted] = useState(false)
  useEffect(() => {
    setStatusRegionCommitted(emptyPhase)
  }, [emptyPhase])

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
  // and a data attribute, without setting the input.indeterminate DOM
  // property; axe -- which requires the property to match the aria value
  // -- flags the mismatch. Restore the property so the DOM state and the
  // accessible state agree.
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
                    sx={{ width: column.width, ...columnVisibilitySx(column.priority) }}
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
            {rows.length === 0 && (
              <TableRow>
                <TableCell
                  colSpan={span}
                  align="center"
                  sx={{ border: 0, padding: 0 }}
                >
                  {/* The status region mounts (empty) for the whole empty
                      phase, so a transition into a new state fills an
                      existing region instead of mounting one together
                      with its text -- see the file header. The region
                      carries the phase's text, loading or empty-result,
                      never silence in between; when not loading that text
                      is visually hidden (clipped, not display:none --
                      the EmptyState beneath is the visual, and a hidden
                      region would not be live). Its first committed frame
                      is empty even when the phase begins already loading
                      or already empty: the text below renders only once
                      `statusRegionCommitted` arms it, one commit after
                      the region mounts. */}
                  <Box
                    role="status"
                    sx={
                      loading
                        ? {
                            display: 'flex',
                            alignItems: 'center',
                            justifyContent: 'center',
                            gap: 1.5,
                            paddingY: 4,
                            color: 'text.secondary',
                          }
                        : { display: 'flex' }
                    }
                  >
                    {statusRegionCommitted ? (
                      loading ? (
                        <>
                          <CircularProgress
                            size={22}
                            aria-label={t('dataTable.loading')}
                          />
                          {t('dataTable.loading')}
                        </>
                      ) : (
                        // The empty-result wording inside the status
                        // region: the EmptyState beneath is the empty
                        // result's visual, so the region's copy takes
                        // the package's clip recipe (see
                        // src/visually-hidden.ts) and no painted space,
                        // staying in the accessibility tree so the
                        // region still has something to announce.
                        <Box component="span" sx={visuallyHiddenSx}>
                          {t('dataTable.noData')}
                        </Box>
                      )
                    ) : null}
                  </Box>
                  {!loading && (
                    <EmptyState
                      variant="empty"
                      title={emptyTitle}
                      description={emptyDescription}
                      action={emptyAction}
                      headingLevel={emptyHeadingLevel}
                      sx={{ paddingY: 4, paddingX: 2 }}
                    />
                  )}
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
                      sx={{ width: column.width, ...columnVisibilitySx(column.priority) }}
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
