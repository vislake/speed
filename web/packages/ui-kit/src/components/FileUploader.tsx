/**
 * FileUploader: the file queue rendered from host-owned rows.
 *
 * Ownership contract: FileUploader is fully controlled, like every other
 * component in this package. `rows` describes the queue the host wants
 * shown right now -- each row's status and progress are props, never
 * widget state -- and every interaction reports up through a callback
 * (`onSelectFiles` for a pick or drop, `onCancel` / `onRetry` / `onRemove`
 * keyed by row id). The component never fetches, never executes an upload,
 * never holds a File longer than the event handler that received it, and
 * keeps no record of rows that are not in the current `rows` prop: an
 * upload in flight is host state, running against host transport code.
 *
 * Announcements: when a row leaves `uploading` for `succeeded` or
 * `failed`, the component announces the settle once in a single polite
 * live region (role="status"), stored structurally and rendered in the
 * language active at render time. Only status transitions announce --
 * rows the host seeds on mount, progress-only changes and rows a host
 * heals directly from `failed` to `succeeded` stay quiet -- and a retry
 * (a `failed` row returning to `uploading`) clears the region first, so a
 * repeat of an identical failure re-announces instead of sitting silent.
 *
 * Render shape: rows render as cards below the trigger once the queue has
 * content; an empty `rows` renders no queue at all. The picker affordance
 * renders only when `onSelectFiles` is given (the label is the built-in
 * bilingual text unless `chooseFilesLabel` overrides it, with the file
 * input visually hidden but focusable inside the label, one tab stop for
 * the whole control). With `allowDrop` a drop surface sits below the
 * trigger and reports through the same callback -- the keyboard path never
 * depends on it. `disabled` renders the affordances inert and ignores
 * picks and drops.
 */

import { useEffect, useRef, useState } from 'react'
import type { ChangeEvent, DragEvent as ReactDragEvent } from 'react'
import type { SxProps, Theme } from '@mui/material/styles'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import LinearProgress from '@mui/material/LinearProgress'
import Typography from '@mui/material/Typography'
import { styled } from '@mui/material/styles'
import { useUiKitTranslation } from '../internal/translation.js'

/** One row's transfer state, owned by the host. */
export type FileUploaderRowStatus = 'uploading' | 'succeeded' | 'failed'

/** One queue row the host wants rendered, exactly as it should appear. */
export interface FileUploaderRow {
  /** Stable row identity; the key of the rendered card and the id in callbacks. */
  readonly id: string
  /** The file's display name, rendered verbatim. */
  readonly name: string
  /** The row's transfer state. */
  readonly status: FileUploaderRowStatus
  /**
   * Upload progress within [0, 1]. An uploading row with no `progress`
   * shows an indeterminate bar; with one, a determinate bar whose value
   * folds out-of-range and non-finite fractions into [0, 1] at render
   * time. Ignored once the row settles.
   */
  readonly progress?: number
  /** The failed row's error text, host-written and host-translated; rendered verbatim when present. */
  readonly error?: string
}

export interface FileUploaderProps {
  /** The queue to render, owned by the host. Uploads run in host code. */
  readonly rows: readonly FileUploaderRow[]
  /**
   * Reports one pick or drop's files (in order, never held by the
   * component after the call). Its presence renders the picker trigger
   * and, with `allowDrop`, the drop surface; without it the component is
   * a pure queue view.
   */
  readonly onSelectFiles?: (files: readonly File[]) => void
  /** Cancels the named row's transfer (an abort is the host's to perform). */
  readonly onCancel?: (rowId: string) => void
  /** Retries the named row (rendered on failed rows only). */
  readonly onRetry?: (rowId: string) => void
  /** Removes the named row from the queue (rendered on settled rows only). */
  readonly onRemove?: (rowId: string) => void
  /** Pick several files per selection; false by default (native input). */
  readonly multiple?: boolean
  /** Forwarded to the picker (advisory only — real validation is the host's upload code). */
  readonly accept?: string
  /** Also accept drag-and-drop; false by default. The keyboard path never depends on it. */
  readonly allowDrop?: boolean
  /** Renders the affordances inert and ignores picks and drops. */
  readonly disabled?: boolean
  /** Picker affordance label; defaults to the built-in bilingual text. */
  readonly chooseFilesLabel?: string
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

/** The last settle, for the live region. Rendered at render time in the active language. */
type Announcement = { readonly kind: 'uploaded' | 'failed'; readonly name: string } | null

/** One commit's announcement effect, derived by diffing `rows` against the previous render's. */
type AnnouncementChange =
  | { readonly type: 'clear' }
  | { readonly type: 'announce'; readonly kind: 'uploaded' | 'failed'; readonly name: string }
  | null

/** Clamp a progress fraction to [0, 1] and scale it to a percent for MUI. */
function toPercent(fraction: number): number {
  if (Number.isNaN(fraction)) {
    return 0
  }
  return Math.round(Math.min(1, Math.max(0, fraction)) * 100)
}

/**
 * The queue renderer: host-owned `rows`, optional pick/drop reporting and
 * per-row action callbacks, and the announcement live region.
 */
export function FileUploader({
  accept,
  allowDrop,
  chooseFilesLabel,
  disabled = false,
  multiple,
  onCancel,
  onRemove,
  onRetry,
  onSelectFiles,
  rows,
  sx,
}: FileUploaderProps) {
  const { t } = useUiKitTranslation()
  const [announcement, setAnnouncement] = useState<Announcement>(null)
  const previousRowsRef = useRef<readonly FileUploaderRow[] | null>(null)
  const [dragDepth, setDragDepth] = useState(0)

  // Diff the rows against the previous commit to find status transitions:
  // only they announce. Mounted rows (the first commit) are seeded and
  // stay quiet; an empty commit clears any standing announcement; a row
  // returning to `uploading` (a retry) clears it so an identical later
  // failure re-announces; a row leaving `uploading` announces the settle,
  // the last one in list order winning a commit with several. Every other
  // flip (a host healing a failed row straight to succeeded) is not a
  // transfer transition and announces nothing.
  useEffect(() => {
    const previous = previousRowsRef.current
    previousRowsRef.current = rows
    if (previous === null) {
      return
    }
    if (rows.length === 0) {
      setAnnouncement(null)
      return
    }
    let change: AnnouncementChange = null
    for (const row of rows) {
      const before = previous.find((candidate) => candidate.id === row.id)
      if (before === undefined || before.status === row.status) {
        continue
      }
      if (row.status === 'uploading') {
        change = { type: 'clear' }
      } else if (before.status === 'uploading') {
        change = {
          type: 'announce',
          kind: row.status === 'succeeded' ? 'uploaded' : 'failed',
          name: row.name,
        }
      }
    }
    if (change === null) {
      return
    }
    const nextChange = change
    setAnnouncement((current) => {
      if (nextChange.type === 'clear') {
        return current === null ? current : null
      }
      return current !== null &&
        current.kind === nextChange.kind &&
        current.name === nextChange.name
        ? current
        : { kind: nextChange.kind, name: nextChange.name }
    })
  }, [rows])

  const handlePick = (event: ChangeEvent<HTMLInputElement>): void => {
    if (disabled) {
      return
    }
    // Snapshot the selection before resetting the input: a real browser's
    // FileList is live, so clearing the input's `value` would empty a
    // captured reference that was only read afterwards (jsdom's snapshot
    // semantics hide the difference).
    const input = event.currentTarget
    const picked = input.files === null ? [] : Array.from(input.files)
    input.value = ''
    if (picked.length === 0) {
      return
    }
    onSelectFiles?.(picked)
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
    if (disabled) {
      return
    }
    const dataTransfer = event.dataTransfer
    const dropped = dataTransfer === null ? [] : Array.from(dataTransfer.files)
    if (dropped.length === 0) {
      return
    }
    onSelectFiles?.(dropped)
  }

  const triggerLabel = chooseFilesLabel ?? t('fileUploader.chooseFiles')
  const dragActive = dragDepth > 0
  const pickerRendered = onSelectFiles !== undefined

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
      {pickerRendered ? (
        <Button
          component="label"
          variant="contained"
          role={undefined}
          tabIndex={-1}
          aria-disabled={disabled || undefined}
          sx={{
            cursor: disabled ? 'default' : 'pointer',
            opacity: disabled
              ? (theme) => theme.palette.action.disabledOpacity
              : undefined,
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
            disabled={disabled}
            onChange={handlePick}
          />
        </Button>
      ) : null}
      {pickerRendered && allowDrop && !disabled ? (
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
      {rows.length > 0 ? (
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
          {rows.map((row) => (
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
                {row.name}
              </Typography>
              {row.status === 'uploading' ? (
                <>
                  <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, mt: 0.5 }}>
                    <LinearProgress
                      aria-label={t('fileUploader.statusUploading')}
                      variant={row.progress === undefined ? 'indeterminate' : 'determinate'}
                      value={row.progress === undefined ? undefined : toPercent(row.progress)}
                      sx={{ flexGrow: 1 }}
                    />
                    {onCancel !== undefined ? (
                      <Button size="small" onClick={() => onCancel(row.id)}>
                        {t('fileUploader.actionCancel')}
                      </Button>
                    ) : null}
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
                  {row.status === 'failed' && onRetry !== undefined ? (
                    <Button size="small" onClick={() => onRetry(row.id)}>
                      {t('fileUploader.actionRetry')}
                    </Button>
                  ) : null}
                  {onRemove !== undefined ? (
                    <Button size="small" onClick={() => onRemove(row.id)}>
                      {t('fileUploader.actionRemove')}
                    </Button>
                  ) : null}
                </Box>
              )}
              {row.status === 'failed' && row.error ? (
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
          ))}
        </Box>
      ) : null}
      {rows.length > 0 ? (
        <Box component="p" role="status" sx={{ m: 0 }}>
          <Typography variant="caption" color="text.secondary" component="span">
            {announcement === null
              ? ''
              : t(
                  announcement.kind === 'uploaded'
                    ? 'fileUploader.announceUploaded'
                    : 'fileUploader.announceFailed',
                  { name: announcement.name },
                )}
          </Typography>
        </Box>
      ) : null}
    </Box>
  )
}
