/**
 * Cross-layer pin: the web's supported-language set equals the language
 * catalogs the platform's Go modules ship.
 *
 * An account language preference is stored by the backend and drives
 * backend-rendered content (verification messages, notification emails,
 * invitations) in the chosen language; a locale no deployed catalog
 * carries is refused with a readable failure code (authn.invalid_locale).
 * The set the web offers and the catalog set the backend can render are
 * therefore one set, and this suite asserts that equality module by
 * module: a language added to or dropped from either side fails here,
 * naming both sides, instead of surfacing as a preference the server
 * refuses or content it cannot render.
 *
 * Both sides are read from the tree rather than declared here: the web
 * side is the exported DEFAULT_SUPPORTED_LANGUAGES (languages.ts), and a
 * module is any immediate go/ subdirectory carrying a locales/ directory,
 * one catalog file per language it speaks -- so a module that starts
 * shipping catalogs, or a module that adds a language, is covered without
 * editing this file. Fixture catalogs nested under a module's internal/
 * directories are not part of the shipped set and stay outside the sweep.
 *
 * The set is a build-time constant today; distributing the deployment's
 * optional languages through public configuration is the future
 * convergence path for it, and until that lands this pin is what keeps
 * the two layers moving together.
 */

import { existsSync, readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { DEFAULT_SUPPORTED_LANGUAGES } from './languages'

const REPO_ROOT = join(
  dirname(fileURLToPath(import.meta.url)),
  '..',
  '..',
  '..',
  '..',
)
const GO_ROOT = join(REPO_ROOT, 'go')

/** The locale ids a module's locales directory ships, from its file names. */
function shippedLocaleIds(moduleName: string): string[] {
  return readdirSync(join(GO_ROOT, moduleName, 'locales'))
    .filter((name) => name.endsWith('.toml'))
    .map((name) => name.slice(0, -'.toml'.length))
}

describe('the web supported-language set against the Go module catalogs', () => {
  const catalogModules = readdirSync(GO_ROOT, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name)
    .filter((name) => existsSync(join(GO_ROOT, name, 'locales')))
    .sort()

  it('discovers the modules that ship locale catalogs', () => {
    // A sweep that discovered nothing would pass vacuously.
    expect(catalogModules.length).toBeGreaterThan(0)
  })

  it.each(catalogModules)(
    'go/%s/locales ships exactly the web supported set',
    (moduleName) => {
      const shipped = shippedLocaleIds(moduleName)
      expect(
        [...shipped].sort(),
        `go/${moduleName}/locales ships [${shipped.join(', ')}] but ` +
          `DEFAULT_SUPPORTED_LANGUAGES ` +
          `(web/packages/i18n/src/languages.ts) is ` +
          `[${DEFAULT_SUPPORTED_LANGUAGES.join(', ')}]`,
      ).toEqual([...DEFAULT_SUPPORTED_LANGUAGES].sort())
    },
  )
})
