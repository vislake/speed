/**
 * SessionEndedScreen behaviour: the screen renders the session-ended
 * title, description and sign-in action from the auth-ui namespace --
 * every ui-kit EmptyState text slot overridden, so a built-in ui-kit
 * string can never leak through -- re-renders in the switched language,
 * fires onSignIn once per action click, renders its en-US bundle on an
 * English-starting instance, and passes axe. Pure presentation: no
 * session, no network, no props beyond the action. Text expectations
 * read the bundle values, never inline language.
 */

import { describe, expect, it, vi } from 'vitest'
import { act, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { switchLanguage } from '@speed/i18n'
import { SessionEndedScreen } from './SessionEndedScreen.js'
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }

describe('SessionEndedScreen', () => {
  it('render the zh-CN title, description and sign-in action', () => {
    renderWithProviders(<SessionEndedScreen onSignIn={() => {}} />)
    expect(screen.getByText(zhCN.sessionEnded.title)).toBeInTheDocument()
    expect(
      screen.getByText(zhCN.sessionEnded.description),
    ).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: zhCN.sessionEnded.signInAction }),
    ).toBeInTheDocument()
  })

  it('re-render all three texts in the switched language', async () => {
    const { i18n } = renderWithProviders(
      <SessionEndedScreen onSignIn={() => {}} />,
    )
    expect(screen.getByText(zhCN.sessionEnded.title)).toBeInTheDocument()
    await act(async () => {
      await switchLanguage(i18n, 'en-US')
    })
    expect(screen.getByText(enUS.sessionEnded.title)).toBeInTheDocument()
    expect(
      screen.getByText(enUS.sessionEnded.description),
    ).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: enUS.sessionEnded.signInAction }),
    ).toBeInTheDocument()
    expect(
      screen.queryByText(zhCN.sessionEnded.title),
    ).not.toBeInTheDocument()
  })

  it('render the en-US bundle on an English-starting instance', () => {
    renderWithProviders(<SessionEndedScreen onSignIn={() => {}} />, {
      language: 'en-US',
    })
    expect(screen.getByText(enUS.sessionEnded.title)).toBeInTheDocument()
    expect(
      screen.getByText(enUS.sessionEnded.description),
    ).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: enUS.sessionEnded.signInAction }),
    ).toBeInTheDocument()
  })

  it('fire onSignIn once per action click', async () => {
    const onSignIn = vi.fn()
    renderWithProviders(<SessionEndedScreen onSignIn={onSignIn} />)
    const user = userEvent.setup()
    const action = screen.getByRole('button', {
      name: zhCN.sessionEnded.signInAction,
    })
    await user.click(action)
    await user.click(action)
    expect(onSignIn).toHaveBeenCalledTimes(2)
  })

  it('pass axe with no violations', async () => {
    renderWithProviders(<SessionEndedScreen onSignIn={() => {}} />)
    await expectNoAxeViolations()
  })
})
