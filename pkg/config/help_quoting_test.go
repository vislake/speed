package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// shellWords hands one rendered default to a real shell and returns the words
// the program would receive in argv. The rendered text is pasted into a
// command of its own, the shell does the splitting and the expansions, and
// printf writes the arguments it ended up with back, NUL-separated.
//
// A non-interactive shell is all this covers: word splitting, quote removal,
// parameter, command and arithmetic expansion, and pathname expansion. History
// expansion and job specs live in the interactive layer, and no driver without
// a terminal can rule on them.
//
// The working directory holds a file the corpus patterns match, because an
// unmatched pattern is left standing: without that file a rendered * would
// come back as itself and a missing pair of quotes would pass unnoticed.
//
// One limit is written down here and worked around by the callers: printf
// writes the same single NUL for no arguments at all as for one empty
// argument, so "the word disappeared" reads as one empty word. The empty
// default is therefore pinned on the rendered text rather than through here.
func shellWords(t *testing.T, rendered string) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the quoting rule targets a POSIX shell, and Windows has none at /bin/sh")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("this machine has no /bin/sh to put the rendered default through: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aXb"), nil, 0o600); err != nil {
		t.Fatalf("seeding the file a pattern would match failed: %v", err)
	}
	cmd := exec.Command("/bin/sh", "-c", `printf '%s\0' `+rendered)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the shell refused the command line carrying the rendered default %s: %v\n%s",
			rendered, err, stderr.String())
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// shellWord is the common case: a default is one value, so it has to reach the
// program as one word. The count is compared before the text, or a value the
// shell split in two would be reported as a mismatched first word.
func shellWord(t *testing.T, rendered string) string {
	t.Helper()
	words := shellWords(t, rendered)
	if len(words) != 1 {
		t.Fatalf("the rendered default %s reaches the program as %d words %q, and a default "+
			"is one value", rendered, len(words), words)
	}
	return words[0]
}

// textFormManifest declares a single item whose leaf carries its own text form.
// The census below rides this shape rather than a plain string because it is
// the shape the rule is about: what the column prints is whatever the type
// wrote, and the decision to quote it can only be read off that text.
func textFormManifest(t *testing.T, value string) *manifest {
	t.Helper()
	type labelOnly struct{ Label helpLabel }
	defaults := labelOnly{Label: helpLabel{name: value}}
	return collect(t, declaring("greeter", Schema{
		Namespace: "greeter",
		Mounts:    []Mount{{Value: &defaults}},
		Items:     map[string]Item{"label": {Origins: OriginFlag, FlagName: "label"}},
	}))
}

// defaultOf picks the rendered default of one argument out of a help output.
func defaultOf(t *testing.T, out, argument string) string {
	t.Helper()
	line := helpLineFor(t, out, argument)
	rendered, ok := renderedDefault(line)
	if !ok {
		t.Fatalf("%s renders as %q, which carries no default at all", argument, line)
	}
	return rendered
}

// TestHelpQuotesATextFormCarryingASpace pins the case the whole rule exists
// for: a type that carries its own text form, is neither a string nor a list
// of strings, and whose form holds a space. Decided by the field's Go kind it
// is printed bare, and the reader copying the line back hands the program two
// arguments instead of one value.
func TestHelpQuotesATextFormCarryingASpace(t *testing.T) {
	rendered := defaultOf(t, renderedHelp(t, defaultsCorpusManifest(t)), "--label")
	if rendered != `'west wing'` {
		t.Fatalf("the text form renders as %s, and its text holds a space, so it is quoted", rendered)
	}
	if got := shellWord(t, rendered); got != "west wing" {
		t.Fatalf("the rendered default %s reaches the program as %q", rendered, got)
	}
}

// TestHelpDefaultsRoundTripThroughARealShell pins the rule the default column
// rests on, over every shape the corpus carries: the reader copies the value
// out of the help output into a command line and gets that same default back.
// The shell is the real one, so the step the rendering has to survive - word
// splitting and the expansions - is performed rather than imagined.
func TestHelpDefaultsRoundTripThroughARealShell(t *testing.T) {
	m := defaultsCorpusManifest(t)
	out := renderedHelp(t, m)
	for _, item := range m.items {
		name := item.item.FlagName
		t.Run(name, func(t *testing.T) {
			if reason, skipped := notTypedBack[name]; skipped {
				t.Skipf("--%s stays out of the round trip: %s", name, reason)
			}
			line := helpLineFor(t, out, "--"+name)
			rendered, ok := renderedDefault(line)
			if !ok {
				t.Skipf("--%s renders as %q, and a marker is not a value", name, line)
			}
			typed := shellWord(t, rendered)
			p, err := parseFlags(m, []string{"--" + name + "=" + typed})
			if err != nil {
				t.Fatalf("--%s renders %s, and the command line does not take it back: %v",
					name, rendered, err)
			}
			d := newData(m)
			if err := d.applyFlags(m, p); err != nil {
				t.Fatalf("--%s renders %s, and the command line does not take it back: %v",
					name, rendered, err)
			}
			if got := d.values[item.path].value; !sameDefault(got, item.def) {
				t.Fatalf("--%s renders %s, which reads back as %#v while the default is %#v",
					name, rendered, got, item.def)
			}
		})
	}
}

// TestHelpQuotedDefaultSurvivesEveryASCIIByte is the criterion put one byte at
// a time: every byte a default can carry is rendered, and what the shell hands
// the program is that same text. The check goes through the rendering rather
// than through the quoting function directly, so it says something about the
// output the reader sees, and each byte is judged on its own: rendered in one
// batch, a byte the old rule left bare turns the whole command into a syntax
// error and the report names no byte at all.
//
// A byte with no printable form is held to the other half of the rule: there
// is no single-line command-line literal standing for it, so the column
// renders an escaped form meant to be read, and the raw byte stays out of the
// reader's terminal.
func TestHelpQuotedDefaultSurvivesEveryASCIIByte(t *testing.T) {
	for b := 0; b < 0x80; b++ {
		value := "a" + string(rune(b)) + "b"
		t.Run(fmt.Sprintf("0x%02x", b), func(t *testing.T) {
			rendered := defaultOf(t, renderedHelp(t, textFormManifest(t, value)), "--label")
			if !strconv.IsPrint(rune(b)) {
				if want := strconv.Quote(value); rendered != want {
					t.Fatalf("the default carrying byte 0x%02x renders as %s, and a byte with "+
						"no printable form is rendered as %s", b, rendered, want)
				}
				return
			}
			if got := shellWord(t, rendered); got != value {
				t.Fatalf("the default carrying byte 0x%02x renders as %s, and the shell hands "+
					"the program %q instead of %q", b, rendered, got, value)
			}
		})
	}
}

// TestHelpEscapesADefaultCarryingAControlCharacter pins the two properties the
// escaped form is chosen for. The help output is one line per item, so a
// default carrying a newline may not break its line; and an escape character
// reaching the terminal raw is read by the terminal rather than by the reader.
func TestHelpEscapesADefaultCarryingAControlCharacter(t *testing.T) {
	plain := renderedHelp(t, textFormManifest(t, "plain"))
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "newline", value: "a\nb"},
		{name: "escape", value: "a\x1bb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderedHelp(t, textFormManifest(t, tc.value))
			if got, want := strings.Count(out, "\n"), strings.Count(plain, "\n"); got != want {
				t.Fatalf("the help output runs to %d lines where a plain default takes %d:\n%s",
					got, want, out)
			}
			if strings.ContainsFunc(out, func(r rune) bool { return r != '\n' && !strconv.IsPrint(r) }) {
				t.Fatalf("the help output carries a raw control character into the terminal:\n%q", out)
			}
		})
	}
}

// TestHelpQuotesEmbeddedSingleQuotes pins the one escape the quoted form has.
// Nothing is special inside single quotes, the quote itself included, so a
// value carrying one leaves the quotes and comes back into them.
func TestHelpQuotesEmbeddedSingleQuotes(t *testing.T) {
	rendered := defaultOf(t, renderedHelp(t, defaultsCorpusManifest(t)), "--quoted")
	if rendered != `'it'\''s'` {
		t.Fatalf("the default carrying a quote renders as %s, and the quote is closed, "+
			"escaped and reopened", rendered)
	}
	if got := shellWord(t, rendered); got != "it's" {
		t.Fatalf("the rendered default %s reaches the program as %q", rendered, got)
	}
}

// TestHelpLeavesAPlainDefaultUnquoted pins the other half of the rule. A value
// whose text a command line takes as it stands is rendered as it stands:
// quoting it would be a pair of characters the reader has to know to drop.
func TestHelpLeavesAPlainDefaultUnquoted(t *testing.T) {
	out := renderedHelp(t, defaultsCorpusManifest(t))
	for name, want := range map[string]string{
		"text":   "Hello",
		"hosts":  "a,b",
		"count":  "8080",
		"flag":   "true",
		"ratio":  "0.1",
		"ttl":    "5m0s",
		"moment": "2026-09-13T10:00:00Z",
		"share":  "50%",
	} {
		if got := defaultOf(t, out, "--"+name); got != want {
			t.Errorf("--%s renders %s, and its text needs no quotes, so it renders as %s",
				name, got, want)
		}
	}
}

// TestHelpQuotesAnEmptyDefault pins the one value quoted for being unreadable
// rather than unsafe: (default: ) leaves the reader unable to tell an empty
// default from a column that failed to print.
func TestHelpQuotesAnEmptyDefault(t *testing.T) {
	if got := defaultOf(t, renderedHelp(t, textFormManifest(t, "")), "--label"); got != `''` {
		t.Errorf("an empty default renders as %q, and an empty value is shown as ''", got)
	}
	if got := defaultOf(t, renderedHelp(t, defaultsCorpusManifest(t)), "--no-hosts"); got != `''` {
		t.Errorf("an empty list renders as %q, and an empty value is shown as ''", got)
	}
}

// TestHelpLeavesALeadingDashDefaultBare pins that a negative number is not
// quoted for opening with a dash. Quotes would change nothing about it: the
// shell removes them and the program still receives a word starting with a
// dash. The parser reads it as a value either way, in both spellings of the
// argument, which is what makes the bare rendering true.
func TestHelpLeavesALeadingDashDefaultBare(t *testing.T) {
	m := defaultsCorpusManifest(t)
	rendered := defaultOf(t, renderedHelp(t, m), "--offset")
	if rendered != "-42" {
		t.Fatalf("the negative default renders as %s, and its text needs no quotes", rendered)
	}
	typed := shellWord(t, rendered)
	for _, args := range [][]string{{"--offset=" + typed}, {"--offset", typed}} {
		p, err := parseFlags(m, args)
		if err != nil {
			t.Fatalf("the command line %q does not take the rendered default back: %v", args, err)
		}
		d := newData(m)
		if err := d.applyFlags(m, p); err != nil {
			t.Fatalf("the command line %q does not take the rendered default back: %v", args, err)
		}
		if got := d.values["greeter.offset"].value; got != -42 {
			t.Fatalf("the command line %q reads the rendered default back as %#v", args, got)
		}
	}
}
