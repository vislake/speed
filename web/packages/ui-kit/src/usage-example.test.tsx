/**
 * The README usage example, compiled and executed by the suite.
 *
 * The README's Quick start composes host pages from the public exports
 * and registers the ui-kit namespace at bootstrap; this file renders
 * that composition through the same host tree (renderWithProviders) and
 * asserts what the pages show, so the documented usage cannot drift from
 * the API -- the package suite compiles and runs it. The upload panel
 * the Quick start documents joins the members page on the same host
 * screen: its host holds only the queue summary reported up through
 * onQueueChange, and its executor is a fixture whose one round trip per
 * file -- a POST of the picked bytes -- is answered by a scripted fetch
 * stub returning genuine Response objects. The stub stands in for the
 * host's generated api-sdk storage call: storage hooks publish in the
 * consumer-shell round (go/storage/AGENTS.md), so ui-kit itself ships no
 * endpoint, and the placeholder URL below is labelled as such.
 *
 * Host-content strings (titles, headers) are English fixtures on
 * purpose: they stand in for a host's own translations and are data in a
 * test file (exempt from the no-literal-text rule), not rendered product
 * text. Assertions derive every built-in string from the locale bundles,
 * never inline translations.
 */

import { act, fireEvent, within } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import Typography from '@mui/material/Typography'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import { selectFiles } from '../test-utils/file-input.js'
import { ConfirmDialog } from './components/ConfirmDialog.js'
import { DataTable } from './components/DataTable.js'
import type { DataTableColumn, DataTableSort } from './components/DataTable.js'
import { EmptyState } from './components/EmptyState.js'
import { FileUploader } from './components/FileUploader.js'
import type { FileUploadExecutor } from './components/FileUploader.js'
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

/**
 * The upload transport stand-in the fixture executor sends its one round
 * trip to. A real host's executor calls a generated api-sdk storage
 * operation here -- storage hooks publish in the consumer-shell round
 * (go/storage/AGENTS.md) -- so this is an explicitly labelled fixture
 * placeholder: ui-kit ships no endpoint, and the suite pins the
 * transport shape without pretending to know a wire protocol.
 */
const STORAGE_UPLOAD_URL = 'https://uploads.example.test/objects'

/** The queue-summary caption text, rendered by the host and asserted here. */
const queueLine = (summary: {
  readonly uploading: number
  readonly succeeded: number
  readonly failed: number
}): string =>
  `${summary.uploading} uploading, ${summary.succeeded} succeeded, ${summary.failed} failed`

/**
 * The README's example upload panel: a host that owns nothing
 * upload-shaped beyond the summary FileUploader reports up through
 * onQueueChange -- the report-up consumer shape. The transport is the
 * host's own, supplied as the `uploadFile` executor prop, so this
 * component never sees a fetch, a URL or an endpoint.
 */
function UploadPanel({ uploadFile }: { readonly uploadFile: FileUploadExecutor }) {
  const [summary, setSummary] = useState({
    uploading: 0,
    succeeded: 0,
    failed: 0,
    total: 0,
  })
  return (
    <>
      <FileUploader multiple execute={uploadFile} onQueueChange={setSummary} />
      <Typography variant="caption" component="p" sx={{ m: 0 }}>
        {summary.uploading} uploading, {summary.succeeded} succeeded, {summary.failed} failed
      </Typography>
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

/**
 * The upload half of the README Quick start, executed: the documented
 * UploadPanel on the same host screen as the members page, its journey
 * driven over the scripted transport stand-in -- one logical round trip
 * per executed file, in the order the files were picked.
 */
describe('README upload panel', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('runs the documented upload journey over the scripted host transport', async () => {
    const portrait = new File(['portrait scan bytes: 0x0118'], 'portrait.png', {
      type: 'image/png',
    })
    const closeup = new File(['closeup scan bytes: 0x0023'], 'closeup.jpg', {
      type: 'image/jpeg',
    })

    // Host-written pre-flight rejection text: the executor's early
    // reject path, before any network -- the shape host-side validation
    // (size, type, count) takes. The fixture gates only the first
    // attempt of closeup.jpg, so the retry leg proves a failed row
    // recovers through the widget's retry affordance.
    const HOST_PREFLIGHT_TEXT = 'The upload service is busy. Retry in a moment.'
    const uploadAttempts = new Map<string, number>()

    const uploadFile: FileUploadExecutor = async (file, { signal, onProgress }) => {
      const attempt = uploadAttempts.get(file.name) ?? 0
      uploadAttempts.set(file.name, attempt + 1)
      if (attempt === 0 && file === closeup) {
        throw new Error(HOST_PREFLIGHT_TEXT)
      }
      // Progress slices between await ticks, driving the determinate bar.
      onProgress(0.25)
      const body = await file.arrayBuffer()
      const response = await fetch(STORAGE_UPLOAD_URL, {
        method: 'POST',
        headers: { 'content-type': file.type },
        body,
        signal,
      })
      if (!response.ok) {
        throw new Error(`The upload answered ${response.status}.`)
      }
      onProgress(0.75)
    }

    // The scripted transport: each request is recorded (url, declared
    // content type, body bytes) and then parked until the test opens its
    // gate, so every in-flight state is observable; the answer is a
    // genuine Response object.
    const uploadCalls: {
      readonly url: string
      readonly contentType: string | null
      readonly bytes: ArrayBuffer
    }[] = []
    const gates: (() => void)[] = []
    const fetchMock = vi.fn(
      async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        uploadCalls.push({
          url:
            typeof input === 'string'
              ? input
              : input instanceof URL
                ? input.href
                : input.url,
          contentType:
            typeof init?.headers === 'object' && init.headers !== null
              ? ((init.headers as Record<string, string>)['content-type'] ?? null)
              : null,
          bytes: init?.body as ArrayBuffer,
        })
        await new Promise<void>((resolve) => gates.push(resolve))
        return new Response(JSON.stringify({ id: 'upload-ok' }), {
          status: 201,
          headers: { 'content-type': 'application/json' },
        })
      },
    )
    vi.stubGlobal('fetch', fetchMock)

    // The upload panel shares the host screen with the members page.
    const utils = renderWithProviders(
      <>
        <MembersPage />
        <UploadPanel uploadFile={uploadFile} />
      </>,
    )
    expect(utils.getByRole('heading', { level: 1, name: 'Members' })).toBeInTheDocument()
    expect(utils.getByText('Ada')).toBeInTheDocument()
    expect(
      utils.getByText(queueLine({ uploading: 0, succeeded: 0, failed: 0 })),
    ).toBeInTheDocument()

    // Pick two files: the first uploads; the second is rejected by the
    // host's pre-flight gate before any network call.
    const input = utils.getByLabelText(zhCN.fileUploader.chooseFiles) as HTMLInputElement
    await act(async () => {
      selectFiles(input, [portrait, closeup])
    })

    // portrait.png: uploading, its bar determinate at the host's first
    // progress slice. closeup.jpg: failed, showing the host-written
    // error text and the retry affordance.
    expect(
      utils.getByText(queueLine({ uploading: 1, succeeded: 0, failed: 1 })),
    ).toBeInTheDocument()
    expect(
      utils.getByRole('progressbar', { name: zhCN.fileUploader.statusUploading }),
    ).toHaveAttribute('aria-valuenow', '25')
    expect(utils.getByText(zhCN.fileUploader.statusUploading)).toBeInTheDocument()
    expect(utils.getByText(zhCN.fileUploader.statusFailed)).toBeInTheDocument()
    expect(utils.getByText(HOST_PREFLIGHT_TEXT)).toBeInTheDocument()
    expect(
      utils.getByRole('button', { name: zhCN.fileUploader.actionRetry }),
    ).toBeInTheDocument()
    expect(utils.getByRole('status')).toHaveTextContent(
      zhCN.fileUploader.announceFailed.replace('{{name}}', closeup.name),
    )

    // The transport saw exactly one request so far: portrait.png's
    // bytes, in order, at the labelled placeholder URL -- closeup.jpg's
    // pre-flight rejection never touched the network.
    expect(uploadCalls).toHaveLength(1)
    const portraitCall = uploadCalls[0]!
    expect(portraitCall.url).toBe(STORAGE_UPLOAD_URL)
    expect(portraitCall.contentType).toBe('image/png')
    expect(new Uint8Array(portraitCall.bytes)).toEqual(
      new Uint8Array(await portrait.arrayBuffer()),
    )

    // Open the first request's gate: the round trip completes and the
    // row settles succeeded.
    await act(async () => {
      gates[0]!()
    })
    expect(utils.getByText(zhCN.fileUploader.statusSucceeded)).toBeInTheDocument()
    expect(
      utils.getByText(queueLine({ uploading: 0, succeeded: 1, failed: 1 })),
    ).toBeInTheDocument()
    expect(utils.getByRole('status')).toHaveTextContent(
      zhCN.fileUploader.announceUploaded.replace('{{name}}', portrait.name),
    )
    expect(utils.queryByRole('progressbar')).not.toBeInTheDocument()

    // Retry the failed row: the host's pre-flight gate has cleared, so
    // the second attempt uploads -- the second request, in order.
    fireEvent.click(utils.getByRole('button', { name: zhCN.fileUploader.actionRetry }))
    await act(async () => {})
    expect(utils.getByText(zhCN.fileUploader.statusUploading)).toBeInTheDocument()
    expect(
      utils.getByText(queueLine({ uploading: 1, succeeded: 1, failed: 0 })),
    ).toBeInTheDocument()
    expect(
      utils.getByRole('progressbar', { name: zhCN.fileUploader.statusUploading }),
    ).toHaveAttribute('aria-valuenow', '25')
    expect(uploadCalls).toHaveLength(2)
    const closeupCall = uploadCalls[1]!
    expect(closeupCall.contentType).toBe('image/jpeg')
    expect(new Uint8Array(closeupCall.bytes)).toEqual(
      new Uint8Array(await closeup.arrayBuffer()),
    )

    await act(async () => {
      gates[1]!()
    })
    expect(utils.getAllByText(zhCN.fileUploader.statusSucceeded)).toHaveLength(2)
    expect(
      utils.getByText(queueLine({ uploading: 0, succeeded: 2, failed: 0 })),
    ).toBeInTheDocument()
    expect(utils.getByRole('status')).toHaveTextContent(
      zhCN.fileUploader.announceUploaded.replace('{{name}}', closeup.name),
    )

    // Remove the first row: the queue keeps the second, the summary
    // follows, and the members page next to it never noticed.
    const portraitRow = utils.getByText(portrait.name).closest('li')!
    fireEvent.click(
      within(portraitRow).getByRole('button', {
        name: zhCN.fileUploader.actionRemove,
      }),
    )
    expect(utils.queryByText(portrait.name)).not.toBeInTheDocument()
    expect(utils.getByText(closeup.name)).toBeInTheDocument()
    expect(
      utils.getByText(queueLine({ uploading: 0, succeeded: 1, failed: 0 })),
    ).toBeInTheDocument()
    expect(utils.getByText('Katherine')).toBeInTheDocument()

    // Language switch leg: the remaining row's built-in texts, the
    // picker label and the announcement follow the bundle; the host's
    // own caption is host content and stays as authored.
    await act(async () => {
      await switchLanguage(utils.i18n, 'en-US')
    })
    expect(utils.getByText(enUS.fileUploader.statusSucceeded)).toBeInTheDocument()
    expect(utils.queryByText(zhCN.fileUploader.statusSucceeded)).not.toBeInTheDocument()
    expect(
      utils.getByRole('button', { name: enUS.fileUploader.actionRemove }),
    ).toBeInTheDocument()
    expect(utils.getByLabelText(enUS.fileUploader.chooseFiles)).toBeInTheDocument()
    expect(utils.queryByLabelText(zhCN.fileUploader.chooseFiles)).not.toBeInTheDocument()
    expect(utils.getByRole('status')).toHaveTextContent(
      enUS.fileUploader.announceUploaded.replace('{{name}}', closeup.name),
    )
    expect(
      utils.getByText(queueLine({ uploading: 0, succeeded: 1, failed: 0 })),
    ).toBeInTheDocument()
    await expectNoAxeViolations()
  })
})
