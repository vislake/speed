/**
 * Flat-config ESLint setup for the whole web/ workspace.
 *
 * The single config site: every package runs "eslint ." from its own
 * directory and ESLint discovers this file by walking up, so a new package
 * needs no config of its own -- and a rule change lands in one place.
 *
 * Deliberately minimal (see AGENTS.md and pr-check.yml's DELIBERATELY-NOT-
 * WIRED notes): typescript-eslint's recommended rules plus an explicit
 * no-explicit-any error, and -- since the first React package round -- the
 * no-literal-text rule (web/eslint-rules/no-literal-text.js, tested in
 * web/eslint-rules/no-literal-text.test.mjs) enforcing that package UI text
 * comes from the i18n namespace. It applies to every package's src but
 * deliberately not to test files and test utilities: fixture strings are
 * data, not rendered product text. The no-hand-written-backend-calls rule
 * still awaits the api-client/api-sdk round, and the react-hooks plugin
 * will earn its place the round that introduces stateful components
 * outside ui-kit's controlled set -- both stay out until then, keeping
 * this config dependency-free.
 */
import tseslint from 'typescript-eslint'
import { noLiteralTextRule } from './eslint-rules/no-literal-text.js'

/**
 * Inline plugin holding the workspace's own rules. Flat config has no
 * "local rule" slot -- every rule must live behind a plugin namespace,
 * hence the bare 'speed' prefix used by the rules below.
 */
const localPlugin = {
  plugins: {
    speed: {
      rules: {
        'no-literal-text': noLiteralTextRule,
      },
    },
  },
}

export default tseslint.config(
  {
    ignores: ['**/node_modules/**', '**/dist/**'],
  },
  ...tseslint.configs.recommended,
  localPlugin,
  {
    rules: {
      '@typescript-eslint/no-explicit-any': 'error',
    },
  },
  {
    files: ['packages/*/src/**/*.{ts,tsx}'],
    ignores: [
      '**/*.test.{ts,tsx}',
      '**/*.spec.{ts,tsx}',
      '**/test-utils/**',
    ],
    rules: {
      'speed/no-literal-text': 'error',
    },
  },
)
