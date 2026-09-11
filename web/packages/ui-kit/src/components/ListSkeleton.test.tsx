/**
 * ListSkeleton contract: one role="status" region carrying the passed
 * label with aria-busy set -- the single loading announcement, no fake
 * heading -- and one envelope row per entry with the shared spacing and
 * the divider between rows (never above the first).
 */

import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import { ListSkeleton } from './ListSkeleton.js'
import { renderWithProviders } from '../../test-utils/render.js'

describe('ListSkeleton', () => {
  it('announces one loading status carrying the passed label', () => {
    renderWithProviders(
      <ListSkeleton label="Loading sessions" rows={[<p key="a">line</p>]} />,
    )
    const status = screen.getByRole('status', { name: 'Loading sessions' })
    expect(status).toHaveAttribute('aria-busy', 'true')
    expect(status).toHaveTextContent('line')
  })

  it('wraps every entry in one row envelope, divider between rows only', () => {
    const { container } = renderWithProviders(
      <ListSkeleton
        label="Loading"
        rows={[
          <p key="a">first row</p>,
          <p key="b">second row</p>,
          <p key="c">third row</p>,
        ]}
      />,
    )
    const status = screen.getByRole('status', { name: 'Loading' })
    const rows = Array.from(status.children)
    expect(rows).toHaveLength(3)
    expect(rows[0]).toHaveTextContent('first row')
    expect(rows[1]).toHaveTextContent('second row')
    expect(rows[2]).toHaveTextContent('third row')
    for (const row of rows) {
      expect(row).toHaveStyle({ paddingTop: '12px', paddingBottom: '12px' })
    }
    expect(rows[0]).not.toHaveStyle({ borderTopStyle: 'solid' })
    expect(rows[1]).toHaveStyle({ borderTopStyle: 'solid' })
    expect(rows[2]).toHaveStyle({ borderTopStyle: 'solid' })
    expect(container.querySelectorAll('.MuiSkeleton-root')).toHaveLength(0)
  })

  it('renders an empty announcement for an entry-less list', () => {
    renderWithProviders(<ListSkeleton label="Loading" rows={[]} />)
    const status = screen.getByRole('status', { name: 'Loading' })
    expect(status.children).toHaveLength(0)
  })
})
