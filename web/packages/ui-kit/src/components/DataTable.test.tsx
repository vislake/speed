/**
 * DataTable contract: a fully controlled table view -- rows are shown
 * exactly as given (no implicit sorting or slicing), sorting shows
 * state and fires change callbacks only, selection toggles the rendered
 * page's keys and leaves other pages' keys alone, the footer labels
 * interpolate the ui-kit namespace text and follow the language,
 * loading appears as a status row only while rows are empty, an empty
 * table renders the stock EmptyState placeholder with overridable
 * slots, the filter input is a controlled field whose filtering the
 * host owns. Assertions derive every user-facing string from the
 * locale bundles (or English fixtures), never inline translations.
 */

import { act, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../../test-utils/render.js'
import {
  expectNoAxeViolations,
  runHeadingOrderCheck,
} from '../../test-utils/axe.js'
import { emittedStyleText } from '../../test-utils/emitted-css.js'
import { DataTable } from './DataTable.js'
import type { DataTableColumn, DataTableProps } from './DataTable.js'

interface Member {
  id: number
  name: string
  credits: number
}

const MEMBERS: readonly Member[] = [
  { id: 1, name: 'Ada', credits: 120 },
  { id: 2, name: 'Grace', credits: 80 },
  { id: 3, name: 'Katherine', credits: 240 },
]

const BASE_COLUMNS: readonly DataTableColumn<Member>[] = [
  { id: 'name', header: 'Name', sortable: true, cell: (row) => row.name },
  {
    id: 'credits',
    header: 'Credits',
    sortable: true,
    align: 'right',
    width: 120,
    cell: (row) => String(row.credits),
  },
]

const keyOf = (row: Member): number => row.id

function renderTable(
  props: Partial<DataTableProps<Member>> = {},
  options: { heading?: string } = {},
) {
  const table = (
    <DataTable
      rows={MEMBERS}
      columns={BASE_COLUMNS}
      rowKey={keyOf}
      {...props}
    />
  )
  return renderWithProviders(
    options.heading !== undefined ? (
      // The axe scans render under a real page h1 (page-has-heading-one
      // is determinate in jsdom -- see the axe helper header), the way
      // a real host page would mount the table. Behavioural tests keep
      // rendering the bare table; only scans opt into the context.
      <div>
        <h1>{options.heading}</h1>
        {table}
      </div>
    ) : (
      table
    ),
  )
}

function renderTemplate(
  template: string,
  vars: Record<string, string | number>,
): string {
  return Object.entries(vars).reduce(
    (out, [key, value]) => out.replaceAll(`{{${key}}}`, String(value)),
    template,
  )
}

describe('DataTable', () => {
  it('renders every column header and cell', () => {
    const utils = renderTable()
    expect(
      utils.getByRole('columnheader', { name: 'Name' }),
    ).toBeInTheDocument()
    expect(
      utils.getByRole('columnheader', { name: 'Credits' }),
    ).toBeInTheDocument()
    expect(utils.getByText('Ada')).toBeInTheDocument()
    expect(utils.getByText('Grace')).toBeInTheDocument()
    expect(utils.getByText('Katherine')).toBeInTheDocument()
  })

  it('passes the row index into the cell render prop', () => {
    const seen: number[] = []
    const columns: readonly DataTableColumn<Member>[] = [
      {
        id: 'name',
        header: 'Name',
        cell: (_row, index) => {
          seen.push(index)
          return 'rendered'
        },
      },
    ]
    renderTable({ columns })
    expect(seen).toEqual([0, 1, 2])
    expect(seen.length).toBe(MEMBERS.length)
  })

  it('wraps the table in a horizontally scrollable container regardless of column count or width', () => {
    // Renders deliberately wider than any reasonable host container
    // (12 columns at 300px each = 3600px) to exercise the contract this
    // round makes explicit in the component's own doc comment: more or
    // wider columns than the host's container scroll inside the
    // TableContainer wrapper rather than overflowing the page. jsdom
    // does no real layout, so this cannot prove a scrollbar renders at
    // any actual width -- it proves the wrapper MUI gives `overflow-x:
    // auto` by default is genuinely present, so a future refactor that
    // drops `TableContainer` (the thing that gives this for free today)
    // would fail this test instead of silently regressing.
    const manyWideColumns: DataTableColumn<Member>[] = Array.from(
      { length: 12 },
      (_, index) => ({
        id: `extra-${index}`,
        header: `Column ${index}`,
        width: 300,
        cell: () => 'value',
      }),
    )
    const utils = renderTable({ columns: manyWideColumns })
    const container = utils.container.querySelector('.MuiTableContainer-root')
    expect(container).not.toBeNull()
    expect(container?.tagName.toLowerCase()).toBe('div')
    expect(container?.querySelector('table')).not.toBeNull()
    expect(container).toHaveStyle({ overflowX: 'auto' })
  })

  it('applies column alignment and width to cells', () => {
    const utils = renderTable()
    const creditCells = utils.container.querySelectorAll(
      'tbody td.MuiTableCell-alignRight',
    )
    expect(creditCells.length).toBe(MEMBERS.length)
    expect(creditCells[0]).toHaveStyle({ width: '120px' })
  })

  it('renders a status row with the translated loading text while rows are empty', () => {
    const utils = renderTable({ rows: [], loading: true })
    const status = utils.getByRole('status')
    expect(status.textContent).toContain(zhCN.dataTable.loading)
    expect(utils.getByRole('progressbar')).toBeInTheDocument()
    expect(utils.queryByRole('row', { name: /Ada/ })).not.toBeInTheDocument()
  })

  it('keeps showing rows during a load when rows are present', () => {
    const utils = renderTable({ loading: true })
    expect(utils.getByText('Ada')).toBeInTheDocument()
    expect(utils.queryByRole('status')).not.toBeInTheDocument()
  })

  it('renders the stock EmptyState placeholder for an empty table', () => {
    const utils = renderTable({ rows: [] })
    expect(utils.getByText(zhCN.emptyState.empty.title)).toBeInTheDocument()
    expect(
      utils.getByText(zhCN.emptyState.empty.description),
    ).toBeInTheDocument()
  })

  it('announces the loading state on the empty-then-refresh path through a live region that already exists (P2-6 regression)', () => {
    // PRE-FIX (P2-6): the role="status" region rendered only while
    // loading, so on the empty-table refresh path (rows stay empty,
    // loading flips false -> true) the region appeared in the same commit
    // as its text. A live region announces content changes that follow
    // its own existence, never text that mounts together with it, so that
    // refresh's loading announcement was silent. POST-FIX: the region is
    // mounted (empty, visually silent) for the whole empty phase and the
    // loading text fills it on refresh -- a text change inside a region
    // the screen reader already knows. The same DOM node must survive the
    // transition, which is what makes the later text change an
    // announcement rather than another mount. A stateful host drives the
    // flip (the refresh is the host's own action): RTL's rerender would
    // replace the provider tree and remount the table, which is exactly
    // the mount the fix must prevent.
    function RefreshHarness() {
      const [loading, setLoading] = useState(false)
      return (
        <>
          <button type="button" onClick={() => setLoading(true)}>
            refresh
          </button>
          <DataTable
            rows={[]}
            columns={BASE_COLUMNS}
            rowKey={keyOf}
            loading={loading}
          />
        </>
      )
    }
    const utils = renderWithProviders(<RefreshHarness />)
    const region = utils.getByRole('status')
    expect(region).toHaveTextContent('')
    fireEvent.click(utils.getByRole('button', { name: 'refresh' }))
    expect(region).toHaveTextContent(zhCN.dataTable.loading)
    expect(utils.getByRole('progressbar')).toBeInTheDocument()
  })

  it('overrides the empty placeholder slots', () => {
    const utils = renderTable({
      rows: [],
      emptyTitle: 'No matches',
      emptyDescription: 'Widen the filter and try again',
      emptyAction: <button type="button">Clear filters</button>,
    })
    expect(utils.getByText('No matches')).toBeInTheDocument()
    expect(
      utils.getByText('Widen the filter and try again'),
    ).toBeInTheDocument()
    expect(utils.getByRole('button', { name: 'Clear filters' })).toBeInTheDocument()
    expect(utils.queryByText(zhCN.emptyState.empty.title)).not.toBeInTheDocument()
  })

  it('labels the select-all and row checkboxes from the namespace', () => {
    const utils = renderTable({ onSelectionChange: vi.fn() })
    expect(
      utils.getByLabelText(zhCN.dataTable.selectAllRows),
    ).toBeInTheDocument()
    expect(
      utils.getByLabelText(
        renderTemplate(zhCN.dataTable.selectRow, { row: 1 }),
      ),
    ).toBeInTheDocument()
    expect(
      utils.getByLabelText(
        renderTemplate(zhCN.dataTable.selectRow, { row: 3 }),
      ),
    ).toBeInTheDocument()
  })

  it('switches the checkbox aria-labels into the new language', async () => {
    const utils = renderTable({ onSelectionChange: vi.fn() })
    expect(
      utils.getByLabelText(
        renderTemplate(zhCN.dataTable.selectRow, { row: 1 }),
      ),
    ).toBeInTheDocument()
    await act(async () => {
      await switchLanguage(utils.i18n, 'en-US')
    })
    expect(
      utils.getByLabelText(
        renderTemplate(enUS.dataTable.selectRow, { row: 1 }),
      ),
    ).toBeInTheDocument()
    expect(
      utils.queryByLabelText(zhCN.dataTable.selectAllRows),
    ).not.toBeInTheDocument()
  })

  it('selects all rendered rows from the header and clears them again', () => {
    const onSelectionChange = vi.fn()
    const utils = renderWithProviders(
      <DataTable
        rows={MEMBERS}
        columns={BASE_COLUMNS}
        rowKey={keyOf}
        selectedRowKeys={[]}
        onSelectionChange={onSelectionChange}
      />,
    )
    const selectAll = utils.getByLabelText(zhCN.dataTable.selectAllRows)
    fireEvent.click(selectAll)
    expect(onSelectionChange).toHaveBeenLastCalledWith([1, 2, 3])
    utils.rerender(
      <DataTable
        rows={MEMBERS}
        columns={BASE_COLUMNS}
        rowKey={keyOf}
        selectedRowKeys={[1, 2, 3]}
        onSelectionChange={onSelectionChange}
      />,
    )
    fireEvent.click(utils.getByLabelText(zhCN.dataTable.selectAllRows))
    expect(onSelectionChange).toHaveBeenLastCalledWith([])
  })

  it('leaves out-of-page keys untouched when toggling all', () => {
    const onSelectionChange = vi.fn()
    const utils = renderWithProviders(
      <DataTable
        rows={MEMBERS}
        columns={BASE_COLUMNS}
        rowKey={keyOf}
        selectedRowKeys={[9]}
        onSelectionChange={onSelectionChange}
      />,
    )
    fireEvent.click(utils.getByLabelText(zhCN.dataTable.selectAllRows))
    expect(onSelectionChange).toHaveBeenLastCalledWith([9, 1, 2, 3])
    utils.rerender(
      <DataTable
        rows={MEMBERS}
        columns={BASE_COLUMNS}
        rowKey={keyOf}
        selectedRowKeys={[9, 1, 2, 3]}
        onSelectionChange={onSelectionChange}
      />,
    )
    fireEvent.click(utils.getByLabelText(zhCN.dataTable.selectAllRows))
    expect(onSelectionChange).toHaveBeenLastCalledWith([9])
  })

  it('toggles one row on and off', () => {
    const onSelectionChange = vi.fn()
    const utils = renderWithProviders(
      <DataTable
        rows={MEMBERS}
        columns={BASE_COLUMNS}
        rowKey={keyOf}
        selectedRowKeys={[]}
        onSelectionChange={onSelectionChange}
      />,
    )
    fireEvent.click(
      utils.getByLabelText(renderTemplate(zhCN.dataTable.selectRow, { row: 1 })),
    )
    expect(onSelectionChange).toHaveBeenLastCalledWith([1])
    utils.rerender(
      <DataTable
        rows={MEMBERS}
        columns={BASE_COLUMNS}
        rowKey={keyOf}
        selectedRowKeys={[1]}
        onSelectionChange={onSelectionChange}
      />,
    )
    fireEvent.click(
      utils.getByLabelText(renderTemplate(zhCN.dataTable.selectRow, { row: 1 })),
    )
    expect(onSelectionChange).toHaveBeenLastCalledWith([])
  })

  it('marks the header checkbox indeterminate for a partial page selection', () => {
    const utils = renderWithProviders(
      <DataTable
        rows={MEMBERS}
        columns={BASE_COLUMNS}
        rowKey={keyOf}
        selectedRowKeys={[1]}
        onSelectionChange={vi.fn()}
      />,
    )
    const header = utils.getByLabelText(
      zhCN.dataTable.selectAllRows,
    ) as HTMLInputElement
    expect(header.indeterminate).toBe(true)
    expect(header.checked).toBe(false)
  })

  it('renders sortable headers as buttons only when sorting is enabled', () => {
    const utils = renderTable()
    expect(
      utils.queryByRole('button', { name: 'Name' }),
    ).not.toBeInTheDocument()
    expect(
      utils.getByRole('columnheader', { name: 'Name' }),
    ).not.toHaveAttribute('aria-sort')
  })

  it('stays inert when only one half of the sort props is given', () => {
    const onSortChange = vi.fn()
    const utils = renderTable({ onSortChange })
    fireEvent.click(utils.getByText('Name'))
    expect(onSortChange).not.toHaveBeenCalled()
  })

  it('fires the flipped direction when the active column header is clicked', () => {
    const onSortChange = vi.fn()
    const utils = renderTable({
      sort: { columnId: 'name', direction: 'asc' },
      onSortChange,
    })
    expect(
      utils.getByRole('columnheader', { name: 'Name' }),
    ).toHaveAttribute('aria-sort', 'ascending')
    fireEvent.click(utils.getByRole('button', { name: 'Name' }))
    expect(onSortChange).toHaveBeenLastCalledWith({
      columnId: 'name',
      direction: 'desc',
    })
  })

  it('fires ascending when a new sortable column is clicked', () => {
    const onSortChange = vi.fn()
    const utils = renderTable({
      sort: { columnId: 'name', direction: 'desc' },
      onSortChange,
    })
    fireEvent.click(utils.getByRole('button', { name: 'Credits' }))
    expect(onSortChange).toHaveBeenLastCalledWith({
      columnId: 'credits',
      direction: 'asc',
    })
    expect(
      utils.getByRole('columnheader', { name: 'Credits' }),
    ).not.toHaveAttribute('aria-sort', 'ascending')
  })

  it('supports an explicit null sort with sorting on', () => {
    const onSortChange = vi.fn()
    const utils = renderTable({ sort: null, onSortChange })
    expect(
      utils.getByRole('columnheader', { name: 'Name' }),
    ).not.toHaveAttribute('aria-sort')
    fireEvent.click(utils.getByRole('button', { name: 'Name' }))
    expect(onSortChange).toHaveBeenLastCalledWith({
      columnId: 'name',
      direction: 'asc',
    })
  })

  it('renders the pagination footer labels with interpolated counts', () => {
    const utils = renderTable({
      pagination: {
        page: 0,
        rowsPerPage: 2,
        count: 5,
        onPageChange: vi.fn(),
        onRowsPerPageChange: vi.fn(),
      },
    })
    expect(utils.getByText(zhCN.dataTable.rowsPerPage)).toBeInTheDocument()
    expect(
      utils.getByText(
        renderTemplate(zhCN.dataTable.displayedRows, {
          from: 1,
          to: 2,
          count: 5,
        }),
      ),
    ).toBeInTheDocument()
  })

  it('uses the unknown-total wording when count is -1', () => {
    const utils = renderTable({
      pagination: {
        page: 0,
        rowsPerPage: 2,
        count: -1,
        onPageChange: vi.fn(),
        onRowsPerPageChange: vi.fn(),
      },
    })
    expect(
      utils.getByText(
        renderTemplate(zhCN.dataTable.displayedRowsUnknown, {
          from: 1,
          to: 2,
        }),
      ),
    ).toBeInTheDocument()
    expect(
      utils.queryByText(zhCN.dataTable.displayedRows.replace('{{count}}', '5')),
    ).not.toBeInTheDocument()
  })

  it('fires the next-page callback and disables previous on the first page', () => {
    const onPageChange = vi.fn()
    // The pager buttons' aria-labels come from the MUI locale the theme
    // merges (English here so no translated string needs inlining).
    const utils = renderWithProviders(
      <DataTable
        rows={MEMBERS}
        columns={BASE_COLUMNS}
        rowKey={keyOf}
        pagination={{
          page: 0,
          rowsPerPage: 2,
          count: 5,
          onPageChange,
          onRowsPerPageChange: vi.fn(),
        }}
      />,
      { language: 'en-US' },
    )
    const previous = utils.getByRole('button', { name: 'Go to previous page' })
    expect(previous).toBeDisabled()
    fireEvent.click(previous)
    expect(onPageChange).not.toHaveBeenCalled()
    fireEvent.click(utils.getByRole('button', { name: 'Go to next page' }))
    expect(onPageChange).toHaveBeenLastCalledWith(1)
  })

  it('fires the rows-per-page callback from the selector', () => {
    const onRowsPerPageChange = vi.fn()
    const utils = renderTable({
      pagination: {
        page: 0,
        rowsPerPage: 2,
        count: 5,
        rowsPerPageOptions: [2, 5],
        onPageChange: vi.fn(),
        onRowsPerPageChange,
      },
    })
    fireEvent.mouseDown(utils.getByRole('combobox'))
    fireEvent.click(utils.getByRole('option', { name: '5' }))
    expect(onRowsPerPageChange).toHaveBeenLastCalledWith(5)
  })

  it('re-renders loading, footer and checkbox text in the new language', async () => {
    const utils = renderWithProviders(
      <DataTable
        rows={[]}
        columns={BASE_COLUMNS}
        rowKey={keyOf}
        loading
        onSelectionChange={vi.fn()}
        pagination={{
          page: 0,
          rowsPerPage: 2,
          count: 5,
          onPageChange: vi.fn(),
          onRowsPerPageChange: vi.fn(),
        }}
      />,
    )
    expect(utils.getByRole('status').textContent).toContain(
      zhCN.dataTable.loading,
    )
    expect(utils.getByText(zhCN.dataTable.rowsPerPage)).toBeInTheDocument()
    await act(async () => {
      await switchLanguage(utils.i18n, 'en-US')
    })
    expect(utils.getByRole('status').textContent).toContain(
      enUS.dataTable.loading,
    )
    expect(utils.getByText(enUS.dataTable.rowsPerPage)).toBeInTheDocument()
    expect(
      utils.getByText(
        renderTemplate(enUS.dataTable.displayedRows, {
          from: 1,
          to: 2,
          count: 5,
        }),
      ),
    ).toBeInTheDocument()
    expect(utils.queryByText(zhCN.dataTable.rowsPerPage)).not.toBeInTheDocument()
  })

  it('renders the controlled filter input and fires value changes', async () => {
    // The input is fully controlled, so the host's value loop must run
    // for typing to land -- the harness plays the host.
    const recorded: string[] = []
    function FilterHarness() {
      const [value, setValue] = useState('gr')
      return (
        <DataTable
          rows={MEMBERS}
          columns={BASE_COLUMNS}
          rowKey={keyOf}
          filter={{
            value,
            onValueChange: (next) => {
              recorded.push(next)
              setValue(next)
            },
          }}
        />
      )
    }
    const utils = renderWithProviders(<FilterHarness />)
    const input = utils.getByLabelText(zhCN.dataTable.filterLabel)
    expect(input).toHaveValue('gr')
    expect(input).toHaveAttribute('placeholder', zhCN.dataTable.filterLabel)
    const user = userEvent.setup()
    await user.type(input, 'ace')
    expect(recorded.at(-1)).toBe('grace')
    expect(input).toHaveValue('grace')
  })

  it('relabels the filter input on language switch', async () => {
    const utils = renderTable({
      filter: { value: '', onValueChange: vi.fn() },
    })
    expect(
      utils.getByLabelText(zhCN.dataTable.filterLabel),
    ).toBeInTheDocument()
    await act(async () => {
      await switchLanguage(utils.i18n, 'en-US')
    })
    expect(
      utils.getByLabelText(enUS.dataTable.filterLabel),
    ).toBeInTheDocument()
  })

  it('uses the rowKey results as the selection keys', () => {
    const onSelectionChange = vi.fn()
    const utils = renderTable({
      rowKey: (row) => `m${row.id}`,
      onSelectionChange,
    })
    fireEvent.click(utils.getByLabelText(zhCN.dataTable.selectAllRows))
    expect(onSelectionChange).toHaveBeenLastCalledWith(['m1', 'm2', 'm3'])
  })

  it('applies the small table size class on request', () => {
    const utils = renderTable({ size: 'small' })
    const cell = utils.container.querySelector('tbody td')
    expect(cell?.className).toContain('MuiTableCell-sizeSmall')
  })

  describe('column priority (responsive reflow)', () => {
    // Same class of proof as FormLayout's `columns={2}` breakpoint-keyed
    // grid test (see that file): a breakpoint-keyed `sx` value compiles
    // to a base rule plus an `@media` rule, and jsdom evaluates neither
    // real layout nor `@media` conditions -- there is no real viewport
    // for either side to be "active" at, so `getComputedStyle`/
    // `toHaveStyle` cannot resolve which one applies. Reading the
    // generated CSS text instead proves the right declarations (hidden
    // below the tier's breakpoint, restored at and above it) were wired
    // into the render; it does not prove a real viewport actually hides
    // or shows the column, which this package's jsdom suite cannot
    // render at all. No other sx in this component is breakpoint-keyed,
    // so the `@media (min-width:...)` and `table-cell` strings these
    // tests look for can only come from this feature.

    it('renders a column with no priority with no visibility override -- unaffected by the feature', () => {
      // BASE_COLUMNS sets no column's priority; this restates the
      // existing "applies column alignment and width to cells" contract
      // as this round's own required backward-compatibility proof: a
      // host that sets no column's priority sees byte-identical
      // rendering, on every viewport.
      const utils = renderTable()
      const creditCells = utils.container.querySelectorAll(
        'tbody td.MuiTableCell-alignRight',
      )
      expect(creditCells[0]).toHaveStyle({ width: '120px' })
      expect(utils.getByText('Ada')).toBeInTheDocument()
    })

    it('hides a high-priority column below sm and restores it from sm up', () => {
      const columns: DataTableColumn<Member>[] = [
        ...BASE_COLUMNS,
        { id: 'plan', header: 'Plan', priority: 'high', cell: () => 'Pro' },
      ]
      renderTable({ columns })
      const css = emittedStyleText()
      expect(css).toMatch(/display:none/)
      expect(css).toMatch(/@media \(min-width:600px\)/)
      expect(css).toMatch(/display:table-cell/)
    })

    it('hides a medium-priority column below md', () => {
      const columns: DataTableColumn<Member>[] = [
        ...BASE_COLUMNS,
        { id: 'plan', header: 'Plan', priority: 'medium', cell: () => 'Pro' },
      ]
      renderTable({ columns })
      const css = emittedStyleText()
      expect(css).toMatch(/@media \(min-width:900px\)/)
      expect(css).toMatch(/display:table-cell/)
    })

    it('hides a low-priority column below lg', () => {
      const columns: DataTableColumn<Member>[] = [
        ...BASE_COLUMNS,
        { id: 'plan', header: 'Plan', priority: 'low', cell: () => 'Pro' },
      ]
      renderTable({ columns })
      const css = emittedStyleText()
      expect(css).toMatch(/@media \(min-width:1200px\)/)
      expect(css).toMatch(/display:table-cell/)
    })

    it('keeps a prioritized column as a real DOM column (CSS-hidden, never conditionally removed)', () => {
      // Hiding is pure CSS, so the cell stays a genuine column in jsdom
      // (which does not evaluate the @media rule either way) -- the
      // point under test is that the column was never conditionally
      // rendered away, which the loading/empty rows' colSpan depends on,
      // and that an always-visible column alongside it is untouched.
      const columns: DataTableColumn<Member>[] = [
        ...BASE_COLUMNS,
        { id: 'plan', header: 'Plan', priority: 'low', cell: () => 'Pro' },
      ]
      const utils = renderTable({ columns })
      expect(
        utils.getByRole('columnheader', { name: 'Plan' }),
      ).toBeInTheDocument()
      expect(utils.getAllByText('Pro')).toHaveLength(MEMBERS.length)
      expect(utils.getByText('Ada')).toBeInTheDocument()
    })

    it('passes axe with a mix of prioritized and always-visible columns', async () => {
      const columns: DataTableColumn<Member>[] = [
        ...BASE_COLUMNS,
        { id: 'plan', header: 'Plan', priority: 'low', cell: () => 'Pro' },
      ]
      renderTable({ columns }, { heading: 'Members' })
      await expectNoAxeViolations()
    })
  })

  // Each scan renders the table under a real page h1 (the renderTable
  // `heading` option): page-has-heading-one is determinate in jsdom now
  // (see the axe helper header), so a scan document without an h1 fails
  // instead of passing by indeterminacy -- the h1 is the page context a
  // real host page supplies.
  it('passes axe over a fully loaded table', async () => {
    renderTable(
      {
        selectedRowKeys: [1],
        onSelectionChange: vi.fn(),
        sort: { columnId: 'name', direction: 'asc' },
        onSortChange: vi.fn(),
        filter: { value: '', onValueChange: vi.fn() },
        pagination: {
          page: 0,
          rowsPerPage: 2,
          count: 3,
          onPageChange: vi.fn(),
          onRowsPerPageChange: vi.fn(),
        },
      },
      { heading: 'Members' },
    )
    await expectNoAxeViolations()
  })

  it('passes axe over the loading state', async () => {
    renderTable({ rows: [], loading: true }, { heading: 'Members' })
    await expectNoAxeViolations()
  })

  it('passes axe over the empty placeholder state', async () => {
    // emptyHeadingLevel supplied, as a correctly-migrated caller would:
    // the stock placeholder h6 under the real page h1 is a genuine
    // heading-order skip (pinned by the two P2-3 tests below).
    renderTable(
      { rows: [], emptyHeadingLevel: 'h2' },
      { heading: 'Members' },
    )
    await expectNoAxeViolations()
  })

  describe('the empty placeholder under a real page heading (P2-3)', () => {
    // Regression for the reviewer finding (web-base-packages ui-kit P2-3):
    // the axe case that claimed to cover the empty state rendered the
    // table with loading: true, so the stock EmptyState placeholder never
    // rendered at all -- and a component-level render has no ancestor
    // heading anyway -- which left the real surface's h1 -> h6 skip
    // invisible to the package suite. The reference-app notes surface is
    // exactly that real shape: a page h1 above an empty table whose stock
    // placeholder defaults to the h6 heading level. These two cases
    // supply the h1 themselves (the same mechanism EmptyState.test.tsx
    // uses) so the skip is reachable, mirroring that suite's pair: the
    // stock default under a real h1 still skips (a compatibility floor,
    // not a claim that h6 is correct), and supplying the level that
    // continues the page order removes the violation entirely.

    it('still skips to the stock h6 default under a real h1 when no emptyHeadingLevel is given', async () => {
      renderWithProviders(
        <div>
          <h1>Notes</h1>
          <DataTable rows={[]} columns={BASE_COLUMNS} rowKey={keyOf} />
        </div>,
      )
      expect(document.querySelector('h6')).not.toBeNull()
      const violations = await runHeadingOrderCheck()
      expect(violations.length).toBeGreaterThan(0)
    })

    it('continues the page heading order when emptyHeadingLevel is supplied', async () => {
      renderWithProviders(
        <div>
          <h1>Notes</h1>
          <DataTable
            rows={[]}
            columns={BASE_COLUMNS}
            rowKey={keyOf}
            emptyHeadingLevel="h2"
          />
        </div>,
      )
      expect(document.querySelector('h6')).toBeNull()
      expect(document.querySelector('h2')).not.toBeNull()
      const violations = await runHeadingOrderCheck()
      expect(violations).toHaveLength(0)
    })
  })
})
