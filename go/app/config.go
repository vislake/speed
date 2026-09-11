package app

import (
	"errors"
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
	pkgconfig "github.com/vislake/speed/go/pkgcore/config"
)

// PlatformConfig is the platform's normative declaration of its bootstrap key
// material: the six keys the platform module components declare as their
// BootstrapKeys, each nested under its module's own key-path segment, so the
// declaration's field paths are character for character the dotted key paths
// the modules declare (authn.blind_index_key, config.cipher_key and so on).
//
// A host holds it -- typically by embedding it in its own configuration
// target -- and tags the embedding config:"-" so the loader's walk of the
// host's target skips it. The engine loads it as its own target, which is
// what keeps the six keys at their declared top-level paths: an embedded
// struct the loader walked would contribute its type name as a leading key
// segment and the keys would resolve under a prefixed spelling instead.
//
// Every field carries the loader's derive tag alone. The declaration pins no
// environment variable names and no defaults: a host resolves its own
// development defaults by pre-filling the fields before load (the loader only
// writes what a source actually supplied, so a pre-filled 32-byte value
// stands when nothing else does), and the environment spelling is the
// loader's own derivation from the declared path -- with ConfigEnvPrefix
// ("APP_"), config.cipher_key reads APP_CONFIG__CIPHER_KEY, while a config
// file entry is spelled config.cipher_key exactly as declared.
//
// The engine consumes the config.cipher_key material itself (it builds the
// platform cipher WithPreDB and WithModules receive); the other five keys are
// the host's to hand to whichever module constructor declares a use for them.
// A key the host never consults stays inert -- the declaration exists so no
// host restates any of the six, and so a platform key added later arrives
// through this struct rather than through every host's own target.
type PlatformConfig struct {
	// Authn carries the two key materials go/authn declares.
	Authn PlatformAuthnKeyMaterial
	// Config carries the key material the config module declares.
	Config PlatformConfigKeyMaterial
	// Notification carries the key material go/notification declares.
	Notification PlatformNotificationKeyMaterial
	// Org carries the key material go/org declares.
	Org PlatformOrgKeyMaterial
	// PKI carries the key material go/pki declares.
	PKI PlatformPKIKeyMaterial
}

// PlatformAuthnKeyMaterial declares the authn.blind_index_key and
// authn.pii_cipher_key key materials: the HMAC key authn indexes its
// encrypted login identifiers under, and the AES key sealing the PII columns
// themselves. They are separate secrets on purpose -- an AES key never
// doubles as an HMAC key.
type PlatformAuthnKeyMaterial struct {
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key path authn.blind_index_key.
	Blind_Index_Key []byte `config:"derive"`
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key path authn.pii_cipher_key.
	PII_Cipher_Key []byte `config:"derive"`
}

// PlatformConfigKeyMaterial declares the config.cipher_key key material: the
// AES key the config module seals every Sensitive dynamic-configuration value
// with, and the platform cipher the engine builds for the host's serializers.
type PlatformConfigKeyMaterial struct {
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key path config.cipher_key.
	Cipher_Key []byte `config:"derive"`
}

// PlatformNotificationKeyMaterial declares the notification.contact_index_key
// key material: the HMAC key the notification module's blind indexers index
// its encrypted contact addresses with. One key serves both indexers, whose
// canonical forms are disjoint.
type PlatformNotificationKeyMaterial struct {
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key path notification.contact_index_key.
	Contact_Index_Key []byte `config:"derive"`
}

// PlatformOrgKeyMaterial declares the org.invitation_email_index_key key
// material: the HMAC key org's blind indexer indexes invitation email
// addresses with.
type PlatformOrgKeyMaterial struct {
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key path org.invitation_email_index_key.
	Invitation_Email_Index_Key []byte `config:"derive"`
}

// PlatformPKIKeyMaterial declares the pki.local_key_cipher_key key material:
// the AES key sealing go/pki's local signer private-key column.
type PlatformPKIKeyMaterial struct {
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key path pki.local_key_cipher_key.
	Local_Key_Cipher_Key []byte `config:"derive"`
}

// platformKeyPaths lists the declared bootstrap key paths PlatformConfig
// declares, in declaration order. A boot verifies its own declaration against
// this list, so a field that stops mapping onto its declared path fails the
// assembly rather than silently resolving nothing.
var platformKeyPaths = []string{
	"authn.blind_index_key",
	"authn.pii_cipher_key",
	"config.cipher_key",
	"notification.contact_index_key",
	"org.invitation_email_index_key",
	"pki.local_key_cipher_key",
}

// platformCipherKeyPath is the declared path of the material the engine
// builds its platform cipher from; it names the key in the refusal a
// malformed value produces.
const platformCipherKeyPath = "config.cipher_key"

// ConfigSpec names what the configuration stage loads: Host is the host's own
// bootstrap target (any non-nil pointer to a struct), and Platform is the
// platform key material's target, typically the PlatformConfig value embedded
// in the host's configuration struct.
//
// Both targets are loaded by one loader through the engine's own load
// orchestration: the same options, the same sources, the same single pass
// over them, so a value can never resolve differently for the two halves.
type ConfigSpec struct {
	// Host is the host's bootstrap target: a non-nil pointer to the struct
	// the loader fills with the host's own keys (its port, its database
	// path, its switches). Required.
	Host any

	// Platform is the platform key material's target. Required, and loaded
	// as its own target so the six declared key paths stay unprefixed.
	Platform *PlatformConfig
}

// ConfigOption customises the loader the configuration stage builds. It is an
// alias of pkgcore/config's own Option, so a host may pass that package's
// options directly as well as through the named constructors below.
type ConfigOption = pkgconfig.Option

// WithConfig declares the configuration the engine loads in stage 1, the
// loader options the load runs with, and is required -- an assembly with no
// configuration target has nothing to bind the modules' declared bootstrap
// keys against.
//
// A host whose option values depend on the loaded configuration (the database
// DSN, the listen address, the kernel's seam composition) resolves them with
// its own loader pass before New and hands the same target over here: the load
// this option drives re-resolves the identical sources into the same struct
// and is idempotent, so the two passes cannot disagree, while the binding
// verification and the platform key material the load produces stay on the
// engine's side.
func WithConfig(spec ConfigSpec, opts ...ConfigOption) Option {
	return func(c *engineConfig) {
		s := spec
		c.configSpec = &s
		for _, opt := range opts {
			if opt != nil {
				c.configOptions = append(c.configOptions, opt)
			}
		}
	}
}

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

// ConfigRootKey installs the 32-byte root secret every derive-tagged field's
// material is derived from when no source supplies that field explicitly. A
// nil or empty key configures none.
func ConfigRootKey(key []byte) ConfigOption { return pkgconfig.WithRootKey(key) }

// ConfigRootKeyEnv names the environment variable holding the root secret as
// 64 hexadecimal characters, read by the loader inside the same load that
// derives from it. An unset or empty variable configures no root key.
func ConfigRootKeyEnv(name string) ConfigOption { return pkgconfig.WithRootKeyEnv(name) }

// ConfigKeyDerivation installs the function derive-tagged material is derived
// with, called once per field the source chain leaves to derivation with the
// root key and the field's declared key path. The platform composition a host
// wires is dbkit.DeriveBootstrapKey; the loader itself knows nothing about
// how a root key becomes material.
func ConfigKeyDerivation(fn func(rootKey []byte, keyPath string) ([]byte, error)) ConfigOption {
	return pkgconfig.WithKeyDerivation(fn)
}

// loadConfiguration is stage 1: the host's target and the platform key
// material, resolved once through one loader. It refuses a spec that names
// neither target, and it proves its own declaration binds the six key paths
// it promises, so a declaration that stopped mapping onto a field fails the
// boot instead of resolving nothing.
func loadConfiguration(cfg *engineConfig) error {
	spec := cfg.configSpec
	if spec.Host == nil {
		return errors.New("app: ConfigSpec.Host must be the host's non-nil configuration target")
	}
	if spec.Platform == nil {
		return errors.New("app: ConfigSpec.Platform must be the non-nil *PlatformConfig carrying the platform key material")
	}

	loader := pkgconfig.New(cfg.configOptions...)
	if err := loader.Load(spec.Host); err != nil {
		return fmt.Errorf("app: load the host configuration: %w", err)
	}
	if err := loader.Load(spec.Platform); err != nil {
		return fmt.Errorf("app: load the platform key material: %w", err)
	}
	if err := pkgconfig.Verify(spec.Platform, platformKeyPaths); err != nil {
		return fmt.Errorf("app: the platform key declaration must bind every key it declares: %w", err)
	}
	return nil
}

// verifyBinding is the schema half of stage 4: every bootstrap key declared
// on the registry's bootstrap seat must map onto a field of one of the host's
// two configuration targets. A declared key that binds to nothing is a key
// whose contract is silently unreachable -- its wiring reads a value no
// source can ever supply -- so it fails the boot, naming every such key. The
// seat carries the host's own declarations; each component's statically
// declared BootstrapKeys are bound against the same targets by the loader's
// stage-prepare check, before anything is constructed.
func verifyBinding(spec *ConfigSpec, reg *pkgcore.Registry) error {
	declared := reg.Bootstrap.Keys()
	var missing []error
	for _, key := range declared {
		if pkgconfig.Verify(spec.Host, []string{key.Key}) == nil {
			continue
		}
		if pkgconfig.Verify(spec.Platform, []string{key.Key}) == nil {
			continue
		}
		missing = append(missing, fmt.Errorf("%w: declared bootstrap key %q maps onto no field of the host configuration target or of the platform key target",
			pkgconfig.ErrInvalidTarget, key.Key))
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("app: the configuration targets must bind every bootstrap key the composed modules declared: %w", errors.Join(missing...))
}
