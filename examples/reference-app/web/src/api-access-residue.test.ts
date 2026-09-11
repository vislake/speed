/**
 * api-access-residue.test.ts -- the reference-app shell's API-access
 * residue pin.
 *
 * The shell reaches every platform module through the generated
 * @speed/api-sdk surface; the hand-written calls left on the app's
 * bound RequestFn are the host's own routes (no OpenAPI fragment
 * describes them, so no generated operation can answer them) plus the
 * share page's credential-less probe. This suite pins that residue as
 * a whitelist in both directions: a new file that calls the transport
 * fails with its own name, and a registered file that stops calling
 * fails too -- so the list cannot rot and no fourth site appears
 * unnoticed.
 *
 * Two legs, because a hand-written accessor shows up in two shapes:
 *
 *  - the CALL sites: a direct generic call on the context's `api`
 *    value (`api<T>(path)`), the app's own calling convention;
 *  - the type IMPORT sites: `import type { RequestFn }` from
 *    '@speed/api-client', the type a hand-written accessor's signature
 *    needs.
 *
 * The registered call sites:
 *
 *  - src/team-api.ts and src/tenant-name.ts -- the app's own routes
 *    (GET /api/reference-app/team-members and /clinic-name): host
 *    compositions with no module fragment behind them;
 *  - src/views/share-view.tsx -- the patient page's credential-less
 *    access probe, which cannot ride the generated mutator (it always
 *    attaches the session's token when one exists, and this route must
 *    answer a visitor who holds none or holds a dead one).
 *
 * The registered importers of the transport type are the two host-route
 * accessors plus src/app-services.tsx, which declares the context's
 * `api: RequestFn` -- the one place the transport value is held.
 *
 * The suites and the shared test harness under test-utils/ are skipped
 * (they bind and drive the transport on purpose). Also pinned: the
 * three deleted accessor modules (org-api, admin-api, share-api) stay
 * gone -- the names reappearing anywhere under the app source or its
 * e2e specs fails here, a stale reference to a deleted file being
 * residue of the same kind.
 *
 * The scan is text-level and enforces the convention it names (a call
 * on the value called `api`): a hand-written call through a renamed
 * reference would slip past this file, which is the residue code
 * review and the architecture discipline own -- the fetch/axios half
 * is the speed/no-direct-http rule's.
 */

import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

/** The app's src directory (this file sits at its root). */
const srcDir = dirname(fileURLToPath(import.meta.url))
/** The app root: src's parent, where the e2e specs live as a sibling. */
const appDir = join(srcDir, '..')

/** The registered call sites on the bound RequestFn. */
const REQUEST_CALL_SITES: readonly string[] = [
  'src/team-api.ts',
  'src/tenant-name.ts',
  'src/views/share-view.tsx',
]

/** The registered importers of the transport type: the two host-route
 * accessors (their signatures name it) and the module that declares
 * the services context holding it. */
const REQUEST_TYPE_IMPORT_SITES: readonly string[] = [
  'src/app-services.tsx',
  'src/team-api.ts',
  'src/tenant-name.ts',
]

/** The modules deleted when the surfaces moved to the generated SDK;
 * their names must not reappear anywhere in the app. */
const DELETED_ACCESSOR_NAMES = /org-api|admin-api|share-api/

/** This suite's own file, app-relative. It necessarily names the
 * deleted modules -- that is what it pins -- so its own text is the one
 * place those names are not residue. */
const SELF_FILE = relative(appDir, fileURLToPath(import.meta.url))

/** Every .ts/.tsx file under the given app-relative root, app-relative
 * itself. The suites and the shared harness are skipped by default;
 * pass skipTests false to sweep them too. */
function sourceFiles(
  root: string,
  { skipTests = true }: { readonly skipTests?: boolean } = {},
): string[] {
  const found: string[] = []
  const walk = (dir: string): void => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name)
      if (entry.isDirectory()) {
        if (skipTests && entry.name === 'test-utils') {
          continue
        }
        walk(path)
        continue
      }
      if (!entry.name.endsWith('.ts') && !entry.name.endsWith('.tsx')) {
        continue
      }
      if (
        skipTests &&
        (entry.name.includes('.test.') || entry.name.includes('.spec.'))
      ) {
        continue
      }
      found.push(relative(appDir, path))
    }
  }
  walk(join(appDir, root))
  return found
}

/** The files whose source matches the pattern. */
function filesMatching(
  files: readonly string[],
  pattern: RegExp,
): readonly string[] {
  return files.filter((file) =>
    pattern.test(readFileSync(join(appDir, file), 'utf8')),
  )
}

describe('the hand-written RequestFn residue', () => {
  it('calls the bound transport only from the registered sites', () => {
    const callSites = [...filesMatching(sourceFiles('src'), /\bapi\s*[(<]/)].sort()
    expect(
      callSites,
      'a direct call on the context api value (`api<T>(...)`) outside the registered sites -- route it through the generated @speed/api-sdk surface, or register the site (and its reason) here',
    ).toEqual([...REQUEST_CALL_SITES].sort())
  })

  it('imports the transport type only from the registered sites', () => {
    const importSites = [
      ...filesMatching(sourceFiles('src'), /\bimport\b[^\n]*\bRequestFn\b/),
    ].sort()
    expect(
      importSites,
      'an import of the RequestFn type outside the registered sites -- a hand-written accessor signature is residue; the surfaces ride the generated SDK',
    ).toEqual([...REQUEST_TYPE_IMPORT_SITES].sort())
  })

  it('keeps the deleted accessor modules out of the app and its e2e specs', () => {
    const stale = filesMatching(
      [
        ...sourceFiles('src', { skipTests: false }),
        ...sourceFiles('e2e', { skipTests: false }),
      ].filter((file) => file !== SELF_FILE),
      DELETED_ACCESSOR_NAMES,
    )
    expect(
      [...stale],
      'a reference to a deleted hand-written accessor module (org-api, admin-api, share-api) -- the surfaces ride the generated SDK, and comments state the current shape',
    ).toEqual([])
  })
})
