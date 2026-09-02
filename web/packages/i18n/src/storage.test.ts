/**
 * Contract tests for instance/storage binding: one storage slot per
 * instance, resolvable afterwards, overridable by a later bind.
 */

import { describe, expect, it } from 'vitest'
import {
  SPEED_LOCALE_STORAGE_KEY,
  bindInstanceStorage,
  boundInstanceStorage,
} from './storage'
import { MemoryStorage } from '../test-utils/memory-storage'

describe('storage binding', () => {
  it('uses the documented storage key for the locale choice', () => {
    expect(SPEED_LOCALE_STORAGE_KEY).toBe('speed.locale')
  })

  it('returns the binding for a bound instance and null for a foreign one', () => {
    const instance = {}
    const storage = new MemoryStorage()
    expect(boundInstanceStorage(instance)).toBeNull()
    bindInstanceStorage(instance, storage, SPEED_LOCALE_STORAGE_KEY)
    expect(boundInstanceStorage(instance)).toEqual({
      storage,
      key: SPEED_LOCALE_STORAGE_KEY,
    })
  })

  it('tracks instances independently', () => {
    const first = {}
    const second = {}
    const firstStorage = new MemoryStorage()
    bindInstanceStorage(first, firstStorage, 'a')
    bindInstanceStorage(second, null, 'b')
    expect(boundInstanceStorage(first)).toEqual({ storage: firstStorage, key: 'a' })
    expect(boundInstanceStorage(second)).toEqual({ storage: null, key: 'b' })
  })

  it('lets a rebind replace the binding (host changes storage at runtime)', () => {
    const instance = {}
    const firstStorage = new MemoryStorage()
    const secondStorage = new MemoryStorage()
    bindInstanceStorage(instance, firstStorage, 'a')
    bindInstanceStorage(instance, secondStorage, 'b')
    expect(boundInstanceStorage(instance)).toEqual({ storage: secondStorage, key: 'b' })
  })
})
