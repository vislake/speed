/**
 * FileUploader foundation: the scaffold-level pins that hold under the
 * behaviour round. The trigger renders its label from the bundles
 * (asserted against the shipped JSON, never inline), forwards
 * multiple/accept to one native file input that stays visually hidden
 * but focusable inside the trigger label -- the control's single tab
 * stop -- and an empty queue renders no rows and no announcements. The
 * queue behaviour itself (enqueue, parallel execute, per-row states,
 * live region) lands with the behaviour work.
 */

import { act } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { switchLanguage } from '@speed/i18n'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../../test-utils/render.js'
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { FileUploader } from './FileUploader.js'
import type { FileUploadExecutor } from './FileUploader.js'

const noopExecute: FileUploadExecutor = async () => {}

describe('FileUploader', () => {
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
})
