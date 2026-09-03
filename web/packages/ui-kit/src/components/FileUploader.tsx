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
 * Render shape: a resident trigger affordance (the picker label is the
 * built-in bilingual text unless `chooseFilesLabel` overrides it, with
 * the file input visually hidden but focusable inside the label, one tab
 * stop for the whole control). The queue rows and the announcement live
 * region render below the trigger once the queue has content; an empty
 * queue renders only the trigger, with no rows and no announcements.
 *
 * The queue behaviour -- per-row state machine, automatic parallel
 * execution, progress reporting, retry/remove/cancel, late-settle
 * guarding, drag-and-drop -- arrives with the behaviour work; the types
 * below are the frozen public contract that behaviour implements.
 */

import type { SxProps, Theme } from '@mui/material/styles'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
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

/**
 * The picker: trigger affordance with the built-in bilingual label and a
 * hidden-but-focusable file input. The queue area and announcement live
 * region mount here below the trigger once the behaviour work lands.
 */
export function FileUploader({
  accept,
  chooseFilesLabel,
  multiple,
  sx,
}: FileUploaderProps) {
  const { t } = useUiKitTranslation()
  const triggerLabel = chooseFilesLabel ?? t('fileUploader.chooseFiles')
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
        {/* The file input: picking here enqueues and starts execution (B2). */}
        <HiddenFileInput type="file" multiple={multiple} accept={accept} />
      </Button>
      {/* B2: queue rows render here while the queue has content; the
          polite announcement live region (role="status") sits beside them
          and carries the settle announcements. An empty queue renders
          neither rows nor announcements. */}
    </Box>
  )
}
