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
 * `failed` -- or joins the queue already settled, its transfer having run
 * entirely in host code before an `uploading` commit ever rendered -- the
 * component announces the settle once in a single polite live region
 * (role="status"), stored structurally and rendered in the language
 * active at render time. Rows the host seeds on mount, rows appended as
 * `uploading`, progress-only changes and rows a host heals directly from
 * `failed` to `succeeded` stay quiet. Two repeat paths stay audible in
 * the region, whose text is the only thing a screen reader hears: a
 * retry (a `failed` row returning to `uploading`) clears the region
 * first, and a settle whose announcement text equals the standing one (a
 * second same-name row reaching the same outcome) empties the region and
 * re-announces a tick later -- identical text is not a change, and a
 * live region only speaks about changes. The announced row is never a
 * ghost: when `rows` stops carrying it (the Remove button's host
 * response dropping a settled row while the queue continues), the
 * announcement retires with the commit that dropped it, and the rendered
 * text additionally derives only from rows the current `rows` prop still
 * carries, so the region never points at a file the queue has let go.
 * The deferred re-announcement honours the same rule at fire time: a
 * host rows update can land between the commit that armed it and its
 * 0ms timer (a microtask-ordered update overtakes the timer), so the
 * re-announcement re-checks its target row against the latest committed
 * rows -- still present and still settled in the announced state -- and
 * stays silent when the row was retried or removed in the meantime,
 * instead of resurrecting stale text over the row's new fate.
 *
 * Render shape: rows render as one real list (a `ul` with the explicit
 * `role="list"` -- WebKit strips list semantics from a list-style-none
 * `ul` -- whose items are the queue cards) below the trigger once the
 * queue has content; an empty `rows` renders no queue at all. Each row's action-
 * button group (Cancel while uploading; Retry/Remove once settled) is a
 * responsive `flexWrap: 'wrap'` flex row, purely additive CSS, so a long
 * file name plus several action buttons cannot overflow a narrow queue
 * card regardless of name length. The picker affordance
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

/**
 * The last settle, for the live region. Rendered at render time in the
 * active language. `rowId` distinguishes two rows that announce the same
 * text (same name, same outcome): the region itself cannot re-speak
 * identical text, so the settle logic uses the row identity to decide
 * when a clear-then-reannounce is owed.
 */
type Announcement = {
  readonly kind: 'uploaded' | 'failed'
  readonly name: string
  readonly rowId: string
} | null

/** One commit's announcement effect, derived by diffing `rows` against the previous render's. */
type AnnouncementChange =
  | { readonly type: 'clear' }
  | {
      readonly type: 'announce'
      readonly kind: 'uploaded' | 'failed'
      readonly name: string
      readonly rowId: string
    }
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

  // Mirror of the announcement state for the diff effect below: that
  // effect must compare an incoming settle against the announcement the
  // previous commit rendered, and it must not re-run on every
  // announcement change (its deps stay `[rows]`, or re-running would
  // re-arm its timers). A pure per-render copy of this component's own
  // state written during render.
  const announcementRef = useRef<Announcement>(null)
  announcementRef.current = announcement
  // Mirror of the latest committed rows for the deferred re-announcement
  // (see the diff effect): that 0ms timer can fire after commits that
  // changed its target row's fate, and only the latest rows say what is
  // still true at fire time. Written during render, like announcementRef.
  const rowsRef = useRef<readonly FileUploaderRow[] | null>(null)
  rowsRef.current = rows
  // The deferred re-announcement of a settle whose text repeats the
  // standing one (see the diff effect). Owned across effect instances;
  // cleared only when superseded, when the queue empties, or on unmount.
  const reannounceTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  // Unmount backstop: a pending re-announcement must never setState on an
  // unmounted component. The mount-only shape keeps this cleanup from
  // running between the effect's own re-runs.
  useEffect(() => {
    return () => {
      if (reannounceTimerRef.current !== null) {
        clearTimeout(reannounceTimerRef.current)
        reannounceTimerRef.current = null
      }
    }
  }, [])

  // Diff the rows against the previous commit to find settles: a row
  // leaving `uploading` announces; a row appended already settled (its
  // transfer ran entirely in host code, so no uploading commit ever
  // rendered) announces too -- only rows the host seeds on mount (the
  // first commit, previous === null) and rows appended as `uploading`
  // stay quiet. An empty commit clears any standing announcement; a row
  // returning to `uploading` (a retry) clears it so an identical later
  // failure re-announces; the last settle in list order wins a commit
  // with several; every other flip (a host healing a failed row straight
  // to succeeded) is not a transfer settle and announces nothing.
  //
  // A settle whose announcement text equals the standing one (a second
  // same-name row reaching the same outcome) empties the region and
  // re-announces on the next tick: a live region only speaks when its
  // text changes, so identical text sitting on top of identical text
  // would otherwise be silent -- the same clear-then-refill the retry
  // path relies on.
  useEffect(() => {
    const previous = previousRowsRef.current
    previousRowsRef.current = rows
    if (previous === null) {
      return
    }
    if (rows.length === 0) {
      setAnnouncement(null)
      if (reannounceTimerRef.current !== null) {
        clearTimeout(reannounceTimerRef.current)
        reannounceTimerRef.current = null
      }
      return
    }
    let change: AnnouncementChange = null
    for (const row of rows) {
      const before = previous.find((candidate) => candidate.id === row.id)
      if (before === undefined) {
        if (row.status !== 'uploading') {
          change = {
            type: 'announce',
            kind: row.status === 'succeeded' ? 'uploaded' : 'failed',
            name: row.name,
            rowId: row.id,
          }
        }
        continue
      }
      if (before.status === row.status) {
        continue
      }
      if (row.status === 'uploading') {
        change = { type: 'clear' }
      } else if (before.status === 'uploading') {
        change = {
          type: 'announce',
          kind: row.status === 'succeeded' ? 'uploaded' : 'failed',
          name: row.name,
          rowId: row.id,
        }
      }
    }
    if (change === null) {
      // The queue can lose a row with no settle in the same commit — the
      // ordinary Remove path, the host dropping a settled row while the
      // queue continues. If the standing announcement belongs to that
      // departed row, it must retire with it: the region may only speak
      // of rows the current `rows` prop still carries (the same rule the
      // rendered text below also enforces), or a sighted user is left
      // reading a status that points at a file the queue has let go.
      const standing = announcementRef.current
      if (standing !== null && !rows.some((row) => row.id === standing.rowId)) {
        setAnnouncement(null)
      }
      return
    }
    const nextChange = change
    if (nextChange.type === 'clear') {
      setAnnouncement((current) => (current === null ? current : null))
      return
    }
    const standing = announcementRef.current
    const repeatsStandingText =
      standing !== null &&
      standing.kind === nextChange.kind &&
      standing.name === nextChange.name
    if (repeatsStandingText) {
      setAnnouncement(null)
      if (reannounceTimerRef.current !== null) {
        clearTimeout(reannounceTimerRef.current)
      }
      reannounceTimerRef.current = setTimeout(() => {
        reannounceTimerRef.current = null
        // A host rows update can land between this commit and the 0ms
        // fire (a microtask-ordered update overtakes the timer). Only
        // refill when the row this re-announcement speaks of is still in
        // the latest committed rows AND still settled in the announced
        // state: if it was removed or retried in the meantime, its stale
        // settle text must not be resurrected -- the commit that changed
        // its fate already decided what the region should say.
        const target = rowsRef.current?.find(
          (row) => row.id === nextChange.rowId,
        )
        const targetStillSettled =
          target !== undefined &&
          (nextChange.kind === 'uploaded'
            ? target.status === 'succeeded'
            : target.status === 'failed')
        if (!targetStillSettled) {
          return
        }
        setAnnouncement((current) =>
          current === null
            ? {
                kind: nextChange.kind,
                name: nextChange.name,
                rowId: nextChange.rowId,
              }
            : current,
        )
      }, 0)
      return
    }
    setAnnouncement({
      kind: nextChange.kind,
      name: nextChange.name,
      rowId: nextChange.rowId,
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

  // The announcement the region may actually speak: only a settle whose
  // row the current `rows` prop still carries. A departed row's
  // announcement is retired by the diff effect on the commit that drops
  // it; this derivation is the render-time half of the same rule, so the
  // region can never display a row the queue no longer holds — not even
  // for the render between that commit and its effect.
  const liveAnnouncement =
    announcement !== null &&
    rows.some((row) => row.id === announcement.rowId)
      ? announcement
      : null
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
        /* The rows are one real list: a screen-reader user hears each
           file as one item of a list with a boundary between rows,
           never a flat div stack. role="list" keeps the list semantics
           under WebKit, which strips them from a list-style-none ul. */
        <Box
          component="ul"
          role="list"
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
                  <Box
                    sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 1, mt: 0.5 }}
                  >
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
                <Box
                  sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 1, mt: 0.5 }}
                >
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
            {liveAnnouncement === null
              ? ''
              : t(
                  liveAnnouncement.kind === 'uploaded'
                    ? 'fileUploader.announceUploaded'
                    : 'fileUploader.announceFailed',
                  { name: liveAnnouncement.name },
                )}
          </Typography>
        </Box>
      ) : null}
    </Box>
  )
}
