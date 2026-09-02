/**
 * ConfirmDialog: the controlled confirmation modal.
 *
 * Everything here is props-driven: `open` shows it, `onConfirm` /
 * `onCancel` report the two exits, `confirmLoading` renders the confirm
 * button busy (and freezes both exits until it clears). The component
 * holds no business state.
 *
 * Destructive confirmations use the danger variant, which paints the
 * confirm button with the error palette role. For the truly irreversible
 * the danger variant pairs with `doubleConfirm`: the first click on the
 * confirm button does not confirm anything -- the button re-labels with
 * the "click again" text (built-in, ui-kit namespace) and only a second
 * click fires onConfirm. The two-step guard is interaction state only
 * (like a tooltip's own open state), reset whenever the dialog closes;
 * hosts never observe it beyond onConfirm firing exactly on the second
 * click.
 *
 * Texts: title/message defaults are generic namespace strings; hosts
 * should pass the real business content (specific object, what the
 * action does) as title/message -- those props accept any ReactNode, so
 * host translations flow naturally. Buttons fall back to the namespace
 * defaults when no labels are passed.
 *
 * Escape and backdrop clicks call onCancel (never onConfirm); while
 * confirmLoading is set both exits are inert.
 */

import { useEffect, useId, useState } from 'react'
import type { ReactNode } from 'react'
import Button from '@mui/material/Button'
import Dialog from '@mui/material/Dialog'
import DialogActions from '@mui/material/DialogActions'
import DialogContent from '@mui/material/DialogContent'
import DialogContentText from '@mui/material/DialogContentText'
import DialogTitle from '@mui/material/DialogTitle'
import { useUiKitTranslation } from '../internal/translation.js'

export type ConfirmDialogVariant = 'default' | 'danger'

export interface ConfirmDialogProps {
  /** Whether the dialog is shown; controlled by the host. */
  readonly open: boolean
  /** Dialog heading. Defaults to the namespace's generic confirm title. */
  readonly title?: ReactNode
  /** What exactly will happen. Defaults to the generic caution message. */
  readonly message?: ReactNode
  /** 'default' or 'danger' (danger paints the confirm button in the error role). */
  readonly variant?: ConfirmDialogVariant
  /**
   * Danger two-step guard: the first confirm click re-labels the button
   * ("click again to confirm", ui-kit namespace) and only the second
   * click fires onConfirm. Meaningless on the default variant.
   */
  readonly doubleConfirm?: boolean
  /** Overrides the built-in confirm label (the "again" label in the second step). */
  readonly confirmLabel?: ReactNode
  /** Overrides the built-in cancel label. */
  readonly cancelLabel?: ReactNode
  /** Busy state of the confirmation; both exits are inert while true. */
  readonly confirmLoading?: boolean
  /** Fired when the user confirms (the second click under doubleConfirm). */
  readonly onConfirm: () => void
  /** Fired on cancel, Escape or backdrop click -- never on confirm. */
  readonly onCancel: () => void
}

/**
 * The controlled confirmation modal with a danger/two-step-confirm
 * mode for destructive actions.
 */
export function ConfirmDialog({
  open,
  title,
  message,
  variant = 'default',
  doubleConfirm = false,
  confirmLabel,
  cancelLabel,
  confirmLoading = false,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  const { t } = useUiKitTranslation()
  const titleId = useId()
  const messageId = useId()
  const [armed, setArmed] = useState(false)

  // The two-step guard is interaction state: closing the dialog (by any
  // exit) resets it so the next open starts from an unarmed confirm.
  useEffect(() => {
    if (!open) {
      setArmed(false)
    }
  }, [open])

  const busy = confirmLoading
  const handleRequestClose = () => {
    if (!busy) {
      onCancel()
    }
  }
  const handleConfirmClick = () => {
    if (busy) {
      return
    }
    if (variant === 'danger' && doubleConfirm && !armed) {
      setArmed(true)
      return
    }
    onConfirm()
  }

  const confirmColor = variant === 'danger' ? 'error' : 'primary'
  return (
    <Dialog
      open={open}
      onClose={handleRequestClose}
      aria-labelledby={titleId}
      aria-describedby={messageId}
      maxWidth="xs"
      fullWidth
    >
      <DialogTitle id={titleId}>{title ?? t('confirmDialog.title')}</DialogTitle>
      <DialogContent>
        <DialogContentText id={messageId}>
          {message ?? t('confirmDialog.message')}
        </DialogContentText>
      </DialogContent>
      <DialogActions>
        <Button onClick={handleRequestClose} disabled={busy} color="inherit">
          {cancelLabel ?? t('confirmDialog.cancelLabel')}
        </Button>
        <Button
          onClick={handleConfirmClick}
          disabled={busy}
          loading={busy}
          color={confirmColor}
          variant="contained"
        >
          {variant === 'danger' && doubleConfirm && armed
            ? t('confirmDialog.confirmAgainLabel')
            : (confirmLabel ?? t('confirmDialog.confirmLabel'))}
        </Button>
      </DialogActions>
    </Dialog>
  )
}
