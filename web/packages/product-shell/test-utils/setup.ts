/**
 * vitest setup for product-shell tests: jest-dom matchers, RTL
 * auto-cleanup, and a desktop-default window.matchMedia stub (see
 * matchMedia.ts) so AppShell's useMediaQuery call has something to call
 * in jsdom. Cleanup is registered explicitly (not through vitest
 * globals, which the workspace does not enable) so a rendered tree never
 * leaks into the next test.
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
