/**
 * Type declarations for speed-aliases.mjs. The manifest stays plain JS
 * so vite/vitest configs load it directly; this twin gives its
 * TypeScript consumers (the reference app's alias-manifest drift test)
 * their types.
 */

/** One specifier -> source-file entry of the workspace package map. */
export interface SpeedPackageEntry {
  /** The @speed/* import specifier (subpaths included). */
  readonly specifier: string
  /** The src file it resolves to, workspace-root-relative. */
  readonly entry: string
}

/**
 * The workspace's @speed specifiers and their src files, in resolution
 * order (a subpath entry before the entry that is its prefix).
 */
export declare const SPEED_PACKAGE_MAP: readonly SpeedPackageEntry[]

/**
 * The map as vite/vitest `resolve.alias` entries with absolute
 * replacements, order preserved.
 */
export declare function speedAliases(
  map?: readonly SpeedPackageEntry[],
): Array<{ find: string; replacement: string }>

/**
 * The map as a tsconfig `paths` object with entries relative to the
 * consumer's tsconfig directory.
 */
export declare function speedTsconfigPaths(
  consumerDir: string,
  map?: readonly SpeedPackageEntry[],
): Record<string, string[]>
