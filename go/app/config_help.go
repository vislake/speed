package app

// config_help.go carries the command-line --help rendering of the component
// configuration surface: the listing an operator reads to learn which
// components carry process-start configuration and how each field resolves.
// It is the second consumer of the root package's FieldDescriptor
// collection -- pkgcore.DescribeComponentSchema -- beside the generated
// configuration reference (tools/configrefgen), so the help an operator
// reads and the committed reference document cannot drift from the
// declaration the assembly enforces.
//
// The rendering reads the declaration only: it composes no assembly, loads
// no source and needs no database, so a host can offer it (and should) even
// when the deployment's bootstrap configuration is broken. What it lists is
// the registered components of the registry it is handed -- what the binary
// carries and can select from -- not the selected subset, which only a run
// of the composition layer determines.
//
// The one asymmetry the rendering owes the operator is masking: a field the
// schema marks sensitive has its documentation cells -- Default and Example
// -- rendered as the redacted marker instead of their declared text, the
// same discipline go/config's Describe applies to a Sensitive item's
// default, because a secret's suggested or fallback value has no more
// business in help output than on the event bus. The field's Description
// still renders: the assembly requires a sensitive field to document itself
// (ErrInvalidComponent otherwise), precisely so an operator can be told how
// to supply and rotate it.

import (
	"fmt"
	"io"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
	pkgconfig "github.com/vislake/speed/go/pkgcore/config"
)

// redactedValue stands in for a sensitive field's documentation cells in the
// rendered help. It is the same marker the rest of the platform renders a
// redacted value as ("[redacted]", go/config's event bus and saasctl's
// config print), so one operator-facing convention covers every surface a
// secret must not cross.
const redactedValue = "[redacted]"

// ComponentConfigSurface is one component's slice of the configuration
// surface: the component's registered name and the field descriptors
// pkgcore.DescribeComponentSchema produced for its ConfigSchema, in the
// schema's declaration order.
type ComponentConfigSurface struct {
	// Name is the component's registered name.
	Name string
	// Fields are the component's resolvable schema fields, each carrying its
	// final key path (namespace prefix included) and its documentation.
	Fields []pkgcore.FieldDescriptor
}

// CollectComponentConfig returns the configuration surface of the component
// set it is handed: one ComponentConfigSurface per component that declares a
// ConfigSchema with at least one resolvable field, in the set's order. A
// component without a schema and a schema without resolvable fields (nil,
// empty, or one whose every field is skipped) both contribute nothing, so a
// component that takes no process-start configuration is an honest absence
// from the listing rather than a placeholder.
//
// Which set to hand in is the caller's reading: pkgcore.GlobalComponents()
// is what the binary carries (the natural --help source -- it needs no
// registry, so the surface renders even when nothing can be assembled), and
// pkgcore.RegisteredComponents(reg) is one registry's registered set.
//
// The fields are collected through pkgcore.DescribeComponentSchema -- the
// root package's one FieldDescriptor collection, which the generated
// configuration reference reads too -- so the key paths, the option
// vocabulary and the documentation entries this listing renders are the
// declaration as the assembly reads it. A schema the collection refuses
// (a malformed tag option, a derive option off a []byte field) fails the
// collection naming the component, because a --help surface that silently
// dropped a component would hide exactly what the assembly would refuse.
func CollectComponentConfig(components []pkgcore.Component) ([]ComponentConfigSurface, error) {
	var surfaces []ComponentConfigSurface
	for _, c := range components {
		if c.ConfigSchema == nil {
			continue
		}
		fields, err := pkgcore.DescribeComponentSchema(c.Name, c.ConfigSchema)
		if err != nil {
			return nil, fmt.Errorf("app: describe component %q's configuration schema: %w", c.Name, err)
		}
		if len(fields) == 0 {
			continue
		}
		surfaces = append(surfaces, ComponentConfigSurface{Name: c.Name, Fields: fields})
	}
	return surfaces, nil
}

// RenderComponentConfigHelp writes the collected configuration surface as
// the help text a host binary prints. envPrefix is the prefix the loader
// derives an unpinned field's environment variable spelling under (the
// host's ConfigEnvPrefix value, so the help names the variable the loader
// would actually read); empty means the loader's own default prefix. A
// field that pins its variable name (the env tag option) renders that exact
// name instead.
//
// The rendering is deterministic: components in the order the slice
// carries, fields in declaration order, one block per field:
//
//	<component name>
//	  <key path>  <type>  <markers>
//	      source: <the sources the field resolves from, in precedence order>
//	      flag: --<key path>            (exposed fields only)
//	      env: <VARIABLE NAME>          (exposed fields only)
//	      description: <the field's doc> (when documented)
//	      default: <...>                (when documented; redacted when sensitive)
//	      example: <...>                (when documented; redacted when sensitive)
//
// An empty surface renders as one line saying so, because a binary whose
// registered components declare no configuration has that as its honest
// help.
func RenderComponentConfigHelp(w io.Writer, surfaces []ComponentConfigSurface, envPrefix string) error {
	if envPrefix == "" {
		envPrefix = pkgconfig.EnvPrefix
	}

	var b strings.Builder
	b.WriteString("Component configuration surface (each field collected through\n")
	b.WriteString("pkgcore.DescribeComponentSchema, the same collection the generated\n")
	b.WriteString("configuration reference reads; a key path is the address its flag,\n")
	b.WriteString("environment variable, config-file entry or derivation spells):\n")
	if len(surfaces) == 0 {
		b.WriteString("\n(no registered component declares a configuration schema)\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	for _, surface := range surfaces {
		b.WriteString("\n")
		b.WriteString(surface.Name)
		b.WriteString("\n")
		for _, field := range surface.Fields {
			renderField(&b, field, envPrefix)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// renderField appends one field's block: the key path with its type and
// declaration markers, then the source line and the documentation lines.
func renderField(b *strings.Builder, field pkgcore.FieldDescriptor, envPrefix string) {
	fmt.Fprintf(b, "  %s  %s%s\n", field.Key, field.Type, fieldMarkers(field))
	fmt.Fprintf(b, "      source: %s\n", fieldSources(field))
	if field.Expose {
		fmt.Fprintf(b, "      flag: --%s\n", field.Key)
		if field.Env != "" {
			fmt.Fprintf(b, "      env: %s (pinned by the declaration)\n", field.Env)
		} else {
			fmt.Fprintf(b, "      env: %s\n", pkgconfig.EnvName(envPrefix, strings.ToLower(field.Key)))
		}
	}
	if field.Doc.Description != "" {
		fmt.Fprintf(b, "      description: %s\n", field.Doc.Description)
	}
	if field.Doc.Default != "" {
		fmt.Fprintf(b, "      default: %s\n", maskedIfSensitive(field.Sensitive, field.Doc.Default))
	}
	if field.Doc.Example != "" {
		fmt.Fprintf(b, "      example: %s\n", maskedIfSensitive(field.Sensitive, field.Doc.Example))
	}
}

// maskedIfSensitive replaces a sensitive field's documentation cell with the
// redacted marker: whatever text the schema declared for the field's default
// or example, help output carries only the marker, never the text that could
// spell a secret's value or its shape.
func maskedIfSensitive(sensitive bool, text string) string {
	if sensitive {
		return redactedValue
	}
	return text
}

// fieldMarkers renders the field's declaration options as a compact suffix,
// in a fixed order so the listing is deterministic: derive, required,
// sensitive and the documentation group.
func fieldMarkers(field pkgcore.FieldDescriptor) string {
	var markers []string
	if field.Derive {
		markers = append(markers, "derive")
	}
	if field.Required {
		markers = append(markers, "required")
	}
	if field.Sensitive {
		markers = append(markers, "sensitive")
	}
	if field.Group != "" {
		markers = append(markers, "group="+field.Group)
	}
	if len(markers) == 0 {
		return ""
	}
	return "  " + strings.Join(markers, " ")
}

// fieldSources names the sources a field resolves from, in the precedence
// order of the chain the engine's resolver runs (component_config.go):
// a plain field is supplied by its configuration block alone, an exposed
// field adds flag over environment over config file above the block's own
// value, and a derive-tagged field's key material continues past the file
// into the root-key derivation and the declared defaults table.
func fieldSources(field pkgcore.FieldDescriptor) string {
	switch {
	case field.Derive:
		return "flag > environment > config file > root-key derivation > declared default"
	case field.Expose:
		return "flag > environment > config file > the configuration block's value"
	default:
		return "the component's configuration block only"
	}
}
