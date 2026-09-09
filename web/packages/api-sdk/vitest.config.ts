/**
 * api-sdk test configuration: the generated react-query hooks are
 * exercised through @testing-library/react's renderHook, which mounts a
 * host component into a DOM -- hence the jsdom environment. No aliases
 * are needed here: package sources import only relative files plus the
 * @speed/api-client type surface (type-only imports, erased at runtime),
 * and react-query/react resolve from the package's own devDependencies.
 */
import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: {
    environment: 'jsdom',
    // Coverage: v8 over src/** only, so aliased sibling sources never
    // count toward this package's numbers; text-summary for local runs,
    // json-summary for the per-package coverage gate. The one file left
    // out of src/** is src/index.ts, the orval-generated output (its
    // DO-NOT-EDIT header); the hand-written src/runtime.ts seam stays
    // measured.
    coverage: {
      provider: 'v8',
      include: ['src/**'],
      exclude: ['src/index.ts'],
      reporter: ['text-summary', 'json-summary'],
    },
  },
})
