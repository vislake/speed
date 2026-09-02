/**
 * Persistence binding for the manually chosen language.
 *
 * One WeakMap slot per instance keeps detection and later switches coherent:
 * createI18n resolves the storage (host-injected or the browser's
 * localStorage) and the storage key once, binds them to the instance, and
 * switchLanguage writes through the same binding. A null binding means "no
 * persistence" -- hosts that must not write storage (embedded contexts,
 * tests) opt out explicitly, and nothing ever falls back to a hidden store.
 */

/** Minimal storage surface; satisfies localStorage and any host test double. */
export interface StorageLike {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
}

/** The storage key the language choice is persisted under. */
export const SPEED_LOCALE_STORAGE_KEY = 'speed.locale'

/** What an instance is bound to: the resolved storage and the key to use. */
export interface InstanceStorage {
  readonly storage: StorageLike | null
  readonly key: string
}

const instanceStorage = new WeakMap<object, InstanceStorage>()

/** Bind resolved storage and key to an instance (called by createI18n). */
export function bindInstanceStorage(
  instance: object,
  storage: StorageLike | null,
  key: string,
): void {
  instanceStorage.set(instance, { storage, key })
}

/** The binding for an instance created by createI18n, or null for foreign instances. */
export function boundInstanceStorage(instance: object): InstanceStorage | null {
  return instanceStorage.get(instance) ?? null
}
