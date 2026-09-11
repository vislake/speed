/**
 * InlineError contract: the banner renders exactly when a code is
 * present, and its text comes only from the injected resolver -- a
 * listed code resolves to its own text, anything else to the family's
 * fallback, and a swapped resolver is what proves the component
 * guesses no namespace of its own. errorCodeOf is pinned beside it: an
 * ApiError-shaped failure keeps its code, anything else -- a bug-shaped
 * throw, a code that is not a non-empty string -- collapses to the
 * code no resolver lists.
 */

import { describe, expect, it, vi } from 'vitest'
import { screen } from '@testing-library/react'
import { InlineError, errorCodeOf } from './InlineError.js'
import { renderWithProviders } from '../../test-utils/render.js'

/** A resolver in the surface families' convention: the listed code's
 * own text, everything else the family's unknown fallback. */
function resolverOf(overrides: Record<string, string> = {}) {
  const known: Record<string, string> = {
    'authn.session_revoked': 'authn.session_revoked text',
    ...overrides,
  }
  return (code: string): string => known[code] ?? 'unknown text'
}

describe('InlineError', () => {
  it('renders nothing while the code is null (no prior failure)', () => {
    const { container } = renderWithProviders(
      <InlineError code={null} resolve={resolverOf()} />,
    )
    expect(container.firstChild).toBeNull()
  })

  it('renders a listed code through the injected resolver in an alert', () => {
    renderWithProviders(
      <InlineError code="authn.session_revoked" resolve={resolverOf()} />,
    )
    expect(screen.getByRole('alert')).toHaveTextContent(
      'authn.session_revoked text',
    )
  })

  it('renders the resolver fallback for a code outside the listing', () => {
    renderWithProviders(
      <InlineError code="authn.future_code" resolve={resolverOf()} />,
    )
    expect(screen.getByRole('alert')).toHaveTextContent('unknown text')
  })

  it('takes its text from the passed resolver alone, never another source', () => {
    const first = vi.fn(resolverOf({ 'authn.session_revoked': 'first source' }))
    const firstView = renderWithProviders(
      <InlineError code="authn.session_revoked" resolve={first} />,
    )
    expect(screen.getByRole('alert')).toHaveTextContent('first source')
    expect(first).toHaveBeenCalledWith('authn.session_revoked')
    firstView.unmount()

    renderWithProviders(
      <InlineError
        code="authn.session_revoked"
        resolve={resolverOf({ 'authn.session_revoked': 'second source' })}
      />,
    )
    expect(screen.getByRole('alert')).toHaveTextContent('second source')
  })
})

describe('errorCodeOf', () => {
  it('keep the code of an ApiError-shaped failure', () => {
    expect(errorCodeOf({ code: 'authn.invalid_credentials' })).toBe(
      'authn.invalid_credentials',
    )
  })

  it('collapse a non-ApiError throw (and a thrown string) to the unknown code', () => {
    expect(errorCodeOf(new Error('boom'))).toBe('client.unknown')
    expect(errorCodeOf('boom')).toBe('client.unknown')
  })

  it('collapse an ApiError whose code is not a non-empty string', () => {
    expect(errorCodeOf({ code: 401 })).toBe('client.unknown')
    expect(errorCodeOf({})).toBe('client.unknown')
  })
})
