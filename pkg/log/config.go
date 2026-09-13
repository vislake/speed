package log

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/vislake/speed/pkg/config"
)

// Config is the set of input items this module accepts. The level belongs to
// configuration rather than to stored settings because it is needed before a
// database is reachable.
type Config struct {
	// Level is the lowest level the chain lets through.
	Level string `config:"level"`
	// Outputs is one entry per destination and format pair.
	Outputs []Output `config:"outputs"`
}

// Output is one output: a destination, a format, and the parameters of that
// destination. A destination that does not use a parameter ignores it, so
// stdout carrying a rotation size does nothing.
type Output struct {
	// To is the destination name, one of the built-in closed set.
	To string `config:"to"`
	// Format is the format name, one of the built-in closed set.
	Format string `config:"format"`
	// Path is the file a file destination writes to.
	Path string `config:"path"`

	// MaxSizeMB is the size a file grows to before it is rotated.
	//
	// The three parameters below are pointers because Outputs is a container
	// leaf: the primary source replaces the whole list rather than merging
	// into it, so a value type could not tell "the key is absent" from "the
	// key is 0", and 0 is the legal way to switch the item off.
	MaxSizeMB *int `config:"max-size-mb"`
	// MaxFiles is how many archives are kept.
	MaxFiles *int `config:"max-files"`
	// MaxAgeDays is how long an archive is kept.
	MaxAgeDays *int `config:"max-age-days"`
}

// The destination names. The set is closed: a new destination is a change to
// this package, not something a host adds.
const (
	destStdout = "stdout"
	destStderr = "stderr"
	destFile   = "file"
)

// The format names, a closed set for the same reason.
const (
	formatText = "text"
	formatJSON = "json"
)

// The defaults of the optional file parameters, applied after decoding rather
// than laid into the config data: the carrier struct's values do not survive
// the wholesale replacement of a container leaf.
const (
	defaultMaxSizeMB  = 100
	defaultMaxFiles   = 10
	defaultMaxAgeDays = 30
)

// configNamespace is where this module's input items are mounted in the config
// data. The module name takes no part in it, so the section does not move if
// the module is renamed.
const configNamespace = "log"

// configPath is the complete path of the carrier struct, namespace plus mount
// path, and is what Decode is given.
const configPath = configNamespace

// configDefaults is the prototype this module declares. It is read for the
// field structure and for the defaults and never receives the values of a run.
//
// The one output it carries is what an assembly gets when the host writes no
// log section at all: logging works with no configuration, the way it does
// before this module is constructed.
var configDefaults = Config{
	Level:   "info",
	Outputs: []Output{{To: destStdout, Format: formatText}},
}

// schema is this module's input item declaration.
//
// outputs takes the primary config source alone. It is a list of structs, and
// such a leaf has no flat form the environment or the command line could give;
// declaring either of them on it is ErrInvalidSchema. The output set is
// therefore changed in the config file or the remote config centre, never by
// an environment variable.
func schema() config.Schema {
	return config.Schema{
		Namespace: configNamespace,
		Mounts:    []config.Mount{{Value: &configDefaults}},
		Items: map[string]config.Item{
			"level": {
				Origins:     config.OriginPrimary | config.OriginEnv | config.OriginFlag,
				FlagName:    "log-level",
				Placeholder: "LEVEL",
				Group:       configGroup,
				Description: "the lowest level that is logged: debug, info, warn or error",
			},
			"outputs": {
				Origins:     config.OriginPrimary,
				Group:       configGroup,
				Description: "the destinations and formats to log to",
			},
		},
	}
}

// configGroup is the section the help output lists this module's items under.
const configGroup = "Logging"

// resolvedConfig is a Config that has been validated and completed: the level
// is a slog.Level and every optional parameter carries a number.
type resolvedConfig struct {
	level   slog.Level
	outputs []resolvedOutput
}

// resolvedOutput is one output with its parameters filled in. A parameter of 0
// means that item is off, which is also what an explicit 0 in the
// configuration means.
type resolvedOutput struct {
	to     string
	format string
	path   string
	// maxSizeMB is the size that triggers a rotation, 0 for no rotation.
	maxSizeMB int
	// maxFiles is the number of archives kept, 0 for no pruning by count.
	maxFiles int
	// maxAgeDays is the age an archive is kept for, 0 for no pruning by age.
	maxAgeDays int
}

// resolve validates the configuration and fills in the optional parameters.
// Every defect it reports aborts the startup, and the message names the output
// it came from: an assembly failure that only says "invalid format" leaves the
// host counting entries in a list.
func (c Config) resolve() (resolvedConfig, error) {
	level, err := parseLevel(c.Level)
	if err != nil {
		return resolvedConfig{}, err
	}
	out := resolvedConfig{level: level, outputs: make([]resolvedOutput, 0, len(c.Outputs))}
	for i, o := range c.Outputs {
		resolved, err := o.resolve(i)
		if err != nil {
			return resolvedConfig{}, err
		}
		out.outputs = append(out.outputs, resolved)
	}
	return out, nil
}

// resolve validates one output. index is its position in the list, which is
// all there is to name it by.
func (o Output) resolve(index int) (resolvedOutput, error) {
	switch o.To {
	case destStdout, destStderr, destFile:
	default:
		return resolvedOutput{}, fmt.Errorf("%w: output %d gives %q", ErrUnknownDestination, index, o.To)
	}
	switch o.Format {
	case formatText, formatJSON:
	default:
		return resolvedOutput{}, fmt.Errorf("%w: output %d, to %s, gives %q",
			ErrUnknownFormat, index, o.To, o.Format)
	}
	if o.To == destFile && o.Path == "" {
		return resolvedOutput{}, fmt.Errorf("%w: output %d writes to a file and needs a path to write to",
			ErrMissingPath, index)
	}
	for _, p := range [...]struct {
		field string
		given *int
	}{
		{"max-size-mb", o.MaxSizeMB},
		{"max-files", o.MaxFiles},
		{"max-age-days", o.MaxAgeDays},
	} {
		if p.given != nil && *p.given < 0 {
			return resolvedOutput{}, fmt.Errorf("%w: output %d gives %s the value %d; "+
				"0 switches the item off and a positive number sets it",
				ErrInvalidFileParam, index, p.field, *p.given)
		}
	}
	return resolvedOutput{
		to:         o.To,
		format:     o.Format,
		path:       o.Path,
		maxSizeMB:  paramOrDefault(o.MaxSizeMB, defaultMaxSizeMB),
		maxFiles:   paramOrDefault(o.MaxFiles, defaultMaxFiles),
		maxAgeDays: paramOrDefault(o.MaxAgeDays, defaultMaxAgeDays),
	}, nil
}

// paramOrDefault reads an optional parameter in its three states: absent takes
// the default, an explicit 0 switches the item off, and a positive number is
// taken as given.
func paramOrDefault(given *int, def int) int {
	if given == nil {
		return def
	}
	return *given
}

// parseLevel maps a configured level name to a slog.Level. The comparison
// folds case, so a host writing INFO in an environment variable gets the level
// it asked for rather than a startup failure over the shift key.
func parseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%w: the configuration gives %q", ErrInvalidLevel, name)
}
