/**
 * Contract tests for the AccessTokenStore seam: the memory store keeps
 * exactly one token, forgets it on sign-out, and never touches any
 * persistence API -- there is no storage seam anywhere in the package
 * (docs/internal/12-frontend.md: an access token in localStorage is a
 * credential an XSS walks away with). The absence is structural: the
 * store has no browser hooks to call, so the moment anyone adds one the
 * typecheck of this package's consumers stops compiling.
 */

import { describe, expect, it } from 'vitest'
import {
  createMemoryAccessTokenStore,
  type AccessTokenStore,
} from './index'

describe('createMemoryAccessTokenStore', () => {
  it('starts signed out and holds one token until cleared', () => {
    const store = createMemoryAccessTokenStore()
    expect(store.get()).toBeNull()

    store.set('access-token-1')
    expect(store.get()).toBe('access-token-1')

    store.set('access-token-2')
    expect(store.get()).toBe('access-token-2')

    store.set(null)
    expect(store.get()).toBeNull()
  })

  it('gives each store its own memory cell', () => {
    const first = createMemoryAccessTokenStore()
    const second = createMemoryAccessTokenStore()
    first.set('first-token')
    expect(second.get()).toBeNull()
  })

  it('is a plain interface hosts implement (sync get/set, string | null)', () => {
    // Auth-core (M1) will bind its own implementation here; the client
    // only depends on the shape, so drift breaks compilation.
    let hostToken: string | null = 'host-issued-token'
    const hostStore: AccessTokenStore = {
      get(): string | null {
        return hostToken
      },
      set(token: string | null): void {
        hostToken = token
      },
    }
    expect(hostStore.get()).toBe('host-issued-token')
    hostStore.set('host-issued-token-2')
    expect(hostStore.get()).toBe('host-issued-token-2')
  })
})
