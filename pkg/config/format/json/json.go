// Package json parses the JSON configuration format. It is written on the
// standard library alone, which is the whole point of keeping it apart from
// the YAML parser: a host that configures itself in JSON carries no parsing
// dependency at all.
//
// Importing the package is all a host does with it, because the parser travels
// as a resource of a module registered in init:
//
//	import _ "github.com/vislake/speed/pkg/config/format/json"
package json

import (
	stdjson "encoding/json"
	"fmt"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
)

// moduleName is the name this parser registers under. Transports and formats
// are named after where they live, so a registry listing says what a module is
// without anybody opening it.
const moduleName = "config.format.json"

// init registers the module with the process registry, which is what makes a
// blank import of this package enough to have JSON config sources parsed.
func init() { core.ProcessRegistry.Register(Module()) }

// Module is this module's descriptor, exported for registries that do not
// inherit the process-level registrations, such as the ones tests build with
// core.New.
//
// It holds a resource and nothing else: a parser is stateless, has no
// lifecycle and delivers no capability. The configuration module collects it
// with core.Resources and never imports this package.
func Module() core.Module {
	return core.Module{Name: moduleName, Resources: []any{format{}}}
}

// Compile-time proof that this parser satisfies the extension point it is
// collected by: the resource is stored as any, so nothing else would catch a
// signature that drifted from the interface.
var _ config.Format = format{}

// format is the parser itself. It holds no state: every call is answered out
// of the data it is given.
type format struct{}

// Name reports the format name this parser is selected by. The name is matched
// exactly, so it is the same string a file source derives from the .json
// extension.
func (format) Name() string { return "json" }

// Unmarshal decodes a JSON document into a tree of string-keyed maps. JSON has
// string keys by construction, so the rule against non-string keys costs this
// parser nothing; numbers arrive as float64, which is what the conversion
// layer expects of a source that cannot tell 1 from 1.0.
//
// A document holding null is an empty configuration rather than a failure: a
// source that carries no settings is a legal way to run, and the layers below
// it decide the values.
func (format) Unmarshal(data []byte) (map[string]any, error) {
	var document any
	if err := stdjson.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	switch top := document.(type) {
	case map[string]any:
		return top, nil
	case nil:
		return map[string]any{}, nil
	default:
		return nil, fmt.Errorf("the document is %s at the top level, and a config source is a "+
			"mapping of keys to values, written {\"cache\": {\"ttl\": \"5m\"}}", describe(document))
	}
}

// describe names what a value is in the words of the format, so the error says
// what was found rather than what Go called it.
func describe(value any) string {
	switch value.(type) {
	case []any:
		return "an array"
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	}
	return fmt.Sprintf("%T", value)
}
