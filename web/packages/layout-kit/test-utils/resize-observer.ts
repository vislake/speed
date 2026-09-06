/**
 * ResizeObserver stub for tests that exercise AppShell's measured-header
 * mechanism.
 *
 * jsdom implements no ResizeObserver (and the browsers AppShell runs in
 * do, so the guard in AppShell's own effect skips the mechanism only
 * where the API is genuinely absent). The stub installs a constructor
 * that records the observed element and the callback, so a test can
 * report a header height exactly the way a real observer entry would and
 * assert how the spacers derived from it react. Install with
 * `vi.unstubAllGlobals()` (or the test file's afterEach) so the stub
 * never leaks into tests that want the no-ResizeObserver path.
 */

import { act } from '@testing-library/react'
import { vi } from 'vitest'

export interface ResizeObserverHandle {
  /** The element the stub has been told to observe, if any. */
  observedElement(): Element | null
  /** Report a new content-box height, as a real observer entry would. */
  emitHeight(height: number): void
}

export function stubResizeObserver(): ResizeObserverHandle {
  let callback: ((entries: ResizeObserverEntry[]) => void) | null = null
  let observed: Element | null = null

  class StubResizeObserver {
    constructor(cb: (entries: ResizeObserverEntry[]) => void) {
      callback = cb
    }

    observe(element: Element): void {
      observed = element
    }

    unobserve(): void {}

    disconnect(): void {
      observed = null
    }
  }

  vi.stubGlobal('ResizeObserver', StubResizeObserver)

  return {
    observedElement: () => observed,
    emitHeight(height: number): void {
      act(() => {
        if (observed === null || callback === null) {
          throw new Error('stubResizeObserver.emitHeight: nothing observed yet')
        }
        callback([
          {
            target: observed,
            contentRect: { height },
          } as unknown as ResizeObserverEntry,
        ])
      })
    },
  }
}
