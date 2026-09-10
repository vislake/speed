/**
 * The package manifest's peer-declaration contract, pinned by the suite.
 *
 * The workspace convention (ui-kit, auth-ui, layout-kit, the reference
 * app) is that every package whose dependency surface statically pulls
 * react-hook-form into a host's build re-declares it as a peer at the
 * shared ^7.0.0 range, so the requirement surfaces at every level of the
 * dependency chain instead of only at the package that imports it: a
 * host that installs @speed/product-shell renders the auth-ui family the
 * shell's sign-in slot pairs with (auth-ui's barrel statically imports
 * useForm), so react-hook-form must be a declared peer of this package
 * too. The devDependency pins the exact workspace version so the
 * package's own suite resolves the peer against a real install.
 *
 * The manifest is read from disk, never imported as a module: vite
 * refuses to resolve a package's own package.json through the module
 * graph (the locale bundles beside it import fine as JSON; the manifest
 * is special-cased). Path derivation follows api-client's README-drift
 * test: fileURLToPath(import.meta.url) plus node:path joins -- the
 * jsdom environment's global URL constructor is not Node's, so a
 * relative URL resolution against the file: base is not reliable.
 */

import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const manifestPath = join(dirname(fileURLToPath(import.meta.url)), '..', 'package.json')
const manifest = JSON.parse(readFileSync(manifestPath, 'utf8')) as {
  readonly exports?: Readonly<Record<string, unknown>>
  readonly dependencies?: Readonly<Record<string, string>>
  readonly peerDependencies?: Readonly<Record<string, string>>
  readonly devDependencies?: Readonly<Record<string, string>>
}

describe('package manifest', () => {
  it('declares react-hook-form as a peer at the workspace-wide range', () => {
    expect(manifest.peerDependencies?.['react-hook-form']).toBe('^7.0.0')
  })

  it('satisfies that peer from its own devDependencies at the workspace-pinned version', () => {
    expect(manifest.devDependencies?.['react-hook-form']).toBe('7.87.0')
  })

  it('exports the bootstrap entry as its own subpath, built from the same src', () => {
    // The app-entry assembly ships as the "./bootstrap" subpath (the
    // api-sdk "./runtime" pattern) so the main entry's dependency
    // floor stays what the docs promise; the subpath must point at the
    // dist pair the package build emits from src/bootstrap.tsx.
    expect(manifest.exports?.['./bootstrap']).toEqual({
      types: './dist/bootstrap.d.ts',
      import: './dist/bootstrap.js',
    })
  })

  it('declares the packages the bootstrap entry imports, and the query client as a host-supplied peer', () => {
    // The bootstrap entry imports @speed/api-client (store + client),
    // @speed/api-sdk (the runtime seam) and @speed/ui-kit (the theme
    // provider) as regular dependencies -- all three already in the
    // main entry's transitive closure -- and react-query as a peer:
    // the host supplies the query library, exactly as it does for
    // @speed/api-sdk.
    expect(manifest.dependencies?.['@speed/api-client']).toBe('workspace:*')
    expect(manifest.dependencies?.['@speed/api-sdk']).toBe('workspace:*')
    expect(manifest.dependencies?.['@speed/ui-kit']).toBe('workspace:*')
    expect(manifest.peerDependencies?.['@tanstack/react-query']).toBe('^5.0.0')
    expect(manifest.devDependencies?.['@tanstack/react-query']).toBe('^5.62.0')
  })
})
