// Package file delivers the file:// transport: it reads the primary config
// source off the local filesystem and names its format by the file extension.
//
// Importing the package is all a host does with it, because the transport
// travels as a resource of a module registered in init:
//
//	import _ "github.com/vislake/speed/pkg/config/source/file"
//
// The package carries no third-party dependency.
package file

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
)

// moduleName is the name this transport registers under. Transports and
// formats are named after where they live, so a registry listing says what a
// module is without anybody opening it.
const moduleName = "config.source.file"

// init registers the module with the process registry, which is what makes a
// blank import of this package enough to have file:// locators served.
func init() { core.ProcessRegistry.Register(Module()) }

// Module is this module's descriptor, exported for registries that do not
// inherit the process-level registrations, such as the ones tests build with
// core.New.
//
// It holds a resource and nothing else: a transport is a stateless parser of
// locators with no lifecycle and no capability to deliver. The configuration
// module collects it with core.Resources and never imports this package.
func Module() core.Module {
	return core.Module{Name: moduleName, Resources: []any{source{}}}
}

// Compile-time proof that this transport satisfies the extension point it is
// collected by: the resource is stored as any, so nothing else would catch a
// signature that drifted from the interface.
var _ config.Source = source{}

// source is the transport itself. It holds no state: every call is answered
// out of the locator it is given.
type source struct{}

// Scheme reports the URI scheme this transport answers for.
func (source) Scheme() string { return "file" }

// formatsByExtension maps a file extension to the format name the parser is
// selected by. The extension is the whole of the evidence: content is never
// sniffed, because valid JSON is also valid YAML and a wrong guess parses the
// same bytes under the other grammar.
var formatsByExtension = map[string]string{
	".json": "json",
	".yaml": "yaml",
	".yml":  "yaml",
}

// Fetch reads the file the locator points at and reports the format name that
// its extension stands for.
//
// The format is settled before the file is opened, so a locator this transport
// could never parse fails on the extension rather than on whatever the read
// happens to hit first.
//
// The context is unused: reading a local file is a single syscall with nothing
// to cancel in between.
func (source) Fetch(_ context.Context, locator *url.URL) (data []byte, format string, err error) {
	name, err := localPath(locator)
	if err != nil {
		return nil, "", err
	}
	format, err = formatOf(name)
	if err != nil {
		return nil, "", err
	}
	data, err = os.ReadFile(name)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s could not be read: %v", config.ErrSourceUnavailable, name, err)
	}
	return data, format, nil
}

// localPath gives the filesystem path a locator designates, refusing the
// shapes this transport does not serve. A refused shape is a malformed
// locator rather than an unavailable source: the host has to fix what it
// wrote, and no amount of retrying changes the outcome.
func localPath(locator *url.URL) (string, error) {
	if locator.Opaque != "" {
		return "", fmt.Errorf("%w: %q has no path of its own. A file locator is written with "+
			"three slashes and an absolute path, as in file:///etc/app.yaml",
			config.ErrMalformedLocator, locator.String())
	}
	if locator.Host != "" {
		return "", fmt.Errorf("%w: %q names the host %q, and this transport reads the local "+
			"filesystem alone. An absolute local path is written file:///etc/app.yaml; a config "+
			"source on another machine needs the transport that serves it",
			config.ErrMalformedLocator, locator.String(), locator.Host)
	}
	if locator.Path == "" {
		return "", fmt.Errorf("%w: %q names no path, as in file:///etc/app.yaml",
			config.ErrMalformedLocator, locator.String())
	}
	return locator.Path, nil
}

// formatOf names the format a file's extension stands for. An extension this
// transport does not recognise leaves the format undetermined, which is a
// failure rather than a guess.
func formatOf(name string) (string, error) {
	ext := strings.ToLower(path.Ext(name))
	if format, known := formatsByExtension[ext]; known {
		return format, nil
	}
	if ext == "" {
		return "", fmt.Errorf("%w: %s has no file extension, and the extension is what names "+
			"the format of a file source. Name the file .json, .yaml or .yml",
			config.ErrUndeterminedFormat, name)
	}
	return "", fmt.Errorf("%w: %s ends in %s, which names no format. The recognised extensions "+
		"are .json, .yaml and .yml; the content is never sniffed, because valid JSON is also "+
		"valid YAML", config.ErrUndeterminedFormat, name, ext)
}
