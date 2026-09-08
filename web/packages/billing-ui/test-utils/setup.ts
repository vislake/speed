/**
 * Shared vitest setup for billing-ui, mirroring auth-ui and
 * account-ui: jest-dom matchers plus an explicit cleanup between tests.
 * React Testing Library's automatic cleanup only fires when the test
 * framework exposes afterEach globally (it checks globalThis), which
 * vitest deliberately does not, so the import must be explicit.
 */
import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

afterEach(() => {
  cleanup()
})
