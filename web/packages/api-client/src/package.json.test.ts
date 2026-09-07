/**
 * The package manifest's peer-declaration contract, pinned by the
 * suite.
 *
 * `react` is a peer of the `./react` subpath (`src/react.ts`) only --
 * the main entry (client, errors, retry, reporter, the token store and
 * the config fetchers) never imports react. npm peer declarations are
 * package-level, never per-subpath, so the declaration alone would
 * force react onto every consumer, including one that only uses the
 * main entry; the optional marker (`peerDependenciesMeta.react.optional`,
 * the same mechanism `@speed/i18n` uses for `@mui/material`) is what
 * makes a react-less install resolve: the main entry's React-free
 * claim is a runtime fact the manifest must not contradict. The
 * package's own suites satisfy the peer from devDependencies.
 *
 * The manifest is read from disk, never imported as a module (the
 * product-shell precedent): path derivation via `fileURLToPath` plus
 * `node:path` joins, so the test works in any environment.
 */

import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const manifestPath = join(
  dirname(fileURLToPath(import.meta.url)),
  '..',
  'package.json',
)
const manifest = JSON.parse(readFileSync(manifestPath, 'utf8')) as {
  readonly peerDependencies?: Readonly<Record<string, string>>
  readonly peerDependenciesMeta?: Readonly<
    Record<string, Readonly<{ optional?: boolean }>>
  >
}

describe('package manifest', () => {
  it('declares react as a peer at the workspace-wide range', () => {
    expect(manifest.peerDependencies?.react).toBe('^18.0.0 || ^19.0.0')
  })

  it('marks that peer optional, so a main-entry-only consumer installs without react', () => {
    expect(manifest.peerDependenciesMeta?.react?.optional).toBe(true)
  })
})
