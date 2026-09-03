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
  },
})
