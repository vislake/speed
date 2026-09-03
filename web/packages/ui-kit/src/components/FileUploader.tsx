/**
 * FileUploader: the file picker with a resident transfer queue.
 *
 * Ownership contract: FileUploader is an execute-injected widget. Picking
 * files enqueues them and the widget calls the host-supplied `execute`
 * once per file, in parallel. The queue and each row's transfer state are
 * interaction-local state -- the same carve-out as ConfirmDialog's
 * `armed` or AppShell's mobile drawer: born and dying with the mount,
 * never lifted into host state. The component itself makes zero network
 * calls, touches no storage and persists nothing; everything the host
 * needs to know is reported up through `onQueueChange`.
 *
 * Queue behaviour: each row runs the uploading -> succeeded | failed
 * state machine. A row settles exactly once: the settle applies only
 * while the row still exists and still uploads, so an executor that
 * resolves or rejects after a cancel, a remove or an unmount is ignored
 * (a cancel aborts the row's signal first). Cancel and remove take the
 * row out of the queue; retry hands the same File back to `execute` with
 * a fresh context. Progress is kept out of the rows so a fraction update
 * is never a queue transition. Settles announce in a single polite live
 * region (role="status") that mounts with the queue; the announcement is
 * stored structurally and rendered in the language active at render
 * time.
 *
 * Render shape: a resident trigger affordance (the picker label is the
 * built-in bilingual text unless `chooseFilesLabel` overrides it, with
 * the file input visually hidden but focusable inside the label, one tab
 * stop for the whole control). With `allowDrop` a drop surface sits below
 * the trigger and enqueues through the same path as a pick -- the
 * keyboard path never depends on it. The queue rows and the announcement
 * live region render below the trigger once the queue has content; an
 * empty queue renders only the trigger, with no rows and no
 * announcements.
 */

import { useEffect, useReducer, useRef, useState } from 'react'
import type { ChangeEvent, DragEvent as ReactDragEvent } from 'react'
import type { SxProps, Theme } from '@mui/material/styles'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import LinearProgress from '@mui/material/LinearProgress'
import Typography from '@mui/material/Typography'
import { styled } from '@mui/material/styles'
import { useUiKitTranslation } from '../internal/translation.js'

/**
 * Per-file transfer context handed to the host's executor.
 */
export interface FileUploadContext {
  /** Aborts when the user cancels/removes the row, or the widget unmounts. */
  readonly signal: AbortSignal
  /**
   * Report progress within [0, 1]. The row shows an indeterminate bar
   * until the first call, then a determinate one.
   */
  readonly onProgress: (fraction: number) => void
}

/**
 * The host's upload execution, called once per picked file, in parallel.
 *
 * Resolving completes the row; rejecting puts the row into the failed
 * state. The rejection's `message` — when non-empty — renders as the
 * row's error text: it is host-written and host-translated (the same
 * contract as FormField's host-authored validation-error text); an
 * empty message falls back to the built-in failure text. Pre-flight
 * validation (size, type, count) is the executor's job: reject early,
 * before touching the network.
 */
export type FileUploadExecutor = (
  file: File,
  context: FileUploadContext,
) => Promise<void>

/** Counts reported up after every queue transition, for host submit gating. */
export interface FileUploadQueueSummary {
  readonly uploading: number
  readonly succeeded: number
  readonly failed: number
  readonly total: number
}

export interface FileUploaderProps {
  /** The upload execution (see FileUploadExecutor). Required. */
  readonly execute: FileUploadExecutor
  /** Pick several files per selection; false by default (native input). */
  readonly multiple?: boolean
  /** Forwarded to the picker (advisory only — real validation is execute's). */
  readonly accept?: string
  /** Also accept drag-and-drop; false by default. The keyboard path never depends on it. */
  readonly allowDrop?: boolean
  /** Picker affordance label; defaults to the built-in bilingual text. */
  readonly chooseFilesLabel?: string
  /** Queue summary after every transition (add, settle, retry, remove, cancel). */
  readonly onQueueChange?: (summary: FileUploadQueueSummary) => void
  /** sx pass-through, consistent with sibling components. */
  readonly sx?: SxProps<Theme>
}

/**
 * The file input itself: visually hidden through the clip technique (not
 * `display: none`), so it stays focusable and keeps its place in the tab
 * order. The trigger is a MUI Button rendered as a <label> wrapping this
 * input, so an activation opens the native picker; the label carries no
 * button role and is removed from the tab order (`role={undefined}` and
 * `tabIndex={-1}`), leaving the input as the control's single tab stop.
 * While the input is focused the trigger shows a visible focus
 * indicator through the label's :focus-within rule.
 */
const HiddenFileInput = styled('input')({
  clip: 'rect(0 0 0 0)',
  clipPath: 'inset(50%)',
  height: 1,
  overflow: 'hidden',
  position: 'absolute',
  bottom: 0,
  left: 0,
  whiteSpace: 'nowrap',
  width: 1,
})

type RowStatus = 'uploading' | 'succeeded' | 'failed'

interface QueueRow {
  readonly id: number
  readonly file: File
  readonly status: RowStatus
  /** The failed row's error text, from the rejection message; null otherwise. */
  readonly error: string | null
}

/** The last settle, for the live region. Rendered at render time in the active language. */
type Announcement = { readonly kind: 'uploaded' | 'failed'; readonly name: string } | null

interface QueueState {
  readonly rows: readonly QueueRow[]
  /**
   * Per-row progress within [0, 1], kept outside the rows so a progress
   * update never changes the rows' identity (and so is never reported as
   * a queue transition). null until the first report.
   */
  readonly progress: Readonly<Record<number, number | null>>
  readonly announcement: Announcement
}

type QueueAction =
  | {
      readonly type: 'enqueue'
      readonly entries: readonly { readonly id: number; readonly file: File }[]
    }
  | { readonly type: 'settle'; readonly id: number; readonly outcome: RowStatus; readonly reason?: unknown }
  | { readonly type: 'retry'; readonly id: number }
  | { readonly type: 'remove'; readonly id: number }
  | { readonly type: 'progress'; readonly id: number; readonly fraction: number }

const initialState: QueueState = { rows: [], progress: {}, announcement: null }

/** The rejection message when it exists and is worth showing; null otherwise. */
function failureMessage(reason: unknown): string | null {
  return reason instanceof Error && reason.message !== '' ? reason.message : null
}

function queueReducer(state: QueueState, action: QueueAction): QueueState {
  switch (action.type) {
    case 'enqueue': {
      const rows: QueueRow[] = [
        ...state.rows,
        ...action.entries.map(({ id, file }) => ({ id, file, status: 'uploading' as const, error: null })),
      ]
      const progress = { ...state.progress }
      for (const { id } of action.entries) {
        progress[id] = null
      }
      return { rows, progress, announcement: state.announcement }
    }
    case 'settle': {
      const index = state.rows.findIndex((row) => row.id === action.id)
      const row = index === -1 ? undefined : state.rows[index]
      // A late settle (after cancel, remove or unmount) or a second settle
      // for an already-settled row changes nothing.
      if (row === undefined || row.status !== 'uploading') {
        return state
      }
      const rows = state.rows.slice()
      const settled =
        action.outcome === 'failed'
          ? { ...row, status: 'failed' as const, error: failureMessage(action.reason) }
          : { ...row, status: 'succeeded' as const, error: null }
      rows[index] = settled
      return {
        rows,
        progress: state.progress,
        announcement: {
          kind: settled.status === 'succeeded' ? 'uploaded' : 'failed',
          name: row.file.name,
        },
      }
    }
    case 'retry': {
      const index = state.rows.findIndex((row) => row.id === action.id)
      const row = index === -1 ? undefined : state.rows[index]
      if (row === undefined || row.status !== 'failed') {
        return state
      }
      const rows = state.rows.slice()
      rows[index] = { ...row, status: 'uploading', error: null }
      return { rows, progress: { ...state.progress, [action.id]: null }, announcement: state.announcement }
    }
    case 'remove': {
      if (!state.rows.some((row) => row.id === action.id)) {
        return state
      }
      const progress = { ...state.progress }
      delete progress[action.id]
      return {
        rows: state.rows.filter((row) => row.id !== action.id),
        progress,
        announcement: state.announcement,
      }
    }
    case 'progress': {
      const row = state.rows.find((candidate) => candidate.id === action.id)
      // Stale progress from a settled or removed row changes nothing.
      if (row === undefined || row.status !== 'uploading') {
        return state
      }
      // Fold out-of-range fractions into [0, 1] so MUI never sees a value
      // outside its [0, 100] window.
      const fraction = Math.min(1, Math.max(0, action.fraction))
      return { rows: state.rows, progress: { ...state.progress, [action.id]: fraction }, announcement: state.announcement }
    }
  }
}

function summarize(rows: readonly QueueRow[]): FileUploadQueueSummary {
  let uploading = 0
  let succeeded = 0
  let failed = 0
  for (const row of rows) {
    if (row.status === 'uploading') uploading += 1
    else if (row.status === 'succeeded') succeeded += 1
    else failed += 1
  }
  return { uploading, succeeded, failed, total: rows.length }
}

/**
 * The picker: trigger affordance with the built-in bilingual label and a
 * hidden-but-focusable file input, an opt-in drop surface, the resident
 * queue and the announcement live region.
 */
export function FileUploader({
  accept,
  allowDrop,
  chooseFilesLabel,
  execute,
  multiple,
  onQueueChange,
  sx,
}: FileUploaderProps) {
  const { t } = useUiKitTranslation()
  const [queue, dispatch] = useReducer(queueReducer, initialState)
  const nextRowId = useRef(0)
  const controllersRef = useRef(new Map<number, AbortController>())
  const [dragDepth, setDragDepth] = useState(0)

  const onQueueChangeRef = useRef(onQueueChange)
  useEffect(() => {
    onQueueChangeRef.current = onQueueChange
  }, [onQueueChange])

  // Report a summary once per queue transition: never on the first
  // render, never when progress moves (the rows keep their identity).
  const reportedRowsRef = useRef(queue.rows)
  useEffect(() => {
    if (queue.rows === reportedRowsRef.current) {
      return
    }
    reportedRowsRef.current = queue.rows
    onQueueChangeRef.current?.(summarize(queue.rows))
  }, [queue.rows])

  // In-flight uploads die with the widget.
  useEffect(() => {
    const controllers = controllersRef.current
    return () => {
      for (const controller of controllers.values()) {
        controller.abort()
      }
    }
  }, [])

  const enqueueFiles = (files: FileList | readonly File[] | null): void => {
    if (files === null) {
      return
    }
    const picked = Array.from(files)
    if (picked.length === 0) {
      return
    }
    const entries = picked.map((file) => ({ id: nextRowId.current++, file }))
    dispatch({ type: 'enqueue', entries })
    for (const entry of entries) {
      startUpload(entry)
    }
  }

  // Called from event handlers (never from an effect), so no controller
  // exists while the mount effects run.
  const startUpload = (entry: { readonly id: number; readonly file: File }): void => {
    const controller = new AbortController()
    controllersRef.current.set(entry.id, controller)
    const context: FileUploadContext = {
      signal: controller.signal,
      onProgress: (fraction) => {
        dispatch({ type: 'progress', id: entry.id, fraction })
      },
    }
    void (async () => {
      try {
        await execute(entry.file, context)
        dispatch({ type: 'settle', id: entry.id, outcome: 'succeeded' })
      } catch (reason) {
        dispatch({ type: 'settle', id: entry.id, outcome: 'failed', reason })
      }
    })()
  }

  const handlePick = (event: ChangeEvent<HTMLInputElement>): void => {
    // Read the selection first, then reset the input so picking the same
    // file again later fires a fresh change event.
    const files = event.currentTarget.files
    event.currentTarget.value = ''
    enqueueFiles(files)
  }

  const handleRetry = (row: QueueRow): void => {
    dispatch({ type: 'retry', id: row.id })
    startUpload({ id: row.id, file: row.file })
  }

  const handleRemove = (row: QueueRow): void => {
    if (row.status === 'uploading') {
      controllersRef.current.get(row.id)?.abort()
    }
    controllersRef.current.delete(row.id)
    dispatch({ type: 'remove', id: row.id })
  }

  const handleDragEnter = (event: ReactDragEvent<HTMLDivElement>): void => {
    event.preventDefault()
    setDragDepth((depth) => depth + 1)
  }

  const handleDragOver = (event: ReactDragEvent<HTMLDivElement>): void => {
    // preventDefault is what permits the drop to land here.
    event.preventDefault()
  }

  const handleDragLeave = (event: ReactDragEvent<HTMLDivElement>): void => {
    event.preventDefault()
    // Enter/leave pairs fire per child crossing; the depth counter keeps
    // the armed state true until the drag leaves the surface for good.
    setDragDepth((depth) => Math.max(0, depth - 1))
  }

  const handleDrop = (event: ReactDragEvent<HTMLDivElement>): void => {
    event.preventDefault()
    setDragDepth(0)
    enqueueFiles(event.dataTransfer === null ? null : event.dataTransfer.files)
  }

  const triggerLabel = chooseFilesLabel ?? t('fileUploader.chooseFiles')
  const dragActive = dragDepth > 0

  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'flex-start',
        gap: 1,
        ...sx,
      }}
    >
      <Button
        component="label"
        variant="contained"
        role={undefined}
        tabIndex={-1}
        sx={{
          cursor: 'pointer',
          '&:focus-within': {
            outline: '2px solid',
            outlineOffset: 2,
            outlineColor: 'primary.main',
          },
        }}
      >
        {triggerLabel}
        <HiddenFileInput
          type="file"
          multiple={multiple}
          accept={accept}
          onChange={handlePick}
        />
      </Button>
      {allowDrop ? (
        <Box
          data-drop-active={dragActive || undefined}
          onDragEnter={handleDragEnter}
          onDragOver={handleDragOver}
          onDragLeave={handleDragLeave}
          onDrop={handleDrop}
          sx={{
            width: '100%',
            py: 1.5,
            px: 2,
            borderRadius: 1,
            border: '1px dashed',
            borderColor: dragActive ? 'primary.main' : 'divider',
            bgcolor: dragActive ? 'action.hover' : 'transparent',
          }}
        >
          <Typography variant="body2" color="text.secondary">
            {t('fileUploader.dropHint')}
          </Typography>
        </Box>
      ) : null}
      {queue.rows.length > 0 ? (
        <Box
          component="ul"
          sx={{
            width: '100%',
            listStyle: 'none',
            m: 0,
            p: 0,
            display: 'flex',
            flexDirection: 'column',
            gap: 1,
          }}
        >
          {queue.rows.map((row) => {
            const progress = queue.progress[row.id] ?? null
            return (
              <Box
                component="li"
                key={row.id}
                sx={{
                  width: '100%',
                  border: '1px solid',
                  borderColor: 'divider',
                  borderRadius: 1,
                  px: 1.5,
                  py: 1,
                }}
              >
                <Typography variant="body2" sx={{ wordBreak: 'break-all' }}>
                  {row.file.name}
                </Typography>
                {row.status === 'uploading' ? (
                  <>
                    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, mt: 0.5 }}>
                      <LinearProgress
                        aria-label={t('fileUploader.statusUploading')}
                        variant={progress === null ? 'indeterminate' : 'determinate'}
                        value={progress === null ? undefined : Math.round(progress * 100)}
                        sx={{ flexGrow: 1 }}
                      />
                      <Button size="small" onClick={() => handleRemove(row)}>
                        {t('fileUploader.actionCancel')}
                      </Button>
                    </Box>
                    <Typography
                      variant="caption"
                      color="text.secondary"
                      component="p"
                      sx={{ mt: 0.5 }}
                    >
                      {t('fileUploader.statusUploading')}
                    </Typography>
                  </>
                ) : (
                  <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, mt: 0.5 }}>
                    <Typography
                      variant="caption"
                      sx={{
                        color:
                          row.status === 'succeeded' ? 'success.main' : 'error.main',
                        flexGrow: 1,
                      }}
                      component="p"
                    >
                      {t(
                        row.status === 'succeeded'
                          ? 'fileUploader.statusSucceeded'
                          : 'fileUploader.statusFailed',
                      )}
                    </Typography>
                    {row.status === 'failed' ? (
                      <Button size="small" onClick={() => handleRetry(row)}>
                        {t('fileUploader.actionRetry')}
                      </Button>
                    ) : null}
                    <Button size="small" onClick={() => handleRemove(row)}>
                      {t('fileUploader.actionRemove')}
                    </Button>
                  </Box>
                )}
                {row.status === 'failed' && row.error !== null ? (
                  <Typography
                    variant="caption"
                    color="error.main"
                    component="p"
                    sx={{ mt: 0.5 }}
                  >
                    {row.error}
                  </Typography>
                ) : null}
              </Box>
            )
          })}
        </Box>
      ) : null}
      {queue.rows.length > 0 ? (
        <Box component="p" role="status" sx={{ m: 0 }}>
          <Typography variant="caption" color="text.secondary" component="span">
            {queue.announcement === null
              ? ''
              : t(
                  queue.announcement.kind === 'uploaded'
                    ? 'fileUploader.announceUploaded'
                    : 'fileUploader.announceFailed',
                  { name: queue.announcement.name },
                )}
          </Typography>
        </Box>
      ) : null}
    </Box>
  )
}
