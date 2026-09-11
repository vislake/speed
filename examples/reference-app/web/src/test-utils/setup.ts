/**
 * Shared vitest setup for reference-app-web tests.
 *
 * The workspace's shared setup side effect (see @speed/test-utils/setup)
 * -- the jest-dom matchers every suite asserts with and the explicit RTL
 * cleanup between tests vitest needs (it exposes no global afterEach, so
 * suites that mount trees would otherwise leak DOM) -- plus the desktop
 * -true window.matchMedia default (see @speed/test-utils/matchMedia), so
 * AppShell's useMediaQuery call -- the frame's desktop/mobile drawer
 * split -- has something to call in jsdom and suites that render the
 * frame need not stub it themselves.
 */

import '@speed/test-utils/setup'
import { mockMatchMedia } from '@speed/test-utils/matchMedia'
import { beforeEach } from 'vitest'

beforeEach(() => {
  mockMatchMedia(true)
})
