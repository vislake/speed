/**
 * reference-app-web's vite configuration: the host's own dev server and
 * production bundler -- the one place in the workspace a bundler is
 * allowed to live. This app is a browser host, not a library package;
 * the web/packages/* libraries stay bundler-free by discipline (their
 * exports point at a dist/ each consumer-facing build emits), and a
 * delivered consumer project shapes its own tooling exactly this way.
 * The server-side half of that question is settled: the reference-app
 * Dockerfile builds this directory's dist/ into the image and the Go
 * server serves it from disk, not go:embed (internal/app/frontend.go,
 * APP_WEB_DIST -- the dist is gitignored build output, so a compile-time
 * embed would force generated assets into the committed tree). This
 * file's own `base` stays vite's default "/", which is what
 * the served page's /assets/* references line up with; nothing here
 * touches the Go server or any image path.
 *
 * Resolution: every @speed/* specifier -- subpaths included -- maps onto
 * the workspace sibling's live src through the workspace's single
 * package map (web/scripts/speed-aliases.mjs), the same map the vitest
 * config imports and the app's tsconfig paths section mirrors. A
 * sibling's published exports resolve against its dist/, which is never
 * committed and may not exist when this app's dev server or build runs,
 * so every leg stays on source exactly like the test leg. Keeping the
 * map in one place is what keeps the three legs from drifting: a
 * specifier is added or moved there, never in this file.
 *
 * JSX: tsconfig.json sets "jsx": "react-jsx" and vite's esbuild transform
 * reads that setting (the same transform pipeline the vitest suites
 * already exercise over these very files), so the automatic runtime
 * needs no @vitejs/plugin-react -- the plugin would add per-file fast
 * refresh at the cost of a dependency, and its absence only means an
 * edit falls back to a full-page reload, fine for this minimal host.
 *
 * Proxy: the app talks to the same origin it is served from (the api
 * client's baseUrl is window.location.origin -- the assembly's default,
 * main.tsx), and every path it uses sits under /api: the generated
 * operations under /api/v1/* plus the two pre-auth config endpoints
 * /api/v1/config/public and /api/v1/config/features
 * (@speed/api-client's fetchPublicConfig / fetchSystemFeatures). In dev
 * those calls proxy to the reference-app backend, defaulting to its own
 * default port (internal/app/server.go's DefaultPort; PORT=8080 in the
 * app's .env.example) and overridable through REFERENCE_APP_API_PROXY.
 * The Host header is passed through unchanged, so the backend's
 * host->tenant DomainResolver sees a host it does not map and serves
 * platform defaults -- the pre-auth login page shape, correct for a dev
 * server. (With the backend down, only /api calls fail; serving the
 * page itself never touches the proxy.)
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
import { speedAliases } from '../../../web/scripts/speed-aliases.mjs'

/** Resolves a workspace-relative path from this config file's own
 * location (examples/reference-app/web), the same depth the vitest
 * config and tsconfig paths use. */
const sibling = (path: string): string =>
  fileURLToPath(new URL(path, import.meta.url))

const appRoot = sibling('.')
const siblingPackages = sibling('../../../web/packages')

export default defineConfig({
  resolve: {
    alias: speedAliases(),
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
