/**
 * ConfirmDialog contract: a controlled modal whose two exits are
 * onConfirm and onCancel -- Escape and backdrop clicks land on onCancel
 * only, cancel never confirms, the danger variant paints the confirm
 * button with the error role, doubleConfirm needs a second click to
 * fire onConfirm -- the arming click holding the confirm button inert
 * through a short lockout so a double click cannot skip the guard --
 * re-arming after every close, and confirmLoading
 * freezes both exits. Built-in texts come from the ui-kit namespace
 * (asserted against the bundles); labels are host-overridable.
 */

import { useState } from 'react'
import { act, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../../test-utils/render.js'
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { ConfirmDialog, CONFIRM_ARM_LOCKOUT_MS } from './ConfirmDialog.js'

/**
 * A minimal stand-in for a host that reuses one mounted ConfirmDialog to
 * confirm a SEQUENCE of targets -- a batch-delete-next-item flow. Its
 * onConfirm handler (handleConfirmed) closes the current confirmation and
 * opens the next one from inside the very onConfirm call that just fired,
 * all in one synchronous callback, so React 18's automatic batching folds
 * every state update here into a single commit. `open` (derived from
 * `current !== 'done'`) never renders false in between item A and item B --
 * it is true continuously across the whole sequence -- which is exactly
 * the shape that defeats an armed-reset keyed only on witnessing `open`
 * turn false.
 */
function BatchDeleteHarness({ onItemBConfirmed }: { onItemBConfirmed: () => void }) {
  const [current, setCurrent] = useState<'itemA' | 'itemB' | 'done'>('itemA')

  const handleConfirmed = () => {
    if (current === 'itemA') {
      setCurrent('itemB')
    } else {
      setCurrent('done')
      onItemBConfirmed()
    }
  }

  return (
    <ConfirmDialog
      open={current !== 'done'}
      variant="danger"
      doubleConfirm
      title={current === 'itemA' ? 'Delete item A?' : 'Delete item B?'}
      onConfirm={handleConfirmed}
      onCancel={() => {}}
    />
  )
}

function setup(overrides: Record<string, unknown> = {}) {
  const onConfirm = vi.fn()
  const onCancel = vi.fn()
  const utils = renderWithProviders(
    <ConfirmDialog open onConfirm={onConfirm} onCancel={onCancel} {...overrides} />,
  )
  return { onConfirm, onCancel, ...utils }
}

describe('ConfirmDialog', () => {
  it('renders nothing while closed', () => {
    const { queryByRole } = renderWithProviders(
      <ConfirmDialog open={false} onConfirm={vi.fn()} onCancel={vi.fn()} />,
    )
    expect(queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('renders the built-in zh-CN title, message and button labels', () => {
    const { getByRole, getByText } = setup()
    expect(getByRole('dialog')).toBeInTheDocument()
    expect(getByText(zhCN.confirmDialog.title)).toBeInTheDocument()
    expect(getByText(zhCN.confirmDialog.message)).toBeInTheDocument()
    expect(
      getByRole('button', { name: zhCN.confirmDialog.confirmLabel }),
    ).toBeInTheDocument()
    expect(
      getByRole('button', { name: zhCN.confirmDialog.cancelLabel }),
    ).toBeInTheDocument()
  })

  it('switches built-in texts to en-US when the language changes', async () => {
    const { i18n, getByRole, getByText, queryByText } = setup()
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    expect(getByText(enUS.confirmDialog.title)).toBeInTheDocument()
    expect(getByText(enUS.confirmDialog.message)).toBeInTheDocument()
    expect(
      getByRole('button', { name: enUS.confirmDialog.confirmLabel }),
    ).toBeInTheDocument()
    expect(queryByText(zhCN.confirmDialog.title)).not.toBeInTheDocument()
  })

  it('lets title, message and both labels be overridden', () => {
    const { getByRole, getByText, queryByText } = setup({
      title: 'Remove patient record?',
      message: 'All scans and attachments are deleted permanently.',
      confirmLabel: 'Delete permanently',
      cancelLabel: 'Keep record',
    })
    expect(getByText('Remove patient record?')).toBeInTheDocument()
    expect(
      getByText('All scans and attachments are deleted permanently.'),
    ).toBeInTheDocument()
    expect(getByRole('button', { name: 'Delete permanently' })).toBeInTheDocument()
    expect(getByRole('button', { name: 'Keep record' })).toBeInTheDocument()
    expect(queryByText(zhCN.confirmDialog.title)).not.toBeInTheDocument()
  })

  it('fires onConfirm once on the confirm click and never onCancel', async () => {
    const { onConfirm, onCancel, getByRole } = setup()
    const user = userEvent.setup()
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmLabel }))
    expect(onConfirm).toHaveBeenCalledTimes(1)
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('fires onCancel on the cancel button and never onConfirm', async () => {
    const { onConfirm, onCancel, getByRole } = setup()
    const user = userEvent.setup()
    await user.click(getByRole('button', { name: zhCN.confirmDialog.cancelLabel }))
    expect(onCancel).toHaveBeenCalledTimes(1)
    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('fires onCancel on Escape and never onConfirm', () => {
    const { onConfirm, onCancel, getByRole } = setup()
    fireEvent.keyDown(getByRole('dialog'), { key: 'Escape' })
    expect(onCancel).toHaveBeenCalledTimes(1)
    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('paints the danger variant confirm button with the error role', () => {
    const { getByRole } = setup({ variant: 'danger' })
    expect(getByRole('button', { name: zhCN.confirmDialog.confirmLabel })).toHaveClass(
      'MuiButton-colorError',
    )
  })

  it('keeps the default variant confirm button in the primary role', () => {
    const { getByRole } = setup()
    expect(getByRole('button', { name: zhCN.confirmDialog.confirmLabel })).toHaveClass(
      'MuiButton-colorPrimary',
    )
  })

  it('doubleConfirm: the first click only arms, the second fires onConfirm once', async () => {
    const { onConfirm, onCancel, getByRole, queryByRole } = setup({
      variant: 'danger',
      doubleConfirm: true,
    })
    const user = userEvent.setup()
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmLabel }))
    expect(onConfirm).not.toHaveBeenCalled()
    expect(
      getByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel }),
    ).toBeInTheDocument()
    expect(queryByRole('button', { name: zhCN.confirmDialog.confirmLabel })).not.toBeInTheDocument()

    // The arming click entered the double-click lockout window; the
    // deliberate second click waits it out (in real time, inside act, so
    // the lockout's auto-clear lands in act), exactly as a user who read
    // the re-labelled button would. The window's own shape is pinned by
    // the double-click regression tests below.
    await act(async () => {
      await new Promise((resolve) =>
        setTimeout(resolve, CONFIRM_ARM_LOCKOUT_MS + 300),
      )
    })
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel }))
    expect(onConfirm).toHaveBeenCalledTimes(1)
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('doubleConfirm: a double click cannot skip the guard -- the armed confirm stays inert through the arming lockout (P1 regression)', async () => {
    // A double click IS two clicks: arming (click 1) re-labels the
    // button, and a second click landing ~instantly would find the
    // guard armed and fire onConfirm, skipping the deliberate second
    // step the guard exists to force. The arming click therefore also
    // enters CONFIRM_ARM_LOCKOUT_MS of lockout during which the confirm
    // button is disabled, so the second event of a double click cannot
    // land on it; the lockout auto-clears and a click after it still
    // confirms. The clock is faked only after render: the single timer
    // that can exist under the fake clock is the lockout timer itself.
    const { onConfirm, getByRole } = setup({
      variant: 'danger',
      doubleConfirm: true,
    })
    const confirm = getByRole('button', { name: zhCN.confirmDialog.confirmLabel })
    const again = () => getByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel })

    vi.useFakeTimers()
    try {
      // Two clicks with no delay in between: the double-click shape. On
      // the unfixed code the second click finds the armed guard and
      // fires onConfirm.
      fireEvent.click(confirm)
      fireEvent.click(confirm)
      expect(onConfirm).not.toHaveBeenCalled()

      // The confirm stays inert for the whole lockout window, up to and
      // including the instant before it ends.
      expect(again()).toBeDisabled()
      await act(async () => {
        await vi.advanceTimersByTimeAsync(CONFIRM_ARM_LOCKOUT_MS - 1)
      })
      fireEvent.click(again())
      expect(onConfirm).not.toHaveBeenCalled()

      // Once the window elapses the armed confirm answers a second click.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1)
      })
      expect(again()).toBeEnabled()
      fireEvent.click(again())
      expect(onConfirm).toHaveBeenCalledTimes(1)
    } finally {
      vi.useRealTimers()
    }
  })

  it('doubleConfirm: cancel and close stay available during the arming lockout', async () => {
    const { onConfirm, onCancel, getByRole, rerender } = setup({
      variant: 'danger',
      doubleConfirm: true,
    })
    const confirm = getByRole('button', { name: zhCN.confirmDialog.confirmLabel })
    const cancel = getByRole('button', { name: zhCN.confirmDialog.cancelLabel })
    // The confirm-again button, whose accessible name only exists once
    // the first click has armed the guard.
    const again = () => getByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel })

    vi.useFakeTimers()
    try {
      fireEvent.click(confirm)
      expect(again()).toBeDisabled()
      // The window covers only the confirm button: the cancel exit (and
      // with it Escape/backdrop, which share its handler) stays live.
      fireEvent.click(cancel)
      expect(onCancel).toHaveBeenCalledTimes(1)
      expect(onConfirm).not.toHaveBeenCalled()

      // Closing mid-window must not leave a late re-enable pending: a
      // reopen after the window would otherwise start from a stale
      // lockout. The reopen renders unarmed and immediately clickable.
      rerender(
        <ConfirmDialog
          open={false}
          variant="danger"
          doubleConfirm
          onConfirm={onConfirm}
          onCancel={onCancel}
        />,
      )
      rerender(
        <ConfirmDialog
          open
          variant="danger"
          doubleConfirm
          onConfirm={onConfirm}
          onCancel={onCancel}
        />,
      )
      await act(async () => {
        await vi.advanceTimersByTimeAsync(CONFIRM_ARM_LOCKOUT_MS)
      })
      const reopened = getByRole('button', { name: zhCN.confirmDialog.confirmLabel })
      expect(reopened).toBeEnabled()
      fireEvent.click(reopened)
      expect(onConfirm).not.toHaveBeenCalled()
    } finally {
      vi.useRealTimers()
    }
  })

  it('doubleConfirm: arming the confirm is announced through a live region (P2-9 regression)', async () => {
    // The first confirm click re-labels the button, and a label change
    // under focus is not something a screen reader reports reliably -- a
    // non-visual user pressing confirm once and waiting would hear
    // nothing about the second click they owe. A polite live region sits
    // inside the dialog (empty until armed, so the arm is a text change
    // inside an existing region, not a region mounting together with its
    // text) and fills with the confirm-again label on the first click.
    const { getByRole, queryByRole } = setup({
      variant: 'danger',
      doubleConfirm: true,
    })
    const region = getByRole('status')
    expect(region).toHaveTextContent('')
    const user = userEvent.setup()
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmLabel }))
    expect(region).toHaveTextContent(zhCN.confirmDialog.confirmAgainLabel)
    expect(
      queryByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel }),
    ).toBeInTheDocument()
  })

  it('doubleConfirm: the override confirmLabel does not leak into the re-armed step', async () => {
    const { getByRole } = setup({
      variant: 'danger',
      doubleConfirm: true,
      confirmLabel: 'Delete permanently',
    })
    const user = userEvent.setup()
    await user.click(getByRole('button', { name: 'Delete permanently' }))
    expect(
      getByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel }),
    ).toBeInTheDocument()
  })

  it('doubleConfirm: cancelling while armed does not confirm, and a reopen re-arms', async () => {
    const { onConfirm, onCancel, getByRole, rerender } = setup({
      variant: 'danger',
      doubleConfirm: true,
    })
    const user = userEvent.setup()
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmLabel }))
    await user.click(getByRole('button', { name: zhCN.confirmDialog.cancelLabel }))
    expect(onCancel).toHaveBeenCalledTimes(1)
    expect(onConfirm).not.toHaveBeenCalled()

    // Reopening starts from an unarmed confirm again: one click must not fire.
    rerender(
      <ConfirmDialog
        open={false}
        variant="danger"
        doubleConfirm
        onConfirm={onConfirm}
        onCancel={onCancel}
      />,
    )
    rerender(
      <ConfirmDialog
        open
        variant="danger"
        doubleConfirm
        onConfirm={onConfirm}
        onCancel={onCancel}
      />,
    )
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmLabel }))
    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('doubleConfirm: a batched close+reopen for the next item inside the onConfirm that just fired does not inherit the armed state (regression: web-base-packages.md P2-3)', async () => {
    const onItemBConfirmed = vi.fn()
    const { getByRole, queryByRole } = renderWithProviders(
      <BatchDeleteHarness onItemBConfirmed={onItemBConfirmed} />,
    )
    const user = userEvent.setup()
    // Each deliberate confirm click below waits the double-click lockout
    // window its arming click entered out (in real time, inside act -- a
    // user who read the re-labelled button would take as long); the
    // window's own shape is pinned by the double-click regression tests.
    const waitOutLockout = () =>
      act(async () => {
        await new Promise((resolve) =>
          setTimeout(resolve, CONFIRM_ARM_LOCKOUT_MS + 300),
        )
      })

    // Arm item A: the first click only arms, never confirms.
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmLabel }))
    expect(
      getByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel }),
    ).toBeInTheDocument()
    expect(onItemBConfirmed).not.toHaveBeenCalled()

    // Confirm item A: the SECOND click. Its onConfirm handler
    // (BatchDeleteHarness's handleConfirmed) synchronously swaps in item
    // B's confirmation, batched into the same commit as this click --
    // the reproduction shape this test pins. Item B must render UNARMED.
    await waitOutLockout()
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel }))
    expect(getByRole('button', { name: zhCN.confirmDialog.confirmLabel })).toBeInTheDocument()
    expect(
      queryByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel }),
    ).not.toBeInTheDocument()
    expect(onItemBConfirmed).not.toHaveBeenCalled()

    // A single click on item B's now-showing dialog must only arm it --
    // it must NOT call onConfirm. The armed state must not survive the
    // close+reopen for the next item: if item B inherited armed=true,
    // this single click would fire onItemBConfirmed immediately.
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmLabel }))
    expect(onItemBConfirmed).not.toHaveBeenCalled()
    expect(
      getByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel }),
    ).toBeInTheDocument()

    // The second click on item B does confirm it -- the guard still works.
    await waitOutLockout()
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel }))
    expect(onItemBConfirmed).toHaveBeenCalledTimes(1)
  })

  it('non-danger variant still confirms on exactly one click, unaffected by the armed-reset change', async () => {
    const { onConfirm, onCancel, getByRole } = setup({ variant: 'default' })
    const user = userEvent.setup()
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmLabel }))
    expect(onConfirm).toHaveBeenCalledTimes(1)
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('danger variant without doubleConfirm still confirms on exactly one click, unaffected by the armed-reset change', async () => {
    const { onConfirm, onCancel, getByRole } = setup({ variant: 'danger' })
    const user = userEvent.setup()
    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmLabel }))
    expect(onConfirm).toHaveBeenCalledTimes(1)
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('confirmLoading: both exits are inert and the confirm button shows the busy state', () => {
    const { onConfirm, onCancel, getByRole } = setup({ confirmLoading: true })
    const confirm = getByRole('button', { name: zhCN.confirmDialog.confirmLabel })
    expect(confirm).toBeDisabled()
    // fireEvent bypasses the pointer-events check user-event enforces on
    // disabled controls -- the disabled attribute is the inertness guard
    // under test, and it is asserted above.
    fireEvent.click(confirm)
    expect(onConfirm).not.toHaveBeenCalled()

    const cancel = getByRole('button', { name: zhCN.confirmDialog.cancelLabel })
    expect(cancel).toBeDisabled()
    fireEvent.click(cancel)
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('confirmLoading: Escape does not cancel while busy', () => {
    const { onCancel, getByRole } = setup({ confirmLoading: true })
    fireEvent.keyDown(getByRole('dialog'), { key: 'Escape' })
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('wires the dialog to its title and message ids', () => {
    const { getByRole } = setup()
    const dialog = getByRole('dialog')
    expect(dialog).toHaveAttribute('aria-labelledby')
    expect(dialog).toHaveAttribute('aria-describedby')
  })

  it('passes axe over an open dialog', async () => {
    setup()
    await expectNoAxeViolations()
  })
})
