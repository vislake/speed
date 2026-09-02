/**
 * ConfirmDialog contract: a controlled modal whose two exits are
 * onConfirm and onCancel -- Escape and backdrop clicks land on onCancel
 * only, cancel never confirms, the danger variant paints the confirm
 * button with the error role, doubleConfirm needs a second click to
 * fire onConfirm (re-arming after every close), and confirmLoading
 * freezes both exits. Built-in texts come from the ui-kit namespace
 * (asserted against the bundles); labels are host-overridable.
 */

import { act, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../../test-utils/render.js'
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { ConfirmDialog } from './ConfirmDialog.js'

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

    await user.click(getByRole('button', { name: zhCN.confirmDialog.confirmAgainLabel }))
    expect(onConfirm).toHaveBeenCalledTimes(1)
    expect(onCancel).not.toHaveBeenCalled()
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
