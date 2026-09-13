package log

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"testing"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
)

// stubReader is a config.Reader that hands back a Config the test wrote. The
// real reader is built by config's own New, which reads os.Args unconditionally
// and refuses the arguments go test passes to a test binary, so the callbacks
// are driven through the interface instead.
type stubReader struct {
	cfg  Config
	err  error
	path string // the path the caller asked for, recorded for the assertion
}

// Decode writes the stubbed configuration into target.
func (s *stubReader) Decode(path string, target any) error {
	s.path = path
	if s.err != nil {
		return s.err
	}
	dst, ok := target.(*Config)
	if !ok {
		return fmt.Errorf("stub reader: this module decodes into *log.Config, got %T", target)
	}
	*dst = s.cfg
	return nil
}

var _ config.Reader = (*stubReader)(nil)

// keepBootstrapLevel restores the bootstrap level after a test that sets it.
// The variable is process-level and shared with every other test in the binary.
func keepBootstrapLevel(t *testing.T) {
	t.Helper()
	previous := bootstrapLevel.Level()
	t.Cleanup(func() { bootstrapLevel.Set(previous) })
}

// TestPrepareStatesEnabledNotAuto pins the stance. Resolution only disables a
// provider that claims exclusivity and states StateAuto; this module claims
// exclusivity and must not stand down, so that a second enabled provider fails
// the startup instead of quietly replacing it. Written as StateAuto, every
// other test here still passes and the module gives way.
func TestPrepareStatesEnabledNotAuto(t *testing.T) {
	keepBootstrapLevel(t)
	got, err := prepare(&stubReader{cfg: Config{Level: "info"}})
	if err != nil {
		t.Fatalf("Prepare failed on a legal configuration: %v", err)
	}
	if got.State != core.StateEnabled {
		t.Errorf("Prepare states %v, want StateEnabled: an exclusive provider stating StateAuto "+
			"is the one shape resolution disables", got.State)
	}
}

// TestPrepareSetsBootstrapLevelFromConfig pins that the configured level
// reaches the chain that is already writing records. The bootstrap chain hands
// out loggers before this module is constructed, so the level has to be applied
// to the variable those loggers read, not stored for later.
func TestPrepareSetsBootstrapLevelFromConfig(t *testing.T) {
	keepBootstrapLevel(t)
	bootstrapLevel.Set(slog.LevelInfo)
	if _, err := prepare(&stubReader{cfg: Config{Level: "debug"}}); err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if got := bootstrapLevel.Level(); got != slog.LevelDebug {
		t.Errorf("the bootstrap level is %v after a debug configuration, want debug", got)
	}
}

// TestPrepareReadsTheModuleOwnPath pins the path the module decodes from. It
// assembles the path itself out of what it declared, so a namespace change that
// misses one of the two sites reads an empty section and silently takes the
// defaults.
func TestPrepareReadsTheModuleOwnPath(t *testing.T) {
	keepBootstrapLevel(t)
	reader := &stubReader{cfg: Config{Level: "info"}}
	if _, err := prepare(reader); err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if reader.path != "log" {
		t.Errorf("Prepare decoded from %q, want the module's own section, log", reader.path)
	}
}

// TestPrepareRejectsAnUnknownLevel keeps a misspelled level from starting the
// process on a level nobody asked for.
func TestPrepareRejectsAnUnknownLevel(t *testing.T) {
	keepBootstrapLevel(t)
	bootstrapLevel.Set(slog.LevelWarn)
	_, err := prepare(&stubReader{cfg: Config{Level: "verbose"}})
	if !errors.Is(err, ErrInvalidLevel) {
		t.Fatalf("Prepare gave %v on the level \"verbose\", want ErrInvalidLevel", err)
	}
	if got := bootstrapLevel.Level(); got != slog.LevelWarn {
		t.Errorf("a rejected level still moved the bootstrap level to %v", got)
	}
}

// TestPrepareFailsWhenTheReaderFails keeps a configuration failure from being
// read as a level nobody configured.
func TestPrepareFailsWhenTheReaderFails(t *testing.T) {
	keepBootstrapLevel(t)
	sentinel := errors.New("the primary source could not be read")
	_, err := prepare(&stubReader{err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Prepare gave %v, want the reader's own error", err)
	}
}

// TestMissingConfigReaderFailsPrepare pins that a registry without a config
// module stops the startup. The level is a configured value, and falling back
// to a default here would hide an assembly that cannot read any configuration
// at all.
func TestMissingConfigReaderFailsPrepare(t *testing.T) {
	keepBootstrapLevel(t)
	m := Module()
	if _, err := m.Prepare(context.Background(), core.New()); !errors.Is(err, core.ErrMissingProvider) {
		t.Fatalf("Prepare on a registry without config gave %v, want core.ErrMissingProvider", err)
	}
}

// TestDescriptorDeclaresExclusiveLoggerAndSchema pins the declaration surface:
// one capability, claimed exclusively, plus the input items. The exclusivity is
// what turns a second logging module into a startup failure.
func TestDescriptorDeclaresExclusiveLoggerAndSchema(t *testing.T) {
	m := Module()
	if m.Name != "log" {
		t.Errorf("the module is named %q, want log", m.Name)
	}
	if len(m.Provides) != 1 {
		t.Fatalf("the descriptor declares %d capabilities, want exactly one", len(m.Provides))
	}
	p := m.Provides[0]
	if want := reflect.TypeOf((*Logger)(nil)); reflect.TypeOf(p.Token) != want {
		t.Errorf("the delivered token is %T, want %v", p.Token, want)
	}
	if !p.Exclusive {
		t.Error("the Logger capability is not claimed exclusively, so a second provider would " +
			"be settled by taking one of them rather than by failing the startup")
	}
	if len(m.Requires) != 0 {
		t.Errorf("the descriptor requires %v; config is an implicit dependency of every module "+
			"and this module needs nothing else at construction time", m.Requires)
	}
	if len(m.Resources) != 1 {
		t.Fatalf("the descriptor declares %d resources, want the config schema alone", len(m.Resources))
	}
	got, ok := m.Resources[0].(config.Schema)
	if !ok {
		t.Fatalf("the declared resource is %T, want config.Schema", m.Resources[0])
	}
	if !reflect.DeepEqual(got, schema()) {
		t.Errorf("the declared schema is %+v, want %+v", got, schema())
	}
}

// TestModuleRegistersItselfWithTheProcessRegistry pins that importing the
// package is all a host does: the registration happens in init, so the host
// does not name the descriptor.
func TestModuleRegistersItselfWithTheProcessRegistry(t *testing.T) {
	registered, ok := core.ProcessRegistry.Lookup("log")
	if !ok {
		t.Fatal("importing the package did not register the module with the process registry")
	}
	if len(registered.Provides) != 1 || !registered.Provides[0].Exclusive {
		t.Errorf("the registered descriptor declares %+v, want the one exclusive capability",
			registered.Provides)
	}
}
