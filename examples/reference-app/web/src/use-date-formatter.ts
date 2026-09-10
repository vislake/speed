/**
 * use-date-formatter.ts -- the one date-formatting hook the app's data
 * surfaces share (the cases list and detail; the notes surface formats
 * inline in its own view, not through this hook). Created-at values
 * render through Intl in the surface language AND the viewer's display
 * timezone -- never hand-formatted, and never in Intl's implicit
 * process-local zone (usePreferredTimeZone resolves profile -> device ->
 * UTC and every formatter here passes the result explicitly, so one
 * account renders the same times on every machine). An unparseable
 * value renders as an empty string rather than reaching Intl and
 * throwing.
 */
import { useMemo } from 'react'
import { usePreferredTimeZone } from './use-preferred-time-zone.js'

/** A date formatter for the given language, recreated when it or the
 * display timezone changes. */
export function useDateFormatter(language: string): (value: string) => string {
  const timeZone = usePreferredTimeZone()
  return useMemo(() => {
    const formatter = new Intl.DateTimeFormat(language, {
      dateStyle: 'medium',
      timeStyle: 'short',
      timeZone,
    })
    return (value: string): string => {
      const date = new Date(value)
      return Number.isNaN(date.getTime()) ? '' : formatter.format(date)
    }
  }, [language, timeZone])
}
