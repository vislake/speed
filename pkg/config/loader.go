package config

import (
	"context"
	"fmt"
	"io"

	"github.com/vislake/speed/pkg/core"
)

// diagnosticPrefix opens every diagnostic line this module writes. Startup
// diagnostics have several writers and all of them go to os.Stderr, which a
// host cannot redirect, so each line leads with the party that produced it.
const diagnosticPrefix = "config: "

// loader runs the load of one registry. The command line, the environment and
// the two streams are fields rather than reads of the process, so a test
// drives a whole load without touching global state.
type loader struct {
	reg     *core.Registry
	args    []string
	environ []string
	stdout  io.Writer
	stderr  io.Writer
}

// load produces the config data of a run and the reader over it.
//
// Collection comes before reading: without the whole manifest there is no way
// to tell an unknown key from a misspelled one, and no way to know which
// arguments the command line consists of. The layers then go on in order,
// each overriding the one below it, and any of them may be absent, including
// the primary config source.
func (l *loader) load(ctx context.Context) (Reader, error) {
	m, err := newManifest(l.reg)
	if err != nil {
		return nil, err
	}
	t, err := collectTransports(l.reg)
	if err != nil {
		return nil, err
	}

	flags, err := parseFlags(m, l.args)
	if err != nil {
		return nil, err
	}
	// Help is answered before the primary source is read, so the output does
	// not depend on any external source being reachable. Rendering returns
	// rather than exits: core is called by the host and does not call it, and
	// leaving through the door would skip the host's own cleanup.
	if flags.help {
		if renderErr := renderHelp(l.stdout, m); renderErr != nil {
			return nil, fmt.Errorf("config: writing the help output failed: %w", renderErr)
		}
		return nil, ErrHelpRequested
	}

	environ := environMap(l.environ)
	d := newData(m)
	if locator := locate(m, environ, flags); locator != "" {
		content, readErr := t.read(ctx, locator)
		if readErr != nil {
			return nil, readErr
		}
		if applyErr := d.applyPrimary(m, content); applyErr != nil {
			return nil, applyErr
		}
	}

	unclaimed, err := d.applyEnv(m, l.environ)
	if err != nil {
		return nil, err
	}
	l.reportUnclaimed(m, unclaimed)

	if err := d.applyFlags(m, flags); err != nil {
		return nil, err
	}
	return &reader{manifest: m, data: d}, nil
}

// reportUnclaimed lists the variables under the host's prefix that no input
// item reads. They are a diagnostic and not a failure: the process environment
// is not the host's alone, and an orchestrator injecting a variable of its own
// must not be able to stop the process.
func (l *loader) reportUnclaimed(m *manifest, names []string) {
	for _, name := range names {
		//nolint:errcheck // l.stderr is the diagnostic sink itself: a failed
		// write has nowhere to be reported, and this line is explicitly not a
		// failure condition -- see the doc comment above.
		fmt.Fprintf(l.stderr, "%senvironment variable %s is set under the %q prefix and no input "+
			"item reads it, so it is ignored\n", diagnosticPrefix, name, m.host.Prefix)
	}
}
