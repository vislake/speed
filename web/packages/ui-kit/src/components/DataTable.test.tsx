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
import { expectNoAxeViolations } from '../../test-utils/axe.js'
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

function renderTable(props: Partial<DataTableProps<Member>> = {}) {
  return renderWithProviders(
    <DataTable
      rows={MEMBERS}
      columns={BASE_COLUMNS}
      rowKey={keyOf}
      {...props}
    />,
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

  it('passes axe over a fully loaded table', async () => {
    renderTable({
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
    })
    await expectNoAxeViolations()
  })

  it('passes axe over the loading and empty states', async () => {
    renderTable({ rows: [], loading: true })
    await expectNoAxeViolations()
  })
})
