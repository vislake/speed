/**
 * The billing surface's Intl formatters, one locale-bound factory so a
 * component memoizes one object per language instead of constructing
 * Intl formatters per row and per field.
 *
 * Money formats through Intl.NumberFormat with the invoice's own
 * currency -- never by hand, since symbol placement and decimal digits
 * are locale- and currency-dependent (frontend standard). A currency
 * value that is not a three-letter code cannot reach Intl (which would
 * throw); the amount then renders as a plain two-decimal number with
 * the raw code appended, the same information without the formatting
 * Intl cannot perform. Dates format through Intl.DateTimeFormat; a
 * timestamp that does not parse renders as null and the caller omits
 * the field rather than showing a malformed string.
 *
 * Dates render in UTC -- the calendar the server issued the document
 * against (the model stores UTC instants) -- so an invoice issued on
 * July 31 stays July 31 for a viewer in any timezone: a billing
 * document's dates are facts of the record, not of the viewer's clock.
 * The period label collapses a billing cycle that lies within one
 * calendar month (in that same UTC calendar) to that month ("July
 * 2026") -- the label a monthly cycle reads as -- and falls back to
 * the full date range when the bounds cross a month boundary; the full
 * range formatter always shows both bounds, for the document detail.
 */

/** The calendar the server issues invoices against: the document dates
 * render here, never in the viewer's zone (see the header). */
const DOCUMENT_TIME_ZONE = 'UTC'

/**
 * A locale-bound set of the billing surface's formatters. All methods
 * are pure for a given locale; dates render as null when the input does
 * not parse, so no malformed server value can reach Intl and throw.
 */
export interface InvoiceFormatters {
  /** The amount in its currency ("¥1,234.00", "US$1,234.00"), or the
   * plain two-decimal amount plus the raw code when the currency is
   * not a three-letter code. */
  amount(cents: number, currency: string): string
  /** A medium date ("Jul 1, 2026"), or null when iso does not parse. */
  date(iso: string): string | null
  /** The billing cycle as one label: the month when both bounds lie in
   * one calendar month ("July 2026"), else the full range below. Null
   * when either bound does not parse. */
  periodLabel(startIso: string, endIso: string): string | null
  /** Both cycle bounds as medium dates joined by a dash ("Jul 1, 2026
   * – Jul 31, 2026"), never collapsed. Null when either bound does not
   * parse. */
  periodRange(startIso: string, endIso: string): string | null
}

const RANGE_SEPARATOR = ' – '

/** Parses a server timestamp; answers null for an absent or malformed
 * value so a bad field can never reach Intl and throw. */
function parseTimestamp(iso: string): Date | null {
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? null : date
}

/** Whether a currency value is a three-letter ISO 4217-style code. */
function isCurrencyCode(currency: string): boolean {
  return /^[A-Za-z]{3}$/.test(currency)
}

/** Creates the locale-bound formatter set described in the header. */
export function createInvoiceFormatters(locale: string): InvoiceFormatters {
  const mediumDate = new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeZone: DOCUMENT_TIME_ZONE,
  })
  const monthYear = new Intl.DateTimeFormat(locale, {
    year: 'numeric',
    month: 'long',
    timeZone: DOCUMENT_TIME_ZONE,
  })
  // One NumberFormat per currency seen: a list mixes currencies, and a
  // formatter is bound to exactly one.
  const moneyByCurrency = new Map<string, Intl.NumberFormat>()
  return {
    amount(cents: number, currency: string): string {
      if (!isCurrencyCode(currency)) {
        const plain = (cents / 100).toFixed(2)
        return currency === '' ? plain : `${plain} ${currency}`
      }
      const code = currency.toUpperCase()
      let money = moneyByCurrency.get(code)
      if (money === undefined) {
        money = new Intl.NumberFormat(locale, {
          style: 'currency',
          currency: code,
        })
        moneyByCurrency.set(code, money)
      }
      return money.format(cents / 100)
    },
    date(iso: string): string | null {
      const parsed = parseTimestamp(iso)
      return parsed === null ? null : mediumDate.format(parsed)
    },
    periodLabel(startIso: string, endIso: string): string | null {
      const start = parseTimestamp(startIso)
      const end = parseTimestamp(endIso)
      if (start === null || end === null) {
        return null
      }
      // The collapse is judged in the UTC calendar the labels render
      // in (see the header): two bounds the medium dates would show as
      // one month collapse to the month label.
      const sameMonth =
        start.getUTCFullYear() === end.getUTCFullYear() &&
        start.getUTCMonth() === end.getUTCMonth()
      if (sameMonth) {
        return monthYear.format(start)
      }
      return `${mediumDate.format(start)}${RANGE_SEPARATOR}${mediumDate.format(end)}`
    },
    periodRange(startIso: string, endIso: string): string | null {
      const start = parseTimestamp(startIso)
      const end = parseTimestamp(endIso)
      if (start === null || end === null) {
        return null
      }
      return `${mediumDate.format(start)}${RANGE_SEPARATOR}${mediumDate.format(end)}`
    },
  }
}
