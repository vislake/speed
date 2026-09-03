/**
 * vitest setup for auth-ui tests: jest-dom matchers and RTL auto-cleanup.
 *
 * Cleanup is registered explicitly (not through vitest globals, which the
 * workspace does not enable) so a rendered tree never leaks into the next
 * test.
 */
import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

afterEach(() => {
  cleanup()
})
