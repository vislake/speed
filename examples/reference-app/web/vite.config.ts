/**
 * reference-app-web's vite configuration: the host's own dev server and
 * production bundler -- the one place in the workspace a bundler is
 * allowed to live. This app is a browser host, not a library package;
 * the web/packages/* libraries stay bundler-free by discipline (their
 * exports point at a dist/ each consumer-facing build emits), and a
 * delivered consumer project shapes its own tooling exactly this way.
 * Whether the reference-app's server image embeds this app's built
 * dist/ is the image round's decision (examples/reference-app/DEPLOY.md);
 * nothing here touches the Go server or any image path.
 *
 * Resolution: every @speed/* specifier -- subpaths included -- maps onto
 * the workspace sibling's live src through the same alias list the
 * tsconfig paths and the vitest config use (prefix-ordered: a subpath
 * entry must come before the entry that is its prefix). A sibling's
 * published exports resolve against its dist/, which is never committed
 * and may not exist when this app's dev server or build runs, so both
 * legs stay on source exactly like the test leg.
 *
 * JSX: tsconfig.json sets "jsx": "react-jsx" and vite's esbuild transform
 * reads that setting (the same transform pipeline the vitest suites
 * already exercise over these very files), so the automatic runtime
 * needs no @vitejs/plugin-react -- the plugin would add per-file fast
 * refresh at the cost of a dependency, and its absence only means an
 * edit falls back to a full-page reload, fine for this minimal host.
 *
 * Proxy: the app talks to the same origin it is served from (the api
 * client's baseUrl is window.location.origin -- main.tsx), and every
 * path it uses sits under /api: the generated operations under
 * /api/v1/* plus the two pre-auth config endpoints /api/config/public
 * and /api/system/features (@speed/api-client's fetchPublicConfig /
 * fetchSystemFeatures). In dev those calls proxy to the reference-app
 * backend, defaulting to its own default port (cmd/server/server.go's
 * defaultPort; PORT=8080 in the app's .env.example) and overridable
 * through REFERENCE_APP_API_PROXY. The Host header is passed through
 * unchanged, so the backend's host->tenant DomainResolver sees a host
 * it does not map and serves platform defaults -- the pre-auth login
 * page shape, correct for a dev server. (With the backend down, only
 * /api calls fail; serving the page itself never touches the proxy.)
 *
 * fs.allow: setting it replaces vite's default (the workspace root
 * search -- vite's docs warn the default is dropped, not extended, when
 * the option is given), so the list names both halves of what the dev
 * server serves: this directory's own root (searchForWorkspaceRoot
 * resolves it through this directory's settings-only pnpm-workspace.yaml,
 * which is what makes this directory its own one-project workspace for
 * pnpm runs started here) and the sibling packages directory the aliases
 * reach into. The pnpm virtual store is added by vite itself.
 */
import { fileURLToPath } from 'node:url'
import { defineConfig, searchForWorkspaceRoot } from 'vite'

/** Resolves a workspace-relative path from this config file's own
 * location (examples/reference-app/web), the same depth the vitest
 * config and tsconfig paths use. */
const sibling = (path: string): string =>
  fileURLToPath(new URL(path, import.meta.url))

const appRoot = sibling('.')
const siblingPackages = sibling('../../../web/packages')

export default defineConfig({
  resolve: {
    alias: [
      {
        find: '@speed/api-sdk/runtime',
        replacement: sibling('../../../web/packages/api-sdk/src/runtime.ts'),
      },
      {
        find: '@speed/api-sdk',
        replacement: sibling('../../../web/packages/api-sdk/src/index.ts'),
      },
      {
        find: '@speed/api-client/react',
        replacement: sibling('../../../web/packages/api-client/src/react.ts'),
      },
      {
        find: '@speed/api-client',
        replacement: sibling('../../../web/packages/api-client/src/index.ts'),
      },
      {
        find: '@speed/auth-core',
        replacement: sibling('../../../web/packages/auth-core/src/index.ts'),
      },
      {
        find: '@speed/auth-ui',
        replacement: sibling('../../../web/packages/auth-ui/src/index.ts'),
      },
      {
        find: '@speed/tenancy-ui',
        replacement: sibling('../../../web/packages/tenancy-ui/src/index.ts'),
      },
      {
        find: '@speed/account-ui',
        replacement: sibling('../../../web/packages/account-ui/src/index.ts'),
      },
      {
        find: '@speed/product-shell',
        replacement: sibling('../../../web/packages/product-shell/src/index.ts'),
      },
      {
        find: '@speed/layout-kit',
        replacement: sibling('../../../web/packages/layout-kit/src/index.ts'),
      },
      {
        find: '@speed/i18n/mui-locale',
        replacement: sibling('../../../web/packages/i18n/src/mui-locale.ts'),
      },
      {
        find: '@speed/i18n',
        replacement: sibling('../../../web/packages/i18n/src/index.ts'),
      },
      {
        find: '@speed/tokens',
        replacement: sibling('../../../web/packages/tokens/src/index.ts'),
      },
      {
        find: '@speed/ui-kit',
        replacement: sibling('../../../web/packages/ui-kit/src/index.ts'),
      },
    ],
  },
  server: {
    fs: {
      allow: [searchForWorkspaceRoot(appRoot), siblingPackages],
    },
    proxy: {
      '/api': {
        target: process.env.REFERENCE_APP_API_PROXY ?? 'http://localhost:8080',
      },
    },
  },
})
