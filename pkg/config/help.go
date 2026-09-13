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
	"unicode/utf8"
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
// worth the name, and a column showing an empty value would have the reader
// conclude that leaving it out means the empty string, while leaving it out
// really means the module never comes up. The marker is decided before the sensitive rule
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
// The syntax that criterion is read against is the command line, and only it.
// Help renders the items exposed as arguments, so that syntax is settled;
// a config source has a type system of its own, where the same text is not
// verbatim-true - YAML reads 42 as a number, and a field that decodes itself
// from a string needs "42" written there - and one rendering cannot hold for
// both.
//
// The shapes are decided in the order the conversion on the way in decides
// them, or the two ends would disagree about what a value is. A list is
// settled first, because a list of strings takes the comma-separated form
// without ever reaching the text path; a duration is settled by its type
// rather than by its kind, or every other int64 would be printed as a span of
// time. An item whose value means being unset never gets here: itemLine
// prints the marker for it instead of asking for a default.
//
// Quoting comes last and is decided on the rendered text alone. It is the only
// place it can be decided: needing quotes is a property of the characters, and
// a rule reading the field's Go kind leaves a type that carries its own text
// form bare however many spaces that form holds.
func formatDefault(item *manifestItem) string {
	v := reflect.ValueOf(item.def)
	if item.kind == kindStringList {
		return quoteForCommandLine(formatStringList(v))
	}
	return quoteForCommandLine(formatScalarDefault(v))
}

// formatStringList renders a list in the flat comma-separated form the command
// line gives it. The separator carries no escape on the way in, so an element
// holding a comma has no flat form at all, and quoting does not rescue one:
// whatever quotes surrounded the text, what is read back is split on the comma.
func formatStringList(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	parts := make([]string, v.Len())
	for i := range v.Len() {
		parts[i] = v.Index(i).String()
	}
	return strings.Join(parts, listSeparator)
}

// formatScalarDefault renders a single value in the form the command line
// would give it.
func formatScalarDefault(v reflect.Value) string {
	// A scalar pointer is a leaf of its own, and what the command line gives
	// it is the value behind it, never the address.
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
		return v.String()
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
// would be a word the help output offers and the command line does not take.
// Two kinds of value arrive:
//
//   - a leaf that reads itself from text but writes none. Its stored value is
//     not the text its own parser takes back - a percentage holding 50 reads
//     itself from "50%" - so neither the number nor the printed form is a
//     value this column can promise.
//   - a value the command line cannot give at all: a complex number, a
//     uintptr, a pointer to a list. These reach the help output because
//     collection lists them, and what they print is fixed here rather than
//     left to chance.
//
// Quoting reaches these along with every other rendered text, and it promises
// them no more than it promises the rest: the word arrives in argv unrewritten.
// Whether the parser then takes that word is a separate question, and for a
// residual value the answer stays no.
func residualDefault(v reflect.Value) string {
	return fmt.Sprintf("%v", v)
}

// bareOnACommandLine holds the punctuation that stands for itself wherever it
// appears in an unquoted word; letters and digits do too, and are decided by
// range. The set is written as what is allowed rather than as what is not, for
// the same reason the criterion is not written in terms of Go kinds: an
// enumeration of the harmful characters is the one that can be incomplete, and
// the two directions fail differently. A character missing from this set costs
// a pair of quotes around a value that would have survived without them, and
// the value still arrives verbatim. A harmful character missing from an
// enumeration reaches the reader as a value the shell rewrites.
//
// The same conservatism decides the three that are absent: ~ expands at the
// head of a word, ! is the history expansion of an interactive shell, and ^
// carries a meaning of its own in more than one shell. % is here, because a
// job spec is read where a command name goes and a default value never stands
// in that position. Anything outside ASCII is outside the set as well, so a
// default written in Chinese renders inside quotes it does not need - the
// harmless direction of the two.
const bareOnACommandLine = "_@%+=:,./-"

// quoteForCommandLine renders text as one word of a POSIX command line: what
// the program receives in argv is this text again, byte for byte. Three cases,
// decided in this order.
//
// Text carrying a character with no printable form is given Go's escaped form,
// and that form is the one thing here the reader cannot copy back. A POSIX
// command line has no single-line literal for a newline - $'a\nb' is an
// extension bash and zsh have and dash does not - while the help output gives
// each item a line of its own, so a value that really broke its line would
// take the layout with it. An escape character is worse than unreadable: put
// through raw it is read by the terminal rather than by the reader. What this
// case offers is a readable single line, not a value that can be typed back.
//
// Text that is empty, or that carries anything outside the bare set, is
// wrapped in single quotes, each quote of its own closed, escaped and
// reopened. Nothing is special inside single quotes, which makes this the one
// form that holds for arbitrary printable text - and the reason Go's quoting
// is not the one to use: "a $HOME" still expands inside double quotes, and the
// \t of "a\tb" is a backslash and a t rather than a tab. Empty text is quoted
// for a reason of its own, since no shell would touch it either way:
// (default: ) leaves the reader unable to tell an empty default from a column
// that failed to print.
//
// Everything else stands for itself.
//
// The form is a POSIX shell's. A command interpreter with rules of its own,
// cmd.exe among them, is not served by inventing a second spelling here.
func quoteForCommandLine(text string) string {
	if !printableText(text) {
		return strconv.Quote(text)
	}
	if text == "" || strings.ContainsFunc(text, func(r rune) bool { return !bareRune(r) }) {
		return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
	}
	return text
}

// bareRune reports whether one character stands for itself in an unquoted word.
func bareRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune(bareOnACommandLine, r)
}

// printableText reports whether every character of the text has a printable
// form. A byte that is not valid UTF-8 answers no along with the control
// characters: neither is something a reader could read off the line.
func printableText(text string) bool {
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 1 {
			return false
		}
		if !strconv.IsPrint(r) {
			return false
		}
		i += size
	}
	return true
}
