// Package config implements the bootstrap configuration loader: the small,
// immutable set of values a process resolves exactly once at startup, before
// anything else is wired.
//
// Bootstrap configuration is deliberately narrow. It answers "how do I reach my
// infrastructure and which deployment mode am I running?" and nothing else.
// Values that operations needs to tune at runtime, and values a tenant may
// override, belong to the separate dynamic configuration module; they are not
// resolved here.
//
// # One key, one layer
//
// A configuration key belongs to exactly one layer. Bootstrap keys are process
// startup input: they are resolved once on the five-source chain below and read
// no more, they have no tenant dimension, and a change takes effect at the next
// start. Runtime configuration items are the dynamic values in the configs
// table: stored per tenant or per platform, edited by an operator while the
// process runs, propagated through the config module's service and events, and
// effective immediately. Declaring the same dotted key on both layers is
// refused where both declaration seats are visible: the two layers give one
// identifier two meanings, two defaults and two edit surfaces, and an operator
// changing "that key" could not tell which one they were changing.
//
// # Sources
//
// Values are resolved from five sources. Highest priority wins:
//
//  1. command-line flags   --database.dsn=postgres://...
//  2. environment variables SPEED_DATABASE__DSN=postgres://...
//  3. a config file        an optional YAML (or JSON) file
//  4. key derivation       a derive-tagged field, from the configured root key
//  5. struct defaults      whatever the caller already set on the target
//
// The fourth source serves derive-tagged key-material fields alone (see the
// key-material section below); for every other field the chain is the four
// sources around it. The struct-default source is implicit: Load only writes
// fields that some source actually supplied, so a field left untouched keeps
// the value the caller assigned before calling Load.
//
// The environment source is read under the loader's prefix, SPEED_ by default;
// WithEnvPrefix replaces it, so a host whose variables carry another prefix
// (APP_, say) drives the same loader without renaming a single variable. The
// one variable that is not read under the prefix is WithRootKeyEnv's root-key
// variable, and that is what makes it work: it is read exactly as named, in
// the same Load that derives from it.
//
// # Key mapping
//
// A key path is derived from exported field names, lowercased and joined with
// KeyDelimiter, so the field Database.DSN maps to the key "database.dsn". An
// embedded struct is not flattened away; it contributes its type name as a key
// segment just as a named field does. The
// same key becomes the flag --database.dsn and, by default, the environment
// variable SPEED_DATABASE__DSN, where EnvSeparator (a double underscore) marks
// each level of nesting. Matching is case-insensitive in every source. Note
// that a single underscore is not a nesting marker: SPEED_DATABASE_DSN does not
// resolve to database.dsn, and keys that match no field are ignored rather than
// rejected, so unrelated SPEED_-prefixed variables are harmless.
//
// A field may instead pin its exact environment variable name (see the env
// struct tag option below). A pinned field is read from that name whatever the
// prefix is, and that name is the only variable it reads -- the spelling its
// key would otherwise derive is not a second way in, so a field never has two
// environment variables competing for it. The pinned name need not be
// derivable from the key at all: PORT is the customary name for "the port this
// platform told me to listen on", and no prefix-derived spelling of the key
// "port" can reach it. A file's or flag's spelling never depends on the prefix
// or on pinning: only environment variable names do.
//
// # Struct tags
//
// Fields may carry a "config" struct tag holding comma-separated options:
//
//	Field string `config:"required"`     // must end up non-zero, from any source
//	Field string `config:"-"`            // never populated from any source
//	Field string `config:"env=PORT"`     // read from the variable PORT, pinned
//
// A pinned name must not be empty, and two fields must not resolve to the same
// environment variable name -- both fields pinning it, or one pinning a name
// another field derives. That ambiguity is refused when the target is described
// (ErrInvalidTarget, naming both fields), because a single variable cannot feed
// two fields whose values the loader would then decide by traversal order.
//
// # Key material
//
// A field typed []byte may carry the bare derive tag option
// (config:"env=APP_CONFIG_KEY,derive"), marking it as one unit of key
// material: 32 bytes, the shape a bootstrap key's "hexkey" format spells as 64
// hexadecimal characters. A derive field resolves through the five-source
// chain above, with the derivation standing exactly where a struct default
// would:
//
//   - An explicit value -- from a flag, the environment or the config file --
//     must be exactly 64 hex characters, and the loader decodes it to the
//     field's 32 bytes. Any other text is a load refusal naming the key and
//     the source it came from; so is a value that is not text at all.
//   - A value an emptied variable (or an empty file entry) would have
//     supplied counts as no value at all, exactly as "" reads as unset for
//     the string fields around it: it neither overrides a lower-priority
//     source nor stops the derivation.
//   - With nothing explicit supplied and a root key configured, the material
//     is derived by the function WithKeyDerivation installed, from the root
//     key and the field's dotted key path. That key path is the derivation's
//     identity: renaming the field (and with it the key path) changes the
//     material derived for it, so a rename is a rotation of that key's
//     material and must ship as one.
//   - With no root key configured, and nothing explicit supplied, the struct
//     default stands like it does for every other field.
//
// The root key comes from exactly one source, declared by exactly one option:
// WithRootKey (the 32 bytes themselves) or WithRootKeyEnv (the name of the
// variable holding the key's 64-hex-character text, which the loader reads in
// the same Load that derives from it -- the reason the option exists, since a
// host forbidden from reading the environment itself cannot resolve the root
// key and hand it over without a chicken-and-egg problem). A configured root
// key that is not 32 bytes -- a variable set to anything but 64 hex
// characters included -- fails Load with ErrInvalidRootKey naming the source.
// A derive field with a root key configured but no deriver installed is a
// wiring error rather than a runtime condition, so Load refuses the target
// with ErrInvalidTarget.
//
// The derive option is deliberately narrow: the field must be typed exactly
// []byte, the option takes no value, and nothing about it changes how any
// other field resolves.
//
// # Verification
//
// Verify checks a list of declared keys against a target struct: every declared
// key must map onto a field. A host that knows the bootstrap keys its modules
// declared (pkgcore.BootstrapKey, the Registry.Bootstrap seat) runs it to prove
// its loader target binds everything those declarations promise, and a
// generated reference runs it so the keys it documents are exactly the keys a
// real target resolves.
//
// # Failure
//
// Loading fails fast. A required key that no source supplied, and a value that
// cannot be applied to the field it maps to, both abort the load with an error
// naming the offending key and listing every source that was consulted for it.
//
// An empty value is a value like any other and is judged by that same rule. It
// is an honest override for a field that can hold it, such as a string, and a
// hard error for a field that has no representation for it, so an unset shell
// variable behind SPEED_DATABASE__PORT= aborts startup instead of quietly
// booting the process on port 0.
package config

import (
	"encoding"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// EnvPrefix is the default prefix every environment variable must carry to be
// considered part of the bootstrap configuration. WithEnvPrefix replaces it per
// Loader.
const EnvPrefix = "SPEED_"

// EnvSeparator marks one level of nesting inside an environment variable name,
// so that the key "database.dsn" is read from SPEED_DATABASE__DSN.
const EnvSeparator = "__"

// KeyDelimiter separates the segments of a config key path, as in
// "database.dsn". It is also the separator used in flag names.
const KeyDelimiter = "."

// EnvName spells a config key the way the environment carries it under prefix:
// the prefix, then the key uppercased with each level of nesting marked by
// EnvSeparator, so EnvPrefix and the key "database.dsn" give
// SPEED_DATABASE__DSN. A field that pins its exact variable name with the env
// struct tag option (config:"env=NAME") is read from that name instead: the
// pin wins over this derivation, and EnvName itself always derives. The prefix
// must be a form WithEnvPrefix accepts (EnvPrefix by default); no form is
// validated here.
func EnvName(prefix, key string) string {
	return prefix + strings.ToUpper(strings.ReplaceAll(key, KeyDelimiter, EnvSeparator))
}

// TagName is the struct tag read by the loader for per-field options.
const TagName = "config"

// Tag options recognised inside a TagName struct tag.
const (
	tagRequired = "required"
	tagSkip     = "-"
	// tagEnvPrefix introduces the option that pins a field's exact environment
	// variable name: config:"env=PORT".
	tagEnvPrefix = "env="
	// tagDerive marks a []byte field as one unit of key material the loader
	// resolves (explicitly, derived or left at its default): config:"derive".
	tagDerive = "derive"
	// tagOptionSeparator splits a tag into its comma-separated options.
	tagOptionSeparator = ","
)

// rootKeySize is the byte length of a root key, and equally of the material
// every field tagged derive holds: 32 bytes (256 bits), the shape a bootstrap
// key's "hexkey" format spells as 64 hexadecimal characters. Both root-key
// sources are held to it -- WithRootKey's bytes directly, WithRootKeyEnv's
// variable as its 64-character hex text -- and the derivation's output must
// match it too, so a deriver cannot hand a field material of another shape.
const rootKeySize = 32

// Command-line flag syntax.
const (
	flagPrefix = "--"
	flagDash   = "-"
	flagAssign = "="
	// flagSetName only ever appears in flag package error text, never in output,
	// because the loader discards the flag set's writer.
	flagSetName = "speed-bootstrap-config"
)

var (
	// ErrInvalidTarget reports that Load was given something other than a
	// non-nil pointer to a struct, or a target whose config tags are malformed.
	ErrInvalidTarget = errors.New("config: invalid target")

	// ErrMissingValue reports that a field tagged as required was left zero
	// because no configuration source supplied a value for it.
	ErrMissingValue = errors.New("config: required value is missing")

	// ErrInvalidValue reports that a value supplied by one of the configuration
	// sources could not be applied to the field its key maps to.
	ErrInvalidValue = errors.New("config: invalid value")

	// ErrSourceUnreadable reports that a configuration source could not be read
	// at all, for example an unparseable config file or a malformed flag.
	ErrSourceUnreadable = errors.New("config: unreadable source")

	// ErrInvalidRootKey reports that a root-key source produced something that
	// is not a 32-byte key: WithRootKey a non-empty key of another length, or
	// WithRootKeyEnv a variable set to a non-empty value that is not 64 hex
	// characters. The error names the source -- the option or the variable --
	// so the operator knows which of the two to fix; a variable that is unset
	// or empty is not an error, it is an unconfigured root key.
	ErrInvalidRootKey = errors.New("config: invalid root key")
)

var (
	anyType             = reflect.TypeOf((*any)(nil)).Elem()
	timeType            = reflect.TypeOf(time.Time{})
	durationType        = reflect.TypeOf(time.Duration(0))
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	byteSliceType       = reflect.TypeOf([]byte(nil))
)

// source identifies which layer of the priority chain supplied a value.
type source int

const (
	sourceDefault source = iota
	sourceDerived
	sourceFile
	sourceEnv
	sourceFlag
)

// String returns a human-readable name for the configuration source.
func (s source) String() string {
	switch s {
	case sourceFlag:
		return "command-line flags"
	case sourceEnv:
		return "environment variables"
	case sourceFile:
		return "the config file"
	case sourceDerived:
		return "the root-key derivation"
	case sourceDefault:
		return "the target struct"
	default:
		return "an unknown source"
	}
}

// Loader resolves bootstrap configuration from flags, the environment, an
// optional config file, the root-key derivation of derive-tagged key-material
// fields, and the defaults already present on the target struct.
//
// The zero value is not usable; construct one with New. A Loader holds no
// mutable state once built, so a single Loader may be reused and is safe for
// concurrent use as long as the targets passed to Load are distinct.
type Loader struct {
	configFile string
	args       []string
	environ    []string
	prefix     string
	argsSet    bool
	environSet bool
	// rootKey and rootKeyEnv are the two mutually exclusive root-key sources
	// (WithRootKey / WithRootKeyEnv); rootKeySet records that one of the two
	// options was given, which is what makes a second one a wiring error.
	rootKey    []byte
	rootKeyEnv string
	rootKeySet bool
	// derivation is the function derive-tagged fields' material comes from
	// (WithKeyDerivation). Nil means no deriver is installed, which is an
	// error only when a root key is configured and the target derives.
	derivation func(rootKey []byte, keyPath string) ([]byte, error)
}

// Option customises a Loader built by New.
type Option func(*Loader)

// New returns a Loader configured by the given options. With no options it
// reads flags from os.Args, the environment from os.Environ under the SPEED_
// prefix, and consults no config file.
func New(opts ...Option) *Loader {
	l := &Loader{prefix: EnvPrefix}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	return l
}

// WithEnvPrefix replaces the prefix a field's environment variable name is
// derived from, SPEED_ by default. It changes environment variable names only:
// config keys and command-line flags are spelled the same whatever the prefix,
// so a host driving this loader with WithEnvPrefix("APP_") still reads the key
// "port" from the flag --port.
//
// A field pinning its name with the env struct tag option is unaffected by the
// prefix, and a prefix matching no field is harmless: variables outside the
// target struct's keys are ignored, never rejected.
//
// The prefix must be non-empty and end in "_"; anything else panics, because it
// is a wiring error rather than a runtime condition. An empty prefix would read
// the whole process environment into the loader's namespace, and a prefix
// without the trailing separator builds misspelled names such as FOOPORT out of
// the key "port".
func WithEnvPrefix(prefix string) Option {
	return func(l *Loader) {
		if prefix == "" {
			panic("config: WithEnvPrefix requires a non-empty prefix")
		}
		if !strings.HasSuffix(prefix, "_") {
			panic("config: WithEnvPrefix requires a prefix ending in \"_\", got " + prefix)
		}
		l.prefix = prefix
	}
}

// WithConfigFile points the loader at a YAML (or JSON) config file. The file is
// optional: if it does not exist that source is skipped silently, because the
// file is a convenience for local development rather than a requirement. A file
// that exists but cannot be read or parsed is a hard error.
func WithConfigFile(path string) Option {
	return func(l *Loader) { l.configFile = path }
}

// WithArgs sets the command-line arguments to scan, in the form of os.Args[1:].
// It exists so that tests can inject arguments instead of depending on the real
// process arguments; passing an empty slice disables the flag source entirely.
func WithArgs(args []string) Option {
	return func(l *Loader) {
		l.args = args
		l.argsSet = true
	}
}

// WithEnviron sets the environment to scan, in the "KEY=value" form returned by
// os.Environ. It exists so that tests can inject variables without mutating the
// real process environment; passing an empty slice disables the environment
// source entirely. WithRootKeyEnv's root-key variable is read from this same
// environment, so an injected environment supplies it too.
func WithEnviron(environ []string) Option {
	return func(l *Loader) {
		l.environ = environ
		l.environSet = true
	}
}

// WithRootKey installs rootKey as the loader's root secret: the 32-byte
// high-entropy key every derive-tagged field's material is derived from when
// no source supplies that field explicitly.
//
// A nil or empty key configures no root key, which is not an error: derive
// fields then keep their struct defaults, the shape a host's documented
// development defaults take. A non-empty key that is not exactly 32 bytes
// fails Load with an error wrapping ErrInvalidRootKey, never quietly deriving
// from a secret of the wrong size.
//
// Pass at most one root-key option. WithRootKey and WithRootKeyEnv name two
// distinct sources of one secret, and two secrets have no "the later one
// wins" reading -- which of two keys a host meant would be undecidable from
// the call site alone -- so a second root-key option panics, whatever the
// combination, the same option twice included.
func WithRootKey(rootKey []byte) Option {
	return func(l *Loader) {
		if l.rootKeySet {
			panic("config: a second root-key option was given; declare the root key once, with WithRootKey or WithRootKeyEnv")
		}
		l.rootKeySet = true
		l.rootKey = rootKey
	}
}

// WithRootKeyEnv makes the loader read its root secret from the named
// environment variable, in the same Load that derives from it: the variable's
// value is the key's 64-hex-character text, decoded to the 32 bytes the
// derivation consumes. The name is read exactly as spelled -- no prefix is
// applied, no case folding happens, and the name should be the host's own
// (APP_ROOT_KEY, say), never a SPEED_-derived spelling it does not actually
// set. An empty name is a wiring error and panics.
//
// The variable being unset -- or set to an empty value -- configures no root
// key: derive-tagged fields keep their struct defaults, and the load
// succeeds. A variable set to a non-empty value must hold exactly 64 hex
// characters; anything else fails Load with an error wrapping
// ErrInvalidRootKey, naming the variable.
//
// The option exists because the root key must be consumed in the same Load
// that uses it, and a host may be unable (or forbidden by its own discipline)
// to read the environment itself: reading the variable here is what lets such
// a host wire derivation without a direct environment read of its own.
//
// Pass at most one root-key option; see WithRootKey for the panic rule two
// sources share.
func WithRootKeyEnv(name string) Option {
	return func(l *Loader) {
		if name == "" {
			panic("config: WithRootKeyEnv requires a non-empty variable name")
		}
		if l.rootKeySet {
			panic("config: a second root-key option was given; declare the root key once, with WithRootKey or WithRootKeyEnv")
		}
		l.rootKeySet = true
		l.rootKeyEnv = name
	}
}

// WithKeyDerivation installs fn as the function every derive-tagged field's
// material is derived from. The loader calls fn once per field the five-source
// chain leaves to derivation, passing the configured root key and the field's
// dotted key path, and stores the returned bytes on the field; fn's result
// must be exactly 32 bytes, or the load fails naming the key.
//
// The loader deliberately knows nothing about how a root key becomes key
// material: fn is a plain function, so this package stays dependency-free and
// the composition stays the host's to declare. The platform composition a
// host wires is pkgcore.BootstrapKeyPurpose over the key path (the one
// supported purpose spelling per declared bootstrap key path) followed by
// dbkit.DeriveKey over the root key and that purpose -- dbkit.DeriveBootstrapKey
// is exactly that pair in one call -- but neither package is a dependency of
// this one, and a host is free to install a different function (a test's
// stand-in, a service's own derivation) as long as it is deterministic.
//
// The key path passed to fn is the field's dotted key path, character for
// character the identifier flags and config files use. That makes the key path
// the derivation's identity: renaming a field changes the key path, which
// changes what fn derives for it, so a rename is a rotation of that field's
// key material and must ship as one.
//
// A target with derive-tagged fields and a configured root key but no deriver
// installed fails Load with ErrInvalidTarget: the schema asks for a derivation
// the wiring cannot deliver. With no root key configured, fn is never called
// and the fields' struct defaults stand. Passing nil leaves the loader without
// a deriver, exactly as not passing the option does.
func WithKeyDerivation(fn func(rootKey []byte, keyPath string) ([]byte, error)) Option {
	return func(l *Loader) { l.derivation = fn }
}

// Load resolves the configuration into target, which must be a non-nil pointer
// to a struct. Fields that no source supplies keep the value they already hold,
// which is how struct defaults participate as the lowest-priority source.
//
// Load returns an error wrapping ErrMissingValue if a field tagged as required
// is still zero afterwards, ErrInvalidValue if a supplied value -- an explicit
// one or a derived one -- does not fit the field it maps to,
// ErrSourceUnreadable if a source could not be read, ErrInvalidRootKey if the
// configured root key is not a 32-byte key, and ErrInvalidTarget if target is
// not a usable struct pointer or its derivation wiring cannot serve it. When
// Load returns an error, target may already have been partially written and
// must not be used; bootstrap configuration is meant to abort process startup,
// not to be salvaged.
func (l *Loader) Load(target any) error {
	schema, err := describe(target)
	if err != nil {
		return err
	}
	if err = l.resolveEnvNames(schema); err != nil {
		return err
	}

	rootKey, err := l.resolveRootKey()
	if err != nil {
		return err
	}
	if rootKey != nil && schema.derives > 0 && l.derivation == nil {
		return fmt.Errorf("%w: %d field(s) carry the %q tag option and a root key is configured, but no deriver is installed; build the loader with WithKeyDerivation, or drop the root key",
			ErrInvalidTarget, schema.derives, tagDerive)
	}

	values, origins, err := l.collect(schema)
	if err != nil {
		return err
	}

	if err := l.checkEmpty(schema, values, origins); err != nil {
		return err
	}

	// Text values are converted here, once, so that the decode and the per-key
	// replay in explain judge the same value by the same rules. The empty-value
	// check above keeps its own error, which states the fault better than a
	// parse failure would.
	if err := l.coerceTextValues(schema, values, origins); err != nil {
		return err
	}

	// Key-material fields are filled here, before the text pipeline below ever
	// sees their keys (applyKeyMaterial removes them): they hold bytes, not
	// decoded text, and the decoder would otherwise write into whatever slice
	// they already carry -- a pre-filled package-level development default,
	// say -- in place.
	if err := l.applyKeyMaterial(target, schema, values, origins, rootKey); err != nil {
		return err
	}

	k := koanf.New(KeyDelimiter)
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if err := k.Set(key, values[key]); err != nil {
			return fmt.Errorf("%w for key %q (supplied by %s): %w; sources checked: %s",
				ErrInvalidValue, key, origins[key], err, l.sourcesFor(schema, key))
		}
	}

	if err := decode(k, target); err != nil {
		return l.explain(schema, target, values, origins, err)
	}

	return l.checkRequired(target, schema)
}

// collect merges every source into one flat map of key to value, lowest
// priority first, and records which source won each key.
func (l *Loader) collect(s *schema) (map[string]any, map[string]source, error) {
	values := make(map[string]any)
	origins := make(map[string]source)

	apply := func(in map[string]any, src source) {
		for key, val := range in {
			key = strings.ToLower(key)
			if !s.accepts(key) {
				continue
			}
			if f, ok := s.byKey[key]; ok && f.derive {
				// For a key-material field an empty value is no value: an
				// emptied variable (or an empty file entry) is the shape a
				// deployment leaves a secret in when it means to leave it
				// unset, exactly as "" reads as unset for the string fields
				// around it. Dropping it here, per source, keeps it from
				// shadowing whatever a lower-priority source supplies, the
				// derivation included.
				if val == nil {
					continue
				}
				if text, isText := val.(string); isText && text == "" {
					continue
				}
			}
			values[key] = val
			origins[key] = src
		}
	}

	fileValues, err := l.readFile()
	if err != nil {
		return nil, nil, err
	}
	apply(fileValues, sourceFile)

	envValues, err := l.readEnv(s)
	if err != nil {
		return nil, nil, err
	}
	apply(envValues, sourceEnv)

	flagValues, err := l.readFlags(s)
	if err != nil {
		return nil, nil, err
	}
	apply(flagValues, sourceFlag)

	return values, origins, nil
}

// resolveRootKey resolves the loader's root secret from whichever source its
// options named, returning nil when no root key is configured. WithRootKeyEnv
// reads its variable here, inside Load, so the key is consumed in the same
// load that derives from it and a host never has to read the environment
// itself. A wrong-shaped result from either source is an error wrapping
// ErrInvalidRootKey, naming the source; an unset or empty variable is not an
// error, it is an unconfigured root key.
func (l *Loader) resolveRootKey() ([]byte, error) {
	if l.rootKeyEnv != "" {
		environ := l.environ
		if !l.environSet {
			environ = os.Environ()
		}
		for _, entry := range environ {
			name, value, ok := strings.Cut(entry, "=")
			if !ok || name != l.rootKeyEnv {
				continue
			}
			if value == "" {
				return nil, nil
			}
			if len(value) != 2*rootKeySize {
				return nil, fmt.Errorf("%w: environment variable %s must hold %d hex characters (a %d-byte root key), got %d",
					ErrInvalidRootKey, l.rootKeyEnv, 2*rootKeySize, rootKeySize, len(value))
			}
			decoded, err := hex.DecodeString(value)
			if err != nil {
				return nil, fmt.Errorf("%w: environment variable %s: %w", ErrInvalidRootKey, l.rootKeyEnv, err)
			}
			return decoded, nil
		}
		return nil, nil
	}

	if len(l.rootKey) == 0 {
		return nil, nil
	}
	if len(l.rootKey) != rootKeySize {
		return nil, fmt.Errorf("%w: the root key handed to WithRootKey must be exactly %d bytes, got %d",
			ErrInvalidRootKey, rootKeySize, len(l.rootKey))
	}
	return l.rootKey, nil
}

// readFile parses the optional config file into a flat map of key to value. A
// missing file yields no values and no error.
func (l *Loader) readFile() (map[string]any, error) {
	if l.configFile == "" {
		return nil, nil
	}
	if _, err := os.Stat(l.configFile); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: config file %s: %w", ErrSourceUnreadable, l.configFile, err)
	}

	k := koanf.New(KeyDelimiter)
	if err := k.Load(file.Provider(l.configFile), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("%w: config file %s: %w", ErrSourceUnreadable, l.configFile, err)
	}
	return k.All(), nil
}

// readEnv reads the environment into a flat map of key to value. A variable is
// accepted when its name is one some field pinned with the env struct tag
// option, or when it carries the loader's prefix and its remainder translates
// EnvSeparator back into KeyDelimiter, lowercased. Every other variable is
// ignored, so an unrelated variable whose name happens to share the prefix
// stays harmless.
//
// A pinned field is read from its pinned name only: the variable its key would
// otherwise derive to is not a second spelling of the same field, because a
// field that could be fed by either name would have two sources whose relative
// weight depended on the environment's order.
func (l *Loader) readEnv(s *schema) (map[string]any, error) {
	environ := l.environ
	if !l.environSet {
		environ = os.Environ()
	}

	pinned := make(map[string]string, len(s.fields))
	onlyPinned := make(map[string]struct{}, len(s.fields))
	for i := range s.fields {
		if s.fields[i].pinned == "" {
			continue
		}
		pinned[s.fields[i].pinned] = s.fields[i].key
		onlyPinned[s.fields[i].key] = struct{}{}
	}

	values := make(map[string]any)
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if key, isPinned := pinned[name]; isPinned {
			values[key] = value
			continue
		}
		if !strings.HasPrefix(name, l.prefix) {
			continue
		}
		key := strings.TrimPrefix(name, l.prefix)
		if key == "" {
			continue
		}
		key = strings.ReplaceAll(strings.ToLower(key), EnvSeparator, KeyDelimiter)
		if _, pinnedElsewhere := onlyPinned[key]; pinnedElsewhere {
			continue
		}
		values[key] = value
	}
	return values, nil
}

// readFlags scans the command line for flags named after known config keys.
// Arguments that do not name a known key are ignored, so the loader can run
// inside a process (or a test binary) that owns flags of its own.
func (l *Loader) readFlags(s *schema) (map[string]any, error) {
	args := l.args
	if !l.argsSet {
		args = os.Args[1:]
	}
	if len(args) == 0 {
		return nil, nil
	}

	set := flag.NewFlagSet(flagSetName, flag.ContinueOnError)
	// Nothing ever renders this flag set's usage, so the per-flag usage strings
	// are left empty rather than carrying untranslated user-facing text.
	set.SetOutput(io.Discard)
	for i := range s.fields {
		set.String(s.fields[i].key, "", "")
	}

	if err := set.Parse(canonicaliseArgs(args, s)); err != nil {
		return nil, fmt.Errorf("%w: command-line flags: %w", ErrSourceUnreadable, err)
	}

	values := make(map[string]any)
	// Visit reports only the flags actually present on the command line, so a
	// flag left unset never shadows a lower-priority source.
	set.Visit(func(f *flag.Flag) { values[f.Name] = f.Value.String() })
	return values, nil
}

// canonicaliseArgs keeps only the arguments that name a known config key and
// rewrites each into the unambiguous "--key=value" form. Every known flag takes
// a value, so an argument directly following one is its value; anything else is
// dropped, including unknown flags and their values.
func canonicaliseArgs(args []string, s *schema) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		name := strings.TrimLeft(args[i], flagDash)
		if name == args[i] || name == "" {
			continue // a positional argument, or a bare "-" / "--".
		}

		value := ""
		hasValue := false
		if eq := strings.Index(name, flagAssign); eq >= 0 {
			name, value, hasValue = name[:eq], name[eq+1:], true
		}

		key := strings.ToLower(name)
		if _, known := s.byKey[key]; !known {
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				// Let the flag package report the missing value.
				out = append(out, flagPrefix+key)
				continue
			}
			value = args[i+1]
			i++
		}
		out = append(out, flagPrefix+key+flagAssign+value)
	}
	return out
}

// decode applies the merged config map onto target. Values arriving from flags
// and the environment are always strings, so weak typing does the conversion
// into the target field's type and reports anything that will not convert.
func decode(k *koanf.Koanf, target any) error {
	return k.UnmarshalWithConf("", target, koanf.UnmarshalConf{})
}

// explain turns an opaque decode failure into an error naming the single key
// responsible. It replays each key on its own against a fresh copy of the
// target type, so the diagnosis uses exactly the same conversion rules as the
// failed decode rather than a second-guessed imitation of them.
func (l *Loader) explain(s *schema, target any, values map[string]any, origins map[string]source, cause error) error {
	elem := reflect.TypeOf(target).Elem()
	for _, key := range slices.Sorted(maps.Keys(values)) {
		probe := koanf.New(KeyDelimiter)
		if err := probe.Set(key, values[key]); err != nil {
			continue
		}
		if err := decode(probe, reflect.New(elem).Interface()); err != nil {
			return fmt.Errorf("%w for key %q (supplied by %s): %w; sources checked: %s",
				ErrInvalidValue, key, origins[key], err, l.sourcesFor(s, key))
		}
	}
	return fmt.Errorf("%w: %w", ErrInvalidValue, cause)
}

// checkEmpty rejects an empty value supplied for a key whose field has no way
// to hold one. The decoder's weak typing would otherwise turn "" into that
// field's zero value and report success, discarding the default the caller had
// set, which is the one thing a fail-fast bootstrap loader must not do quietly:
// an unset shell variable behind SPEED_DATABASE__PORT= would boot the process
// on port 0 with nothing said. Only values that arrive as strings are vetted,
// because only a text source can produce an empty one.
func (l *Loader) checkEmpty(s *schema, values map[string]any, origins map[string]source) error {
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if text, isText := values[key].(string); !isText || text != "" {
			continue
		}
		t, known := s.targetType(key)
		if !known || acceptsEmpty(t) {
			continue
		}
		return fmt.Errorf("%w for key %q (supplied by %s): a field of type %s has no representation for an empty value; sources checked: %s",
			ErrInvalidValue, key, origins[key], t, l.sourcesFor(s, key))
	}
	return nil
}

// checkRequired reports every field tagged as required that no source, the
// struct's own defaults included, managed to fill.
func (l *Loader) checkRequired(target any, s *schema) error {
	root := reflect.ValueOf(target).Elem()
	var missing []error
	for _, f := range s.fields {
		if !f.required {
			continue
		}
		if v, ok := fieldValue(root, f.index); ok && !v.IsZero() {
			continue
		}
		missing = append(missing, fmt.Errorf("%w: key %q was not supplied; sources checked: %s",
			ErrMissingValue, f.key, l.sourcesFor(s, f.key)))
	}
	return errors.Join(missing...)
}

// applyKeyMaterial fills every derive-tagged field the text sources did not
// explicitly supply -- and keeps every derive-tagged key out of the text
// pipeline: an explicit value is decoded here, a field this step leaves alone
// keeps its struct default, and the keys are dropped from values so the koanf
// decode below never sees them. That removal is what protects a pre-filled
// slice: mapstructure decodes a text value into a non-nil []byte field by
// writing through the slice's own backing array, so a derive-tagged default a
// host shares with a package-level constant would be silently rewritten in
// place. Every value this step does store is either freshly decoded or a copy
// of the deriver's result, never a buffer someone else still holds.
func (l *Loader) applyKeyMaterial(target any, s *schema, values map[string]any, origins map[string]source, rootKey []byte) error {
	root := reflect.ValueOf(target).Elem()
	for i := range s.fields {
		f := &s.fields[i]
		if !f.derive {
			continue
		}

		var material []byte
		if raw, supplied := values[f.key]; supplied {
			text, isText := raw.(string)
			if !isText {
				return fmt.Errorf("%w for key %q (supplied by %s): a key-material value must be a string of %d hex characters -- a config file's value may need quoting so its parser delivers text -- got %T; sources checked: %s",
					ErrInvalidValue, f.key, origins[f.key], 2*rootKeySize, raw, l.sourcesFor(s, f.key))
			}
			if len(text) != 2*rootKeySize {
				return fmt.Errorf("%w for key %q (supplied by %s): value must hold %d hex characters (a %d-byte key), got %d; sources checked: %s",
					ErrInvalidValue, f.key, origins[f.key], 2*rootKeySize, rootKeySize, len(text), l.sourcesFor(s, f.key))
			}
			decoded, err := hex.DecodeString(text)
			if err != nil {
				return fmt.Errorf("%w for key %q (supplied by %s): %w; sources checked: %s",
					ErrInvalidValue, f.key, origins[f.key], err, l.sourcesFor(s, f.key))
			}
			material = decoded
		} else if rootKey != nil {
			derived, err := l.derivation(rootKey, f.key)
			if err != nil {
				return fmt.Errorf("%w for key %q (supplied by %s): %w; sources checked: %s",
					ErrInvalidValue, f.key, sourceDerived, err, l.sourcesFor(s, f.key))
			}
			if len(derived) != rootKeySize {
				return fmt.Errorf("%w for key %q (supplied by %s): derived value must be %d bytes, got %d; sources checked: %s",
					ErrInvalidValue, f.key, sourceDerived, rootKeySize, len(derived), l.sourcesFor(s, f.key))
			}
			// The field gets the loader's own copy, never the deriver's buffer,
			// which the host is free to reuse across calls.
			material = slices.Clone(derived)
		} else {
			continue
		}

		// Whether or not a source supplied this key, the text pipeline must not
		// see it: an emptied variable's key was already dropped by collect, and
		// an explicit value has just been decoded above.
		delete(values, f.key)

		leaf, ok := fieldValue(root, f.index)
		if !ok {
			return fmt.Errorf("%w for key %q: the field is unreachable through a nil pointer", ErrInvalidTarget, f.key)
		}
		leaf.SetBytes(material)
	}
	return nil
}

// sourcesFor lists, in priority order, every place the loader looked for a key,
// so the reader of an error knows exactly where to put the missing value. The
// environment name it names is the one this loader actually read -- a field's
// pinned name when it has one, its prefix-derived name otherwise -- never a
// hardcoded SPEED_ spelling. For a derive-tagged key the list also names the
// derivation, which stands between the file and the struct default.
func (l *Loader) sourcesFor(s *schema, key string) string {
	parts := []string{
		"command-line flag " + flagPrefix + key,
		"environment variable " + s.envNameFor(l, key),
	}
	if l.configFile != "" {
		parts = append(parts, fmt.Sprintf("key %s in config file %s", key, l.configFile))
	} else {
		parts = append(parts, "no config file configured")
	}
	if f, ok := s.byKey[key]; ok && f.derive {
		parts = append(parts, "the root-key derivation over declared key path "+key)
	}
	return strings.Join(append(parts, "the default set on the target struct"), ", ")
}

// envNameFor returns the environment variable name a config key is read from:
// the field's pinned name when it has one, its prefix-derived name otherwise.
// A key nested under a map-like leaf has no field of its own and is always
// derived.
func (s *schema) envNameFor(l *Loader, key string) string {
	if f, ok := s.byKey[key]; ok && f.pinned != "" {
		return f.pinned
	}
	return l.derivedEnvName(key)
}

// derivedEnvName spells a config key the way the environment carries it for
// this loader. It applies EnvName to the loader's own prefix -- the single
// implementation both share, so the name the loader reads and the name the
// documented derivation reports can never drift.
func (l *Loader) derivedEnvName(key string) string {
	return EnvName(l.prefix, key)
}

// field is one leaf of the target struct: a value a source can actually supply.
type field struct {
	key      string       // dotted key path, for example "database.dsn"
	index    []int        // field index path from the root struct
	typ      reflect.Type // the leaf's own type, which a supplied value must fit
	pinned   string       // the exact environment variable name, tagged config:"env=NAME"; empty when derived
	required bool         // tagged config:"required"
	derive   bool         // tagged config:"derive": one []byte unit of key material
	subKeys  bool         // a map-like leaf, so keys nested under it belong to it
}

// schema is the flattened description of a target struct.
type schema struct {
	fields  []field
	byKey   map[string]*field
	derives int // how many fields carry the derive tag option
}

// accepts reports whether a key from a source maps onto this target at all.
// Unknown keys are dropped rather than rejected, which keeps unrelated
// SPEED_-prefixed variables harmless and keeps the merge deterministic by
// making it impossible for a key and its own prefix to both be set.
func (s *schema) accepts(key string) bool {
	_, ok := s.targetType(key)
	return ok
}

// targetType returns the type a value supplied for key has to fit. For a key
// nested under a map-like leaf that is the map's element type rather than the
// map itself, because the leaf only holds the collection. It reports false for
// a key that maps onto no field at all.
func (s *schema) targetType(key string) (reflect.Type, bool) {
	if f, ok := s.byKey[key]; ok {
		return f.typ, true
	}
	for i := range s.fields {
		if !s.fields[i].subKeys || !strings.HasPrefix(key, s.fields[i].key+KeyDelimiter) {
			continue
		}
		if t := deref(s.fields[i].typ); t.Kind() == reflect.Map {
			return t.Elem(), true
		}
		return anyType, true // an interface leaf holds whatever it is given
	}
	return nil, false
}

// resolveEnvNames refuses a target whose fields would be read from one
// environment variable twice: both pinning the same name, or one pinning a name
// another field derives. Such a target has no single-valued reading -- which
// field a variable feeds would depend on traversal order -- so it fails here,
// before any source is consulted, naming both keys and the shared name.
func (l *Loader) resolveEnvNames(s *schema) error {
	owners := make(map[string]string, len(s.fields))
	for i := range s.fields {
		f := &s.fields[i]
		name := f.pinned
		pinned := f.pinned != ""
		if name == "" {
			name = l.derivedEnvName(f.key)
		}
		if owner, taken := owners[name]; taken {
			how := "both derive"
			if pinned {
				how = "pins"
			}
			return fmt.Errorf("%w: key %q %s environment variable %s, which key %q also reads; give one of them a distinct env name",
				ErrInvalidTarget, f.key, how, name, owner)
		}
		owners[name] = f.key
	}
	return nil
}

// Verify reports every declared key that maps onto no field of target, so that
// a host proving its loader target binds the declarations its modules
// registered gets a named list of what is missing rather than a load failure
// later. target must be the same kind of non-nil struct pointer Load accepts;
// an invalid one returns the same error describe produces.
//
// Keys are compared as dotted key paths, lowercased, exactly as Load resolves
// them; a key nested under a map-like field has no field of its own and is
// reported as missing, because a declaration names a value the loader must
// resolve, not a collection member.
func Verify(target any, declared []string) error {
	s, err := describe(target)
	if err != nil {
		return err
	}
	var missing []error
	for _, key := range declared {
		if _, ok := s.byKey[strings.ToLower(key)]; !ok {
			missing = append(missing, fmt.Errorf("%w: declared key %q maps onto no field of the target struct",
				ErrInvalidTarget, key))
		}
	}
	return errors.Join(missing...)
}

// coerceTextValues converts the text of a flag, environment variable or
// string-valued config file entry into the type the field it maps to must hold,
// so a text value is judged by strconv's rules -- and reported by this loader,
// naming the key and every source consulted -- rather than by the decoder's
// weaker ones. Values already carrying a type (a YAML integer, say) are left
// alone, as are fields that parse their own text and fields that hold text
// verbatim.
func (l *Loader) coerceTextValues(s *schema, values map[string]any, origins map[string]source) error {
	for _, key := range slices.Sorted(maps.Keys(values)) {
		text, isText := values[key].(string)
		if !isText {
			continue
		}
		t, known := s.targetType(key)
		if !known {
			continue
		}
		converted, handled, err := parseText(text, t)
		if err != nil {
			return fmt.Errorf("%w for key %q (supplied by %s): %w; sources checked: %s",
				ErrInvalidValue, key, origins[key], err, l.sourcesFor(s, key))
		}
		if handled {
			values[key] = converted
		}
	}
	return nil
}

// parseText converts one text value into t's own type, reporting whether the
// type is one the conversion covers. Integral fields read Go's integer literal
// syntax (strconv.ParseInt and ParseUint with base 0: 0x10, 0o17 and 1_0 are
// literals, and a leading zero makes 010 octal), bool fields read
// strconv.ParseBool's full set (1/t/T/TRUE/true/True and their false
// counterparts), and duration fields read Go's duration syntax. Everything
// else -- text, interfaces, collections, structs, and types that parse their
// own text -- stays as it arrived.
func parseText(text string, t reflect.Type) (any, bool, error) {
	elem := deref(t)
	if isScalarStruct(elem) {
		return nil, false, nil
	}

	v := reflect.New(elem).Elem()
	switch elem.Kind() {
	case reflect.Bool:
		parsed, err := strconv.ParseBool(text)
		if err != nil {
			return nil, true, fmt.Errorf("value %q is not a valid bool", text)
		}
		v.SetBool(parsed)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if elem == durationType {
			parsed, err := time.ParseDuration(text)
			if err != nil {
				return nil, true, fmt.Errorf("value %q is not a valid duration", text)
			}
			v.SetInt(int64(parsed))
			break
		}
		parsed, err := strconv.ParseInt(text, 0, elem.Bits())
		if err != nil {
			return nil, true, fmt.Errorf("value %q is not a valid integer", text)
		}
		v.SetInt(parsed)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseUint(text, 0, elem.Bits())
		if err != nil {
			return nil, true, fmt.Errorf("value %q is not a valid unsigned integer", text)
		}
		v.SetUint(parsed)
	case reflect.Float32, reflect.Float64:
		parsed, err := strconv.ParseFloat(text, elem.Bits())
		if err != nil {
			return nil, true, fmt.Errorf("value %q is not a valid number", text)
		}
		v.SetFloat(parsed)
	default:
		return nil, false, nil
	}
	return v.Interface(), true, nil
}

// describe validates the target and flattens its type into a schema.
func describe(target any) (*schema, error) {
	rv := reflect.ValueOf(target)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: want a non-nil pointer to a struct, got %T", ErrInvalidTarget, target)
	}

	s := &schema{}
	if err := s.walk(rv.Type().Elem(), nil, nil, map[reflect.Type]bool{}); err != nil {
		return nil, err
	}

	s.byKey = make(map[string]*field, len(s.fields))
	for i := range s.fields {
		s.byKey[s.fields[i].key] = &s.fields[i]
	}
	return s, nil
}

// walk appends one field entry per leaf of t, descending into nested structs and
// stopping at any type a source can supply directly. The visiting set breaks
// recursive types. An embedded struct is not flattened away: it contributes its
// type name as a key segment exactly like a named field would, which keeps the
// key paths the loader advertises identical to the ones it can actually fill.
func (s *schema) walk(t reflect.Type, prefix []string, index []int, visiting map[reflect.Type]bool) error {
	if visiting[t] {
		return nil
	}
	visiting[t] = true
	defer delete(visiting, t)

	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}

		name, pinned, required, derive, skip, err := parseTag(sf)
		if err != nil {
			return err
		}
		if skip {
			continue
		}

		idx := append(slices.Clip(index), i)
		path := append(slices.Clip(prefix), name)
		if inner, nested := structType(sf.Type); nested {
			if err := s.walk(inner, path, idx, visiting); err != nil {
				return err
			}
			continue
		}

		s.fields = append(s.fields, field{
			key:      strings.Join(path, KeyDelimiter),
			index:    idx,
			typ:      sf.Type,
			pinned:   pinned,
			required: required,
			derive:   derive,
			subKeys:  acceptsSubKeys(sf.Type),
		})
		if derive {
			s.derives++
		}
	}
	return nil
}

// parseTag reads a field's config tag, yielding its key segment, its pinned
// environment variable name when the tag carries one, the remaining options,
// and whether the field is one the loader fills with key material.
func parseTag(sf reflect.StructField) (name, pinned string, required, derive, skip bool, err error) {
	name = strings.ToLower(sf.Name)
	tag, ok := sf.Tag.Lookup(TagName)
	if !ok {
		return name, "", false, false, false, nil
	}

	for _, opt := range strings.Split(tag, tagOptionSeparator) {
		opt = strings.TrimSpace(opt)
		switch {
		case opt == "":
		case opt == tagSkip:
			return "", "", false, false, true, nil
		case opt == tagRequired:
			required = true
		case opt == tagDerive:
			// A repeated token leaves what the first one meant undecidable, and
			// a field of any other type cannot hold the 32 bytes the option
			// promises, so both are refused with the target's own sentence.
			if derive {
				return "", "", false, false, false, fmt.Errorf("%w: field %s repeats the %s tag option; it takes no value and appears at most once",
					ErrInvalidTarget, sf.Name, tagDerive)
			}
			if sf.Type != byteSliceType {
				return "", "", false, false, false, fmt.Errorf("%w: field %s carries the %s tag option but is typed %s, and only a []byte field can hold key material",
					ErrInvalidTarget, sf.Name, tagDerive, sf.Type)
			}
			derive = true
		case strings.HasPrefix(opt, tagEnvPrefix):
			// A pin without a name is a misspelling of the option itself, not a
			// request to read an unnamed variable, so it is rejected here.
			if pinned != "" || opt == tagEnvPrefix {
				return "", "", false, false, false, fmt.Errorf("%w: field %s has a malformed or repeated %s tag option %q; it needs exactly one variable name, as in %sNAME",
					ErrInvalidTarget, sf.Name, TagName, opt, tagEnvPrefix)
			}
			pinned = strings.TrimPrefix(opt, tagEnvPrefix)
		default:
			return "", "", false, false, false, fmt.Errorf("%w: field %s has unknown %s tag option %q",
				ErrInvalidTarget, sf.Name, TagName, opt)
		}
	}
	return name, pinned, required, derive, false, nil
}

// structType reports whether a field is a struct the loader should descend
// into, returning the struct type to descend into. Pointers are followed, and
// struct types a source can supply as a single scalar are not descended into.
func structType(t reflect.Type) (reflect.Type, bool) {
	if isScalarStruct(t) {
		return nil, false
	}
	elem := deref(t)
	if elem.Kind() != reflect.Struct {
		return nil, false
	}
	return elem, true
}

// isScalarStruct reports whether a struct type is filled from a single value
// rather than from nested keys, which is the case for time.Time and for
// anything that knows how to parse itself from text.
func isScalarStruct(t reflect.Type) bool {
	elem := deref(t)
	return elem == timeType ||
		elem.Implements(textUnmarshalerType) ||
		reflect.PointerTo(elem).Implements(textUnmarshalerType)
}

// acceptsSubKeys reports whether nested keys underneath a leaf belong to it,
// which is how a map-typed field is populated from a config file.
func acceptsSubKeys(t reflect.Type) bool {
	switch deref(t).Kind() {
	case reflect.Map, reflect.Interface:
		return true
	default:
		return false
	}
}

// acceptsEmpty reports whether an empty value is one a field of type t can
// genuinely hold, rather than one weak typing would coerce into t's zero value.
// A string field holds it, and so does a field typed any; an int, a float, a
// bool, a duration or a nested struct has no representation for it. A type that
// parses itself from text answers for itself, by being handed the empty text.
func acceptsEmpty(t reflect.Type) bool {
	t = deref(t)
	if u, ok := reflect.New(t).Interface().(encoding.TextUnmarshaler); ok {
		return u.UnmarshalText([]byte("")) == nil
	}
	switch t.Kind() {
	case reflect.String, reflect.Interface:
		return true
	default:
		return false
	}
}

// deref resolves a type through any number of pointers.
func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// fieldValue follows an index path from the root struct value. It reports false
// when a nil pointer breaks the path, which means the leaf holds no value.
func fieldValue(root reflect.Value, index []int) (reflect.Value, bool) {
	v := root
	for _, i := range index {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		v = v.Field(i)
	}
	return v, true
}
