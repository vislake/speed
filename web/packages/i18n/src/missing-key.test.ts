/**
 * Contract tests for the missing-key handlers: the visible default warning
 * and the factory that adapts the host's handler to i18next's signature.
 */

import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  defaultMissingKeyHandler,
  missingKeyHandlerFactory,
  type MissingKeyDetails,
} from './missing-key'

afterEach(() => {
  vi.restoreAllMocks()
})

const details: MissingKeyDetails = {
  languages: ['en-US'],
  namespace: 'welcome',
  key: 'greeting.hello',
}

describe('defaultMissingKeyHandler', () => {
  it('warns visibly with the key, namespace and languages in the message', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    defaultMissingKeyHandler(details)
    expect(warn).toHaveBeenCalledTimes(1)
    const message = warn.mock.calls[0]![0] as string
    expect(message).toContain('[speed-i18n]')
    expect(message).toContain('greeting.hello')
    expect(message).toContain('welcome')
    expect(message).toContain('en-US')
  })
})

describe('missingKeyHandlerFactory', () => {
  it('adapts the default handler when the host provides none', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const handler = missingKeyHandlerFactory()
    handler(details.languages, details.namespace, details.key)
    expect(warn).toHaveBeenCalledTimes(1)
  })

  it('forwards structured details to the host handler instead of warning', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const onMissingKey = vi.fn()
    const handler = missingKeyHandlerFactory(onMissingKey)
    handler(details.languages, details.namespace, details.key)
    expect(onMissingKey).toHaveBeenCalledTimes(1)
    expect(onMissingKey).toHaveBeenCalledWith(details)
    expect(warn).not.toHaveBeenCalled()
  })
})
