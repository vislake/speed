/**
 * NotesView placeholder contract: the interim notes surface renders the
 * ui-kit EmptyState speaking the app namespace in the active language.
 * These two bilingual checks pin the placeholder's copy to its bundle
 * keys until the surface's own commit replaces this file (the notes
 * list, the create flow and the RouteGuard wiring); they render with
 * the plain provider harness because the placeholder composes no app
 * services.
 */

import { describe, expect, it } from 'vitest'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../test-utils/render.js'
import { NotesView } from './notes-view.js'

describe('NotesView', () => {
  it('renders the zh placeholder copy', () => {
    const view = renderWithProviders(<NotesView />)
    expect(view.getByText(zhCN.placeholder.notesTitle)).toBeInTheDocument()
    expect(
      view.getByText(zhCN.placeholder.notesDescription),
    ).toBeInTheDocument()
  })

  it('renders the en placeholder copy on an English-starting instance', () => {
    const view = renderWithProviders(<NotesView />, { language: 'en-US' })
    expect(view.getByText(enUS.placeholder.notesTitle)).toBeInTheDocument()
    expect(
      view.getByText(enUS.placeholder.notesDescription),
    ).toBeInTheDocument()
  })
})
