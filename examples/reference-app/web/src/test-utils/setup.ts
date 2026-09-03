/**
 * Shared vitest setup for reference-app-web tests.
 *
 * React Testing Library's auto-cleanup only fires when the test framework
 * exposes a global afterEach (Jest, mocha); vitest does not, so tests that
 * mount trees would leak DOM between tests without this file. The
 * jsdom-environment tests render with the shared provider stack of
 * src/test-utils/render.tsx; this setup registers the jest-dom matchers
 * every suite asserts with, cleans up after each test, and installs a
 * desktop-default window.matchMedia stub (see matchMedia.ts) so
 * AppShell's useMediaQuery call -- the frame's desktop/mobile drawer
 * split -- has something to call in jsdom, deterministically taking the
 * permanent-drawer branch (the product-shell setup pattern, layer-local
 * as in every suite that renders the frame).
 */

import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach, beforeEach } from 'vitest'
import { mockMatchMedia } from './matchMedia.js'

beforeEach(() => {
  mockMatchMedia(true)
})

afterEach(() => {
  cleanup()
})
