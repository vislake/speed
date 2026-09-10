/**
 * profile-preference-sync.test.tsx -- the host's profile -> instance
 * language bridge: the stored preference applies to the live instance
 * after sign-in ONLY when the chain's higher tiers (the ?lang= URL
 * override, the manual-choice slot) have decided nothing, and the
 * application never persists (the profile must not be written into the
 * manual slot, where it would shadow later profile changes).
 */

import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { waitFor } from '@testing-library/react'
import { act } from '@testing-library/react'
import { SPEED_LOCALE_STORAGE_KEY } from '@speed/i18n'
import { ProfilePreferenceSync } from './profile-preference-sync.js'
import { renderWithAppServices } from '../test-utils/render.js'
import { makeRealClientRig } from '../test-utils/real-client.js'
import { demoServer, DEMO_OWNER_IDENTIFIER } from '../test-utils/demo-server.js'
import { APP_PASSWORD } from '../test-utils/app-harness.js'

// This environment carries no working localStorage global (Node 26's
// experimental getter offers nothing without --localstorage-file, and
// the pinned toolchain's Node has no global at all): a memory-backed
// stub keeps the sync's manual-slot read and its non-persisting write
// assertable deterministically, the same stub shape main.test.tsx
// installs for the bootstrap's own storage leg.
let realLocalStorage: PropertyDescriptor | undefined
let memoryStorage: Map<string, string>

beforeEach(() => {
  realLocalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  memoryStorage = new Map()
  Object.defineProperty(globalThis, 'localStorage', {
    value: {
      getItem: (key: string) => memoryStorage.get(key) ?? null,
      setItem: (key: string, value: string) => {
        memoryStorage.set(key, value)
      },
      removeItem: (key: string) => {
        memoryStorage.delete(key)
      },
    },
    configurable: true,
    writable: true,
  })
})

afterEach(() => {
  if (realLocalStorage !== undefined) {
    Object.defineProperty(globalThis, 'localStorage', realLocalStorage)
  } else {
    delete (globalThis as { localStorage?: unknown }).localStorage
  }
})

/** Signs the rig's session in as the demo owner (the demo server issues
 * tokens on the password login), the frame transition the sync applies
 * its effect at. */
async function signIn(rig: ReturnType<typeof makeRealClientRig>): Promise<void> {
  await act(async () => {
    await rig.session.loginWithPassword({
      identifier: DEMO_OWNER_IDENTIFIER,
      password: APP_PASSWORD,
    })
  })
}

describe('ProfilePreferenceSync', () => {
  it('applies the stored profile language to the instance after sign-in, without persisting it', async () => {
    const rig = makeRealClientRig(
      demoServer({ initialPreferences: { locale: 'en-US' } }),
    )
    const view = renderWithAppServices(<ProfilePreferenceSync />, {
      session: rig.session,
      api: rig.api,
    })
    expect(view.i18n.language).toBe('zh-CN')

    await signIn(rig)

    await waitFor(() => expect(view.i18n.language).toBe('en-US'))
    // The application is non-persisting: the manual-choice slot stays
    // empty, so a later profile change is never shadowed by this write.
    expect(globalThis.localStorage.getItem(SPEED_LOCALE_STORAGE_KEY)).toBeNull()
  })

  it('leaves a manual-choice decision in charge', async () => {
    globalThis.localStorage.setItem(SPEED_LOCALE_STORAGE_KEY, 'zh-CN')
    try {
      const rig = makeRealClientRig(
        demoServer({ initialPreferences: { locale: 'en-US' } }),
      )
      const view = renderWithAppServices(<ProfilePreferenceSync />, {
        session: rig.session,
        api: rig.api,
      })

      await signIn(rig)
      // The stored profile read lands (the demo server answers), but
      // the manual slot already decided: the instance must not move.
      await waitFor(() =>
        expect(
          rig.calls.some(
            (call) => call.path === '/api/v1/authn/me/preferences',
          ),
        ).toBe(true),
      )
      expect(view.i18n.language).toBe('zh-CN')
    } finally {
      globalThis.localStorage.removeItem(SPEED_LOCALE_STORAGE_KEY)
    }
  })

  it('leaves a ?lang= URL decision in charge', async () => {
    window.history.replaceState({}, '', '/?lang=zh-CN')
    try {
      const rig = makeRealClientRig(
        demoServer({ initialPreferences: { locale: 'en-US' } }),
      )
      const view = renderWithAppServices(<ProfilePreferenceSync />, {
        session: rig.session,
        api: rig.api,
      })

      await signIn(rig)
      await waitFor(() =>
        expect(
          rig.calls.some(
            (call) => call.path === '/api/v1/authn/me/preferences',
          ),
        ).toBe(true),
      )
      expect(view.i18n.language).toBe('zh-CN')
    } finally {
      window.history.replaceState({}, '', '/')
    }
  })

  it('ignores a stored locale the instance cannot speak', async () => {
    const rig = makeRealClientRig(
      demoServer({ initialPreferences: { locale: 'fr-FR' } }),
    )
    const view = renderWithAppServices(<ProfilePreferenceSync />, {
      session: rig.session,
      api: rig.api,
    })

    await signIn(rig)
    await waitFor(() =>
      expect(
        rig.calls.some((call) => call.path === '/api/v1/authn/me/preferences'),
      ).toBe(true),
    )
    expect(view.i18n.language).toBe('zh-CN')
  })
})
