package app

import (
	pkgconfig "github.com/vislake/speed/go/pkgcore/config"
)

// config.go carries the configuration stage's outward face: the loader
// options a load runs with, and the named constructors for them. The load
// itself is the loader's (loader.go), driven by a host's LoadSpec.
//
// The bootstrap-key declarations of the composed modules need no counterpart
// here. The engine's loader reads them off the registered components and
// resolves them through the declaration-driven entry (loader.go), so a key a
// component declares -- the platform key material included -- needs no field
// on any host or engine struct: a declaration is resolved where it is made.

// ConfigOption customises the loader the configuration stage builds. It is an
// alias of pkgcore/config's own Option, so a host may pass that package's
// options directly as well as through the named constructors below.
type ConfigOption = pkgconfig.Option

// ConfigFile points the loader at a YAML (or JSON) configuration file. A
// missing file is skipped silently; an unreadable or unparseable one fails
// the load. An empty path configures no file.
func ConfigFile(path string) ConfigOption { return pkgconfig.WithConfigFile(path) }

// ConfigArgs sets the command-line arguments the loader scans, in os.Args[1:]
// form; an empty slice disables the flag source. It exists so a test can
// inject arguments instead of depending on the process's own.
func ConfigArgs(args []string) ConfigOption { return pkgconfig.WithArgs(args) }

// ConfigEnvPrefix replaces the environment prefix the loader derives variable
// names from (SPEED_ by default); it must be non-empty and end in "_". The
// prefix is the host's own deployment surface -- "APP_" for both consumers in
// this repository -- and changes environment variable names only, never key
// paths or flag spellings.
func ConfigEnvPrefix(prefix string) ConfigOption { return pkgconfig.WithEnvPrefix(prefix) }

// ConfigRootKey installs the 32-byte root secret every declared key material
// is derived from when no source supplies that key explicitly. A nil or empty
// key configures none.
func ConfigRootKey(key []byte) ConfigOption { return pkgconfig.WithRootKey(key) }

// ConfigRootKeyEnv names the environment variable holding the root secret as
// 64 hexadecimal characters, read by the loader inside the same load that
// derives from it. An unset or empty variable configures no root key.
func ConfigRootKeyEnv(name string) ConfigOption { return pkgconfig.WithRootKeyEnv(name) }

// ConfigKeyDerivation installs the function declared key material is derived
// with, called once per key the source chain leaves to derivation with the
// root key and the declared key path. The platform composition a host wires
// is dbkit.DeriveBootstrapKey; the loader itself knows nothing about how a
// root key becomes material.
func ConfigKeyDerivation(fn func(rootKey []byte, keyPath string) ([]byte, error)) ConfigOption {
	return pkgconfig.WithKeyDerivation(fn)
}

// ConfigDevDefaults installs the declared defaults table: the values a
// declared hexkey key falls back to when neither an explicit source
// (flag, environment, config file) nor the root-key derivation supplies it,
// addressed by declared key path. It is the assembling party's documented
// development convenience -- a host passing its own recognizable non-secret
// dev keys -- and the engine's own zero-setup dev path is exactly such a
// table; a deployment taking its key material from a secret store passes
// none, and a declared key then resolves to nothing until a source supplies
// it.
func ConfigDevDefaults(defaults map[string][]byte) ConfigOption {
	return pkgconfig.WithDevDefaults(defaults)
}
