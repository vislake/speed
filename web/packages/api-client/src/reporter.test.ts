/**
 * Contract tests for the reporter seam: createConsoleReporter delegates
 * to console.error/console.warn with the constant message plus the
 * attribute object (undefined when the caller omitted it). The console
 * sink itself is the documented STOPGAP until the M1 round wires the
 * real app-shell diagnostics pipeline.
 */

import { afterEach, describe, expect, it, vi } from 'vitest'
import { createConsoleReporter } from './index'

describe('createConsoleReporter', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('forwards error() to console.error with message and attrs', () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const reporter = createConsoleReporter()
    reporter.error('request failed', { code: 'client.network' })
    expect(errorSpy).toHaveBeenCalledWith('request failed', {
      code: 'client.network',
    })
  })

  it('forwards warn() to console.warn with message and attrs', () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const reporter = createConsoleReporter()
    reporter.warn('access token refresh failed', { status: 401 })
    expect(warnSpy).toHaveBeenCalledWith('access token refresh failed', {
      status: 401,
    })
  })

  it('passes the attribute-less call through unchanged', () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const reporter = createConsoleReporter()
    reporter.warn('access token refresh failed')
    expect(warnSpy).toHaveBeenCalledWith('access token refresh failed', undefined)
  })

  it('never writes anywhere but the two console channels', () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const logSpy = vi.spyOn(console, 'log').mockImplementation(() => {})
    const reporter = createConsoleReporter()
    reporter.error('boom', { code: 'client.network' })
    reporter.warn('note', { code: 'client.timeout' })
    expect(errorSpy).toHaveBeenCalledTimes(1)
    expect(warnSpy).toHaveBeenCalledTimes(1)
    expect(logSpy).not.toHaveBeenCalled()
  })
})
