/**
 * Entry-point test: the runtime surface of @speed/tokens is exactly the two
 * symbols the README documents -- defaultTokens and deepMerge. Everything
 * else ships as types only, so no import site pays for unused runtime code.
 */

import { describe, expect, it } from 'vitest'
import * as tokens from './index'

describe('@speed/tokens entry', () => {
  it('exports exactly defaultTokens and deepMerge at runtime', () => {
    expect(Object.keys(tokens).sort()).toEqual(['deepMerge', 'defaultTokens'])
  })

  it('exports the default token tree', () => {
    expect(tokens.defaultTokens.color.semantic.primary.main).toBe('#2563EB')
    expect(typeof tokens.defaultTokens).toBe('object')
  })

  it('exports a function-valued deepMerge', () => {
    expect(typeof tokens.deepMerge).toBe('function')
  })
})
