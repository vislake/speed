/**
 * Flat-config ESLint setup for the whole web/ workspace.
 *
 * The single config site: every package runs "eslint ." from its own
 * directory and ESLint discovers this file by walking up, so a new package
 * needs no config of its own -- and a rule change lands in one place.
 *
 * Deliberately minimal (see AGENTS.md and pr-check.yml's DELIBERATELY-NOT-
 * WIRED notes): typescript-eslint's recommended rules plus an explicit
 * no-explicit-any error. The JSX-era rules (no bare text nodes, no i18n
 * literals, no hand-written backend calls) arrive with the first React
 * package round, which is also when the react-hooks plugin earns its place.
 */
import tseslint from 'typescript-eslint'

export default tseslint.config(
  {
    ignores: ['**/node_modules/**', '**/dist/**'],
  },
  ...tseslint.configs.recommended,
  {
    rules: {
      '@typescript-eslint/no-explicit-any': 'error',
    },
  },
)
