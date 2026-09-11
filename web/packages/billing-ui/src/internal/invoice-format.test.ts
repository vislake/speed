/**
 * The invoice formatters' behaviour, pinned through the locale-bound
 * factory in both languages.
 *
 * The happy-path assertions recompute the expected strings through the
 * same Intl constructors the factory uses, so an ICU update that
 * rewords a locale's output can never fail the suite -- the assertions
 * pin the factory's own choices: amounts go through Intl currency
 * formatting in the invoice's currency (never hand-formatted), dates
 * through Intl medium dates, and the period label collapses exactly
 * when both bounds lie in one calendar month. The guard assertions pin
 * what the factory does with values Intl cannot format: a currency
 * that is not a three-letter code renders a plain two-decimal amount
 * with the raw code appended, and a timestamp that does not parse
 * yields null so no malformed server value can ever reach Intl and
 * throw.
 */

import { describe, expect, it } from 'vitest'
import {
  createInvoiceFormatters,
  invoiceFormatters,
} from './invoice-format.js'

// The factory renders document dates in the UTC calendar (see
// invoice-format.ts's header), so the expected strings below recompute
// through UTC-anchored Intl constructors -- the assertions then hold in
// whatever timezone the runner sits in, midnight bounds included.
const JULY_START = '2026-07-01T00:00:00Z'
const JULY_END = '2026-07-31T23:59:59Z'
const JUNE_START = '2026-06-01T00:00:00Z'
const AUGUST_START = '2026-08-01T00:00:00Z'
const NOT_A_TIMESTAMP = 'not-a-timestamp'

function zhMoney(cents: number, currency: string): string {
  return new Intl.NumberFormat('zh-CN', {
    style: 'currency',
    currency,
  }).format(cents / 100)
}

function enMoney(cents: number, currency: string): string {
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency,
  }).format(cents / 100)
}

function zhDate(iso: string): string {
  return new Intl.DateTimeFormat('zh-CN', {
    dateStyle: 'medium',
    timeZone: 'UTC',
  }).format(new Date(iso))
}

function enDate(iso: string): string {
  return new Intl.DateTimeFormat('en-US', {
    dateStyle: 'medium',
    timeZone: 'UTC',
  }).format(new Date(iso))
}

describe('amount', () => {
  it('format through Intl currency in the invoice currency, both languages', () => {
    const zh = createInvoiceFormatters('zh-CN')
    const en = createInvoiceFormatters('en-US')
    expect(zh.amount(12000, 'CNY')).toBe(zhMoney(12000, 'CNY'))
    expect(zh.amount(9800, 'USD')).toBe(zhMoney(9800, 'USD'))
    expect(en.amount(9800, 'USD')).toBe(enMoney(9800, 'USD'))
    expect(en.amount(12000, 'CNY')).toBe(enMoney(12000, 'CNY'))
  })

  it('divide cents by one hundred before formatting', () => {
    const zh = createInvoiceFormatters('zh-CN')
    expect(zh.amount(12000, 'CNY')).toContain('120')
    expect(zh.amount(1, 'CNY')).toContain('0.01')
  })

  it('fall back to a plain two-decimal amount with the raw code for a currency Intl cannot format', () => {
    const zh = createInvoiceFormatters('zh-CN')
    // Not three letters (Intl would throw a RangeError), and a
    // lowercase three-letter code that is not what the model stores.
    expect(zh.amount(12000, '')).toBe('120.00')
    expect(zh.amount(12000, 'CN¥')).toBe('120.00 CN¥')
    expect(zh.amount(-500, 'CNY123')).toBe('-5.00 CNY123')
  })
})

describe('date', () => {
  it('format a parseable timestamp as a medium date in the current language', () => {
    const zh = createInvoiceFormatters('zh-CN')
    const en = createInvoiceFormatters('en-US')
    expect(zh.date(JULY_START)).toBe(zhDate(JULY_START))
    expect(en.date(JULY_START)).toBe(enDate(JULY_START))
  })

  it('answer null for a timestamp that does not parse', () => {
    const zh = createInvoiceFormatters('zh-CN')
    expect(zh.date(NOT_A_TIMESTAMP)).toBeNull()
    expect(zh.date('')).toBeNull()
  })
})

describe('periodLabel', () => {
  it('collapse a cycle whose bounds lie in one calendar month to that month', () => {
    const zh = createInvoiceFormatters('zh-CN')
    const en = createInvoiceFormatters('en-US')
    const zhMonth = new Intl.DateTimeFormat('zh-CN', {
      year: 'numeric',
      month: 'long',
      timeZone: 'UTC',
    }).format(new Date(JULY_START))
    const enMonth = new Intl.DateTimeFormat('en-US', {
      year: 'numeric',
      month: 'long',
      timeZone: 'UTC',
    }).format(new Date(JULY_START))
    expect(zh.periodLabel(JULY_START, JULY_END)).toBe(zhMonth)
    expect(en.periodLabel(JULY_START, JULY_END)).toBe(enMonth)
  })

  it('render the full date range when the bounds cross a month boundary', () => {
    const zh = createInvoiceFormatters('zh-CN')
    expect(zh.periodLabel(JUNE_START, AUGUST_START)).toBe(
      `${zhDate(JUNE_START)} – ${zhDate(AUGUST_START)}`,
    )
  })

  it('answer null when either bound does not parse', () => {
    const zh = createInvoiceFormatters('zh-CN')
    expect(zh.periodLabel(NOT_A_TIMESTAMP, JULY_END)).toBeNull()
    expect(zh.periodLabel(JULY_START, NOT_A_TIMESTAMP)).toBeNull()
  })
})

describe('periodRange', () => {
  it('always render both bounds as medium dates, never collapsed', () => {
    const zh = createInvoiceFormatters('zh-CN')
    expect(zh.periodRange(JULY_START, JULY_END)).toBe(
      `${zhDate(JULY_START)} – ${zhDate(JULY_END)}`,
    )
  })

  it('answer null when either bound does not parse', () => {
    const zh = createInvoiceFormatters('zh-CN')
    expect(zh.periodRange(JULY_START, NOT_A_TIMESTAMP)).toBeNull()
  })
})

describe('invoiceFormatters', () => {
  it('hand out one set per locale, the same instance on every call', () => {
    expect(invoiceFormatters('zh-CN')).toBe(invoiceFormatters('zh-CN'))
    expect(invoiceFormatters('en-US')).toBe(invoiceFormatters('en-US'))
    expect(invoiceFormatters('zh-CN')).not.toBe(invoiceFormatters('en-US'))
  })

  it('hand out a set whose formatters agree with a fresh construction', () => {
    const cached = invoiceFormatters('zh-CN')
    const fresh = createInvoiceFormatters('zh-CN')
    expect(cached.amount(123456, 'CNY')).toBe(fresh.amount(123456, 'CNY'))
    expect(cached.date(JULY_START)).toBe(fresh.date(JULY_START))
    expect(cached.periodLabel(JULY_START, JULY_END)).toBe(
      fresh.periodLabel(JULY_START, JULY_END),
    )
  })
})
