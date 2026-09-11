/**
 * The visually-hidden recipe's shape, pinned: every hidden element in
 * the kit and in the consuming packages takes exactly this property
 * set, so each property a hidden element needs is decided here once --
 * clipping that keeps the element in the accessibility tree (never
 * display:none), the 1x1 out-of-flow box, and the margin reset.
 */

import { describe, expect, it } from 'vitest'
import { visuallyHiddenSx } from './visually-hidden.js'

describe('visuallyHiddenSx', () => {
  it('clips without removing the element from the accessibility tree', () => {
    expect(visuallyHiddenSx).toEqual({
      clip: 'rect(0 0 0 0)',
      clipPath: 'inset(50%)',
      height: 1,
      width: 1,
      overflow: 'hidden',
      position: 'absolute',
      bottom: 0,
      left: 0,
      whiteSpace: 'nowrap',
      margin: 0,
    })
  })

  it('never hides through display or visibility', () => {
    expect(visuallyHiddenSx).not.toHaveProperty('display')
    expect(visuallyHiddenSx).not.toHaveProperty('visibility')
  })
})
