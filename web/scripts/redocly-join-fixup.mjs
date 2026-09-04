/**
 * One deterministic rewrite over the redocly-joined merged document
 * (build/openapi/speed.yaml), run after every merge:
 *
 * redocly's `join` keeps the FIRST input document's `info` block, so
 * the merged application-wide document inherits the notes fragment's
 * identity: title "notes" and a description whose closing sentence
 * ("Merging this fragment into an application-wide
 * build/openapi/speed.yaml stays future work ...") contradicts the
 * merged document it now lives inside. redocly join has no CLI way to
 * override info, so this script stamps the block deterministically
 * after every join -- an application-level title and description,
 * keeping the joined version line. The stamp is load-bearing beyond
 * the document itself: orval's DO-NOT-EDIT header in the generated
 * @speed/api-sdk entry stamps `// Source: <info.title>`, which would
 * otherwise read "notes" for the whole application surface.
 *
 * Run from anywhere in the repo, after every redocly join (the
 * Taskfile api:merge task and api-contract.yml's merge step both run
 * it between `join` and `lint`, keeping the two in lockstep):
 *
 *   node web/scripts/redocly-join-fixup.mjs
 *
 * An optional argv[2] overrides the default target
 * build/openapi/speed.yaml (resolved against the current directory).
 * Exits non-zero when the info block is not exactly the three keys
 * this stamp understands (title, description, version -- redocly or
 * fragment drift, or a fragment suddenly shipping info.license or
 * info.contact), so the rewrite never silently drops an info field:
 * the guard forces a conscious update of this script instead.
 */
import { readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { resolve } from 'node:path'

const TITLE = 'speed'
const DESCRIPTION =
  'The speed application-wide OpenAPI document: the redocly join of the module ' +
  'fragments (the notes, authn and notification fragments today), regenerated ' +
  'by `task api:merge` and committed. Each module fragment stays authoritative ' +
  "for its own operations; this merged document is the frontend SDK's " +
  'generation input and the linted whole-surface view ' +
  '(docs/internal/21-api-contract.md).'

const defaultTarget = fileURLToPath(
  new URL('../../build/openapi/speed.yaml', import.meta.url),
)
const targetPath = process.argv[2]
  ? resolve(process.cwd(), process.argv[2])
  : defaultTarget

function fail(message) {
  console.error(`[redocly-join-fixup] ${message}`)
  process.exit(1)
}

const lines = readFileSync(targetPath, 'utf8').split('\n')

// Exactly one top-level `info:` key (column 0).
const infoIndexes = []
lines.forEach((line, index) => {
  if (line === 'info:') infoIndexes.push(index)
})
if (infoIndexes.length !== 1) {
  fail(
    `expected exactly one top-level 'info:' in ${targetPath}, found ` +
      `${infoIndexes.length}. redocly emission changed (version drift?) -- ` +
      'regenerate with the pinned redocly and re-run, then commit the artifact.',
  )
}

// The info block runs from `info:` to the first line that is neither a
// two-space key line (`  title: ...`) nor a deeper-indented continuation
// line (the folded description's `    ...` body). Anything else ends it.
let blockEnd = infoIndexes[0] + 1
while (blockEnd < lines.length && /^ {2,}/.test(lines[blockEnd])) {
  blockEnd += 1
}
const block = lines.slice(infoIndexes[0] + 1, blockEnd)

// Classify every block line; the block must be exactly one title, one
// description (folded `>-` with an indented body, or a single-line
// scalar) and one version -- nothing else.
let titleValue = null
let versionValue = null
let sawDescription = false
let inDescriptionBody = false
const guard = (ok, detail) => {
  if (!ok) {
    fail(
      `unexpected info-block structure in ${targetPath} (${detail}). The ` +
        'block must contain exactly title, description and version -- ' +
        'redocly emission changed, or a fragment now ships another info ' +
        "field (license/contact) this stamp doesn't know; update the " +
        'script consciously rather than dropping data.',
    )
  }
}
for (const line of block) {
  const titleMatch = /^  title: (.+)$/.exec(line)
  const versionMatch = /^  version: (.+)$/.exec(line)
  const descriptionMatch = /^  description: (.*)$/.exec(line)
  if (titleMatch) {
    guard(titleValue === null, 'two title keys')
    guard(!sawDescription && !inDescriptionBody, 'title after description')
    titleValue = titleMatch[1].trim()
  } else if (versionMatch) {
    guard(versionValue === null, 'two version keys')
    versionValue = versionMatch[1].trim()
  } else if (descriptionMatch) {
    guard(!sawDescription, 'two description keys')
    guard(!inDescriptionBody, 'description body before description key')
    sawDescription = true
    inDescriptionBody = descriptionMatch[1].trim() !== ''
  } else if (/^ {4,}/.test(line)) {
    guard(sawDescription, 'indented lines before the description key')
    inDescriptionBody = true
  } else {
    guard(false, `unclassifiable line ${JSON.stringify(line)}`)
  }
}
guard(sawDescription, 'no description key')
guard(titleValue !== null, 'no title key')
guard(versionValue !== null, 'no version key')

const stampedDescription = `  description: ${JSON.stringify(DESCRIPTION)}`
if (titleValue === TITLE && block.includes(stampedDescription)) {
  console.log(`[redocly-join-fixup] info block already stamped in ${targetPath}`)
} else {
  const stampedBlock = [
    'info:',
    `  title: ${TITLE}`,
    stampedDescription,
    `  version: ${versionValue}`,
  ]
  const rewritten = [
    ...lines.slice(0, infoIndexes[0]),
    ...stampedBlock,
    ...lines.slice(blockEnd),
  ]
  const result = rewritten.join('\n')
  writeFileSync(targetPath, result)
  console.log(
    `[redocly-join-fixup] stamped title '${TITLE}' and the application-level ` +
      `description into ${targetPath}`,
  )
}
