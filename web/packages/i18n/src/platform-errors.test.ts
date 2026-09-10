/**
 * Platform error bundle tests: registration and its refusals, the
 * fallback-namespace wiring, and the two properties the bundle exists to
 * hold -- every bundled key is an apperr code the census contains, and
 * backend-only content (an invitation email, a notification template, an
 * SMS body) is unreachable as error copy. The census side is read from
 * the committed machine-readable index (docs/error-codes.json), the
 * generator's own twin, so the membership assertion runs on every web
 * PR rather than only in the docs pipeline.
 */

import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createInstance } from 'i18next'
import { describe, expect, it, vi } from 'vitest'
import { createI18n, type MissingKeyDetails } from './index'
import { PLATFORM_ERRORS_NAMESPACE, registerPlatformErrors } from './platform-errors'
import { MemoryStorage } from '../test-utils/memory-storage'
import { registerWelcome } from '../test-utils/welcome'
import bundleZh from './platform-errors/locales/zh-CN.json' with { type: 'json' }
import bundleEn from './platform-errors/locales/en-US.json' with { type: 'json' }

const REPO_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', '..')

type Bundle = { readonly errors: Record<string, string> }
const zhBundle = bundleZh as unknown as Bundle
const enBundle = bundleEn as unknown as Bundle

/** The bundle's text for a code; throws instead of asserting on undefined. */
function codeText(bundle: Bundle, code: string): string {
  const text = bundle.errors[code]
  if (text === undefined) {
    throw new Error(`the bundle has no entry for ${code}`)
  }
  return text
}

function createPlatformI18n(
  options: { readonly onMissingKey?: (details: MissingKeyDetails) => void } = {},
) {
  const instance = createI18n({
    storage: new MemoryStorage(),
    navigatorLanguages: [],
    onMissingKey: options.onMissingKey,
    fallbackNamespaces: [PLATFORM_ERRORS_NAMESPACE],
  })
  registerWelcome(instance)
  registerPlatformErrors(instance)
  return instance
}

describe('registerPlatformErrors', () => {
  it('registers the bundle and answers a package lookup through the fallback', () => {
    const instance = createPlatformI18n()
    const expected = codeText(enBundle, 'authn.invalid_credentials')
    // Direct namespace lookup, and the fallback path the host wires: the
    // welcome fixture carries no errors section.
    expect(instance.t(`platform-errors:errors.authn.invalid_credentials`)).toBe(expected)
    expect(instance.t('welcome:errors.authn.invalid_credentials')).toBe(expected)
  })

  it('resolves each language from its own bundle', () => {
    const instance = createPlatformI18n()
    const code = 'authn.invalid_credentials'
    expect(codeText(zhBundle, code)).not.toBe(codeText(enBundle, code))
    expect(instance.t(`platform-errors:errors.${code}`)).toBe(codeText(enBundle, code))
  })

  it('refuses a supported language the bundle does not ship, naming it', () => {
    const instance = createI18n({
      storage: new MemoryStorage(),
      navigatorLanguages: [],
      supportedLanguages: ['zh-CN', 'en-US', 'fr-FR'],
    })
    expect(() => registerPlatformErrors(instance)).toThrow(/fr-FR/)
  })

  it('refuses a bare i18next instance, naming createI18n', () => {
    expect(() => registerPlatformErrors(createInstance())).toThrow(/createI18n/)
  })

  it('refuses a second registration on the same instance', () => {
    const instance = createPlatformI18n()
    expect(() => registerPlatformErrors(instance)).toThrow(/already registered/)
  })
})

describe('the bundle contents', () => {
  it('carry the same key set in both languages', () => {
    expect(Object.keys(enBundle.errors).length).toBeGreaterThan(0)
    expect(Object.keys(zhBundle.errors).sort()).toEqual(
      Object.keys(enBundle.errors).sort(),
    )
  })

  it('never carries a backend content entry', () => {
    // The red line: these ids exist in the modules' catalogs but no Go
    // source constructs them, so there is no code for a client to look
    // them up with -- invitation email copy, a notification template, an
    // SMS body. A lookup misses visibly (the key renders as itself and
    // the missing-key handler fires) rather than resolving the content.
    const onMissingKey = vi.fn()
    const instance = createPlatformI18n({ onMissingKey })
    const contentIds = [
      'org.invitation.subject',
      'authn.sms.verification_code',
      'notification.contact.verify_code.email.subject',
    ]
    for (const id of contentIds) {
      expect(enBundle.errors).not.toHaveProperty(id)
      expect(zhBundle.errors).not.toHaveProperty(id)
      expect(instance.t(`welcome:errors.${id}`)).toBe(`errors.${id}`)
    }
    expect(onMissingKey).toHaveBeenCalledTimes(contentIds.length)
  })

  it('ships only codes the apperr census contains', () => {
    const census = JSON.parse(
      readFileSync(join(REPO_ROOT, 'docs', 'error-codes.json'), 'utf8'),
    ) as { rows: { code: string }[] }
    const censusCodes = new Set(census.rows.map((row) => row.code))
    for (const code of Object.keys(enBundle.errors)) {
      expect(censusCodes.has(code), code).toBe(true)
    }
  })

  it('carries i18next placeholders, with no Go template syntax left over', () => {
    for (const [code, text] of Object.entries(enBundle.errors)) {
      expect(text.length, code).toBeGreaterThan(0)
      expect(text, code).not.toContain('{{.')
      for (const placeholder of text.matchAll(/\{\{([^}]*)\}\}/g)) {
        expect(placeholder[1], code).toMatch(/^[A-Za-z_]\w*$/)
      }
    }
  })
})
