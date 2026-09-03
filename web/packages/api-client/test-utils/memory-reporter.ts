/**
 * An in-memory Reporter for client tests: records every error/warn call
 * instead of writing to console, so tests can assert the constant
 * message and snake_case attributes the client reports (currently only
 * the access-token-refresh-failed warning).
 */

import type { Reporter } from '../src/index'

/** One recorded report: the constant message plus its attributes. */
export interface MemoryReport {
  readonly message: string
  readonly attrs: Readonly<Record<string, unknown>> | undefined
}

export interface MemoryReporter {
  /** Inject as ClientOptions.reporter. */
  readonly reporter: Reporter
  /** Every error() call, in order. */
  readonly errors: readonly MemoryReport[]
  /** Every warn() call, in order. */
  readonly warns: readonly MemoryReport[]
}

/** Builds a recording reporter around two arrays. */
export function createMemoryReporter(): MemoryReporter {
  const errors: MemoryReport[] = []
  const warns: MemoryReport[] = []
  return {
    reporter: {
      error(message: string, attrs?: Readonly<Record<string, unknown>>): void {
        errors.push({ message, attrs })
      },
      warn(message: string, attrs?: Readonly<Record<string, unknown>>): void {
        warns.push({ message, attrs })
      },
    },
    errors,
    warns,
  }
}
