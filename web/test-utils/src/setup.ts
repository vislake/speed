/**
 * The vitest setup side effect every DOM suite in the workspace shares:
 * jest-dom matchers plus an explicit cleanup between tests. React
 * Testing Library's automatic cleanup only fires when the test framework
 * exposes afterEach globally (it checks globalThis), which vitest
 * deliberately does not, so the import must be explicit -- otherwise a
 * rendered tree leaks into the next test.
 *
 * A package's own test-utils/setup.ts (the file its vitest config
 * points setupFiles at) imports this module and adds whatever else its
 * environment needs -- the desktop-default matchMedia stub, say (see
 * matchMedia.ts).
 */
import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

afterEach(() => {
  cleanup()
})
