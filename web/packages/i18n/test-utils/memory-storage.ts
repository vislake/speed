/**
 * Deterministic in-memory StorageLike for the i18n tests and for hosts that
 * must run without a DOM. createI18n treats an injected storage exactly
 * like localStorage: the negotiated language is read from it at creation
 * and persisted to it on switchLanguage.
 */

import type { StorageLike } from '../src/storage'

/** In-memory storage: read/write like localStorage, assertable afterwards. */
export class MemoryStorage implements StorageLike {
  private readonly values = new Map<string, string>()

  getItem(key: string): string | null {
    return this.values.get(key) ?? null
  }

  setItem(key: string, value: string): void {
    this.values.set(key, value)
  }

  /** Snapshot of every stored entry, for assertions. */
  entries(): ReadonlyMap<string, string> {
    return this.values
  }
}
