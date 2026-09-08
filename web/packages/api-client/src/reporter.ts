/**
 * The structured-reporting seam, the frontend mirror of the backend's
 * structured logging discipline: a constant English message plus a
 * snake_case attribute object, never a concatenated sentence.
 *
 * The default sink writes to console.error/console.warn: the browser
 * has no structured log backend, so api-client reports must reach
 * whatever sink hosts install. Hosts replace the default through
 * ClientOptions.reporter; production-relevant signals belong here,
 * never in console.log calls sprinkled through app code.
 */

/**
 * A structured diagnostic sink with two channels. `error` is for
 * failures that are terminal for the operation being reported; `warn`
 * for problems the client worked around. The client itself calls only
 * `warn` -- its one internal diagnostic is the
 * access-token-refresh-failed warning, while terminal failures reject
 * as an ApiError to the caller, who owns reporting them. `error` is
 * therefore a host-facing channel of the sink shape rather than a
 * package call site: hosts' own diagnostics pipelines and
 * fatal-invariant reports use it, and a Reporter implementation
 * without it would not match the full shape those hosts implement.
 */
export interface Reporter {
  /** Report a failure: constant message, snake_case attributes. */
  error(message: string, attrs?: Readonly<Record<string, unknown>>): void
  /** Report something worth noting: constant message, snake_case attrs. */
  warn(message: string, attrs?: Readonly<Record<string, unknown>>): void
}

/**
 * The console-backed Reporter used when ClientOptions.reporter is
 * omitted. Deliberately thin: hosts with a real diagnostics pipeline
 * pass their own Reporter through the client rather than importing
 * this.
 */
export function createConsoleReporter(): Reporter {
  return {
    error(message: string, attrs?: Readonly<Record<string, unknown>>): void {
      console.error(message, attrs)
    },
    warn(message: string, attrs?: Readonly<Record<string, unknown>>): void {
      console.warn(message, attrs)
    },
  }
}
