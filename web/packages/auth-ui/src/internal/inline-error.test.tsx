/**
 * InlineError contract tests: the banner renders exactly when a code is
 * present, resolves whitelisted and unlisted codes through the error-text
 * resolver (dedicated text vs the unknown fallback), re-renders in the
 * switched language, and errorCodeOf classifies failures -- ApiError-shaped
 * answers keep their code, anything else collapses to the unknown code.
 * Text expectations read the zh-CN and en-US bundle values, never inline
 * language.
 */

import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import { switchLanguage } from '@speed/i18n'
import { InlineError, errorCodeOf } from './inline-error.js'
import { renderWithProviders } from '../../test-utils/render.js'
import { apiError } from '../../test-utils/session-harness.js'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }

describe('InlineError', () => {
  it('render nothing while the code is null (no prior failure)', () => {
    const { container } = renderWithProviders(<InlineError code={null} />)
    expect(container.firstChild).toBeNull()
  })

  it('render a whitelisted authn code with its own text in an alert', () => {
    renderWithProviders(<InlineError code="authn.invalid_credentials" />)
    expect(screen.getByRole('alert')).toHaveTextContent(
      zhCN.errors.authn.invalid_credentials,
    )
  })

  it('render the unknown fallback for a code outside the whitelist', () => {
    renderWithProviders(<InlineError code="authn.future_code" />)
    expect(screen.getByRole('alert')).toHaveTextContent(zhCN.errors.unknown)
  })

  it('re-render in the switched language', async () => {
    const { i18n } = renderWithProviders(
      <InlineError code="authn.email_already_registered" />,
    )
    expect(screen.getByRole('alert')).toHaveTextContent(
      zhCN.errors.authn.email_already_registered,
    )
    await switchLanguage(i18n, 'en-US')
    expect(screen.getByRole('alert')).toHaveTextContent(
      enUS.errors.authn.email_already_registered,
    )
  })
})

describe('errorCodeOf', () => {
  it('keep the code of an ApiError-shaped failure', () => {
    expect(errorCodeOf(apiError(401, 'authn.invalid_credentials'))).toBe(
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
