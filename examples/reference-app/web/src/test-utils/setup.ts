/**
 * Shared vitest setup for reference-app-web tests.
 *
 * React Testing Library's auto-cleanup only fires when the test framework
 * exposes a global afterEach (Jest, mocha); vitest does not, so tests that
 * mount trees would leak DOM between tests without this file. The
 * jsdom-environment tests render with the shared provider stack of
 * src/test-utils/render.tsx; this setup registers the jest-dom matchers
 * every suite asserts with and cleans up after each test.
 */

import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

afterEach(() => {
  cleanup()
})
