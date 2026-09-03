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
 * data, not rendered product text. Since the api-client round, the
 * no-direct-http rule (web/eslint-rules/no-direct-http.js, tested in
 * web/eslint-rules/no-direct-http.test.mjs) enforces the other half of the
 * API-contract discipline: hand-written HTTP happens only inside
 * @speed/api-client, which the rule's sole config-level whitelist
 * exempts -- the no-literal-text rule still applies inside api-client,
 * whitelisting HTTP does not whitelist text. The react-hooks plugin will
 * earn its place the round that introduces stateful components outside
 * ui-kit's controlled set; it stays out until then, keeping this config
 * dependency-free. That set is re-assessed on every component round, and
 * the FileUploader redesign (2026-09-04) passed it: the queue renders
 * from host-owned rows props, every pick, cancel, retry and remove
 * reports up through a callback, and the upload transport is host code
 * -- no stateful component entered the package, so the plugin still has
 * no seat here.
 */
import tseslint from 'typescript-eslint'
import { noDirectHttpRule } from './eslint-rules/no-direct-http.js'
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
        'no-direct-http': noDirectHttpRule,
        'no-literal-text': noLiteralTextRule,
      },
    },
  },
}

/** Package runtime code: src minus tests and test utilities. */
const PACKAGE_RUNTIME_SRC = {
  files: ['packages/*/src/**/*.{ts,tsx}'],
  ignores: [
    '**/*.test.{ts,tsx}',
    '**/*.spec.{ts,tsx}',
    '**/test-utils/**',
  ],
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
    ...PACKAGE_RUNTIME_SRC,
    rules: {
      'speed/no-literal-text': 'error',
    },
  },
  {
    ...PACKAGE_RUNTIME_SRC,
    // The single whitelist: api-client is the one package where
    // hand-written HTTP happens, by design (its createClient wires the
    // injectable fetch, timeout, retry and silent refresh). Extending
    // this whitelist is an architecture change -- see the rule file
    // and docs/internal/21-api-contract.md.
    ignores: [...PACKAGE_RUNTIME_SRC.ignores, 'packages/api-client/**'],
    rules: {
      'speed/no-direct-http': 'error',
    },
  },
)
