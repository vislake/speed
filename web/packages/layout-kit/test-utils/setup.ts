/**
 * vitest setup for layout-kit tests: the workspace's shared setup side
 * effect (see @speed/test-utils/setup) plus a desktop-true
 * window.matchMedia default before each test, so AppShell's
 * useMediaQuery call has something to call in jsdom and suites that do
 * not care about the split need not stub it themselves (see
 * @speed/test-utils/matchMedia).
 */
import '@speed/test-utils/setup'
import { mockMatchMedia } from '@speed/test-utils/matchMedia'
import { beforeEach } from 'vitest'

beforeEach(() => {
  mockMatchMedia(true)
})
