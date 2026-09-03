/**
 * AccountView placeholder contract: the interim account surface renders
 * the ui-kit EmptyState speaking the app namespace in the active
 * language. These two bilingual checks pin the placeholder's copy to
 * its bundle keys until the surface's own commit replaces this file
 * (the account-ui sections over the generated API and the binding
 * callback subroute); they render with the plain provider harness
 * because the placeholder composes no app services.
 */

import { describe, expect, it } from 'vitest'
import zhCN from '../locales/zh-CN.json' with { type: 'json' }
import enUS from '../locales/en-US.json' with { type: 'json' }
import { renderWithProviders } from '../test-utils/render.js'
import { AccountView } from './account-view.js'

describe('AccountView', () => {
  it('renders the zh placeholder copy', () => {
    const view = renderWithProviders(<AccountView />)
    expect(view.getByText(zhCN.placeholder.accountTitle)).toBeInTheDocument()
    expect(
      view.getByText(zhCN.placeholder.accountDescription),
    ).toBeInTheDocument()
  })

  it('renders the en placeholder copy on an English-starting instance', () => {
    const view = renderWithProviders(<AccountView />, { language: 'en-US' })
    expect(view.getByText(enUS.placeholder.accountTitle)).toBeInTheDocument()
    expect(
      view.getByText(enUS.placeholder.accountDescription),
    ).toBeInTheDocument()
  })
})
