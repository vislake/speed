/**
 * window.matchMedia mock for the app's DOM tests.
 *
 * jsdom does not implement matchMedia, so AppShell's
 * `useMediaQuery(theme.breakpoints.up('md'))` has nothing to call in a
 * unit test unless matchMedia is stubbed. `mockMatchMedia(matches)`
 * installs a MediaQueryList-shaped stub that reports `matches` for every
 * query -- sufficient for AppShell's single `up('md')` query. The app's
 * test-utils/setup.ts installs a desktop-true default before each test,
 * so suites that render the AppShell frame (whose desktop branch mounts
 * the permanent nav drawer) need not call it at all; a suite that must
 * pin the mobile branch calls it before rendering.
 *
 * This is the app layer's own copy of the same stub the layout-kit and
 * product-shell packages ship (jsdom's gap is every DOM suite's gap).
 * The copy is layer-local by design -- each layer holds its own test
 * utilities rather than importing a sibling package's (extracting a
 * shared rig package is recorded DEFERRED, as in real-client.ts).
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
