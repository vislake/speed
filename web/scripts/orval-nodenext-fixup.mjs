/**
 * One deterministic rewrite over the orval-generated api-sdk entry
 * file, run after every regeneration:
 *
 * orval emits every configured mutator import without a file extension
 * -- `import { speedRequest } from './runtime';` (and, for each
 * per-operation override, a second line such as `import {
 * speedRequestCredentialless } from './runtime';`) -- a specifier
 * TypeScript accepts under bundler resolution but rejects under
 * nodenext (TS2835), where this package's build (tsconfig.build.json)
 * runs. This script rewrites the specifier of every such import to the
 * explicit './runtime.js' form, keeping the committed generated file
 * identical to what CI regenerates: the api-contract.yml regen step
 * runs orval and then this script before `git diff --exit-code`, so a
 * future orval version that changes the emission fails loudly instead
 * of shipping an unbuildable package.
 *
 * Run from anywhere in the repo, after every orval run:
 *
 *   node web/scripts/orval-nodenext-fixup.mjs
 *
 * Exits non-zero when no './runtime' mutator import is found (orval
 * drift -- a future version that changes the emission shape, or a
 * mutator configuration change that leaves the generated file without
 * one), or when the seam file the rewritten specifier points at does
 * not exist.
 */
import { readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const indexPath = fileURLToPath(
  new URL('../packages/api-sdk/src/index.ts', import.meta.url),
)
const seamPath = fileURLToPath(
  new URL('../packages/api-sdk/src/runtime.ts', import.meta.url),
)

// One mutator import per line, braces between: orval emits a distinct
// line per mutator name, all from './runtime'. The specifier -- not the
// imported names -- is what nodenext rejects, so the rewrite replaces
// only the specifier and never needs to know which mutators exist.
const importPattern = /^import \{[^}]*\} from '\.\/runtime';$/
const specifier = "from './runtime';"
const explicitSpecifier = "from './runtime.js';"

const lines = readFileSync(indexPath, 'utf8').split('\n')
const matches = lines.filter((line) => importPattern.test(line))

if (matches.length === 0) {
  console.error(
    `[orval-nodenext-fixup] expected at least one './runtime' mutator import in ${indexPath}, found none. ` +
      'orval emission changed (version drift?) -- regenerate with the pinned orval and re-run, then commit the artifact.',
  )
  process.exit(1)
}

const rewritten = lines.map((line) =>
  importPattern.test(line)
    ? line.replace(specifier, explicitSpecifier)
    : line,
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
    `[orval-nodenext-fixup] rewrote ${matches.length} mutator import(s) to './runtime.js' in ${indexPath}`,
  )
} else {
  console.log(
    `[orval-nodenext-fixup] mutator imports already carry the .js extension in ${indexPath}`,
  )
}
