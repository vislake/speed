/**
 * FileUploader: foundation pins and queue behaviour.
 *
 * The foundation pins hold under the behaviour round: the trigger renders
 * its label from the bundles (asserted against the shipped JSON, never
 * inline), forwards multiple/accept to one native file input that stays
 * visually hidden but focusable inside the trigger label -- the control's
 * single tab stop -- and an empty queue renders no rows and no
 * announcements.
 *
 * The behaviour tests drive the controlled queue the contract pins: a
 * pick starts one parallel `execute` per file (fixture executors hold or
 * abort, never a real upload), each row runs the uploading/succeeded/
 * failed state machine with retry, cancel/remove and late-settle guards,
 * progress reports switch the bar from indeterminate to determinate, and
 * every settle is announced in the single polite live region. The drop
 * surface (allowDrop) and the en/zh language switch ride the same paths.
 */

import { act, fireEvent } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../../test-utils/render.js'
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { dropFiles, selectFiles } from '../../test-utils/file-input.js'
import { FileUploader } from './FileUploader.js'
import type { FileUploadContext, FileUploadExecutor } from './FileUploader.js'

const noopExecute: FileUploadExecutor = async () => {}

function makeFile(name: string): File {
  return new File(['x-ray bytes'], name, { type: 'application/octet-stream' })
}

function inputOf(container: HTMLElement): HTMLInputElement {
  const input = container.querySelector('input[type="file"]')
  if (input === null) {
    throw new Error('FileUploader did not render its file input')
  }
  return input as HTMLInputElement
}

interface HeldUpload {
  readonly file: File
  readonly context: FileUploadContext
  readonly resolve: () => void
  readonly reject: (reason?: unknown) => void
}

/** An executor whose uploads stay in flight until the test settles them. */
function createHeldExecutor(): { execute: FileUploadExecutor; uploads: HeldUpload[] } {
  const uploads: HeldUpload[] = []
  const execute: FileUploadExecutor = (file, context) =>
    new Promise<void>((resolve, reject) => {
      uploads.push({ file, context, resolve, reject })
    })
  return { execute, uploads }
}

/** An executor that rejects with an AbortError when its signal aborts. */
function createAbortingExecutor(): {
  execute: FileUploadExecutor
  contexts: FileUploadContext[]
} {
  const contexts: FileUploadContext[] = []
  const execute: FileUploadExecutor = (_file, context) => {
    contexts.push(context)
    return new Promise<void>((_resolve, reject) => {
      const rejectAbort = (): void => {
        reject(new DOMException('The upload was cancelled.', 'AbortError'))
      }
      if (context.signal.aborted) {
        rejectAbort()
      } else {
        context.signal.addEventListener('abort', rejectAbort, { once: true })
      }
    })
  }
  return { execute, contexts }
}

type DragInit = Parameters<typeof fireEvent.dragEnter>[1]
/** Drag events carry no meaningful payload here; jsdom has no DataTransfer. */
const emptyDragInit = { dataTransfer: {} } as DragInit

describe('FileUploader', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('renders the trigger with the built-in zh-CN label by default', () => {
    const { getByText } = renderWithProviders(<FileUploader execute={noopExecute} />)
    expect(getByText(zhCN.fileUploader.chooseFiles)).toBeInTheDocument()
  })

  it('switches the trigger label to the en-US bundle when the language changes', async () => {
    const { i18n, getByText, queryByText } = renderWithProviders(
      <FileUploader execute={noopExecute} />,
    )
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    expect(getByText(enUS.fileUploader.chooseFiles)).toBeInTheDocument()
    expect(queryByText(zhCN.fileUploader.chooseFiles)).not.toBeInTheDocument()
  })

  it('lets chooseFilesLabel override the built-in label', () => {
    const { getByText, queryByText } = renderWithProviders(
      <FileUploader execute={noopExecute} chooseFilesLabel="Upload a scan" />,
    )
    expect(getByText('Upload a scan')).toBeInTheDocument()
    expect(queryByText(zhCN.fileUploader.chooseFiles)).not.toBeInTheDocument()
  })

  it('hosts a single native file input without multiple or accept by default', () => {
    const { container } = renderWithProviders(<FileUploader execute={noopExecute} />)
    const input = container.querySelector('input[type="file"]')
    expect(input).not.toBeNull()
    expect(input).not.toHaveAttribute('multiple')
    expect(input).not.toHaveAttribute('accept')
  })

  it('forwards multiple and accept onto the input', () => {
    const { container } = renderWithProviders(
      <FileUploader execute={noopExecute} multiple accept="image/*" />,
    )
    const input = container.querySelector('input[type="file"]')
    expect(input).toHaveAttribute('multiple')
    expect(input).toHaveAttribute('accept', 'image/*')
  })

  it('keeps the input focusable inside the trigger label, the one tab stop', () => {
    const { container } = renderWithProviders(<FileUploader execute={noopExecute} />)
    const input = container.querySelector('input[type="file"]')
    const label = input?.closest('label')
    expect(label).not.toBeNull()
    expect(label!.tagName).toBe('LABEL')
    // The label does not masquerade as a button and is taken out of the
    // tab order: the visually hidden (clip, never display:none or
    // hidden) input inside it is the control's single tab stop.
    expect(label).not.toHaveAttribute('role')
    expect(label).toHaveAttribute('tabindex', '-1')
    expect(input).not.toHaveAttribute('hidden')
    expect(input).not.toHaveAttribute('aria-hidden')
    expect(input).not.toBeDisabled()
  })

  it('renders no rows and no announcements while the queue is empty', () => {
    const { queryByRole, queryByText } = renderWithProviders(
      <FileUploader execute={noopExecute} />,
    )
    expect(queryByRole('status')).not.toBeInTheDocument()
    expect(queryByRole('progressbar')).not.toBeInTheDocument()
    expect(queryByText(zhCN.fileUploader.statusUploading)).not.toBeInTheDocument()
    expect(queryByText(zhCN.fileUploader.statusSucceeded)).not.toBeInTheDocument()
    expect(queryByText(zhCN.fileUploader.statusFailed)).not.toBeInTheDocument()
  })

  it('does not call execute while idle', () => {
    const execute = vi.fn(noopExecute)
    renderWithProviders(<FileUploader execute={execute} />)
    expect(execute).not.toHaveBeenCalled()
  })

  it('passes axe over the idle picker', async () => {
    renderWithProviders(<FileUploader execute={noopExecute} />)
    await expectNoAxeViolations()
  })

  it('enqueues every picked file and starts one parallel upload per row', () => {
    const held = createHeldExecutor()
    const { container, getByRole, getAllByRole, getAllByText, getByText, queryByText } =
      renderWithProviders(<FileUploader execute={held.execute} multiple />)
    selectFiles(inputOf(container), [makeFile('panorama.png'), makeFile('closeup.jpg')])
    // Both uploads started at once, each with its own live context.
    expect(held.uploads).toHaveLength(2)
    expect(held.uploads[0]!.file.name).toBe('panorama.png')
    expect(held.uploads[1]!.file.name).toBe('closeup.jpg')
    expect(held.uploads[0]!.context.signal.aborted).toBe(false)
    expect(held.uploads[1]!.context.signal).not.toBe(held.uploads[0]!.context.signal)
    // Two rows, each with an indeterminate bar, an uploading caption and
    // a cancel action; no success/failure chrome yet.
    expect(getByText('panorama.png')).toBeInTheDocument()
    expect(getByText('closeup.jpg')).toBeInTheDocument()
    const bars = getAllByRole('progressbar')
    expect(bars).toHaveLength(2)
    for (const bar of bars) {
      expect(bar).not.toHaveAttribute('aria-valuenow')
    }
    expect(getAllByText(zhCN.fileUploader.statusUploading)).toHaveLength(2)
    expect(
      getAllByRole('button', { name: zhCN.fileUploader.actionCancel }),
    ).toHaveLength(2)
    expect(queryByText(zhCN.fileUploader.statusSucceeded)).not.toBeInTheDocument()
    expect(queryByText(zhCN.fileUploader.statusFailed)).not.toBeInTheDocument()
    // The live region mounts with the queue but stays silent until a settle.
    expect(getByRole('status').textContent).toBe('')
  })

  it('shows an indeterminate bar until the first report, then the fraction', () => {
    const held = createHeldExecutor()
    const { container, getByRole } = renderWithProviders(
      <FileUploader execute={held.execute} />,
    )
    selectFiles(inputOf(container), [makeFile('panorama.png')])
    const bar = getByRole('progressbar')
    expect(bar).not.toHaveAttribute('aria-valuenow')
    act(() => {
      held.uploads[0]!.context.onProgress(0.5)
    })
    expect(bar).toHaveAttribute('aria-valuenow', '50')
    // Out-of-range fractions fold into the bar's [0, 100] window instead
    // of reaching MUI and triggering its dev-mode range error.
    act(() => {
      held.uploads[0]!.context.onProgress(7)
    })
    expect(bar).toHaveAttribute('aria-valuenow', '100')
    act(() => {
      held.uploads[0]!.context.onProgress(-0.5)
    })
    expect(bar).toHaveAttribute('aria-valuenow', '0')
  })

  it('settles a resolved upload as succeeded, keeping the row with its actions', async () => {
    const held = createHeldExecutor()
    const { container, getByRole, getByText, queryByRole, queryByText } =
      renderWithProviders(<FileUploader execute={held.execute} />)
    selectFiles(inputOf(container), [makeFile('panorama.png')])
    await act(async () => {
      held.uploads[0]!.resolve()
    })
    // The row is retained with the success caption and a remove action;
    // the transfer chrome is gone.
    expect(getByText('panorama.png')).toBeInTheDocument()
    expect(getByText(zhCN.fileUploader.statusSucceeded)).toBeInTheDocument()
    expect(queryByText(zhCN.fileUploader.statusUploading)).not.toBeInTheDocument()
    expect(queryByRole('progressbar')).not.toBeInTheDocument()
    expect(
      getByRole('button', { name: zhCN.fileUploader.actionRemove }),
    ).toBeInTheDocument()
    expect(
      queryByRole('button', { name: zhCN.fileUploader.actionCancel }),
    ).not.toBeInTheDocument()
    // The settle is announced in the single polite live region.
    const expected = zhCN.fileUploader.announceUploaded.replace('{{name}}', 'panorama.png')
    expect(getByRole('status').textContent).toBe(expected)
    await expectNoAxeViolations()
  })

  it('settles a rejected upload as failed, rendering the rejection message', async () => {
    const held = createHeldExecutor()
    const { container, getByRole, getByText, queryByRole } = renderWithProviders(
      <FileUploader execute={held.execute} />,
    )
    selectFiles(inputOf(container), [makeFile('panorama.png')])
    await act(async () => {
      held.uploads[0]!.reject(new Error('File exceeds the 5 MB limit.'))
    })
    // The host-authored, host-translated rejection message renders as the
    // row's error text next to the built-in failure caption.
    expect(getByText(zhCN.fileUploader.statusFailed)).toBeInTheDocument()
    expect(getByText('File exceeds the 5 MB limit.')).toBeInTheDocument()
    expect(
      getByRole('button', { name: zhCN.fileUploader.actionRetry }),
    ).toBeInTheDocument()
    expect(
      getByRole('button', { name: zhCN.fileUploader.actionRemove }),
    ).toBeInTheDocument()
    expect(
      queryByRole('button', { name: zhCN.fileUploader.actionCancel }),
    ).not.toBeInTheDocument()
    const expected = zhCN.fileUploader.announceFailed.replace('{{name}}', 'panorama.png')
    expect(getByRole('status').textContent).toBe(expected)
    await expectNoAxeViolations()
  })

  it('falls back to the built-in failure text when the rejection carries no message', async () => {
    const held = createHeldExecutor()
    const { container, getAllByText, queryByText } = renderWithProviders(
      <FileUploader execute={held.execute} multiple />,
    )
    selectFiles(inputOf(container), [makeFile('blank.png'), makeFile('odd.bin')])
    await act(async () => {
      held.uploads[0]!.reject(new Error(''))
      held.uploads[1]!.reject('boom') // not an Error: no message at all
    })
    expect(getAllByText(zhCN.fileUploader.statusFailed)).toHaveLength(2)
    expect(queryByText('boom')).not.toBeInTheDocument()
    expect(container).not.toHaveTextContent(/undefined|\[object Object\]/)
  })

  it('retries the failed upload with the same file', async () => {
    const attempts = new Map<string, number>()
    const execute = vi.fn(async (file: File): Promise<void> => {
      const attempt = (attempts.get(file.name) ?? 0) + 1
      attempts.set(file.name, attempt)
      if (file.name === 'fragile.jpg' && attempt === 1) {
        throw new Error('temporary network glitch')
      }
    })
    const { container, getByRole, getByText, queryByText } = renderWithProviders(
      <FileUploader execute={execute} />,
    )
    selectFiles(inputOf(container), [makeFile('fragile.jpg')])
    await act(async () => {})
    expect(getByText(zhCN.fileUploader.statusFailed)).toBeInTheDocument()
    expect(getByText('temporary network glitch')).toBeInTheDocument()
    const firstFile = execute.mock.calls[0]![0] as File
    fireEvent.click(getByRole('button', { name: zhCN.fileUploader.actionRetry }))
    // The same File object is handed back to the executor.
    expect(execute).toHaveBeenCalledTimes(2)
    expect(execute.mock.calls[1]![0]).toBe(firstFile)
    // The row is uploading again, its error text cleared.
    expect(getByText(zhCN.fileUploader.statusUploading)).toBeInTheDocument()
    expect(queryByText('temporary network glitch')).not.toBeInTheDocument()
    await act(async () => {})
    expect(getByText(zhCN.fileUploader.statusSucceeded)).toBeInTheDocument()
  })

  it('cancel removes the row, aborts the signal and swallows the late settle', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const aborting = createAbortingExecutor()
    const onQueueChange = vi.fn()
    const { container, getByRole, queryByRole, queryByText } = renderWithProviders(
      <FileUploader execute={aborting.execute} onQueueChange={onQueueChange} />,
    )
    selectFiles(inputOf(container), [makeFile('panorama.png')])
    expect(onQueueChange).toHaveBeenCalledTimes(1)
    fireEvent.click(getByRole('button', { name: zhCN.fileUploader.actionCancel }))
    // The signal aborts and the row leaves with it; the queue is empty
    // again, so the live region is gone.
    expect(aborting.contexts[0]!.signal.aborted).toBe(true)
    expect(queryByText('panorama.png')).not.toBeInTheDocument()
    expect(queryByText(zhCN.fileUploader.statusUploading)).not.toBeInTheDocument()
    expect(queryByRole('status')).not.toBeInTheDocument()
    expect(
      queryByRole('button', { name: zhCN.fileUploader.actionCancel }),
    ).not.toBeInTheDocument()
    expect(onQueueChange).toHaveBeenCalledTimes(2)
    expect(onQueueChange.mock.calls[1]![0]).toEqual({
      uploading: 0,
      succeeded: 0,
      failed: 0,
      total: 0,
    })
    // The executor's AbortError settle lands after the row is gone: no
    // resurrection, no further report, nothing on the console.
    await act(async () => {})
    expect(queryByText('panorama.png')).not.toBeInTheDocument()
    expect(queryByRole('status')).not.toBeInTheDocument()
    expect(onQueueChange).toHaveBeenCalledTimes(2)
    expect(errorSpy).not.toHaveBeenCalled()
  })

  it('removing the last row returns the queue to its idle shape', async () => {
    const held = createHeldExecutor()
    const onQueueChange = vi.fn()
    const { container, getByRole, getByText, queryByRole, queryByText } =
      renderWithProviders(<FileUploader execute={held.execute} onQueueChange={onQueueChange} />)
    selectFiles(inputOf(container), [makeFile('panorama.png')])
    await act(async () => {
      held.uploads[0]!.resolve()
    })
    expect(getByText(zhCN.fileUploader.statusSucceeded)).toBeInTheDocument()
    fireEvent.click(getByRole('button', { name: zhCN.fileUploader.actionRemove }))
    expect(queryByText('panorama.png')).not.toBeInTheDocument()
    expect(queryByRole('status')).not.toBeInTheDocument()
    expect(queryByRole('progressbar')).not.toBeInTheDocument()
    expect(onQueueChange.mock.calls.map((call) => call[0])).toEqual([
      { uploading: 1, succeeded: 0, failed: 0, total: 1 },
      { uploading: 0, succeeded: 1, failed: 0, total: 1 },
      { uploading: 0, succeeded: 0, failed: 0, total: 0 },
    ])
  })

  it('aborts every in-flight upload on unmount and tolerates late settles', async () => {
    const held = createHeldExecutor()
    const { container, unmount } = renderWithProviders(
      <FileUploader execute={held.execute} multiple />,
    )
    selectFiles(inputOf(container), [makeFile('panorama.png'), makeFile('closeup.jpg')])
    unmount()
    expect(held.uploads[0]!.context.signal.aborted).toBe(true)
    expect(held.uploads[1]!.context.signal.aborted).toBe(true)
    // A host that ignores the abort and settles long after unmount must
    // be harmless: no crash, no warnings.
    await act(async () => {
      held.uploads[0]!.resolve()
      held.uploads[1]!.reject(new Error('late failure'))
    })
  })

  it('reports the queue summary once per transition across a full journey', async () => {
    const held = createHeldExecutor()
    const onQueueChange = vi.fn()
    const { container, getByRole, getAllByRole, getByText } = renderWithProviders(
      <FileUploader execute={held.execute} multiple onQueueChange={onQueueChange} />,
    )
    // No report while idle.
    expect(onQueueChange).not.toHaveBeenCalled()
    selectFiles(inputOf(container), [makeFile('panorama.png'), makeFile('fragile.jpg')])
    expect(onQueueChange).toHaveBeenCalledTimes(1)
    // First upload succeeds, second fails with a host message.
    await act(async () => {
      held.uploads[0]!.resolve()
    })
    await act(async () => {
      held.uploads[1]!.reject(new Error('File exceeds the 5 MB limit.'))
    })
    expect(getByText('File exceeds the 5 MB limit.')).toBeInTheDocument()
    // Retry the failed row: a fresh context starts for the same file.
    fireEvent.click(getByRole('button', { name: zhCN.fileUploader.actionRetry }))
    expect(held.uploads).toHaveLength(3)
    expect(held.uploads[2]!.context.signal.aborted).toBe(false)
    await act(async () => {
      held.uploads[2]!.resolve()
    })
    // Remove the two succeeded rows in DOM order.
    const removeButtons = (): ReturnType<typeof getAllByRole> =>
      getAllByRole('button', { name: zhCN.fileUploader.actionRemove })
    fireEvent.click(removeButtons()[0]!)
    fireEvent.click(removeButtons()[0]!)
    expect(onQueueChange.mock.calls.map((call) => call[0])).toEqual([
      { uploading: 2, succeeded: 0, failed: 0, total: 2 },
      { uploading: 1, succeeded: 1, failed: 0, total: 2 },
      { uploading: 0, succeeded: 1, failed: 1, total: 2 },
      { uploading: 1, succeeded: 1, failed: 0, total: 2 },
      { uploading: 0, succeeded: 2, failed: 0, total: 2 },
      { uploading: 0, succeeded: 1, failed: 0, total: 1 },
      { uploading: 0, succeeded: 0, failed: 0, total: 0 },
    ])
  })

  it('keeps two same-named files as distinct rows', async () => {
    const held = createHeldExecutor()
    const { container, getByRole, getAllByRole, getAllByText } = renderWithProviders(
      <FileUploader execute={held.execute} multiple />,
    )
    selectFiles(inputOf(container), [makeFile('scan.png'), makeFile('scan.png')])
    expect(getAllByText('scan.png')).toHaveLength(2)
    expect(held.uploads[0]!.file).not.toBe(held.uploads[1]!.file)
    expect(getAllByRole('progressbar')).toHaveLength(2)
    await act(async () => {
      held.uploads[0]!.resolve()
      held.uploads[1]!.resolve()
    })
    expect(getAllByText(zhCN.fileUploader.statusSucceeded)).toHaveLength(2)
    const expected = zhCN.fileUploader.announceUploaded.replace('{{name}}', 'scan.png')
    expect(getByRole('status').textContent).toBe(expected)
  })

  it('renders no drop surface by default and ignores drop events', () => {
    const held = createHeldExecutor()
    const { container, queryByRole, queryByText } = renderWithProviders(
      <FileUploader execute={held.execute} />,
    )
    expect(queryByText(zhCN.fileUploader.dropHint)).not.toBeInTheDocument()
    const root = container.firstElementChild
    expect(root).not.toBeNull()
    dropFiles(root!, [makeFile('panorama.png')])
    expect(held.uploads).toHaveLength(0)
    expect(queryByText('panorama.png')).not.toBeInTheDocument()
    expect(queryByRole('progressbar')).not.toBeInTheDocument()
  })

  it('allowDrop: drags toggle the surface state and a drop enqueues like a pick', async () => {
    const held = createHeldExecutor()
    const { getByText, queryByText } = renderWithProviders(
      <FileUploader execute={held.execute} allowDrop />,
    )
    const hint = getByText(zhCN.fileUploader.dropHint)
    const zone = hint.parentElement
    expect(zone).not.toBeNull()
    // Enter arms the visual state; leaving clears it.
    fireEvent.dragEnter(zone!, emptyDragInit)
    expect(zone!.getAttribute('data-drop-active')).toBe('true')
    await expectNoAxeViolations()
    fireEvent.dragOver(zone!, emptyDragInit)
    expect(zone!.getAttribute('data-drop-active')).toBe('true')
    fireEvent.dragLeave(zone!, emptyDragInit)
    expect(zone!.getAttribute('data-drop-active')).toBeNull()
    // Drag back in and drop: the same enqueue path as picking.
    fireEvent.dragEnter(zone!, emptyDragInit)
    dropFiles(zone!, [makeFile('panorama.png')])
    expect(zone!.getAttribute('data-drop-active')).toBeNull()
    expect(held.uploads).toHaveLength(1)
    expect(held.uploads[0]!.file.name).toBe('panorama.png')
    expect(getByText('panorama.png')).toBeInTheDocument()
    expect(queryByText(zhCN.fileUploader.statusUploading)).toBeInTheDocument()
    await expectNoAxeViolations()
  })

  it('re-renders every queue state in the language switched to', async () => {
    const held = createHeldExecutor()
    const { i18n, container, getByRole, getByText, queryByText } = renderWithProviders(
      <FileUploader execute={held.execute} multiple />,
    )
    selectFiles(inputOf(container), [makeFile('panorama.png'), makeFile('fragile.jpg')])
    await act(async () => {
      held.uploads[1]!.reject(new Error('host-authored message'))
    })
    // An uploading row and a failed row coexist; switch the app language.
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    expect(getByText(enUS.fileUploader.statusUploading)).toBeInTheDocument()
    expect(getByText(enUS.fileUploader.statusFailed)).toBeInTheDocument()
    expect(getByText('host-authored message')).toBeInTheDocument()
    expect(
      getByRole('button', { name: enUS.fileUploader.actionCancel }),
    ).toBeInTheDocument()
    expect(
      getByRole('button', { name: enUS.fileUploader.actionRetry }),
    ).toBeInTheDocument()
    expect(
      getByRole('button', { name: enUS.fileUploader.actionRemove }),
    ).toBeInTheDocument()
    // The announcement re-renders in the switched language too.
    const expected = enUS.fileUploader.announceFailed.replace('{{name}}', 'fragile.jpg')
    expect(getByRole('status').textContent).toBe(expected)
    expect(queryByText(zhCN.fileUploader.statusUploading)).not.toBeInTheDocument()
    // A settle after the switch announces in the switched language.
    await act(async () => {
      held.uploads[0]!.resolve()
    })
    const uploaded = enUS.fileUploader.announceUploaded.replace('{{name}}', 'panorama.png')
    expect(getByRole('status').textContent).toBe(uploaded)
  })
})
