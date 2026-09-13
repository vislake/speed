package log

import (
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/config"
)

// ptr is the way a test writes an explicitly given optional parameter.
func ptr(v int) *int { return &v }

// TestOptionalParamsDistinguishAbsentFromZero pins the three states of an
// optional file parameter. A value type could only express two of them: the
// zero value would stand both for a key nobody wrote and for a key written as
// 0, and 0 is how a host switches rotation or pruning off.
//
// This test does not compile against value-typed fields, which is the point.
func TestOptionalParamsDistinguishAbsentFromZero(t *testing.T) {
	for _, tc := range []struct {
		name  string
		given *int
		def   int
		want  int
	}{
		{"absent takes the default", nil, defaultMaxSizeMB, defaultMaxSizeMB},
		{"an explicit zero switches the item off", ptr(0), defaultMaxSizeMB, 0},
		{"a positive number is taken as given", ptr(7), defaultMaxSizeMB, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := paramOrDefault(tc.given, tc.def); got != tc.want {
				t.Errorf("paramOrDefault(%v, %d) = %d, want %d", tc.given, tc.def, got, tc.want)
			}
		})
	}

	// The same three states through the whole of one output, so that a fill
	// that is right in isolation but never reached still fails.
	file := func(size, files, age *int) resolvedOutput {
		t.Helper()
		out, err := Output{
			To: destFile, Format: formatJSON, Path: "/tmp/app.log",
			MaxSizeMB: size, MaxFiles: files, MaxAgeDays: age,
		}.resolve(0)
		if err != nil {
			t.Fatalf("resolving a legal file output failed: %v", err)
		}
		return out
	}
	absent := file(nil, nil, nil)
	if absent.maxSizeMB != defaultMaxSizeMB || absent.maxFiles != defaultMaxFiles || absent.maxAgeDays != defaultMaxAgeDays {
		t.Errorf("an output that gives no parameters resolved to %+v, want the documented defaults %d/%d/%d",
			absent, defaultMaxSizeMB, defaultMaxFiles, defaultMaxAgeDays)
	}
	off := file(ptr(0), ptr(0), ptr(0))
	if off.maxSizeMB != 0 || off.maxFiles != 0 || off.maxAgeDays != 0 {
		t.Errorf("an output that gives 0 for every parameter resolved to %+v, want every item off", off)
	}
	given := file(ptr(5), ptr(3), ptr(1))
	if given.maxSizeMB != 5 || given.maxFiles != 3 || given.maxAgeDays != 1 {
		t.Errorf("an output that gives 5/3/1 resolved to %+v, want those values", given)
	}
}

// TestDefaultConfigPrototypeIsOneStdoutTextOutput pins what a host gets when
// it writes no log section at all: one text output to stdout, at info. It is
// the most common configuration there is, and the assembly has to reach it
// without anybody writing it down.
func TestDefaultConfigPrototypeIsOneStdoutTextOutput(t *testing.T) {
	if configDefaults.Level != "info" {
		t.Errorf("the prototype's level is %q, want info", configDefaults.Level)
	}
	if len(configDefaults.Outputs) != 1 {
		t.Fatalf("the prototype carries %d outputs, want exactly one", len(configDefaults.Outputs))
	}
	o := configDefaults.Outputs[0]
	if o.To != destStdout || o.Format != formatText {
		t.Errorf("the prototype's output is %+v, want to=stdout format=text", o)
	}
	resolved, err := configDefaults.resolve()
	if err != nil {
		t.Fatalf("the prototype does not resolve: %v", err)
	}
	if resolved.level != slog.LevelInfo {
		t.Errorf("the prototype resolves to level %v, want info", resolved.level)
	}
}

// TestSchemaDeclaresOutputsAsPrimaryOnly keeps the output list off the
// environment and the command line. It is a list of structs, which has no flat
// form those layers could give, and declaring either of them on such an item
// is ErrInvalidSchema at load time. That failure is only reachable from a real
// assembly, so the declaration is pinned here instead.
func TestSchemaDeclaresOutputsAsPrimaryOnly(t *testing.T) {
	s := schema()
	outputs, ok := s.Items["outputs"]
	if !ok {
		t.Fatalf("the schema declares no item for outputs; items: %v", reflect.ValueOf(s.Items).MapKeys())
	}
	if outputs.Origins != config.OriginPrimary {
		t.Errorf("outputs declares origins %b, want OriginPrimary alone (%b)", outputs.Origins, config.OriginPrimary)
	}
	level, ok := s.Items["level"]
	if !ok {
		t.Fatal("the schema declares no item for level")
	}
	for _, want := range []config.Origin{config.OriginPrimary, config.OriginEnv, config.OriginFlag} {
		if level.Origins&want == 0 {
			t.Errorf("level does not accept origin %b; the design gives it all three", want)
		}
	}
	if level.FlagName == "" {
		t.Error("level accepts the command line and gives no FlagName, which is ErrInvalidSchema at load time")
	}
}

// TestSchemaItemKeysMatchCarrierFields catches a misspelled Items key. config
// refuses a key that reaches no field of the mounts, and that refusal is only
// reachable from a real assembly.
func TestSchemaItemKeysMatchCarrierFields(t *testing.T) {
	fields := map[string]bool{}
	ct := reflect.TypeOf(Config{})
	for i := range ct.NumField() {
		fields[ct.Field(i).Tag.Get("config")] = true
	}
	for key := range schema().Items {
		if !fields[key] {
			t.Errorf("the schema declares the item %q, which matches no config tag of Config", key)
		}
	}
	mounts := schema().Mounts
	if len(mounts) != 1 || mounts[0].Path != "" || mounts[0].Value != &configDefaults {
		t.Errorf("the schema mounts %+v, want the prototype alone at the namespace root", mounts)
	}
}

// TestResolveRejects walks the design's error table: every defect names its own
// sentinel, so a host reading the failure knows which kind of edit to make.
func TestResolveRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want error
	}{
		{
			"an unknown level",
			Config{Level: "verbose"},
			ErrInvalidLevel,
		},
		{
			"an unknown destination",
			Config{Level: "info", Outputs: []Output{{To: "syslog", Format: formatText}}},
			ErrUnknownDestination,
		},
		{
			"an unknown format",
			Config{Level: "info", Outputs: []Output{{To: destStdout, Format: "logfmt"}}},
			ErrUnknownFormat,
		},
		{
			"a file output without a path",
			Config{Level: "info", Outputs: []Output{{To: destFile, Format: formatJSON}}},
			ErrMissingPath,
		},
		{
			"a negative size",
			Config{Level: "info", Outputs: []Output{{To: destFile, Format: formatJSON, Path: "/tmp/a.log", MaxSizeMB: ptr(-1)}}},
			ErrInvalidFileParam,
		},
		{
			"a negative retention count",
			Config{Level: "info", Outputs: []Output{{To: destFile, Format: formatJSON, Path: "/tmp/a.log", MaxFiles: ptr(-2)}}},
			ErrInvalidFileParam,
		},
		{
			"a negative retention age",
			Config{Level: "info", Outputs: []Output{{To: destFile, Format: formatJSON, Path: "/tmp/a.log", MaxAgeDays: ptr(-3)}}},
			ErrInvalidFileParam,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.resolve()
			if !errors.Is(err, tc.want) {
				t.Fatalf("resolving %+v gave %v, want %v", tc.cfg, err, tc.want)
			}
		})
	}
}

// TestUnknownNameErrorsListEveryLegalValue keeps the fixing action in the
// message. The legal values are a closed set, so listing them is the whole of
// what the host has to know.
func TestUnknownNameErrorsListEveryLegalValue(t *testing.T) {
	_, err := Config{Level: "info", Outputs: []Output{{To: "syslog", Format: formatText}}}.resolve()
	for _, name := range []string{destStdout, destStderr, destFile} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the unknown-destination error %q does not name the legal value %q", err, name)
		}
	}
	_, err = Config{Level: "info", Outputs: []Output{{To: destStdout, Format: "logfmt"}}}.resolve()
	for _, name := range []string{formatText, formatJSON} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the unknown-format error %q does not name the legal value %q", err, name)
		}
	}
}

// TestNegativeFileParamIsItsOwnSentinel keeps a negative number apart from the
// neighbouring failures: the host has to change a number here, not a name or a
// path.
func TestNegativeFileParamIsItsOwnSentinel(t *testing.T) {
	_, err := Config{
		Level:   "info",
		Outputs: []Output{{To: destFile, Format: formatJSON, Path: "/tmp/a.log", MaxFiles: ptr(-1)}},
	}.resolve()
	if !errors.Is(err, ErrInvalidFileParam) {
		t.Fatalf("a negative max-files gave %v, want ErrInvalidFileParam", err)
	}
	for name, other := range sentinels {
		if name == "ErrInvalidFileParam" {
			continue
		}
		if errors.Is(err, other) {
			t.Errorf("a negative max-files also matches %s", name)
		}
	}
	if !strings.Contains(err.Error(), "max-files") {
		t.Errorf("the error %q does not name the field that is wrong", err)
	}
}

// TestLevelNamesMapToSlogLevels pins the closed set of level names, and that
// the comparison folds case: an environment variable written in capitals asks
// for a level, not for a startup failure.
func TestLevelNamesMapToSlogLevels(t *testing.T) {
	for name, want := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"INFO":  slog.LevelInfo,
	} {
		got, err := parseLevel(name)
		if err != nil {
			t.Errorf("parseLevel(%q) failed: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", name, got, want)
		}
	}
	for _, name := range []string{"", "trace", "warning", "info+1"} {
		if _, err := parseLevel(name); !errors.Is(err, ErrInvalidLevel) {
			t.Errorf("parseLevel(%q) gave %v, want ErrInvalidLevel", name, err)
		}
	}
}

// TestResolveKeepsEveryOutputInOrder pins that resolution carries the whole
// list through: a chain built from a truncated list would silently lose a
// destination.
func TestResolveKeepsEveryOutputInOrder(t *testing.T) {
	cfg := Config{Level: "warn", Outputs: []Output{
		{To: destStdout, Format: formatText},
		{To: destStderr, Format: formatJSON},
		{To: destFile, Format: formatJSON, Path: "/tmp/app.log", MaxSizeMB: ptr(0)},
	}}
	got, err := cfg.resolve()
	if err != nil {
		t.Fatalf("resolving a legal configuration failed: %v", err)
	}
	if got.level != slog.LevelWarn {
		t.Errorf("level resolved to %v, want warn", got.level)
	}
	want := []resolvedOutput{
		{to: destStdout, format: formatText, maxSizeMB: defaultMaxSizeMB, maxFiles: defaultMaxFiles, maxAgeDays: defaultMaxAgeDays},
		{to: destStderr, format: formatJSON, maxSizeMB: defaultMaxSizeMB, maxFiles: defaultMaxFiles, maxAgeDays: defaultMaxAgeDays},
		{to: destFile, format: formatJSON, path: "/tmp/app.log", maxSizeMB: 0, maxFiles: defaultMaxFiles, maxAgeDays: defaultMaxAgeDays},
	}
	if !reflect.DeepEqual(got.outputs, want) {
		t.Errorf("outputs resolved to %+v, want %+v", got.outputs, want)
	}
}
