/**
 * Suite for the fully controlled FileUploader.
 *
 * Every queue shape is driven through the `rows` prop and every
 * interaction is asserted on the callbacks it fires — the component holds
 * no queue state of its own, so the tests render hosts that own it. The
 * suite carries three real-browser regression tests jsdom would hide:
 * the live-FileList snapshot (clearing an input empties any FileList
 * captured before the clear), non-finite progress reaching MUI, and the
 * live-region re-announcement rule (a retry must clear the standing
 * announcement text, or an identical repeated failure is never
 * re-announced because the region's text did not change).
 */

import { useState } from 'react'
import { act, fireEvent, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { FileUploader } from './FileUploader.js'
import type { FileUploaderProps, FileUploaderRow } from './FileUploader.js'
import { renderWithProviders } from '../../test-utils/render.js'
import type { RenderWithProvidersResult } from '../../test-utils/render.js'
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { dropFiles, selectFiles } from '../../test-utils/file-input.js'

function makeFile(name: string): File {
  return new File(['x-ray bytes'], name, { type: 'application/octet-stream' })
}

const uploadingRow = (id: string, name: string, progress?: number): FileUploaderRow => ({
  id,
  name,
  status: 'uploading',
  progress,
})
const succeededRow = (id: string, name: string): FileUploaderRow => ({
  id,
  name,
  status: 'succeeded',
})
const failedRow = (id: string, name: string, error?: string): FileUploaderRow => ({
  id,
  name,
  status: 'failed',
  error,
})

const uploadedAnnouncement = (name: string): string =>
  zhCN.fileUploader.announceUploaded.replace('{{name}}', name)
const failedAnnouncement = (name: string): string =>
  zhCN.fileUploader.announceFailed.replace('{{name}}', name)

/**
 * Mount the component through a stateful host that owns `rows` — the way
 * a host owning the queue would — and expose `setRows` for later
 * commits. The host mounts exactly once and never re-renders from
 * outside: RTL's own `rerender` cannot be used here, because
 * `renderWithProviders` inlines the provider tree into the ui it mounts
 * (it takes no wrapper option), so a rerender would replace the whole
 * tree and remount FileUploader, losing the announcement effect's
 * previous-rows ref along with the rest of its state.
 */
function renderHarness(
  initial: FileUploaderProps,
): RenderWithProvidersResult & {
  setRows(rows: readonly FileUploaderRow[]): void
} {
  const applyRowsRef: {
    current: ((rows: readonly FileUploaderRow[]) => void) | null
  } = { current: null }
  function Harness() {
    const [rows, setRows] = useState(initial.rows)
    applyRowsRef.current = setRows
    return <FileUploader {...initial} rows={rows} />
  }
  const view = renderWithProviders(<Harness />)
  return {
    ...view,
    setRows(rows: readonly FileUploaderRow[]): void {
      act(() => {
        if (applyRowsRef.current === null) {
          throw new Error('renderHarness.setRows called before mount')
        }
        applyRowsRef.current(rows)
      })
    },
  }
}

function fileInputOf(container: HTMLElement): HTMLInputElement {
  const input = container.querySelector<HTMLInputElement>('input[type="file"]')
  if (input === null) {
    throw new Error('no file input rendered')
  }
  return input
}

const pickerLabel = zhCN.fileUploader.chooseFiles
const dropHint = zhCN.fileUploader.dropHint
const statusUploading = zhCN.fileUploader.statusUploading
const statusSucceeded = zhCN.fileUploader.statusSucceeded
const statusFailed = zhCN.fileUploader.statusFailed
const actionRetry = zhCN.fileUploader.actionRetry
const actionRemove = zhCN.fileUploader.actionRemove
const actionCancel = zhCN.fileUploader.actionCancel

type DragInit = Parameters<typeof fireEvent.dragEnter>[1]
const emptyDragInit = { dataTransfer: {} } as DragInit

describe('FileUploader', () => {
  describe('the picker trigger', () => {
    it('renders the built-in bilingual label by default', () => {
      const view = renderHarness({ rows: [], onSelectFiles: vi.fn() })
      expect(view.getByText(pickerLabel)).toBeInTheDocument()
    })

    it('follows the active language', async () => {
      const view = renderHarness({ rows: [], onSelectFiles: vi.fn() })
      expect(view.getByText(pickerLabel)).toBeInTheDocument()
      await act(async () => {
        await switchLanguage(view.i18n, 'en-US')
      })
      expect(view.getByText(enUS.fileUploader.chooseFiles)).toBeInTheDocument()
      expect(view.queryByText(pickerLabel)).not.toBeInTheDocument()
    })

    it('renders the chooseFilesLabel override in place of the built-in text', () => {
      const view = renderHarness({
        rows: [],
        onSelectFiles: vi.fn(),
        chooseFilesLabel: 'Upload X-rays',
      })
      expect(view.getByText('Upload X-rays')).toBeInTheDocument()
      expect(view.queryByText(pickerLabel)).not.toBeInTheDocument()
    })

    it('defaults to a single-file pick with no accept filter', () => {
      const view = renderHarness({ rows: [], onSelectFiles: vi.fn() })
      const input = fileInputOf(view.container)
      expect(input.multiple).toBe(false)
      expect(input.accept).toBe('')
    })

    it('forwards multiple and accept onto the hidden input', () => {
      const view = renderHarness({
        rows: [],
        onSelectFiles: vi.fn(),
        multiple: true,
        accept: 'image/jpeg,image/png',
      })
      const input = fileInputOf(view.container)
      expect(input.multiple).toBe(true)
      expect(input.accept).toBe('image/jpeg,image/png')
    })

    it('is one tab stop: the input is focusable and the label is not a button', () => {
      const view = renderHarness({ rows: [], onSelectFiles: vi.fn() })
      const label = view.container.querySelector('label')
      expect(label).not.toBeNull()
      expect(label).not.toHaveAttribute('role')
      expect(label).toHaveAttribute('tabindex', '-1')
      const input = fileInputOf(view.container)
      expect(input).not.toHaveAttribute('hidden')
      expect(input).not.toHaveAttribute('aria-hidden')
      expect(input.disabled).toBe(false)
    })

    it('renders no picker affordance when onSelectFiles is absent — a pure queue view', () => {
      const view = renderHarness({
        rows: [uploadingRow('r1', 'scan.jpg')],
        allowDrop: true,
      })
      expect(view.container.querySelector('label')).toBeNull()
      expect(view.container.querySelector('input')).toBeNull()
      expect(view.queryByText(dropHint)).not.toBeInTheDocument()
      expect(view.getByText('scan.jpg')).toBeInTheDocument()
    })

    it('renders an idle shape for an empty queue: no rows, no progress, no live region', () => {
      const view = renderHarness({ rows: [], onSelectFiles: vi.fn() })
      expect(view.queryAllByRole('listitem')).toHaveLength(0)
      expect(view.queryByRole('progressbar')).not.toBeInTheDocument()
      expect(view.queryByRole('status')).not.toBeInTheDocument()
    })

    it('is disabled as one control: inert label, disabled input, no drop surface', () => {
      const view = renderHarness({
        rows: [],
        onSelectFiles: vi.fn(),
        allowDrop: true,
        disabled: true,
      })
      const label = view.container.querySelector('label')
      expect(label).toHaveAttribute('aria-disabled', 'true')
      expect(fileInputOf(view.container).disabled).toBe(true)
      expect(view.queryByText(dropHint)).not.toBeInTheDocument()
    })
  })

  describe('picking', () => {
    it('reports one pick as a single ordered call, files by identity', () => {
      const onSelectFiles = vi.fn()
      const view = renderHarness({ rows: [], onSelectFiles })
      const xray = makeFile('closeup.jpg')
      const bitewing = makeFile('bitewing.png')
      selectFiles(fileInputOf(view.container), [xray, bitewing])
      expect(onSelectFiles).toHaveBeenCalledTimes(1)
      const reported = onSelectFiles.mock.calls[0]![0]
      expect(reported).toHaveLength(2)
      expect(reported[0]).toBe(xray)
      expect(reported[1]).toBe(bitewing)
    })

    it('ignores an empty selection (the picker dialog was cancelled)', () => {
      const onSelectFiles = vi.fn()
      const view = renderHarness({ rows: [], onSelectFiles })
      selectFiles(fileInputOf(view.container), [])
      expect(onSelectFiles).not.toHaveBeenCalled()
    })

    it('is quiet while disabled, even when a change event reaches the input', () => {
      const onSelectFiles = vi.fn()
      const view = renderHarness({
        rows: [],
        onSelectFiles,
        disabled: true,
      })
      selectFiles(fileInputOf(view.container), [makeFile('scan.jpg')])
      expect(onSelectFiles).not.toHaveBeenCalled()
    })

    it('snapshots the selection before clearing the input — a live FileList dies with its input', () => {
      const onSelectFiles = vi.fn()
      const view = renderHarness({ rows: [], onSelectFiles })
      const input = fileInputOf(view.container)
      const file = makeFile('scan.jpg')

      // A real browser's input.files is a *live* FileList: clearing the
      // input's value — which FileUploader does after every pick so
      // choosing the same file again re-fires change — empties any
      // reference captured before the clear. jsdom hands out snapshot
      // lists, so emulate the live semantics with instance accessors and
      // dispatch a plain change with no target props (the test helpers'
      // target would overwrite the accessors with own data properties).
      let live: readonly File[] = [file]
      Object.defineProperty(input, 'files', {
        configurable: true,
        get: () => live,
      })
      Object.defineProperty(input, 'value', {
        configurable: true,
        get: () => (live.length === 0 ? '' : 'C:\\fakepath\\scan.jpg'),
        set: (next: string) => {
          if (next === '') {
            live = []
          }
        },
      })

      fireEvent.change(input)
      expect(onSelectFiles).toHaveBeenCalledTimes(1)
      expect(onSelectFiles.mock.calls[0]![0]![0]).toBe(file)
      // The input was still reset for the next pick.
      expect(input.value).toBe('')
    })
  })

  describe('dropping', () => {
    it('renders no drop surface by default, and a stray drop is ignored', () => {
      const onSelectFiles = vi.fn()
      const view = renderHarness({
        rows: [],
        onSelectFiles,
      })
      expect(view.queryByText(dropHint)).not.toBeInTheDocument()
      dropFiles(view.container, [makeFile('scan.jpg')])
      expect(onSelectFiles).not.toHaveBeenCalled()
    })

    it('renders the surface with allowDrop, arms it visually during a drag, and reports the drop', () => {
      const onSelectFiles = vi.fn()
      const view = renderHarness({
        rows: [],
        onSelectFiles,
        allowDrop: true,
      })
      const surface = view.getByText(dropHint).closest('div')
      expect(surface).not.toBeNull()
      expect(surface).not.toHaveAttribute('data-drop-active')

      fireEvent.dragEnter(surface!, emptyDragInit)
      expect(surface).toHaveAttribute('data-drop-active', 'true')
      // Enter/leave pairs fire per child crossing; one leave keeps the
      // armed state true, the second disarms.
      fireEvent.dragEnter(surface!, emptyDragInit)
      fireEvent.dragLeave(surface!, emptyDragInit)
      expect(surface).toHaveAttribute('data-drop-active', 'true')
      fireEvent.dragLeave(surface!, emptyDragInit)
      expect(surface).not.toHaveAttribute('data-drop-active')

      const file = makeFile('scan.jpg')
      dropFiles(surface!, [file])
      expect(onSelectFiles).toHaveBeenCalledTimes(1)
      expect(onSelectFiles.mock.calls[0]![0]![0]).toBe(file)
      expect(surface).not.toHaveAttribute('data-drop-active')
    })
  })

  describe('queue rendering', () => {
    it('renders one card per row with the row name verbatim', () => {
      const view = renderHarness({
        rows: [
          uploadingRow('r1', 'closeup.jpg'),
          succeededRow('r2', 'bitewing.png'),
        ],
      })
      expect(view.getByText('closeup.jpg')).toBeInTheDocument()
      expect(view.getByText('bitewing.png')).toBeInTheDocument()
    })

    it('shows an uploading row with an indeterminate bar until progress arrives, then determinate', () => {
      const view = renderHarness({ rows: [uploadingRow('r1', 'scan.jpg')] })
      const bar = view.getByRole('progressbar', { name: statusUploading })
      expect(bar).not.toHaveAttribute('aria-valuenow')
      view.setRows([uploadingRow('r1', 'scan.jpg', 0.5)])
      expect(view.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '50')
    })

    it('renders the uploading status text', () => {
      const view = renderHarness({ rows: [uploadingRow('r1', 'scan.jpg')] })
      expect(view.getByText(statusUploading)).toBeInTheDocument()
    })

    it('folds out-of-range progress into [0, 1]', () => {
      const view = renderHarness({ rows: [uploadingRow('r1', 'scan.jpg', 7)] })
      expect(view.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '100')
      view.setRows([uploadingRow('r1', 'scan.jpg', -0.5)])
      expect(view.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '0')
    })

    it('folds non-finite progress to 0 instead of handing MUI a NaN value', () => {
      const view = renderHarness({ rows: [uploadingRow('r1', 'scan.jpg', Number.NaN)] })
      expect(view.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '0')
      expect(view.container).not.toHaveTextContent('NaN')
    })

    it('marks a succeeded row with its caption and a failed row with its caption', () => {
      const view = renderHarness({
        rows: [succeededRow('r1', 'a.jpg'), failedRow('r2', 'b.jpg')],
      })
      expect(view.getByText(statusSucceeded)).toBeInTheDocument()
      expect(view.getByText(statusFailed)).toBeInTheDocument()
    })

    it('renders the failed row error text verbatim when present', () => {
      const view = renderHarness({
        rows: [failedRow('r1', 'b.jpg', 'The upload answered 503.')],
      })
      expect(view.getByText('The upload answered 503.')).toBeInTheDocument()
    })

    it('renders no error line and no raw junk when a failed row carries no message', () => {
      const view = renderHarness({ rows: [failedRow('r1', 'b.jpg')] })
      const card = view.getByText('b.jpg').closest('li')
      expect(card).not.toBeNull()
      expect(within(card!).queryByText(statusFailed)).toBeInTheDocument()
      expect(card).not.toHaveTextContent(/undefined|\[object Object\]/)
    })

    it('keeps two same-named rows distinct, each acting on its own row id', () => {
      const onRemove = vi.fn()
      const view = renderHarness({
        rows: [succeededRow('a', 'scan.jpg'), succeededRow('b', 'scan.jpg')],
        onRemove,
      })
      expect(view.getAllByText('scan.jpg')).toHaveLength(2)
      const cards = view.getAllByRole('listitem')
      expect(cards).toHaveLength(2)
      within(cards[0]!).getByRole('button', { name: actionRemove }).click()
      expect(onRemove).toHaveBeenCalledWith('a')
      within(cards[1]!).getByRole('button', { name: actionRemove }).click()
      expect(onRemove).toHaveBeenCalledWith('b')
    })

    it('gives each row status its own action vocabulary', () => {
      const view = renderHarness({
        rows: [
          uploadingRow('r1', 'up.jpg'),
          failedRow('r2', 'bad.jpg', 'boom'),
          succeededRow('r3', 'done.jpg'),
        ],
        onCancel: vi.fn(),
        onRetry: vi.fn(),
        onRemove: vi.fn(),
      })
      const cards = view.getAllByRole('listitem')
      expect(cards).toHaveLength(3)
      const uploading = cards[0]!
      const failed = cards[1]!
      const succeeded = cards[2]!

      const uploadingActions = within(uploading).queryAllByRole('button')
      expect(uploadingActions).toHaveLength(1)
      expect(uploadingActions[0]).toHaveAccessibleName(actionCancel)

      const failedActions = within(failed).queryAllByRole('button')
      expect(failedActions.map((button) => button.getAttribute('aria-label') ?? button.textContent)).toEqual([
        actionRetry,
        actionRemove,
      ])

      const succeededActions = within(succeeded).queryAllByRole('button')
      expect(succeededActions).toHaveLength(1)
      expect(succeededActions[0]).toHaveTextContent(actionRemove)
    })

    it('omits each action whose callback is absent', () => {
      const view = renderHarness({
        rows: [
          uploadingRow('r1', 'up.jpg'),
          failedRow('r2', 'bad.jpg'),
          succeededRow('r3', 'done.jpg'),
        ],
        onCancel: vi.fn(),
        onRetry: vi.fn(),
      })
      const cards = view.getAllByRole('listitem')
      expect(within(cards[0]!).queryByRole('button')).toBeInTheDocument()
      expect(within(cards[1]!).getAllByRole('button')).toHaveLength(1)
      expect(within(cards[2]!).queryByRole('button')).not.toBeInTheDocument()
    })

    it('reports cancel, retry and remove by row id through their callbacks', () => {
      const onCancel = vi.fn()
      const onRetry = vi.fn()
      const onRemove = vi.fn()
      const view = renderHarness({
        rows: [
          uploadingRow('r1', 'up.jpg'),
          failedRow('r2', 'bad.jpg'),
          succeededRow('r3', 'done.jpg'),
        ],
        onCancel,
        onRetry,
        onRemove,
      })
      const cards = view.getAllByRole('listitem')
      within(cards[0]!).getByRole('button', { name: actionCancel }).click()
      expect(onCancel).toHaveBeenCalledWith('r1')
      within(cards[1]!).getByRole('button', { name: actionRetry }).click()
      expect(onRetry).toHaveBeenCalledWith('r2')
      within(cards[1]!).getByRole('button', { name: actionRemove }).click()
      expect(onRemove).toHaveBeenCalledWith('r2')
    })

    it('returns to the idle shape when the last row leaves', () => {
      const view = renderHarness({
        rows: [succeededRow('r1', 'done.jpg')],
        onRemove: vi.fn(),
      })
      view.setRows([])
      expect(view.queryAllByRole('listitem')).toHaveLength(0)
      expect(view.queryByRole('status')).not.toBeInTheDocument()
    })
  })

  describe('the live region', () => {
    it('mounts with the queue and stays quiet for rows the host seeds', () => {
      const view = renderHarness({
        rows: [succeededRow('r1', 'done.jpg'), failedRow('r2', 'bad.jpg')],
      })
      const region = view.getByRole('status')
      expect(region).toHaveTextContent('')
    })

    it('announces a settle when a row leaves uploading', () => {
      const view = renderHarness({
        rows: [uploadingRow('r1', 'done.jpg')],
      })
      view.setRows([succeededRow('r1', 'done.jpg')])
      expect(view.getByRole('status')).toHaveTextContent(uploadedAnnouncement('done.jpg'))

      view.setRows([uploadingRow('r2', 'bad.jpg')])
      view.setRows([failedRow('r2', 'bad.jpg')])
      expect(view.getByRole('status')).toHaveTextContent(failedAnnouncement('bad.jpg'))
    })

    it('announces once per settle; a re-render with no transition changes nothing', () => {
      const view = renderHarness({
        rows: [uploadingRow('r1', 'done.jpg')],
      })
      view.setRows([succeededRow('r1', 'done.jpg')])
      view.setRows([succeededRow('r1', 'done.jpg')])
      expect(view.getByRole('status')).toHaveTextContent(uploadedAnnouncement('done.jpg'))
    })

    it('announces a multi-settle commit once, the last row in list order winning', () => {
      const view = renderHarness({
        rows: [uploadingRow('r1', 'a.jpg'), uploadingRow('r2', 'b.jpg')],
      })
      view.setRows([succeededRow('r1', 'a.jpg'), failedRow('r2', 'b.jpg', 'boom')])
      expect(view.getByRole('status')).toHaveTextContent(failedAnnouncement('b.jpg'))
    })

    it('stays quiet for progress-only commits', () => {
      const view = renderHarness({
        rows: [uploadingRow('r1', 'a.jpg')],
      })
      view.setRows([uploadingRow('r1', 'a.jpg', 0.25)])
      expect(view.getByRole('status')).toHaveTextContent('')
      view.setRows([uploadingRow('r1', 'a.jpg', 1)])
      expect(view.getByRole('status')).toHaveTextContent('')
    })

    it('clears the region on a retry, so an identical repeated failure is re-announced', () => {
      const view = renderHarness({
        rows: [uploadingRow('r1', 'a.jpg')],
      })
      view.setRows([failedRow('r1', 'a.jpg', 'The upload answered 503.')])
      expect(view.getByRole('status')).toHaveTextContent(failedAnnouncement('a.jpg'))
      // The retry puts the row back on uploading; the region must empty,
      // or the same failure text below would not re-announce (a live
      // region speaks only when its text changes).
      view.setRows([uploadingRow('r1', 'a.jpg')])
      expect(view.getByRole('status')).toHaveTextContent('')
      view.setRows([failedRow('r1', 'a.jpg', 'The upload answered 503.')])
      expect(view.getByRole('status')).toHaveTextContent(failedAnnouncement('a.jpg'))
    })

    it('treats a host healing a failed row straight to succeeded as no transition', () => {
      const view = renderHarness({
        rows: [failedRow('r1', 'a.jpg', 'boom')],
      })
      expect(view.getByRole('status')).toHaveTextContent('')
      view.setRows([succeededRow('r1', 'a.jpg')])
      expect(view.getByRole('status')).toHaveTextContent('')
    })

    it('clears a standing announcement when the queue empties, and a later pick is quiet until its settle', () => {
      const view = renderHarness({
        rows: [uploadingRow('r1', 'a.jpg')],
      })
      view.setRows([succeededRow('r1', 'a.jpg')])
      expect(view.getByRole('status')).toHaveTextContent(uploadedAnnouncement('a.jpg'))
      view.setRows([])
      expect(view.queryByRole('status')).not.toBeInTheDocument()
      view.setRows([uploadingRow('r2', 'b.jpg')])
      expect(view.getByRole('status')).toHaveTextContent('')
      view.setRows([succeededRow('r2', 'b.jpg')])
      expect(view.getByRole('status')).toHaveTextContent(uploadedAnnouncement('b.jpg'))
    })

    it('re-renders a standing announcement in the new language and announces settles after the switch in it', async () => {
      const view = renderHarness({
        rows: [uploadingRow('r1', 'a.jpg')],
      })
      view.setRows([succeededRow('r1', 'a.jpg'), uploadingRow('r2', 'b.jpg', 0.5)])
      expect(view.getByRole('status')).toHaveTextContent(uploadedAnnouncement('a.jpg'))

      await act(async () => {
        await switchLanguage(view.i18n, 'en-US')
      })
      const enStatusSucceeded = enUS.fileUploader.statusSucceeded
      const enStatusUploading = enUS.fileUploader.statusUploading
      expect(view.getByText(enStatusSucceeded)).toBeInTheDocument()
      expect(view.getByText(enStatusUploading)).toBeInTheDocument()
      expect(view.getByRole('status')).toHaveTextContent(
        enUS.fileUploader.announceUploaded.replace('{{name}}', 'a.jpg'),
      )

      view.setRows([succeededRow('r1', 'a.jpg'), failedRow('r2', 'b.jpg', 'boom')])
      expect(view.getByRole('status')).toHaveTextContent(
        enUS.fileUploader.announceFailed.replace('{{name}}', 'b.jpg'),
      )
    })
  })

  describe('accessibility', () => {
    it('is axe-clean at idle', async () => {
      renderHarness({
        rows: [],
        onSelectFiles: vi.fn(),
        allowDrop: true,
      })
      await expectNoAxeViolations()
    })

    it('is axe-clean across populated states', async () => {
      renderHarness({
        rows: [
          uploadingRow('r1', 'up.jpg', 0.4),
          failedRow('r2', 'bad.jpg', 'The upload answered 503.'),
          succeededRow('r3', 'done.jpg'),
        ],
        onSelectFiles: vi.fn(),
        allowDrop: true,
        onCancel: vi.fn(),
        onRetry: vi.fn(),
        onRemove: vi.fn(),
      })
      await expectNoAxeViolations()
    })

    it('is axe-clean as a queue-only view and when disabled', async () => {
      renderHarness({
        rows: [uploadingRow('r1', 'up.jpg'), succeededRow('r2', 'done.jpg')],
      })
      await expectNoAxeViolations()
      renderHarness({
        rows: [],
        onSelectFiles: vi.fn(),
        allowDrop: true,
        disabled: true,
      })
      await expectNoAxeViolations()
    })
  })
})
