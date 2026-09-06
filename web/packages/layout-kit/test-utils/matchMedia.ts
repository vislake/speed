/**
 * window.matchMedia mock for tests that exercise AppShell's responsive
 * desktop/mobile split.
 *
 * jsdom does not implement matchMedia, so
 * `useMediaQuery(theme.breakpoints.up('md'))` has nothing to call in a
 * unit test unless matchMedia is stubbed. `mockMatchMedia(matches)`
 * installs a MediaQueryList-shaped stub that reports `matches` for every
 * query -- sufficient for AppShell's single `up('md')` query. Call it
 * before rendering to pick the desktop or mobile branch deterministically;
 * `test-utils/setup.ts` installs a desktop-true default before each test
 * so suites that do not care about the split need not call it at all.
 *
 * The returned handle's `changeMatches(next)` flips the reported answer
 * and fires the stub's own 'change' listeners -- the one subscription
 * path MUI's useMediaQuery keeps on the list object it memoized at first
 * render (it reads `.matches` live through useSyncExternalStore) -- so a
 * single mounted AppShell can be driven across the desktop/mobile split
 * (the breakpoint-crossing tests) instead of only picking a side before
 * the first render. Call mockMatchMedia once and flip through the handle;
 * installing a second mock mid-test is invisible to a mounted
 * useMediaQuery, which still holds the first list.
 */

import { vi } from 'vitest'

/** Fires the installed mock's own media 'change' listeners. */
export interface MatchMediaHandle {
  /** Report `next` as the new answer and notify subscribed listeners. */
  changeMatches(matches: boolean): void
}

type ChangeListener = (event: { matches: boolean; media: string }) => void

export function mockMatchMedia(matches: boolean): MatchMediaHandle {
  let current = matches
  let media = ''
  const listeners = new Set<ChangeListener>()

  // The literal is cast through `unknown` because MediaQueryList is a
  // nominal interface here; the cast drops contextual typing, so every
  // member is annotated by hand. The legacy addListener/removeListener
  // pair and the modern addEventListener pair land in the same set: MUI
  // v9 subscribes through addEventListener, and the legacy path costs
  // nothing.
  const list = {
    get matches(): boolean {
      return current
    },
    get media(): string {
      return media
    },
    onchange: null,
    addListener: (listener: ChangeListener): void => {
      listeners.add(listener)
    },
    removeListener: (listener: ChangeListener): void => {
      listeners.delete(listener)
    },
    addEventListener: (_type: string, listener: ChangeListener): void => {
      listeners.add(listener)
    },
    removeEventListener: (_type: string, listener: ChangeListener): void => {
      listeners.delete(listener)
    },
    dispatchEvent: (): boolean => true,
  } as unknown as MediaQueryList

  window.matchMedia = vi.fn().mockImplementation((query: string) => {
    media = query
    return list
  })

  return {
    changeMatches(next: boolean): void {
      current = next
      for (const listener of listeners) {
        listener({ matches: next, media })
      }
    },
  }
}
