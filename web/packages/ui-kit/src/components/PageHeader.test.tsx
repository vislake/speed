/**
 * PageHeader contract: the page title renders as the single h1 in the
 * heading-4 role, description and actions render when given, and the
 * breadcrumb trail renders above the title inside a navigation
 * landmark whose accessible name comes from the ui-kit namespace --
 * asserted against the bundles, never against inlined language text.
 * Visible crumb labels are caller content.
 */

import { describe, expect, it, vi } from 'vitest'
import { act, fireEvent, within } from '@testing-library/react'
import { switchLanguage } from '@speed/i18n'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../../test-utils/render.js'
import { expectNoAxeViolations } from '../../test-utils/axe.js'
import { PageHeader } from './PageHeader.js'
import type { PageHeaderBreadcrumb } from './PageHeader.js'

const CRUMBS: readonly PageHeaderBreadcrumb[] = [
  { label: 'Workspaces', href: '/workspaces' },
  { label: 'Dental Care' },
  { label: 'Members', href: '/workspaces/dental-care/members' },
]

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

  it('renders the breadcrumb trail above the title in a labeled navigation landmark', () => {
    const { getByRole } = renderWithProviders(
      <PageHeader title="Members" breadcrumbs={CRUMBS} />,
    )
    const nav = getByRole('navigation', { name: zhCN.pageHeader.breadcrumbNav })
    expect(nav).toBeInTheDocument()
    const crumbs = within(nav)
    expect(crumbs.getByText('Workspaces')).toBeInTheDocument()
    expect(crumbs.getByText('Dental Care')).toBeInTheDocument()
    expect(crumbs.getByText('Members')).toBeInTheDocument()
    // The trail comes before the page title inside the header element.
    const heading = getByRole('heading', { level: 1 })
    const header = heading.closest('header')
    expect(header).not.toBeNull()
    expect(header!.contains(nav)).toBe(true)
    expect(nav.compareDocumentPosition(heading) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0)
  })

  it('renders a crumb with href as a link and one without as plain text', () => {
    const { getByRole, queryByRole } = renderWithProviders(
      <PageHeader title="Members" breadcrumbs={CRUMBS} />,
    )
    expect(getByRole('link', { name: 'Workspaces' })).toHaveAttribute('href', '/workspaces')
    expect(getByRole('link', { name: 'Members' })).toHaveAttribute(
      'href',
      '/workspaces/dental-care/members',
    )
    expect(queryByRole('link', { name: 'Dental Care' })).not.toBeInTheDocument()
  })

  it('marks a link last crumb as the current page', () => {
    const { getByRole } = renderWithProviders(
      <PageHeader title="Members" breadcrumbs={CRUMBS} />,
    )
    expect(getByRole('link', { name: 'Members' })).toHaveAttribute('aria-current', 'page')
    const nav = getByRole('navigation', { name: zhCN.pageHeader.breadcrumbNav })
    expect(within(nav).getByText('Dental Care')).not.toHaveAttribute('aria-current', 'page')
  })

  it('marks a plain-text last crumb as the current page too', () => {
    // The current-page semantics must not depend on the crumb being a link.
    const { getByRole, container } = renderWithProviders(
      <PageHeader title="Overview" breadcrumbs={[{ label: 'Workspaces', href: '/workspaces' }, { label: 'Overview' }]} />,
    )
    const navPlain = getByRole('navigation', { name: zhCN.pageHeader.breadcrumbNav })
    const current = container.querySelector('[aria-current="page"]')
    expect(current).not.toBeNull()
    expect(navPlain.contains(current)).toBe(true)
    expect(current).toHaveTextContent('Overview')
  })

  it('fires onClick when a crumb link is clicked', () => {
    const onCrumbClick = vi.fn()
    const { getByRole } = renderWithProviders(
      <PageHeader
        title="Members"
        breadcrumbs={[
          {
            label: 'Workspaces',
            href: '/workspaces',
            // A host's SPA interception handler prevents the default
            // navigation (router push instead); without preventDefault
            // jsdom reports an unimplemented document navigation.
            onClick: (event) => {
              event.preventDefault()
              onCrumbClick()
            },
          },
          { label: 'Members' },
        ]}
      />,
    )
    fireEvent.click(getByRole('link', { name: 'Workspaces' }))
    expect(onCrumbClick).toHaveBeenCalledTimes(1)
  })

  it('renders no navigation landmark without breadcrumbs', () => {
    const { queryByRole } = renderWithProviders(<PageHeader title="Settings" />)
    expect(queryByRole('navigation')).not.toBeInTheDocument()
  })

  it('switches the breadcrumb nav accessible name with the language', async () => {
    const { i18n, getByRole, queryByRole } = renderWithProviders(
      <PageHeader title="Members" breadcrumbs={CRUMBS} />,
    )
    expect(getByRole('navigation', { name: zhCN.pageHeader.breadcrumbNav })).toBeInTheDocument()
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    expect(getByRole('navigation', { name: enUS.pageHeader.breadcrumbNav })).toBeInTheDocument()
    expect(queryByRole('navigation', { name: zhCN.pageHeader.breadcrumbNav })).not.toBeInTheDocument()
  })

  it('labels the collapse-expand control from the namespace when the trail overflows', () => {
    const trail = Array.from({ length: 9 }, (_, i) => ({
      label: `Crumb ${i + 1}`,
      href: `/crumb/${i + 1}`,
    }))
    const { getByRole } = renderWithProviders(
      <PageHeader title="Deep page" breadcrumbs={trail} />,
    )
    expect(
      getByRole('button', { name: zhCN.pageHeader.showFullPath }),
    ).toBeInTheDocument()
  })

  it('passes axe over the header with a breadcrumb trail', async () => {
    renderWithProviders(
      <PageHeader
        title="Members"
        description="Everyone with access to this workspace."
        breadcrumbs={CRUMBS}
        actions={<button type="button">Invite member</button>}
      />,
    )
    await expectNoAxeViolations()
  })
})
