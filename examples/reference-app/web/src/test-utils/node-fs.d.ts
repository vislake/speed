/**
 * The one Node built-in this app's suites touch: codes-alignment.test.ts
 * reads the Go sources its server-code citations cite, to verify each
 * citation sits at the line that declares its sentinel.
 *
 * The app's type-check compilation carries no @types/node -- the host
 * is a browser app, and pulling the Node type package into its
 * dependency set for one test's file read would be out of proportion
 * -- so the single function used is declared here instead, in a
 * declaration file with no imports (which keeps it an ambient module
 * declaration rather than an augmentation of a module TypeScript
 * cannot find). The DOM lib's URL type covers the path argument, so
 * the citation verifier can read relative to the test file's own URL.
 */
declare module 'node:fs' {
  export function readFileSync(path: string | URL, encoding: 'utf8'): string
}
