/**
 * InlineError contract tests: the family's banner (ui-kit's shared
 * InlineError bound to this namespace's error-text resolver) renders
 * exactly when a code is present, resolves whitelisted and unlisted codes
 * through the resolver (dedicated text vs the unknown fallback, in this
 * namespace only), and re-renders in the switched language. The failure
 * classifier's own contract lives beside the classifier, in ui-kit's
 * InlineError suite. Text expectations read the zh-CN and en-US bundle
 * values, never inline language.
 */

import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import { switchLanguage } from '@speed/i18n'
import { InlineError } from './inline-error.js'
import { renderWithProviders } from '../../test-utils/render.js'
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

  // The sign-out call (and a host's own protected operations) can answer
  // with the session-lifecycle codes; each resolves to its own bundle
  // text, so a whitelist or locale typo in any one of them fails here
  // even when the component tests only render representatives.
  const sessionCodes = [
    ['authn.session_not_found', zhCN.errors.authn.session_not_found],
    ['authn.session_revoked', zhCN.errors.authn.session_revoked],
    ['authn.refresh_token_invalid', zhCN.errors.authn.refresh_token_invalid],
    ['authn.refresh_token_reused', zhCN.errors.authn.refresh_token_reused],
    ['authn.token_expired', zhCN.errors.authn.token_expired],
  ] as const

  it.each(sessionCodes)(
    'render the %s session-lifecycle code with its own text',
    (code, text) => {
      renderWithProviders(<InlineError code={code} />)
      expect(screen.getByRole('alert')).toHaveTextContent(text)
    },
  )

  // The register and SMS request paths answer with the identifier-format
  // codes -- authn.invalid_phone for a number with no E.164 form, and
  // authn.invalid_email from the register email slot's canonical-form
  // gate -- each with its own bundle text, so a whitelist or locale typo
  // in either fails here too.
  const identifierCodes = [
    ['authn.invalid_email', zhCN.errors.authn.invalid_email],
    ['authn.invalid_phone', zhCN.errors.authn.invalid_phone],
  ] as const

  it.each(identifierCodes)(
    'render the %s identifier-format code with its own text',
    (code, text) => {
      renderWithProviders(<InlineError code={code} />)
      expect(screen.getByRole('alert')).toHaveTextContent(text)
    },
  )

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

