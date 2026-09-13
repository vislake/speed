package config

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/vislake/speed/pkg/core"
)

// transports are the Source and Format implementations this binary carries,
// indexed by the name each one answers for. They arrive as resources rather
// than as capabilities: this module is constructed before any other instance
// exists, so a capability lookup would find nothing, while resources are
// settled before Run and answer the same at every stage.
type transports struct {
	sources map[string]Source
	formats map[string]Format
}

// collectTransports gathers the transports and formats, refusing two that
// answer for the same name. Selection is an exact match rather than a contest
// of priorities, so a duplicate leaves nothing to choose between; it is found
// here, while the declaring modules are still in hand, rather than at the
// moment the primary source is read.
func collectTransports(reg *core.Registry) (*transports, error) {
	t := &transports{sources: map[string]Source{}, formats: map[string]Format{}}
	declaredBy := map[string]string{}
	for _, res := range core.Resources[Source](reg) {
		scheme := res.Value.Scheme()
		if other, taken := declaredBy[scheme]; taken {
			return nil, fmt.Errorf("%w: modules %q and %q both deliver a Source for the scheme "+
				"%q, and a scheme selects one transport exactly. Import one of them",
				ErrConfigConflict, other, res.Module, scheme)
		}
		declaredBy[scheme] = res.Module
		t.sources[scheme] = res.Value
	}
	declaredBy = map[string]string{}
	for _, res := range core.Resources[Format](reg) {
		name := res.Value.Name()
		if other, taken := declaredBy[name]; taken {
			return nil, fmt.Errorf("%w: modules %q and %q both deliver a Format named %q, and a "+
				"format name selects one parser exactly. Import one of them",
				ErrConfigConflict, other, res.Module, name)
		}
		declaredBy[name] = res.Module
		t.formats[name] = res.Value
	}
	return t, nil
}

// locate gives the config locator of this run. It has to be settled before any
// configuration is read and therefore cannot come out of the configuration
// itself: the host's default, then <PREFIX>_CONFIG, then --config, each
// overriding the one before it.
//
// With no prefix the environment leg does not apply at all: <PREFIX>_CONFIG is
// a derived name and there is nothing to derive it from. The locator then
// comes from the host's default or the command line.
//
// All three being silent is a legal configuration: there is no primary config
// source, and the defaults, the environment and the command line carry the run
// on their own.
func locate(m *manifest, environ map[string]string, flags *parsedFlags) string {
	locator := m.host.DefaultLocator
	if m.host.Prefix != "" {
		if given, set := environ[locatorEnvName(m.host.Prefix)]; set {
			locator = given
		}
	}
	if flags.hasLocator {
		locator = flags.locator
	}
	return locator
}

// read fetches the primary config source and parses it. The transport answers
// "where do the bytes come from" and the format answers "how are they read",
// and the two are chosen independently: a file source and a remote config
// centre share the same parsers.
func (t *transports) read(ctx context.Context, locator string) (map[string]any, error) {
	parsed, err := url.Parse(locator)
	if err != nil {
		return nil, fmt.Errorf("%w: %q does not parse as a URI: %w", ErrMalformedLocator, locator, err)
	}
	if parsed.Scheme == "" {
		return nil, fmt.Errorf("%w: %q names no scheme, and the scheme is what selects the "+
			"transport. A local file is written file:///etc/app.yaml", ErrMalformedLocator, locator)
	}
	source, known := t.sources[parsed.Scheme]
	if !known {
		// The path is the scheme appended to the transport subpackage prefix,
		// taken exactly as url.Parse produced it: the standard library has
		// already lowercased the scheme and held it to ALPHA followed by
		// [a-z0-9+.-], so normalising it again would only make the name in the
		// message disagree with the key the lookup just failed on.
		return nil, fmt.Errorf("%w: %q names the scheme %q, which no imported Source answers for. "+
			"Import a transport that answers for it: by convention that is "+
			"github.com/vislake/speed/pkg/config/source/%s. The convention is a lead, not a "+
			"guarantee: an implementation is free to live elsewhere, and this module cannot "+
			"enumerate the ones that exist",
			ErrUnknownScheme, locator, parsed.Scheme, parsed.Scheme)
	}

	raw, formatName, err := source.Fetch(ctx, parsed)
	if err != nil {
		return nil, fetchError(locator, err)
	}
	if formatName == "" {
		return nil, fmt.Errorf("%w: the transport for %q reported no format name. A file source "+
			"reads it off the extension and a remote one off the format query parameter; neither "+
			"guesses from the content, because valid JSON is also valid YAML",
			ErrUndeterminedFormat, locator)
	}
	format, known := t.formats[formatName]
	if !known {
		// The name is quoted because a transport reports whatever it was told:
		// on the remote leg that is the operator's own format query parameter,
		// which may carry spaces, slashes or upper case. The path is built
		// from that same string unconditionally, with no test of whether it
		// looks like a path segment, and the lead below covers the odd path a
		// degenerate name produces.
		return nil, fmt.Errorf("%w: %q is in the %q format, which no imported Format parses. "+
			"Import a parser for it: by convention that is "+
			"github.com/vislake/speed/pkg/config/format/%s. The convention is a lead, not a "+
			"guarantee: an implementation is free to live elsewhere, and this module cannot "+
			"enumerate the ones that exist",
			ErrUnknownFormat, locator, formatName, formatName)
	}

	content, err := format.Unmarshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %q does not parse as %s: %w", ErrMalformedConfig, locator, formatName, err)
	}
	if content == nil {
		content = map[string]any{}
	}
	return content, nil
}

// fetchError classifies what a transport reported. A transport that refuses
// the shape of the locator, such as a file source handed file://host/path,
// says so with ErrMalformedLocator and keeps that classification: the host has
// the same thing to do either way, which is to fix the locator it gave.
// Anything else is the source being unreachable.
func fetchError(locator string, err error) error {
	for _, sentinel := range []error{ErrMalformedLocator, ErrUndeterminedFormat, ErrSourceUnavailable} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("%w: %q could not be read: %w", ErrSourceUnavailable, locator, err)
}
