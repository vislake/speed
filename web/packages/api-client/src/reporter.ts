/**
 * The structured-reporting seam, mirroring the backend's structured
 * logging discipline (docs/internal/09-observability.md): a constant
 * English message plus a snake_case attribute object, never a
 * concatenated sentence.
 *
 * STOPGAP: the default sink writes to console.error/console.warn. That
 * is a placeholder until the M1 round wires the app-shell diagnostics
 * pipeline (the browser has no structured log backend; api-client
 * reports must reach whatever sink hosts install). Hosts replace the
 * default through ClientOptions.reporter; production-relevant signals
 * belong here, never in console.log calls sprinkled through app code.
 */

/** A structured diagnostic sink. */
export interface Reporter {
  /** Report a failure: constant message, snake_case attributes. */
  error(message: string, attrs?: Readonly<Record<string, unknown>>): void
  /** Report something worth noting: constant message, snake_case attrs. */
  warn(message: string, attrs?: Readonly<Record<string, unknown>>): void
}

/**
 * The console-backed Reporter used when ClientOptions.reporter is
 * omitted. Deliberately thin: the M1 diagnostics round replaces the
 * sink, and every api-client user already passes their own Reporter
 * through the client rather than importing this.
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
