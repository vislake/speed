/**
 * useHashRoute.test.tsx -- the behaviour of the micro hash-navigation
 * hook: it reads the fragment at mount, follows every hashchange event
 * while mounted, and stops listening after unmount (a hash change that
 * lands after unmount must not reach the unmounted component).
 *
 * The tests drive window.location.hash assignments inside act() and
 * dispatch the HashChangeEvent the browser would fire itself; where the
 * assignment's own navigation also fires the event, the listener simply
 * runs twice with the same value, which the hook absorbs.
 */

import { act, renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { useHashRoute } from './useHashRoute.js'

describe('useHashRoute', () => {
  it('reads the fragment at mount, without its leading hash', () => {
    act(() => {
      window.location.hash = '#/notes'
    })
    const { result } = renderHook(() => useHashRoute())
    expect(result.current).toBe('/notes')
  })

  it('reports the bare page as an empty route', () => {
    act(() => {
      window.location.hash = ''
    })
    const { result } = renderHook(() => useHashRoute())
    expect(result.current).toBe('')
  })

  it('follows hashchange events while mounted', () => {
    act(() => {
      window.location.hash = '#/start'
    })
    const { result } = renderHook(() => useHashRoute())

    act(() => {
      window.location.hash = '#/notes'
      window.dispatchEvent(new HashChangeEvent('hashchange'))
    })
    expect(result.current).toBe('/notes')

    act(() => {
      window.location.hash = '#/notes/42'
      window.dispatchEvent(new HashChangeEvent('hashchange'))
    })
    expect(result.current).toBe('/notes/42')
  })

  it('stops listening after unmount', () => {
    act(() => {
      window.location.hash = '#/before'
    })
    const { result, unmount } = renderHook(() => useHashRoute())

    unmount()
    act(() => {
      window.location.hash = '#/after'
      window.dispatchEvent(new HashChangeEvent('hashchange'))
    })
    expect(result.current).toBe('/before')
  })
})
