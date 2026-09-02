/**
 * PageHeader contract: the page title renders as the single h1 in the
 * heading-4 role, description and actions render when given, and the
 * block carries no built-in text of its own (content is caller
 * content, so nothing here depends on the ui-kit namespace).
 */

import { describe, expect, it } from 'vitest'
import { renderWithProviders } from '../../test-utils/render.js'
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { PageHeader } from './PageHeader.js'

describe('PageHeader', () => {
  it('renders the title as an h1 heading', () => {
    const { getByRole } = renderWithProviders(<PageHeader title="Billing overview" />)
    const heading = getByRole('heading', { level: 1 })
    expect(heading).toHaveTextContent('Billing overview')
  })

  it('renders the description under the title when given', () => {
    const { getByText } = renderWithProviders(
      <PageHeader title="Billing overview" description="Usage and invoices across your workspaces." />,
    )
    expect(getByText('Usage and invoices across your workspaces.')).toBeInTheDocument()
  })

  it('renders the actions area when given', () => {
    const { getByRole } = renderWithProviders(
      <PageHeader title="Members" actions={<button type="button">Invite</button>} />,
    )
    expect(getByRole('button', { name: 'Invite' })).toBeInTheDocument()
  })

  it('stays a plain header without description or actions', () => {
    const { getByRole, queryByRole, queryByText } = renderWithProviders(
      <PageHeader title="Settings" />,
    )
    expect(getByRole('heading', { level: 1 })).toHaveTextContent('Settings')
    // No empty action gutter or description paragraph materializes.
    expect(getByRole('heading').closest('header')).toBeInTheDocument()
    expect(queryByRole('paragraph')).not.toBeInTheDocument()
    expect(queryByText('', { selector: 'button' })).not.toBeInTheDocument()
  })

  it('passes axe over the header structure', async () => {
    renderWithProviders(
      <PageHeader
        title="Members"
        description="Everyone with access to this workspace."
        actions={<button type="button">Invite member</button>}
      />,
    )
    await expectNoAxeViolations()
  })
})
