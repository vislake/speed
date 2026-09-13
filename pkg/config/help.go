package config

import (
	"encoding"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
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

// noDefaultMarker stands where the default of an optional item that has none
// would be.
const noDefaultMarker = "(no default)"

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
	if meansUnset(reflect.ValueOf(item.def)) {
		return withNote(line, noDefaultMarker)
	}
	if item.item.Sensitive {
		return line
	}
	return withNote(line, "(default: "+formatDefault(item)+")")
}

// meansUnset reports whether a field's current value stands for "nobody
// supplied one" rather than for a value of its own. It is a question put to
// the value, not a list of types: the next type that expresses being unset
// gains a branch here and nothing else changes.
//
// A nil scalar pointer is the one shape that answers yes today. It is how a
// module tells "nobody gave it" from "it was given the zero value", so neither
// the zero nor Go's <nil> is the truth about it.
func meansUnset(v reflect.Value) bool {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return true
		}
		v = v.Elem()
	}
	return false
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
	if booleanLeaf(item.typ) {
		return ""
	}
	return "VALUE"
}

// formatDefault renders the declared default in the form a reader can type
// back. The help output is a human interface, so the criterion is that a
// reader copying this value into the command line gets exactly this default
// again: 5m rather than a count of nanoseconds, a,b rather than [a b].
//
// The shapes are decided in the order the conversion on the way in decides
// them, or the two ends would disagree about what a value is. A list is
// settled first, because a list of strings takes the comma-separated form
// without ever reaching the text path; a duration is settled by its type
// rather than by its kind, or every other int64 would be printed as a span of
// time. An item whose value means being unset never gets here: itemLine
// prints the marker for it instead of asking for a default.
func formatDefault(item *manifestItem) string {
	v := reflect.ValueOf(item.def)
	if item.kind == kindStringList {
		return formatStringList(v)
	}
	return formatScalarDefault(v)
}

// formatStringList renders a list in the flat form the command line and the
// environment give it, quoted as one word so the separator survives the shell.
func formatStringList(v reflect.Value) string {
	if !v.IsValid() {
		return `""`
	}
	parts := make([]string, v.Len())
	for i := range v.Len() {
		parts[i] = v.Index(i).String()
	}
	return strconv.Quote(strings.Join(parts, listSeparator))
}

// formatScalarDefault renders a single value in the form a source would give
// it.
func formatScalarDefault(v reflect.Value) string {
	// A scalar pointer is a leaf of its own, and what a source gives it is
	// the value behind it, never the address.
	for v.Kind() == reflect.Pointer && !meansUnset(v) {
		v = v.Elem()
	}
	t := v.Type()
	if t == durationType {
		return time.Duration(v.Int()).String()
	}
	if decodesFromText(t) {
		// A leaf that reads itself from a string is rendered by that same
		// type's output form, which is the one text its own parser is meant
		// to take back. Nothing else here knows how to write it.
		if text, ok := textForm(v); ok {
			return text
		}
		return residualDefault(v)
	}
	switch t.Kind() {
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.String:
		// Quoted, so the reader copies the whole word: the shell takes one
		// layer of quotes off and the parser receives the value as it stands.
		return strconv.Quote(v.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		// The width is the field's own: read as 64 bits, a declared 0.1 in a
		// 32-bit field grows a tail of digits the module never wrote.
		return strconv.FormatFloat(v.Float(), 'g', -1, t.Bits())
	}
	return residualDefault(v)
}

// textForm renders a value through its own text output form.
//
// The value is copied into an addressable one first: a default is held in an
// interface and cannot be addressed, so asking it directly for the interface
// would miss every implementation carried on the pointer receiver - which is
// where math/big.Int carries both of its text methods. This is the mirror of
// how the conversion on the way in reaches the decoding half.
func textForm(v reflect.Value) (string, bool) {
	held := reflect.New(v.Type())
	held.Elem().Set(v)
	m, ok := held.Interface().(encoding.TextMarshaler)
	if !ok {
		return "", false
	}
	text, err := m.MarshalText()
	if err != nil {
		return "", false
	}
	return string(text), true
}

// residualDefault renders a value that has no form a reader could type back.
// Go's own printed form is what it gets, because a spelling invented here
// would be a word the help output offers and no source accepts. Two kinds of
// value arrive:
//
//   - a leaf that reads itself from text but writes none. Its stored value is
//     not the text its own parser takes back - a percentage holding 50 reads
//     itself from "50%" - so neither the number nor the printed form is a
//     value this column can promise.
//   - a value no source can give at all: a complex number, a uintptr, a
//     pointer to a list. These reach the help output because collection lists
//     them, and what they print is fixed here rather than left to chance.
func residualDefault(v reflect.Value) string {
	return fmt.Sprintf("%v", v)
}
