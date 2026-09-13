package config

import (
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// ungroupedHeading is the section items that named no group are listed under.
const ungroupedHeading = "Options"

// reservedHeading is the section the two names this module keeps are listed
// under. They are arguments the program accepts, so a help output that left
// them out would not describe the command line it parses. It is a section of
// its own rather than a group, so a module naming a group the same way does
// not land in it.
const reservedHeading = "Reserved"

// helpLine is one rendered argument: the names column and the text beside it.
type helpLine struct {
	names string
	doc   string
}

// renderHelp writes the help output. Only items exposed as command-line
// arguments appear: an item with no OriginFlag has no argument to describe.
//
// Rendering happens before the primary config source is read, so the help
// output does not depend on any external source being reachable. A sensitive
// item shows its name and its description alone, which is why a description is
// mandatory for one: with the default withheld there would otherwise be
// nothing to go on.
func renderHelp(w io.Writer, m *manifest) error {
	groups := make(map[string][]helpLine)
	for _, item := range m.items {
		if !item.origins.has(OriginFlag) {
			continue
		}
		groups[item.item.Group] = append(groups[item.item.Group], itemLine(item))
	}
	reserved := []helpLine{
		{
			names: names("", locatorFlag, "LOCATOR"),
			doc: "the URI of the primary config source, for example file:///etc/app.yaml. " +
				"It overrides the default the host declared and the environment",
		},
		{names: names("", helpFlag, ""), doc: "print this help and exit"},
	}

	width := 0
	for _, lines := range groups {
		for _, line := range lines {
			width = max(width, len(line.names))
		}
	}
	for _, line := range reserved {
		width = max(width, len(line.names))
	}

	var b strings.Builder
	for i, heading := range headings(groups) {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s:\n", heading)
		writeLines(&b, width, groups[headingKey(heading)])
	}
	if len(groups) > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%s:\n", reservedHeading)
	writeLines(&b, width, reserved)

	_, err := io.WriteString(w, b.String())
	return err
}

// headings orders the sections the declared items fall into: the ungrouped
// ones first, then the named groups in name order. The order does not depend
// on the order the modules were registered in.
func headings(groups map[string][]helpLine) []string {
	declared := slices.Sorted(maps.Keys(groups))
	out := make([]string, 0, len(declared))
	if _, ok := groups[""]; ok {
		out = append(out, ungroupedHeading)
	}
	for _, group := range declared {
		if group == "" {
			continue
		}
		out = append(out, group)
	}
	return out
}

// writeLines renders one section, its arguments in name order.
func writeLines(b *strings.Builder, width int, lines []helpLine) {
	slices.SortFunc(lines, func(a, c helpLine) int { return strings.Compare(a.names, c.names) })
	for _, line := range lines {
		fmt.Fprintf(b, "%-*s  %s\n", width, line.names, line.doc)
	}
}

// headingKey maps a rendered heading back to the key it was collected under.
func headingKey(heading string) string {
	if heading == ungroupedHeading {
		return ""
	}
	return heading
}

// requiredMarker stands where the default of a required item would be.
const requiredMarker = "(required)"

// itemLine renders one input item.
//
// A required item carries the marker instead of a default. It has no default
// worth the name, and (default: "") would have the reader conclude that
// leaving it out means the empty string, while leaving it out really means the
// module never comes up. The marker is decided before the sensitive rule
// returns, or an item that is both would show neither a default nor a marker,
// which is the very state this rule exists to remove.
func itemLine(item *manifestItem) helpLine {
	line := helpLine{
		names: names(item.item.FlagShort, item.item.FlagName, placeholder(item)),
		doc:   item.item.Description,
	}
	if item.item.Required {
		return withNote(line, requiredMarker)
	}
	if item.item.Sensitive {
		return line
	}
	return withNote(line, "(default: "+formatDefault(item)+")")
}

// withNote puts the trailing note beside the description, or in its place when
// the item has none.
func withNote(line helpLine, note string) helpLine {
	if line.doc == "" {
		line.doc = note
		return line
	}
	line.doc += " " + note
	return line
}

// names renders the names column, keeping the long names aligned whether or
// not the item has a short name.
func names(short, long, place string) string {
	head := "      "
	if short != "" {
		head = fmt.Sprintf("  -%s, ", short)
	}
	out := head + "--" + long
	if place != "" {
		out += " " + place
	}
	return out
}

// placeholder is the value placeholder of an item: the declared one, or a
// neutral stand-in. A boolean has none, since its argument may be written
// without a value.
func placeholder(item *manifestItem) string {
	if item.item.Placeholder != "" {
		return item.item.Placeholder
	}
	if item.typ.Kind() == reflect.Bool {
		return ""
	}
	return "VALUE"
}

// formatDefault renders the declared default in the form the command line
// would take it back.
func formatDefault(item *manifestItem) string {
	if item.kind == kindStringList {
		v := reflect.ValueOf(item.def)
		if !v.IsValid() {
			return `""`
		}
		parts := make([]string, v.Len())
		for i := range v.Len() {
			parts[i] = v.Index(i).String()
		}
		return fmt.Sprintf("%q", strings.Join(parts, listSeparator))
	}
	if s, ok := item.def.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%v", item.def)
}
