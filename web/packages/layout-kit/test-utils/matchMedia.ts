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
 */

import { vi } from 'vitest'

export function mockMatchMedia(matches: boolean): void {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    matches,
    media: query,
    onchange: null,
    addListener: vi.fn(),
    removeListener: vi.fn(),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
  }))
}
