/**
 * The README usage example, compiled and executed by the suite.
 *
 * The README's Quick start composes a host page from the public exports
 * and registers the ui-kit namespace at bootstrap; this file renders
 * that composition through the same host tree (renderWithProviders) and
 * asserts what the page shows, so the documented usage cannot drift from
 * the API -- the package suite compiles and runs it. Host-content
 * strings (titles, headers) are English fixtures on purpose: they stand
 * in for a host's own translations and are data in a test file (exempt
 * from the no-literal-text rule), not rendered product text. Assertions
 * derive every built-in string from the locale bundles, never inline
 * translations.
 */

import { act, fireEvent } from '@testing-library/react'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import { ConfirmDialog } from './components/ConfirmDialog.js'
import { DataTable } from './components/DataTable.js'
import type { DataTableColumn, DataTableSort } from './components/DataTable.js'
import { EmptyState } from './components/EmptyState.js'
import { PageHeader } from './components/PageHeader.js'

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

const COLUMNS: readonly DataTableColumn<Member>[] = [
  { id: 'name', header: 'Name', sortable: true, cell: (row) => row.name },
  {
    id: 'credits',
    header: 'Credits',
    align: 'right',
    cell: (row) => String(row.credits),
  },
]

/**
 * The README's example host page: it owns every piece of state the
 * components report through props -- sort, selection, filter and
 * pagination are all controlled from here.
 */
function MembersPage() {
  const [sort, setSort] = useState<DataTableSort | null>(null)
  const [selected, setSelected] = useState<readonly (string | number)[]>([])
  const [query, setQuery] = useState('')
  const visible = MEMBERS.filter((member) =>
    member.name.toLowerCase().includes(query.toLowerCase()),
  )
  return (
    <>
      <PageHeader
        title="Members"
        description="Everyone with access to this workspace."
      />
      <DataTable
        rows={visible}
        columns={COLUMNS}
        rowKey={(row) => row.id}
        sort={sort}
        onSortChange={setSort}
        selectedRowKeys={selected}
        onSelectionChange={setSelected}
        filter={{ value: query, onValueChange: setQuery }}
        pagination={{
          page: 0,
          rowsPerPage: 10,
          count: MEMBERS.length,
          onPageChange: vi.fn(),
          onRowsPerPageChange: vi.fn(),
        }}
      />
    </>
  )
}

describe('README usage example', () => {
  it('renders the quick-start composition with the zh-CN built-in texts', async () => {
    const utils = renderWithProviders(<MembersPage />)
    expect(
      utils.getByRole('heading', { level: 1, name: 'Members' }),
    ).toBeInTheDocument()
    expect(
      utils.getByText('Everyone with access to this workspace.'),
    ).toBeInTheDocument()
    expect(
      utils.getByRole('columnheader', { name: 'Name' }),
    ).toBeInTheDocument()
    expect(utils.getByText('Ada')).toBeInTheDocument()
    expect(utils.getByText('Katherine')).toBeInTheDocument()
    expect(
      utils.getByLabelText(zhCN.dataTable.selectAllRows),
    ).toBeInTheDocument()
    expect(utils.getByText(zhCN.dataTable.rowsPerPage)).toBeInTheDocument()
    await expectNoAxeViolations()
  })

  it('follows a language switch into the en-US built-in texts', async () => {
    const utils = renderWithProviders(<MembersPage />)
    expect(
      utils.getByLabelText(zhCN.dataTable.selectAllRows),
    ).toBeInTheDocument()
    await act(async () => {
      await switchLanguage(utils.i18n, 'en-US')
    })
    expect(
      utils.getByLabelText(enUS.dataTable.selectAllRows),
    ).toBeInTheDocument()
    expect(utils.getByText(enUS.dataTable.rowsPerPage)).toBeInTheDocument()
    expect(
      utils.queryByLabelText(zhCN.dataTable.selectAllRows),
    ).not.toBeInTheDocument()
    expect(utils.queryByText(zhCN.dataTable.rowsPerPage)).not.toBeInTheDocument()
  })

  it('renders the stock empty and confirm states the README shows', async () => {
    const onCancel = vi.fn()
    const utils = renderWithProviders(
      <>
        <DataTable rows={[]} columns={COLUMNS} />
        <EmptyState variant="error" />
        <ConfirmDialog open onConfirm={vi.fn()} onCancel={onCancel} />
      </>,
    )
    expect(utils.getByText(zhCN.emptyState.empty.title)).toBeInTheDocument()
    expect(utils.getByText(zhCN.emptyState.error.title)).toBeInTheDocument()
    fireEvent.click(
      utils.getByRole('button', { name: zhCN.confirmDialog.cancelLabel }),
    )
    expect(onCancel).toHaveBeenCalledTimes(1)
    await expectNoAxeViolations()
  })
})
