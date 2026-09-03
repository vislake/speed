/**
 * One deterministic rewrite over the orval-generated api-sdk entry
 * file, run after every regeneration:
 *
 * orval emits the configured mutator import without a file extension --
 * `import { speedRequest } from './runtime';` -- a specifier TypeScript
 * accepts under bundler resolution but rejects under nodenext
 * (TS2835), where this package's build (tsconfig.build.json) runs.
 * This script rewrites that single import to the explicit
 * './runtime.js' form, keeping the committed generated file identical
 * to what CI regenerates: the api-contract.yml regen step runs orval
 * and then this script before `git diff --exit-code`, so a future orval
 * version that changes the emission fails loudly instead of shipping an
 * unbuildable package.
 *
 * Run from anywhere in the repo, after every orval run:
 *
 *   node web/scripts/orval-nodenext-fixup.mjs
 *
 * Exits non-zero when the expected single import is absent or
 * ambiguous (orval drift), or when the seam file the rewritten
 * specifier points at does not exist.
 */
import { readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const indexPath = fileURLToPath(
  new URL('../packages/api-sdk/src/index.ts', import.meta.url),
)
const seamPath = fileURLToPath(
  new URL('../packages/api-sdk/src/runtime.ts', import.meta.url),
)

const importPattern =
  /^import \{[^}]*\bspeedRequest\b[^}]*\} from '([^']+)';$/

const lines = readFileSync(indexPath, 'utf8').split('\n')
const matches = lines.filter((line) => importPattern.test(line))

if (matches.length !== 1) {
  console.error(
    `[orval-nodenext-fixup] expected exactly one speedRequest import in ${indexPath}, found ${matches.length}. ` +
      'orval emission changed (version drift?) -- regenerate with the pinned orval and re-run, then commit the artifact.',
  )
  process.exit(1)
}

const rewritten = lines.map((line) =>
  importPattern.test(line) ? line.replace(importPattern, "import { speedRequest } from './runtime.js';") : line,
)
const result = rewritten.join('\n')

let seamOk = true
try {
  readFileSync(seamPath, 'utf8')
} catch {
  seamOk = false
}
if (!seamOk) {
  console.error(
    `[orval-nodenext-fixup] the rewritten import points at ${seamPath}, which does not exist.`,
  )
  process.exit(1)
}

if (result !== lines.join('\n')) {
  writeFileSync(indexPath, result)
  console.log(
    `[orval-nodenext-fixup] rewrote the mutator import to './runtime.js' in ${indexPath}`,
  )
} else {
  console.log(
    `[orval-nodenext-fixup] mutator import already carries the .js extension in ${indexPath}`,
  )
}
