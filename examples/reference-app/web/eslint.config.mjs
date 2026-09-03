/**
 * reference-app-web ESLint flat configuration.
 *
 * The app lives outside web/ (examples/reference-app/web is a workspace
 * member through the web/ pnpm-workspace.yaml, not a file inside it), so
 * ESLint's walk-up discovery from this directory can never reach
 * web/eslint.config.mjs: the app carries its own config file, importing
 * the web config wholesale and appending the two speed discipline rules
 * over the app's own src/.
 *
 * The imported web config's rule blocks keep the base paths of the file
 * that declared them (flat-config objects are anchored to their defining
 * config's directory), so its package-src patterns never reach app
 * files -- the two rules therefore restate below, scoped to this app:
 * runtime src (everything except the *.test suites and the shared
 * test-utils) must not carry inline user-facing text, and must not call
 * HTTP directly (fetch/axios/XHR belong to @speed/api-client alone; the
 * app reaches the backend through @speed/api-sdk's generated operations
 * over the bindRequestFn seam).
 */
import webConfig from '../../../web/eslint.config.mjs'

/** Runtime src of the app: where the two discipline rules apply. */
const APP_RUNTIME_SRC = {
  files: ['src/**/*.{ts,tsx}'],
  ignores: ['**/*.test.{ts,tsx}', '**/*.spec.{ts,tsx}', '**/test-utils/**'],
}

export default [
  ...webConfig,
  {
    ...APP_RUNTIME_SRC,
    rules: { 'speed/no-literal-text': 'error' },
  },
  {
    ...APP_RUNTIME_SRC,
    rules: { 'speed/no-direct-http': 'error' },
  },
]
