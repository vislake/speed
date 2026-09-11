/**
 * AsyncSection contract: the four states of an async read map to the
 * four fixed renderings -- loading announcement, retryable error
 * placeholder, empty placeholder, content -- with the absent payload
 * taking precedence over the empty decision, the empty placeholder
 * requiring its copy, the content callback receiving the narrowed
 * payload, and the placeholders' titles rendering at the passed heading
 * level (h2, the section-title level, by default).
 */

import { describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AsyncSection } from './AsyncSection.js'
import { renderWithProviders } from '../../test-utils/render.js'

const ERROR_STATE = {
  title: 'Could not load items',
  description: 'Something went wrong loading the items.',
  retryLabel: 'Retry',
  onRetry: vi.fn(),
} as const

const EMPTY_STATE = {
  title: 'No items',
  description: 'Items will show up here.',
} as const

describe('AsyncSection', () => {
  it('renders the loading announcement while pending', () => {
    renderWithProviders(
      <AsyncSection
        pending
        payload={['one']}
        empty={false}
        loading={<p>loading node</p>}
        errorState={ERROR_STATE}
        emptyState={EMPTY_STATE}
      >
        {(items) => <p>content {items.join(',')}</p>}
      </AsyncSection>,
    )
    expect(screen.getByText('loading node')).toBeInTheDocument()
    expect(screen.queryByText(/content/)).not.toBeInTheDocument()
  })

  it('renders the retryable error placeholder for a settled read without a payload', async () => {
    const onRetry = vi.fn()
    renderWithProviders(
      <AsyncSection
        pending={false}
        payload={undefined as readonly string[] | undefined}
        empty
        loading={<p>loading node</p>}
        errorState={{ ...ERROR_STATE, onRetry }}
        emptyState={EMPTY_STATE}
      >
        {(items) => <p>content {items.join(',')}</p>}
      </AsyncSection>,
    )
    expect(screen.getByText(ERROR_STATE.title)).toBeInTheDocument()
    expect(screen.getByText(ERROR_STATE.description)).toBeInTheDocument()
    expect(screen.queryByText(EMPTY_STATE.title)).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: ERROR_STATE.retryLabel }))
    expect(onRetry).toHaveBeenCalledTimes(1)
  })

  it('renders the empty placeholder for a settled empty payload', () => {
    const children = vi.fn(() => <p>content</p>)
    renderWithProviders(
      <AsyncSection
        pending={false}
        payload={[]}
        empty
        loading={<p>loading node</p>}
        errorState={ERROR_STATE}
        emptyState={EMPTY_STATE}
      >
        {children}
      </AsyncSection>,
    )
    expect(screen.getByText(EMPTY_STATE.title)).toBeInTheDocument()
    expect(screen.getByText(EMPTY_STATE.description)).toBeInTheDocument()
    // The content callback is never built for the placeholder branches.
    expect(children).not.toHaveBeenCalled()
  })

  it('renders the content from the narrowed payload for a settled non-empty read', () => {
    renderWithProviders(
      <AsyncSection
        pending={false}
        payload={['alpha', 'beta']}
        empty={false}
        loading={<p>loading node</p>}
        errorState={ERROR_STATE}
        emptyState={EMPTY_STATE}
      >
        {(items) => <p>content {items.join(',')}</p>}
      </AsyncSection>,
    )
    expect(screen.getByText('content alpha,beta')).toBeInTheDocument()
    expect(screen.queryByText(ERROR_STATE.title)).not.toBeInTheDocument()
  })

  it('renders the content when the section omits an empty state', () => {
    renderWithProviders(
      <AsyncSection
        pending={false}
        payload={{ locale: 'zh-CN' }}
        empty={false}
        loading={<p>loading node</p>}
        errorState={ERROR_STATE}
      >
        {(settings) => <p>content {settings.locale}</p>}
      </AsyncSection>,
    )
    expect(screen.getByText('content zh-CN')).toBeInTheDocument()
  })

  it('renders the placeholder titles at h2 by default and at the passed level', () => {
    const first = renderWithProviders(
      <AsyncSection
        pending={false}
        payload={undefined}
        empty={false}
        loading={<p>loading node</p>}
        errorState={ERROR_STATE}
        emptyState={EMPTY_STATE}
      >
        {(items: readonly string[]) => <p>content {items.join(',')}</p>}
      </AsyncSection>,
    )
    expect(
      screen.getByRole('heading', { level: 2, name: ERROR_STATE.title }),
    ).toBeInTheDocument()
    first.unmount()

    renderWithProviders(
      <AsyncSection
        pending={false}
        payload={[]}
        empty
        headingLevel="h3"
        loading={<p>loading node</p>}
        errorState={ERROR_STATE}
        emptyState={EMPTY_STATE}
      >
        {(items: readonly string[]) => <p>content {items.join(',')}</p>}
      </AsyncSection>,
    )
    expect(
      screen.getByRole('heading', { level: 3, name: EMPTY_STATE.title }),
    ).toBeInTheDocument()
  })
})
