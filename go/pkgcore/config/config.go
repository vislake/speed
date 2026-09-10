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
// startup input: they are resolved once on the four-source chain below and read
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
// Values are resolved from four sources. Highest priority wins:
//
//  1. command-line flags   --database.dsn=postgres://...
//  2. environment variables SPEED_DATABASE__DSN=postgres://...
//  3. a config file        an optional YAML (or JSON) file
//  4. struct defaults      whatever the caller already set on the target
//
// The fourth source is implicit: Load only writes fields that some source
// actually supplied, so a field left untouched keeps the value the caller
// assigned before calling Load.
//
// The environment source is read under the loader's prefix, SPEED_ by default;
// WithEnvPrefix replaces it, so a host whose variables carry another prefix
// (APP_, say) drives the same loader without renaming a single variable.
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

// TagName is the struct tag read by the loader for per-field options.
const TagName = "config"

// Tag options recognised inside a TagName struct tag.
const (
	tagRequired = "required"
	tagSkip     = "-"
	// tagEnvPrefix introduces the option that pins a field's exact environment
	// variable name: config:"env=PORT".
	tagEnvPrefix = "env="
	// tagOptionSeparator splits a tag into its comma-separated options.
	tagOptionSeparator = ","
)

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
)

var (
	anyType             = reflect.TypeOf((*any)(nil)).Elem()
	timeType            = reflect.TypeOf(time.Time{})
	durationType        = reflect.TypeOf(time.Duration(0))
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

// source identifies which layer of the priority chain supplied a value.
type source int

const (
	sourceDefault source = iota
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
	case sourceDefault:
		return "the target struct"
	default:
		return "an unknown source"
	}
}

// Loader resolves bootstrap configuration from flags, the environment, an
// optional config file and the defaults already present on the target struct.
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
// source entirely.
func WithEnviron(environ []string) Option {
	return func(l *Loader) {
		l.environ = environ
		l.environSet = true
	}
}

// Load resolves the configuration into target, which must be a non-nil pointer
// to a struct. Fields that no source supplies keep the value they already hold,
// which is how struct defaults participate as the lowest-priority source.
//
// Load returns an error wrapping ErrMissingValue if a field tagged as required
// is still zero afterwards, ErrInvalidValue if a supplied value does not fit the
// field it maps to, ErrSourceUnreadable if a source could not be read, and
// ErrInvalidTarget if target is not a usable struct pointer. When Load returns
// an error, target may already have been partially written and must not be
// used; bootstrap configuration is meant to abort process startup, not to be
// salvaged.
func (l *Loader) Load(target any) error {
	schema, err := describe(target)
	if err != nil {
		return err
	}
	if err = l.resolveEnvNames(schema); err != nil {
		return err
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

// sourcesFor lists, in priority order, every place the loader looked for a key,
// so the reader of an error knows exactly where to put the missing value. The
// environment name it names is the one this loader actually read -- a field's
// pinned name when it has one, its prefix-derived name otherwise -- never a
// hardcoded SPEED_ spelling.
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
// this loader: the prefix, then the key uppercased with each level of nesting
// marked by EnvSeparator.
func (l *Loader) derivedEnvName(key string) string {
	return l.prefix + strings.ToUpper(strings.ReplaceAll(key, KeyDelimiter, EnvSeparator))
}

// field is one leaf of the target struct: a value a source can actually supply.
type field struct {
	key      string       // dotted key path, for example "database.dsn"
	index    []int        // field index path from the root struct
	typ      reflect.Type // the leaf's own type, which a supplied value must fit
	pinned   string       // the exact environment variable name, tagged config:"env=NAME"; empty when derived
	required bool         // tagged config:"required"
	subKeys  bool         // a map-like leaf, so keys nested under it belong to it
}

// schema is the flattened description of a target struct.
type schema struct {
	fields []field
	byKey  map[string]*field
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

		name, pinned, required, skip, err := parseTag(sf)
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
			subKeys:  acceptsSubKeys(sf.Type),
		})
	}
	return nil
}

// parseTag reads a field's config tag, yielding its key segment, its pinned
// environment variable name when the tag carries one, and the remaining
// options.
func parseTag(sf reflect.StructField) (name, pinned string, required, skip bool, err error) {
	name = strings.ToLower(sf.Name)
	tag, ok := sf.Tag.Lookup(TagName)
	if !ok {
		return name, "", false, false, nil
	}

	for _, opt := range strings.Split(tag, tagOptionSeparator) {
		opt = strings.TrimSpace(opt)
		switch {
		case opt == "":
		case opt == tagSkip:
			return "", "", false, true, nil
		case opt == tagRequired:
			required = true
		case strings.HasPrefix(opt, tagEnvPrefix):
			// A pin without a name is a misspelling of the option itself, not a
			// request to read an unnamed variable, so it is rejected here.
			if pinned != "" || opt == tagEnvPrefix {
				return "", "", false, false, fmt.Errorf("%w: field %s has a malformed or repeated %s tag option %q; it needs exactly one variable name, as in %sNAME",
					ErrInvalidTarget, sf.Name, TagName, opt, tagEnvPrefix)
			}
			pinned = strings.TrimPrefix(opt, tagEnvPrefix)
		default:
			return "", "", false, false, fmt.Errorf("%w: field %s has unknown %s tag option %q",
				ErrInvalidTarget, sf.Name, TagName, opt)
		}
	}
	return name, pinned, required, false, nil
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
