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
 * click fires onConfirm. Arming also holds the confirm button inert for
 * a short lockout (CONFIRM_ARM_LOCKOUT_MS): a double click is two
 * clicks ~100-200ms apart, and without that window the second event
 * would land on the armed button and fire onConfirm, skipping the
 * deliberate second step the guard exists to force. The window outlasts
 * the platform double-click threshold, so a true double click's second
 * event always lands on a disabled button while a user who read the
 * re-labelled button -- the deliberate path -- clicks after it by
 * construction; it auto-clears, covers the confirm button alone, and
 * leaves Cancel, Escape and the backdrop live throughout. The two-step
 * guard is interaction state only (like a tooltip's own open state);
 * hosts never observe it beyond onConfirm firing exactly on the second
 * click.
 *
 * The armed flag and its lockout window are reset two ways, deliberately
 * redundant: an effect clears them whenever `open` is observed false on
 * a committed render (the ordinary close/reopen case), and -- because a
 * host that reuses one mounted dialog instance across a *sequence* of
 * confirmations (e.g. a batch-delete-next-item flow) may close the
 * current one and open the next from inside the very onConfirm/onCancel
 * call this component just made, all in one React state-update batch, so
 * `open` never renders false in between and the effect never fires --
 * both exit handlers also clear them synchronously before invoking their
 * callback. Ending the lockout cancels its pending auto-clear timer, so
 * neither a closed nor a reused dialog can fire a late re-enable; the
 * timer additionally dies with the component on unmount. That reset
 * rides the same batch as whatever the host does next, so the next
 * confirmation always starts unarmed and unlocked no matter how the host
 * chains its own state updates.
 *
 * Texts: title/message defaults are generic namespace strings; hosts
 * should pass the real business content (specific object, what the
 * action does) as title/message -- those props accept any ReactNode, so
 * host translations flow naturally. Buttons fall back to the namespace
 * defaults when no labels are passed.
 *
 * Escape and backdrop clicks call onCancel (never onConfirm); while
 * confirmLoading is set both exits are inert.
 *
 * Arming is announced: under the doubleConfirm guard a visually hidden
 * polite live region (role="status") sits inside the dialog, empty while
 * the confirm is unarmed, and fills with the confirm-again label the
 * moment the first click arms the button. The button relabels itself
 * (visual feedback) but a label change under focus is not something a
 * screen reader reports reliably; the live region is the non-visual
 * channel for "the next click will confirm".
 */

import { useCallback, useEffect, useId, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import Box from '@mui/material/Box'
import Button from '@mui/material/Button'
import Dialog from '@mui/material/Dialog'
import DialogActions from '@mui/material/DialogActions'
import DialogContent from '@mui/material/DialogContent'
import DialogContentText from '@mui/material/DialogContentText'
import DialogTitle from '@mui/material/DialogTitle'
import { useUiKitTranslation } from '../internal/translation.js'
import { visuallyHiddenSx } from '../visually-hidden.js'

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
   * click fires onConfirm. The arming click also holds the confirm
   * button inert for CONFIRM_ARM_LOCKOUT_MS, so a double click cannot
   * skip the guard. Meaningless on the default variant.
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
 * How long the confirm button stays inert after the arming click under
 * `doubleConfirm`. A double click is two clicks ~100-200ms apart (the
 * platform double-click threshold is ~500ms); without this window the
 * second click would land on the armed button and fire onConfirm,
 * skipping the deliberate second step the guard exists to force. The
 * window outlasts the platform threshold, so any true double click's
 * second event lands on a disabled button, then auto-clears; a user who
 * read the re-labelled button clicks later than the window by
 * construction, so the deliberate path is unaffected.
 */
export const CONFIRM_ARM_LOCKOUT_MS = 600

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
  // The double-click lockout the arming click enters: while true the
  // confirm button is disabled (see the file header). A state of its
  // own, not folded into `armed`, because the two reset on different
  // schedules -- the lockout auto-clears after CONFIRM_ARM_LOCKOUT_MS,
  // while `armed` waits for the user's second click.
  const [armLockout, setArmLockout] = useState(false)
  // The lockout's auto-clear timer, null once fired or cancelled. Held
  // in a ref so every reset path can cancel a pending re-enable: a
  // dialog that closed, confirmed or unmounted mid-lockout must never
  // fire one at a confirm button that is gone.
  const armLockoutTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  // Ends the lockout now: cancels the pending auto-clear timer and
  // clears the state. Every reset of `armed` calls this, so the lockout
  // can never outlive the arm that entered it.
  const endArmLockout = useCallback(() => {
    if (armLockoutTimer.current !== null) {
      clearTimeout(armLockoutTimer.current)
      armLockoutTimer.current = null
    }
    setArmLockout(false)
  }, [])

  // Ordinary-close backstop: if `open` is ever observed false on a
  // committed render, make sure armed -- and the lockout that came with
  // it -- are false too. The exit handlers below cover the batched-
  // reopen case this effect cannot (see the file header), so this is a
  // redundant safety net, not the only guard.
  useEffect(() => {
    if (!open) {
      setArmed(false)
      endArmLockout()
    }
  }, [open, endArmLockout])

  // A dialog unmounted mid-lockout must not leave its re-enable timer
  // alive to fire against nothing. The timer dies with the component.
  useEffect(() => {
    return () => {
      if (armLockoutTimer.current !== null) {
        clearTimeout(armLockoutTimer.current)
        armLockoutTimer.current = null
      }
    }
  }, [])

  const busy = confirmLoading
  const handleRequestClose = () => {
    if (busy) {
      return
    }
    // Clear the guard before calling out: if this callback's own host
    // handler closes this confirmation and opens the next one in the same
    // batch, the next one must not inherit an armed state it never
    // earned -- nor a lockout, whose pending re-enable timer dies here.
    setArmed(false)
    endArmLockout()
    onCancel()
  }
  const handleConfirmClick = () => {
    if (busy || armLockout) {
      return
    }
    if (variant === 'danger' && doubleConfirm && !armed) {
      setArmed(true)
      // Enter the double-click lockout. The window outlasts the platform
      // double-click threshold, so the second event of a double click --
      // already inside the OS's own double-click interval -- lands on the
      // disabled button; the armed state then waits for a click that is
      // slower than a double click by construction. Timer start is safe
      // to overwrite: the lockout guard above means no arming can happen
      // while an earlier lockout is still pending.
      setArmLockout(true)
      armLockoutTimer.current = setTimeout(() => {
        armLockoutTimer.current = null
        setArmLockout(false)
      }, CONFIRM_ARM_LOCKOUT_MS)
      return
    }
    // Same reasoning as handleRequestClose: reset synchronously, in the
    // same tick as the call that is about to fire, not after it.
    setArmed(false)
    endArmLockout()
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
        {/* The arming live region. Mounted (empty) while the dialog shows
            under the doubleConfirm guard -- a region that appeared
            together with its text could not announce the arm -- and
            filled when the first click arms the confirm. Visually hidden
            through the clip technique (never display:none: a hidden
            region would not be live). */}
        {variant === 'danger' && doubleConfirm ? (
          <Box component="p" role="status" sx={visuallyHiddenSx}>
            {armed ? t('confirmDialog.confirmAgainLabel') : ''}
          </Box>
        ) : null}
      </DialogContent>
      <DialogActions>
        <Button onClick={handleRequestClose} disabled={busy} color="inherit">
          {cancelLabel ?? t('confirmDialog.cancelLabel')}
        </Button>
        <Button
          onClick={handleConfirmClick}
          // armLockout is the double-click window the arming click
          // entered: disabled here means the second event of a double
          // click cannot activate the armed guard (the handler guard
          // above backs the same window for any event that still
          // arrives). Cancel stays live -- the window covers the confirm
          // button alone.
          disabled={busy || armLockout}
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
