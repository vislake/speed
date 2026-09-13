// Package yaml parses the YAML configuration format. It is the one place in
// the configuration module that carries a third-party dependency, which is
// why it is a package of its own: a host that configures itself in JSON never
// imports it and never pays for it.
//
// Importing the package is all a host does with it, because the parser travels
// as a resource of a module registered in init:
//
//	import _ "github.com/vislake/speed/pkg/config/format/yaml"
package yaml

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
	yamlv3 "go.yaml.in/yaml/v3"
)

// moduleName is the name this parser registers under. Transports and formats
// are named after where they live, so a registry listing says what a module is
// without anybody opening it.
const moduleName = "config.format.yaml"

// The tags this parser judges keys by. YAML resolves an unquoted 1 to !!int
// and a quoted "1" to !!str, which is exactly the distinction a config key
// needs; a merge key carries a tag of its own and is therefore not a string
// key either.
const (
	stringTag = "!!str"
	mergeTag  = "!!merge"
)

// init registers the module with the process registry, which is what makes a
// blank import of this package enough to have YAML config sources parsed.
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
// exactly, so it is the same string a file source derives from the .yaml and
// .yml extensions.
func (format) Name() string { return "yaml" }

// Unmarshal decodes a YAML document into a tree of string-keyed maps.
//
// It decodes to the document tree and walks it rather than letting the library
// decode straight into map[string]any: YAML permits keys of any type, and the
// rule that a non-string key is refused anywhere in the tree needs the node
// tags to hold to it. Values keep the types YAML resolves them to, so 5m stays
// a string for a duration field to parse and 1 arrives as an integer.
//
// A document carrying nothing is an empty configuration rather than a failure:
// a source with no settings is a legal way to run, and the layers below it
// decide the values.
func (format) Unmarshal(data []byte) (map[string]any, error) {
	decoder := yamlv3.NewDecoder(bytes.NewReader(data))

	var document yamlv3.Node
	switch err := decoder.Decode(&document); {
	case errors.Is(err, io.EOF):
		return map[string]any{}, nil
	case err != nil:
		return nil, err
	}
	if err := refuseSecondDocument(decoder); err != nil {
		return nil, err
	}

	top := &document
	if top.Kind == yamlv3.DocumentNode {
		if len(top.Content) == 0 {
			return map[string]any{}, nil
		}
		top = top.Content[0]
	}

	value, err := newConverter().value(top)
	if err != nil {
		return nil, err
	}
	switch root := value.(type) {
	case map[string]any:
		return root, nil
	case nil:
		return map[string]any{}, nil
	default:
		return nil, fmt.Errorf("line %d: the document is %s at the top level, and a config "+
			"source is a mapping of keys to values, with a key such as cache: carrying the "+
			"settings beneath it", top.Line, describe(value))
	}
}

// refuseSecondDocument rejects a stream carrying more than one document. YAML
// allows several in one file and a config source is one of them; decoding the
// first and dropping the rest would leave the settings somebody wrote with no
// effect and nothing said about it.
func refuseSecondDocument(decoder *yamlv3.Decoder) error {
	var extra yamlv3.Node
	switch err := decoder.Decode(&extra); {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return err
	}
	return fmt.Errorf("line %d: the data holds a second YAML document, and a config source is "+
		"one document. Remove the --- separator and merge the settings into one", extra.Line)
}

// converter walks the document tree. It remembers the nodes on the current
// path, because an anchor may hold an alias to itself: the library parses that
// without complaint and leaves a cycle in the tree, which a walk with no
// memory follows forever.
type converter struct {
	onPath map[*yamlv3.Node]bool
}

func newConverter() *converter {
	return &converter{onPath: map[*yamlv3.Node]bool{}}
}

// value converts one node to the value it stands for.
func (c *converter) value(node *yamlv3.Node) (any, error) {
	if c.onPath[node] {
		return nil, fmt.Errorf("line %d: an anchor holds itself through an alias, and a value "+
			"that contains itself cannot be written out", node.Line)
	}
	c.onPath[node] = true
	defer delete(c.onPath, node)

	switch node.Kind {
	case yamlv3.AliasNode:
		target, err := resolveAlias(node)
		if err != nil {
			return nil, err
		}
		return c.value(target)
	case yamlv3.MappingNode:
		return c.mapping(node)
	case yamlv3.SequenceNode:
		items := make([]any, 0, len(node.Content))
		for _, item := range node.Content {
			converted, err := c.value(item)
			if err != nil {
				return nil, err
			}
			items = append(items, converted)
		}
		return items, nil
	case yamlv3.ScalarNode:
		var scalar any
		if err := node.Decode(&scalar); err != nil {
			return nil, fmt.Errorf("line %d: %v", node.Line, err)
		}
		return scalar, nil
	}
	// The zero kind is what an empty document decodes to.
	return nil, nil
}

// mapping converts a mapping node, refusing every key that is not a string.
func (c *converter) mapping(node *yamlv3.Node) (map[string]any, error) {
	out := make(map[string]any, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, err := stringKey(node.Content[i])
		if err != nil {
			return nil, err
		}
		value, err := c.value(node.Content[i+1])
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

// stringKey gives the key a node names, refusing anything that is not a plain
// string. Turning a number into its text silently would make 1 and "1" the
// same key, and a config key is lowercase letters, digits and dashes in the
// first place, so a number was never one.
func stringKey(node *yamlv3.Node) (string, error) {
	resolved := node
	seen := map[*yamlv3.Node]bool{}
	for resolved.Kind == yamlv3.AliasNode {
		if seen[resolved] {
			return "", fmt.Errorf("line %d: the alias this key is written as holds itself", resolved.Line)
		}
		seen[resolved] = true
		target, err := resolveAlias(resolved)
		if err != nil {
			return "", err
		}
		resolved = target
	}
	if resolved.Tag == mergeTag {
		return "", fmt.Errorf("line %d: the merge key << is not a string key and is not "+
			"supported. Write the keys it would have merged out in full", resolved.Line)
	}
	if resolved.Kind != yamlv3.ScalarNode || resolved.Tag != stringTag {
		return "", fmt.Errorf("line %d: the key %s is %s, and a config key is a string. Quote "+
			"it if a string is what it is meant to be", resolved.Line, keyText(resolved), describeTag(resolved))
	}
	return resolved.Value, nil
}

// resolveAlias gives the node an alias points at.
func resolveAlias(node *yamlv3.Node) (*yamlv3.Node, error) {
	if node.Alias == nil {
		return nil, fmt.Errorf("line %d: the alias *%s refers to an anchor that is not defined",
			node.Line, node.Value)
	}
	return node.Alias, nil
}

// keyText renders a key for an error message, falling back to its kind when
// the key is a whole collection rather than a scalar.
func keyText(node *yamlv3.Node) string {
	if node.Kind == yamlv3.ScalarNode {
		return fmt.Sprintf("%q", node.Value)
	}
	return "written here"
}

// describeTag names what YAML resolved a node to, in the words of the format.
func describeTag(node *yamlv3.Node) string {
	switch node.Kind {
	case yamlv3.MappingNode:
		return "a mapping"
	case yamlv3.SequenceNode:
		return "a sequence"
	}
	if node.Tag == "" {
		return "not a string"
	}
	return "of type " + node.Tag
}

// describe names what a converted value is, so the error says what was found
// rather than what Go called it.
func describe(value any) string {
	switch value.(type) {
	case []any:
		return "a sequence"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case int, int64, uint64, float64:
		return "a number"
	}
	return fmt.Sprintf("%T", value)
}
