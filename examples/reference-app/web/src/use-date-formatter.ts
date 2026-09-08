/**
 * use-date-formatter.ts -- the one date-formatting hook the app's data
 * surfaces share (the cases list and detail; the notes surface formats
 * inline in its own view, not through this hook). Created-at values
 * render through Intl in the surface language -- never hand-formatted
 * -- and an unparseable value renders as an empty string rather than
 * reaching Intl and throwing.
 */
import { useMemo } from 'react'

/** A date formatter for the given language, recreated when it changes. */
export function useDateFormatter(language: string): (value: string) => string {
  return useMemo(() => {
    const formatter = new Intl.DateTimeFormat(language, {
      dateStyle: 'medium',
      timeStyle: 'short',
    })
    return (value: string): string => {
      const date = new Date(value)
      return Number.isNaN(date.getTime()) ? '' : formatter.format(date)
    }
  }, [language])
}
