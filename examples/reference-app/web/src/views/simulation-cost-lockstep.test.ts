/**
 * simulation-cost-lockstep.test.ts -- the mechanical pin that keeps the
 * price the block-D surface displays ("this simulation cost N credits",
 * rendered beside a completed generation on the case page) in step with
 * the amount the server actually charges: the display reads
 * SIMULATION_CREDIT_COST from photo-simulation-panel.tsx, and this
 * suite reads the authoritative constant out of the Go source that
 * performs the reservation (internal/smilesim/service.go's
 * CreditsPerSimulation, the flat cost Simulate reserves for one
 * generation) and fails if the two ever drift -- the same
 * cross-source mechanical discipline codes-alignment.test.ts applies to
 * its sentinel citations (a Go edit that moves or changes the constant
 * fails here rather than shipping a displayed price the ledger does not
 * charge).
 *
 * The Go file is read relative to this test file's own URL (the app
 * lives at examples/reference-app/web, and internal/smilesim sits two
 * directories above the app root), the same string-arithmetic the
 * codes-alignment suite uses for its repository-root derivation -- vite
 * rewrites `new URL(..., import.meta.url)` patterns as asset
 * references, so the path is assembled by hand.
 */

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { SIMULATION_CREDIT_COST } from './photo-simulation-panel.js'

/** The service file whose constant is the authority on what one
 * generation charges. */
const CREDIT_COST_SOURCE = '../../../internal/smilesim/service.go'

/** The current authoritative declaration of CreditsPerSimulation. */
const CREDIT_COST_PATTERN = /const CreditsPerSimulation int64 = (\d+)/

/** The repository app root derived from this test file's location:
 * web/src -> web -> reference-app, so the internal/ sources sit at
 * '../../'. */
function serviceSourcePath(): string {
  const url = import.meta.url
  const withoutScheme = url.startsWith('file://') ? url.slice(7) : url
  const srcDir = withoutScheme.slice(0, withoutScheme.lastIndexOf('/'))
  return `${srcDir}/${CREDIT_COST_SOURCE}`
}

describe('the displayed simulation cost stays in step with the charge', () => {
  it('reads the authoritative constant out of the Go source', () => {
    const source = readFileSync(serviceSourcePath(), 'utf8')
    expect(source).toMatch(CREDIT_COST_PATTERN)
  })

  it('matches the amount the server reserves per generation', () => {
    const source = readFileSync(serviceSourcePath(), 'utf8')
    const match = CREDIT_COST_PATTERN.exec(source)
    if (match === null) {
      throw new Error(
        'internal/smilesim/service.go no longer declares the flat ' +
          'CreditsPerSimulation constant the block-D surface mirrors',
      )
    }
    expect(
      SIMULATION_CREDIT_COST,
      'the displayed cost drifted from internal/smilesim/service.go\'s ' +
        'CreditsPerSimulation -- a generation would say it cost ' +
        `${SIMULATION_CREDIT_COST} credits while the server charges ` +
        `${match[1]}`,
    ).toBe(Number(match[1]))
  })
})
