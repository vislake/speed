/**
 * PreferencesSection behaviour: the language and timezone selects over
 * GET/PATCH /api/v1/authn/me/preferences.
 *
 * What this suite pins, per story: the stored values render into the
 * controls (including a stored timezone the engine's own list omits,
 * which must be added as its own option rather than silently displaying
 * the wrong one); a language change PATCHes the account FIRST and only a
 * confirmed write switches the instance (the manual slot and the account
 * move together) -- and a refused write renders the two-line failure
 * alert (save line plus the server's code text) with the instance left
 * on the old language; a timezone change and a timezone CLEAR (the
 * "not chosen" choice, whose value is the empty string) PATCH the field;
 * and a settled load that delivered no data renders the EmptyState error
 * variant with a retry that refetches. Every write's request body is
 * pinned, so the PATCH contract (partial update, empty string clears) is
 * asserted from the wire up.
 */

import { describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import zhCN from './locales/zh-CN.json' with { type: 'json' }
import enUS from './locales/en-US.json' with { type: 'json' }
import {
  errorResponse,
  jsonResponse,
  makePair,
  makeRealClientRig,
  signInWithPassword,
  type RealCall,
  type RealResponder,
} from '../test-utils/real-client.js'
import { renderWithProviders } from '../test-utils/render.js'
import { expectNoAxeViolations } from '../test-utils/axe.js'
import { PreferencesSection } from './PreferencesSection.js'

const LOGIN_PATH = '/api/v1/authn/login/password'
const PREFERENCES_PATH = '/api/v1/authn/me/preferences'

/** The language label the component computes for `tag` under `ui`, using
 * the same Intl.DisplayNames call (with the same raw-tag fallback), so
 * the assertion tracks the engine's own naming. */
function languageLabel(tag: string, ui: string): string {
  try {
    return new Intl.DisplayNames([ui], { type: 'language' }).of(tag) ?? tag
  } catch {
    return tag
  }
}

/** A stateful preferences responder: GET answers the current pair, PATCH
 * merges the partial body into it (absent field unchanged, empty string
 * clears -- the endpoint's contract) and answers the stored pair. */
function makePreferencesResponder(initial: {
  locale?: string
  timezone?: string
}) {
  const stored: { locale?: string; timezone?: string } = { ...initial }
  const respond: RealResponder = async (call: RealCall) => {
    if (call.method === 'POST' && call.path === LOGIN_PATH) {
      return jsonResponse(200, makePair())
    }
    if (call.path !== PREFERENCES_PATH) {
      throw new Error(`unexpected ${call.method} ${call.path}`)
    }
    if (call.method === 'GET') {
      const body: Record<string, string> = {}
      if (stored.locale !== undefined && stored.locale !== '') {
        body.locale = stored.locale
      }
      if (stored.timezone !== undefined && stored.timezone !== '') {
        body.timezone = stored.timezone
      }
      return jsonResponse(200, body)
    }
    if (call.method === 'PATCH') {
      const patch = call.body as { locale?: string; timezone?: string }
      if (patch.locale !== undefined) {
        stored.locale = patch.locale
      }
      if (patch.timezone !== undefined) {
        stored.timezone = patch.timezone
      }
      return jsonResponse(200, stored)
    }
    throw new Error(`unexpected ${call.method} ${call.path}`)
  }
  return { respond, stored }
}

/** Opens a MUI select and picks the option whose accessible name is
 * `optionName`, through the dispatched events the component's own
 * DataTable-pattern suites use (mousedown opens the menu, the click on
 * the rendered option commits the choice). */
function chooseOption(combobox: HTMLElement, optionName: string): void {
  fireEvent.mouseDown(combobox)
  fireEvent.click(screen.getByRole('option', { name: optionName }))
}

/** Mounts the section over a rig whose login and preferences operations
 * answer from `respond`. */
async function mountSection(respond: RealResponder) {
  const rig = makeRealClientRig(respond)
  await signInWithPassword(rig)
  const rendered = renderWithProviders(<PreferencesSection />)
  return { rig, ...rendered }
}

describe('PreferencesSection', () => {
  it('renders the stored preferences into the controls and passes axe', async () => {
    const { respond } = makePreferencesResponder({
      locale: 'en-US',
      timezone: 'Asia/Tokyo',
    })
    const { rig } = await mountSection(respond)

    expect(
      await screen.findByRole('heading', { name: zhCN.preferences.title }),
    ).toBeTruthy()

    const language = await screen.findByRole('combobox', {
      name: zhCN.preferences.language.label,
    })
    expect(language).toHaveTextContent(languageLabel('en-US', 'zh-CN'))
    const timezone = screen.getByRole('combobox', {
      name: zhCN.preferences.timezone.label,
    })
    expect(timezone).toHaveTextContent('Asia/Tokyo')

    // Both controls' values come from the one preferences read.
    expect(
      rig.calls.filter((call) => call.path === PREFERENCES_PATH),
    ).toHaveLength(1)

    await expectNoAxeViolations()
  })

  it('PATCHes the language first, then switches the instance (manual slot and account move together)', async () => {
    const { respond, stored } = makePreferencesResponder({})
    const { rig, i18n } = await mountSection(respond)

    const language = await screen.findByRole('combobox', {
      name: zhCN.preferences.language.label,
    })
    // A fresh account's empty locale reads as the "not chosen" choice.
    expect(language).toHaveTextContent(zhCN.preferences.language.notChosen)

    chooseOption(language, languageLabel('en-US', 'zh-CN'))

    await waitFor(() => expect(i18n.language).toBe('en-US'))
    expect(stored.locale).toBe('en-US')
    const patch = rig.calls.find(
      (call) => call.method === 'PATCH' && call.path === PREFERENCES_PATH,
    )
    expect(patch).toBeDefined()
    // The body is the partial update the endpoint documents: only the
    // changed field.
    expect(patch?.body).toEqual({ locale: 'en-US' })
    // The UI switched with the account: the heading now speaks en-US.
    expect(
      await screen.findByRole('heading', { name: enUS.preferences.title }),
    ).toBeTruthy()
  })

  it('renders the two-line failure alert and does not switch when the language write is refused', async () => {
    const respond: RealResponder = async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === PREFERENCES_PATH) {
        return jsonResponse(200, {})
      }
      if (call.method === 'PATCH' && call.path === PREFERENCES_PATH) {
        return errorResponse(400, 'authn.invalid_locale')
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    }
    const { i18n } = await mountSection(respond)

    const language = await screen.findByRole('combobox', {
      name: zhCN.preferences.language.label,
    })
    chooseOption(language, languageLabel('en-US', 'zh-CN'))

    const alert = await screen.findByRole('alert')
    // Line one says what happened; line two is the server's own code text.
    expect(alert).toHaveTextContent(zhCN.preferences.saveFailed)
    expect(alert).toHaveTextContent(zhCN.errors.authn.invalid_locale)
    // A refused write must not move the device language.
    expect(i18n.language).toBe('zh-CN')
  })

  // This test's cost is the engine's own timezone list: ~418 zones
  // rendered into the open select menu twice (the pick and the clear)
  // and two option queries by accessible name over the whole list --
  // ~1.2s on a quiet machine under the coverage gate, and past the
  // default 5s budget on loaded shared runners. The explicit 20s budget
  // (the same bound the usage-example journey carries for this package's
  // render-heavy class) covers that loaded-runner cost with headroom; a
  // genuinely stuck write still fails at this bound.
  it('PATCHes a timezone choice and the cleared empty string', async () => {
    const { respond, stored } = makePreferencesResponder({
      timezone: 'Asia/Tokyo',
    })
    const { rig } = await mountSection(respond)

    const timezone = await screen.findByRole('combobox', {
      name: zhCN.preferences.timezone.label,
    })
    expect(timezone).toHaveTextContent('Asia/Tokyo')

    // A new zone: the PATCH carries it alone. (The engine's own list
    // omits 'UTC' by specification, so a real city zone is the choice
    // here; the stored-value guard's UTC coverage lives in the omitted-
    // value test below.)
    chooseOption(timezone, 'Europe/Berlin')
    await waitFor(() => expect(stored.timezone).toBe('Europe/Berlin'))
    let patches = rig.calls.filter((call) => call.method === 'PATCH')
    expect(patches[0]?.body).toEqual({
      timezone: 'Europe/Berlin',
    })

    // Back to "not chosen": the clearing value is the empty string.
    chooseOption(
      screen.getByRole('combobox', {
        name: zhCN.preferences.timezone.label,
      }),
      zhCN.preferences.timezone.notChosen,
    )
    await waitFor(() => expect(stored.timezone).toBe(''))
    patches = rig.calls.filter((call) => call.method === 'PATCH')
    expect(patches[1]?.body).toEqual({
      timezone: '',
    })
  }, 20000)

  it('adds a stored timezone the engine list omits as its own option', async () => {
    // The engine's list is narrowed for this test: the stored value is
    // not in it, and the control must still offer (and show) it -- a
    // select whose value matches no option would silently display
    // another zone.
    const supportedValues = vi
      .spyOn(Intl, 'supportedValuesOf')
      .mockReturnValue(['UTC'])
    try {
      const { respond } = makePreferencesResponder({
        timezone: 'Legacy/Zone',
      })
      await mountSection(respond)

      const timezone = await screen.findByRole('combobox', {
        name: zhCN.preferences.timezone.label,
      })
      expect(timezone).toHaveTextContent('Legacy/Zone')

      fireEvent.mouseDown(timezone)
      expect(screen.getByRole('option', { name: 'Legacy/Zone' })).toBeTruthy()
      expect(screen.getByRole('option', { name: 'UTC' })).toBeTruthy()
    } finally {
      supportedValues.mockRestore()
    }
  })

  it('renders the error state with a retry that refetches when the load fails', async () => {
    const user = userEvent.setup()
    let failLoad = true
    const respond: RealResponder = async (call) => {
      if (call.method === 'POST' && call.path === LOGIN_PATH) {
        return jsonResponse(200, makePair())
      }
      if (call.method === 'GET' && call.path === PREFERENCES_PATH) {
        if (failLoad) {
          return errorResponse(500, 'authn.internal_error')
        }
        return jsonResponse(200, { timezone: 'Asia/Tokyo' })
      }
      throw new Error(`unexpected ${call.method} ${call.path}`)
    }
    await mountSection(respond)

    expect(
      await screen.findByText(zhCN.preferences.error.title),
    ).toBeTruthy()
    expect(
      screen.queryByRole('combobox', {
        name: zhCN.preferences.language.label,
      }),
    ).toBeNull()

    failLoad = false
    await user.click(screen.getByRole('button', { name: zhCN.preferences.retry }))
    expect(
      await screen.findByRole('combobox', {
        name: zhCN.preferences.timezone.label,
      }),
    ).toHaveTextContent('Asia/Tokyo')
  })
})
