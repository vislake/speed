package template

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// This file pins the generated project README against the template it
// ships beside -- the P3-6 audit finding's whole point: the golden/embed
// tests elsewhere in this package check the template tree's structure, but
// nothing before this file checked that the README's own PROSE agrees with
// the template's own source. Each test here extracts ground truth directly
// from the template source text (never a hand-copied literal this file
// could itself fall behind) and compares it against the README.

// readmeContent reads the embedded, shared project README once per test.
func readmeContent(t *testing.T) string {
	t.Helper()
	content, err := fs.ReadFile(Project, ProjectRoot+"/README.md")
	if err != nil {
		t.Fatalf("read project README: %v", err)
	}
	return string(content)
}

// goDirectivePattern matches a go.mod's "go X.Y.Z" directive line.
var goDirectivePattern = regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)$`)

// TestReadmeGoVersionMatchesGoModTxt: the "Go X.Y.Z or newer" prerequisite
// line in the README must name the exact version every selection's own
// go.mod.txt declares in its "go" directive -- not a stale figure from an
// earlier toolchain line. The check runs against every one of the five
// legal selections (README's own claim is selection-independent), so a
// selection whose go.mod.txt drifts from the others is caught too.
func TestReadmeGoVersionMatchesGoModTxt(t *testing.T) {
	readme := readmeContent(t)
	readmeVersionPattern := regexp.MustCompile(`Go (\d+\.\d+\.\d+) or newer`)
	m := readmeVersionPattern.FindStringSubmatch(readme)
	if m == nil {
		t.Fatal(`README does not state a "Go X.Y.Z or newer" prerequisite; the version-parity check has nothing to compare against`)
	}
	readmeVersion := m[1]

	for _, key := range validSelectionKeys {
		path := ProjectRoot + "/selection/" + key + "/go.mod.txt"
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		directive := goDirectivePattern.FindStringSubmatch(string(content))
		if directive == nil {
			t.Errorf("%s: no \"go X.Y.Z\" directive found", path)
			continue
		}
		if directive[1] != readmeVersion {
			t.Errorf("%s declares go %s but the README's Prerequisites section states Go %s or newer; the two have drifted",
				path, directive[1], readmeVersion)
		}
	}
}

// envVarBacktickPattern matches a backtick-quoted, all-uppercase (with
// digits and underscores) token in the README -- the exact shape every one
// of the seventeen bootstrap environment variable names takes when the
// README refers to it (e.g. APP_S3_ENDPOINT or PORT, each wrapped in a
// pair of backticks).
var envVarBacktickPattern = regexp.MustCompile("`([A-Z][A-Z0-9_]*)`")

// configGoEnvVarPattern matches one "<identifier>Env = \"<VALUE>\""
// constant declaration in the template's config.go -- deliberately
// duplicated from the identical pattern in
// internal/appconfig/appconfig_test.go's own drift-proof test rather than
// shared, since the two packages check two different kinds of drift (that
// one checks the Go twin's parse behavior, this one checks the README's
// prose) and neither should import the other's test helpers to do it.
var configGoEnvVarPattern = regexp.MustCompile(`(?m)^\s*[A-Za-z0-9]+Env\s*=\s*"([A-Za-z0-9_]+)"`)

// readEnvVarNamesFromConfigGo extracts the set of environment variable
// names the embedded template's cmd/server/config.go actually parses.
func readEnvVarNamesFromConfigGo(t *testing.T) map[string]bool {
	t.Helper()
	content, err := fs.ReadFile(Project, ProjectRoot+"/cmd/server/config.go")
	if err != nil {
		t.Fatalf("read the embedded template config.go: %v", err)
	}
	names := map[string]bool{}
	for _, m := range configGoEnvVarPattern.FindAllStringSubmatch(string(content), -1) {
		names[m[1]] = true
	}
	if len(names) == 0 {
		t.Fatal("extracted zero environment variable names from config.go; the extraction pattern itself has drifted")
	}
	return names
}

// TestReadmeEnvTableMatchesConfigGoExactly is the P3-6 fix's env-var
// consistency test: every environment variable the template's config.go
// actually parses must be documented in the README (this README is the
// consumer's first-run manual, so an undocumented variable is itself a
// finding), and every backtick-quoted, all-caps token the README's
// Bootstrap Environment section presents as a variable must be one
// config.go genuinely parses -- never a stale or invented name. Before
// this fix, the README named only 5 of the 17 variables config.go parses;
// this test fails on that state (RED), listing the twelve config.go
// parses that the README does not document.
func TestReadmeEnvTableMatchesConfigGoExactly(t *testing.T) {
	configVars := readEnvVarNamesFromConfigGo(t)
	readme := readmeContent(t)

	readmeVars := map[string]bool{}
	for _, m := range envVarBacktickPattern.FindAllStringSubmatch(readme, -1) {
		readmeVars[m[1]] = true
	}

	for name := range configVars {
		if !readmeVars[name] {
			t.Errorf("config.go parses %s but the README never mentions it; this README is the consumer's first-run manual and must document it", name)
		}
	}
	for name := range readmeVars {
		if !configVars[name] {
			t.Errorf("README names %s as a bootstrap variable but config.go does not parse it; the README has drifted ahead of (or never matched) the template", name)
		}
	}
}

// devKeyBacktickPattern matches a backtick-quoted "dev*" identifier in the
// README -- the shape every committed development key placeholder takes
// when the README names it (e.g. devConfigKey or devPKILocalKeyCipherKey,
// each wrapped in a pair of backticks).
var devKeyBacktickPattern = regexp.MustCompile("`(dev[A-Za-z0-9]+)`")

// devKeyDeclPattern matches one "dev<Name> = " assignment -- a var
// declaration's own opening line, whether config.go's own standalone
// "var dev<Name> = []byte{" form or an aligned one inside a var (...)
// block with no per-line "var" keyword (each selection's server.go).
var devKeyDeclPattern = regexp.MustCompile(`(?m)^\s*(?:var\s+)?(dev[A-Za-z0-9]+)\s*=`)

// TestReadmeNamesOnlyRealDevKeyIdentifiers is the P3-6 fix's stale-name
// guard: it collects every "dev*" identifier actually declared across the
// shared config.go and every selection's own server.go, then asserts the
// README's own "dev*" references are a subset of that real set -- a
// named-identifier check, not a single-string grep, so it catches any
// future stale name the same way it catches devSigningKeySeed today.
// Before this fix, the README named devSigningKeySeed, an identifier
// deleted from every generated server long ago (the pki-integration round
// replaced it with the devBlindIndexKey/devPIICipherKey/
// devPKILocalKeyCipherKey trio -- see go/pki/AGENTS.md's round-2 entry);
// this test fails on that state (RED), naming the orphaned identifier.
func TestReadmeNamesOnlyRealDevKeyIdentifiers(t *testing.T) {
	real := map[string]bool{}
	collect := func(path string) {
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range devKeyDeclPattern.FindAllStringSubmatch(string(content), -1) {
			real[m[1]] = true
		}
	}
	collect(ProjectRoot + "/cmd/server/config.go")
	for _, key := range validSelectionKeys {
		collect(ProjectRoot + "/selection/" + key + "/server.go")
	}
	if len(real) == 0 {
		t.Fatal("extracted zero dev* key identifiers from the template; the extraction pattern itself has drifted")
	}

	readme := readmeContent(t)
	referenced := map[string]bool{}
	for _, m := range devKeyBacktickPattern.FindAllStringSubmatch(readme, -1) {
		referenced[m[1]] = true
	}
	if len(referenced) == 0 {
		t.Fatal("README names zero dev* key identifiers; the reference extraction pattern itself has drifted")
	}
	for name := range referenced {
		if !real[name] {
			t.Errorf("README names %s but no template file declares it; the README refers to an identifier that does not exist in the generated project", name)
		}
	}
}

// TestReadmeEditingSectionStillNamesTheShippedCommands is a narrow
// regression guard for TestProjectReadmeNamesTheShippedMaintenanceCommands
// in embed_test.go: the "Editing and regenerating" section's own
// [redacted]-secrets sentence must still name every secret-shaped print
// row (the two key variables plus the two infrastructure credentials this
// round adds), so the README's own description of `saasctl config print`
// stays truthful once the command renders more than five lines.
func TestReadmeEditingSectionStillNamesTheShippedCommands(t *testing.T) {
	readme := readmeContent(t)
	const section = "## Editing and regenerating"
	start := strings.Index(readme, section)
	if start < 0 {
		t.Fatalf("README has no %q section", section)
	}
	body := strings.Join(strings.Fields(readme[start:]), " ")
	for _, want := range []string{"S3 secret key", "SMTP password"} {
		if !strings.Contains(body, want) {
			t.Errorf("%s section's config-print description does not mention %q; it will misrepresent which rows are redacted", section, want)
		}
	}
}
