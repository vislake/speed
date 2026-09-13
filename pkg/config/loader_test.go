package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/pkg/core"
)

type loadOptions struct {
	Addr     string
	TTL      time.Duration
	Salutate string
}

// loaderSetup builds a registry carrying a host identity, one declaring
// module, a transport and a format, and returns a loader over it together with
// the two streams and the transport's call counter.
type loaderSetup struct {
	loader *loader
	stdout *strings.Builder
	stderr *strings.Builder
	// fetches counts what the transport was asked for, which is how a case
	// shows that something happened before the primary source was read.
	fetches *int
}

func newLoaderSetup(t *testing.T, host HostIdentity, content map[string]any, args, environ []string, extra ...core.Module) loaderSetup {
	t.Helper()
	fetches := 0
	defaults := loadOptions{Addr: "localhost:6379", TTL: 5 * time.Minute, Salutate: "Hello"}
	reg := core.New()
	reg.Register(identifying("host", host))
	reg.Register(providing("source", fakeSource{scheme: "file", data: []byte("x"), format: "yaml", calls: &fetches}))
	reg.Register(providing("format", fakeFormat{name: "yaml", content: content}))
	reg.Register(declaring("cache", Schema{
		Namespace: "cache",
		Mounts:    []Mount{{Value: &defaults}},
		Items: map[string]Item{
			"salutate": {Origins: OriginPrimary | OriginEnv | OriginFlag, FlagName: "salutation"},
		},
	}))
	for _, m := range extra {
		reg.Register(m)
	}
	s := loaderSetup{stdout: &strings.Builder{}, stderr: &strings.Builder{}, fetches: &fetches}
	s.loader = &loader{reg: reg, args: args, environ: environ, stdout: s.stdout, stderr: s.stderr}
	return s
}

func decodeCache(t *testing.T, r Reader) loadOptions {
	t.Helper()
	var opts loadOptions
	if err := r.Decode("cache", &opts); err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	return opts
}

// TestLoadAppliesEveryLayerInOrder pins the whole stack: the declared
// defaults, then the primary source, then the environment, then the command
// line, each overriding the one below it and no layer touching what a higher
// one already settled.
func TestLoadAppliesEveryLayerInOrder(t *testing.T) {
	s := newLoaderSetup(t,
		HostIdentity{Prefix: "MYAPP", DefaultLocator: "file:///app.yaml"},
		map[string]any{"cache": map[string]any{"addr": "from-file:1", "ttl": "1m", "salutate": "from-file"}},
		[]string{"--salutation=from-flag"},
		[]string{"MYAPP_CACHE__SALUTATE=from-env", "MYAPP_CACHE__ADDR=from-env:2"},
	)
	r, err := s.loader.load(t.Context())
	if err != nil {
		t.Fatalf("loading failed: %v", err)
	}
	opts := decodeCache(t, r)
	if opts.TTL != time.Minute {
		t.Fatalf("cache.ttl decoded to %v, want the primary source's value: no higher layer gave it one", opts.TTL)
	}
	if opts.Addr != "from-env:2" {
		t.Fatalf("cache.addr decoded to %q, want the environment's value over the file's", opts.Addr)
	}
	if opts.Salutate != "from-flag" {
		t.Fatalf("cache.salutate decoded to %q, want the command line's value over the other two", opts.Salutate)
	}
}

// TestNoPrimarySourceStillLoads pins that a run with no locator at all is a
// legal configuration, the other layers carrying it on their own.
func TestNoPrimarySourceStillLoads(t *testing.T) {
	s := newLoaderSetup(t, HostIdentity{Prefix: "MYAPP"}, nil, nil, nil)
	r, err := s.loader.load(t.Context())
	if err != nil {
		t.Fatalf("loading without a primary config source failed: %v", err)
	}
	if *s.fetches != 0 {
		t.Fatalf("the transport was asked for something %d time(s) with no locator given", *s.fetches)
	}
	if got := decodeCache(t, r).Addr; got != "localhost:6379" {
		t.Fatalf("cache.addr decoded to %q, want the declared default", got)
	}
}

// TestLocatorFromEnvVar pins the middle leg of the locator's three steps.
func TestLocatorFromEnvVar(t *testing.T) {
	s := newLoaderSetup(t,
		HostIdentity{Prefix: "MYAPP"},
		map[string]any{"cache": map[string]any{"addr": "from-file:1"}},
		nil,
		[]string{"MYAPP_CONFIG=file:///app.yaml"},
	)
	r, err := s.loader.load(t.Context())
	if err != nil {
		t.Fatalf("loading failed: %v", err)
	}
	if got := decodeCache(t, r).Addr; got != "from-file:1" {
		t.Fatalf("cache.addr decoded to %q, want the file the variable pointed at", got)
	}
}

// TestLocatorKeysNotInConfigData pins that the two reserved names stay out of
// the config data. Were they to go in, the unknown-key check would trip over
// this module's own names first.
func TestLocatorKeysNotInConfigData(t *testing.T) {
	s := newLoaderSetup(t,
		HostIdentity{Prefix: "MYAPP"},
		map[string]any{},
		[]string{"--config=file:///app.yaml"},
		[]string{"MYAPP_CONFIG=file:///app.yaml"},
	)
	r, err := s.loader.load(t.Context())
	if err != nil {
		t.Fatalf("loading failed: %v", err)
	}
	held := r.(*reader)
	for _, path := range []string{"config", "help", "MYAPP_CONFIG"} {
		if _, present := held.data.values[path]; present {
			t.Fatalf("the config data carries %q, which is a reserved name and not an input item", path)
		}
	}
	if strings.Contains(s.stderr.String(), "MYAPP_CONFIG") {
		t.Fatalf("the diagnostics report the locator variable as unread:\n%s", s.stderr)
	}
}

// TestHelpShortCircuitsBeforeFetch pins that help is answered before the
// primary source is read, so the output does not depend on any external source
// being reachable.
func TestHelpShortCircuitsBeforeFetch(t *testing.T) {
	s := newLoaderSetup(t,
		HostIdentity{Prefix: "MYAPP", DefaultLocator: "file:///app.yaml"},
		map[string]any{},
		[]string{"--help"},
		nil,
	)
	r, err := s.loader.load(t.Context())
	if !errors.Is(err, ErrHelpRequested) {
		t.Fatalf("asking for help returned %v, want ErrHelpRequested", err)
	}
	if r != nil {
		t.Fatal("asking for help produced a reader as well")
	}
	if *s.fetches != 0 {
		t.Fatalf("the transport was asked for something %d time(s) while rendering help", *s.fetches)
	}
}

// TestHelpGoesToStdoutAndTheSentinelCarriesNoText pins where the help output
// goes. Standard output is where a command-line program puts it, diagnostics
// and errors going to standard error, and the sentinel stays bare so that a
// wrapping of it does not produce a second copy of the whole page.
func TestHelpGoesToStdoutAndTheSentinelCarriesNoText(t *testing.T) {
	s := newLoaderSetup(t, HostIdentity{Prefix: "MYAPP"}, nil, []string{"--help"}, nil)
	_, err := s.loader.load(t.Context())
	if !errors.Is(err, ErrHelpRequested) {
		t.Fatalf("asking for help returned %v, want ErrHelpRequested", err)
	}
	if !strings.Contains(s.stdout.String(), "--salutation") {
		t.Fatalf("the help output did not reach standard output:\n%s", s.stdout)
	}
	if s.stderr.Len() != 0 {
		t.Fatalf("standard error carries %q, and the help output belongs on standard output alone", s.stderr)
	}
	if strings.Contains(err.Error(), "--salutation") {
		t.Fatalf("the error reads %q, and carrying the page would reproduce it at every wrapping", err)
	}
}

// brokenWriter is a stdout whose other end has gone away, which is what
// `--help | head` leaves behind once head has read its lines and exited.
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("stdout is gone") }

// TestHelpSurvivesAWriteThatFails pins that failing to write the help output
// does not change what Load returns. Help was asked for and has been answered
// either way, and the usual way the write fails is a reader that closed the
// pipe early -- ordinary use of a command-line program, not a startup that
// went wrong.
func TestHelpSurvivesAWriteThatFails(t *testing.T) {
	s := newLoaderSetup(t, HostIdentity{Prefix: "MYAPP"}, nil, []string{"--help"}, nil)
	s.loader.stdout = brokenWriter{}

	r, err := s.loader.load(t.Context())
	if !errors.Is(err, ErrHelpRequested) {
		t.Fatalf("help whose output could not be written returned %v, want ErrHelpRequested", err)
	}
	if r != nil {
		t.Fatal("asking for help produced a reader as well")
	}
	if s.stderr.Len() != 0 {
		t.Fatalf("standard error carries %q; a broken pipe on help is not a diagnostic", s.stderr)
	}
}

// TestConflictDetectedBeforeFetch pins that collection comes first: two
// modules claiming the same path stop the startup before anything is read.
func TestConflictDetectedBeforeFetch(t *testing.T) {
	clash := declaring("other", Schema{
		Namespace: "cache",
		Mounts:    []Mount{{Value: &struct{ Addr string }{}}},
	})
	s := newLoaderSetup(t,
		HostIdentity{Prefix: "MYAPP", DefaultLocator: "file:///app.yaml"},
		map[string]any{},
		nil, nil,
		clash,
	)
	_, err := s.loader.load(t.Context())
	if !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("two modules claiming one path returned %v, want ErrConfigConflict", err)
	}
	if *s.fetches != 0 {
		t.Fatalf("the transport was asked for something %d time(s) despite the conflict", *s.fetches)
	}
}

// TestUnknownEnvDiagnosticsOnStderr pins the one origin that reports rather
// than fails, and the shape of the line it writes: it leads with the party
// that produced it, since several writers share a stream the host cannot
// redirect.
func TestUnknownEnvDiagnosticsOnStderr(t *testing.T) {
	s := newLoaderSetup(t,
		HostIdentity{Prefix: "MYAPP"},
		nil, nil,
		[]string{"MYAPP_SERVICE_HOST=10.0.0.1"},
	)
	if _, err := s.loader.load(t.Context()); err != nil {
		t.Fatalf("a variable under the prefix that no item reads failed the startup: %v", err)
	}
	line := s.stderr.String()
	if !strings.HasPrefix(line, "config: ") {
		t.Fatalf("the diagnostic reads %q, and a line on a shared stream has to name its writer", line)
	}
	if !strings.Contains(line, "MYAPP_SERVICE_HOST") {
		t.Fatalf("the diagnostic reads %q, which does not name the variable", line)
	}
}

// TestPrimarySourceUnknownKeyStopsTheStartup pins that the checks reach
// through the whole load, not only the unit that applies a layer.
func TestPrimarySourceUnknownKeyStopsTheStartup(t *testing.T) {
	s := newLoaderSetup(t,
		HostIdentity{Prefix: "MYAPP", DefaultLocator: "file:///app.yaml"},
		//nolint:misspell // "addres" is the unknown-key input under test, for
		// the reason data_test.go's identical case gives.
		map[string]any{"cache": map[string]any{"addres": "typo"}},
		nil, nil,
	)
	if _, err := s.loader.load(t.Context()); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("a misspelled key in the primary source returned %v, want ErrUnknownKey", err)
	}
}
