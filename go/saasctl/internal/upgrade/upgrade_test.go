package upgrade

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// target is the release version most data-level tests rewrite to.
const target = "v1.0.0"

// readFixture returns the named file from testdata.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return data
}

// requireVersion splits one line of a go.mod require (a require-block
// member or a single-line require) into its module path and version
// token, reporting ok=false for anything else (comments, blank lines).
func requireVersion(line string) (path, version string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.HasPrefix(fields[0], "//") {
		return "", "", false
	}
	if fields[0] == "require" {
		fields = fields[1:]
	}
	if len(fields) < 2 || strings.HasPrefix(fields[1], "//") {
		return "", "", false
	}
	return fields[0], fields[1], true
}

// speedRequireCount returns how many requires in data name a speed module.
func speedRequireCount(t *testing.T, data []byte) int {
	t.Helper()
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	n := 0
	for _, req := range f.Require {
		if strings.HasPrefix(req.Mod.Path, modulePrefix) {
			n++
		}
	}
	return n
}

// TestRewriteTouchesOnlySpeedRequireLines pins the upgrade contract at the
// byte level: against a consumer-shaped go.mod, rewriting to a target
// version changes exactly the speed module require lines, each of them by
// nothing more than its version token. Third-party requires, replace
// directives, comments, blank lines and formatting must survive verbatim.
func TestRewriteTouchesOnlySpeedRequireLines(t *testing.T) {
	orig := readFixture(t, "consumer.go.mod")
	want := speedRequireCount(t, orig)
	if want != 9 {
		t.Fatalf("fixture drift: consumer.go.mod carries %d speed requires, want 9", want)
	}
	out, changed, err := Rewrite(orig, target)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if changed != want {
		t.Fatalf("changed = %d, want %d", changed, want)
	}
	origLines := strings.Split(strings.TrimSuffix(string(orig), "\n"), "\n")
	outLines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(origLines) != len(outLines) {
		t.Fatalf("line count changed: %d in, %d out:\n%s", len(origLines), len(outLines), out)
	}
	for i := range origLines {
		if origLines[i] == outLines[i] {
			continue
		}
		path, ver, ok := requireVersion(origLines[i])
		if !ok || !strings.HasPrefix(path, modulePrefix) {
			t.Errorf("line %d changed but is not a speed module require line:\n  in:  %q\n  out: %q", i+1, origLines[i], outLines[i])
			continue
		}
		if ver == target {
			t.Errorf("line %d already carried %s yet changed:\n  in:  %q\n  out: %q", i+1, target, origLines[i], outLines[i])
		}
		if wantLine := strings.Replace(origLines[i], ver, target, 1); outLines[i] != wantLine {
			t.Errorf("line %d differs beyond its version token:\n  want: %q\n  out:  %q", i+1, wantLine, outLines[i])
		}
	}
}

// TestRewritePreservesNonSpeedContent checks the same contract
// structurally: the rewritten go.mod carries every non-speed require at its
// original version and every replace directive from the input.
func TestRewritePreservesNonSpeedContent(t *testing.T) {
	orig := readFixture(t, "consumer.go.mod")
	out, _, err := Rewrite(orig, target)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	before, err := modfile.Parse("go.mod", orig, nil)
	if err != nil {
		t.Fatalf("parse input: %v", err)
	}
	after, err := modfile.Parse("go.mod", out, nil)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	for _, req := range before.Require {
		if strings.HasPrefix(req.Mod.Path, modulePrefix) {
			continue
		}
		for _, got := range after.Require {
			if got.Mod.Path == req.Mod.Path {
				if got.Mod.Version != req.Mod.Version {
					t.Errorf("non-speed require %s changed: %s -> %s", req.Mod.Path, req.Mod.Version, got.Mod.Version)
				}
				break
			}
		}
	}
	if !equalReplaceKeys(replaceKeys(before), replaceKeys(after)) {
		t.Errorf("replace directives changed across the rewrite:\nbefore: %v\nafter:  %v", replaceKeys(before), replaceKeys(after))
	}
	// Every speed require sits at exactly the target version now, and the
	// // indirect marker on the ratelimit require survived with its line.
	for _, req := range after.Require {
		if strings.HasPrefix(req.Mod.Path, modulePrefix) && req.Mod.Version != target {
			t.Errorf("speed require %s at %s, want %s", req.Mod.Path, req.Mod.Version, target)
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		path, ver, ok := requireVersion(line)
		if ok && path == modulePrefix+"ratelimit" && ver == target && !strings.Contains(line, "// indirect") {
			t.Errorf("ratelimit require lost its // indirect marker: %q", line)
		}
	}
}

// TestRewriteHealsMixedVersions pins the lockstep rule: a consumer go.mod
// whose speed requires drifted out of lockstep is healed to one uniform
// version, never refused and never left mixed.
func TestRewriteHealsMixedVersions(t *testing.T) {
	mixed := readFixture(t, "mixed.go.mod")
	if n := speedRequireCount(t, mixed); n != 9 {
		t.Fatalf("fixture drift: mixed.go.mod carries %d speed requires, want 9", n)
	}
	out, changed, err := Rewrite(mixed, target)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if changed != 9 {
		t.Fatalf("changed = %d, want 9 (the v0.1.0 rbac line included)", changed)
	}
	// The fixture's own comment prose mentions v0.1.0 deliberately; what
	// must not survive is a require line off the target version.
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if path, ver, ok := requireVersion(line); ok && strings.HasPrefix(path, modulePrefix) && ver != target {
			t.Errorf("%s still at %s after the rewrite, want %s", path, ver, target)
		}
	}
}

// TestRewriteAlreadyAtTargetReturnsInputUnchanged: rewriting a go.mod that
// already carries the target everywhere is a success no-op returning the
// input bytes verbatim.
func TestRewriteAlreadyAtTargetReturnsInputUnchanged(t *testing.T) {
	orig := readFixture(t, "consumer.go.mod")
	once, _, err := Rewrite(orig, target)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	out, changed, err := Rewrite(once, target)
	if err != nil {
		t.Fatalf("second Rewrite: %v", err)
	}
	if changed != 0 {
		t.Errorf("changed = %d on an already-rewritten go.mod, want 0", changed)
	}
	if !bytes.Equal(out, once) {
		t.Errorf("second rewrite returned different bytes:\nwant:\n%s\ngot:\n%s", once, out)
	}
}

// TestRewriteIdempotent: rewriting twice equals rewriting once, byte for
// byte, on the consumer fixture and on the mixed one.
func TestRewriteIdempotent(t *testing.T) {
	for _, name := range []string{"consumer.go.mod", "mixed.go.mod"} {
		data := readFixture(t, name)
		once, _, err := Rewrite(data, target)
		if err != nil {
			t.Fatalf("%s: Rewrite: %v", name, err)
		}
		twice, _, err := Rewrite(once, target)
		if err != nil {
			t.Fatalf("%s: second Rewrite: %v", name, err)
		}
		if !bytes.Equal(once, twice) {
			t.Errorf("%s: rewrite is not idempotent:\nfirst:\n%s\nsecond:\n%s", name, once, twice)
		}
	}
}

// TestRewriteNoSpeedRequires: a go.mod with no speed module requires --
// third-party-only or with no requires at all -- is an execution error, not
// a silent no-op: the consumer did not run this command against the project
// it thinks it did.
func TestRewriteNoSpeedRequires(t *testing.T) {
	for name, data := range map[string][]byte{
		"third-party-only go.mod": readFixture(t, "thirdparty-only.go.mod"),
		"empty go.mod":            {},
	} {
		out, changed, err := Rewrite(data, target)
		if err == nil {
			t.Errorf("%s: Rewrite succeeded, want error", name)
			continue
		}
		if changed != 0 || out != nil {
			t.Errorf("%s: Rewrite returned changed=%d out=%q alongside its error", name, changed, out)
		}
		if !strings.Contains(err.Error(), "no github.com/vislake/speed/go/* requires found") {
			t.Errorf("%s: error %q does not name the missing speed requires", name, err)
		}
	}
}

// TestRewriteRejectsMalformedGoMod: malformed go.mod text is a parse error
// naming the parse step, never a partial rewrite.
func TestRewriteRejectsMalformedGoMod(t *testing.T) {
	out, changed, err := Rewrite([]byte("module example.com/app\n\ngo 1.25.0\n\nrequire (\n"), target)
	if err == nil || !strings.Contains(err.Error(), "parse go.mod") {
		t.Errorf("Rewrite error = %v, want a parse go.mod error", err)
	}
	if changed != 0 || out != nil {
		t.Errorf("malformed input produced changed=%d out=%q", changed, out)
	}
}

// TestRewriteRejectsInvalidVersion: versions outside the release-version
// form the pipeline validates are refused by the rewrite itself, before any
// file is touched.
func TestRewriteRejectsInvalidVersion(t *testing.T) {
	orig := readFixture(t, "consumer.go.mod")
	for _, bad := range []string{"", "v1", "1.2.3", "v1.2.3.4", "v1.2", "1.2.3.4", "latest"} {
		out, changed, err := Rewrite(orig, bad)
		if err == nil {
			t.Errorf("target %q: Rewrite succeeded, want error", bad)
			continue
		}
		if changed != 0 || out != nil {
			t.Errorf("target %q: Rewrite returned changed=%d out=%q alongside its error", bad, changed, out)
		}
		if !strings.Contains(err.Error(), "invalid release version") {
			t.Errorf("target %q: error %q is not a version-form error", bad, err)
		}
	}
}

// TestSelfCheckDetectsMixedVersions reaches the offline self-check with a
// hand-crafted mixed-version go.mod -- the one input the rewrite itself can
// never produce -- proving the check catches a non-uniform result.
func TestSelfCheckDetectsMixedVersions(t *testing.T) {
	mixed, err := modfile.Parse("go.mod", readFixture(t, "mixed.go.mod"), nil)
	if err != nil {
		t.Fatalf("parse mixed fixture: %v", err)
	}
	err = selfCheck(mixed, "v0.1.0", replaceKeys(mixed))
	if err == nil {
		t.Fatal("selfCheck passed a mixed-version go.mod, want failure")
	}
	for _, want := range []string{
		"github.com/vislake/speed/go/authn",
		"v0.0.0-00010101000000-000000000000",
		"not the lockstep version v0.1.0",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("selfCheck error %q does not mention %q", err, want)
		}
	}
}

// TestSelfCheckDetectsReplaceDrift: the same check must catch replace
// directives that differ from the input's own.
func TestSelfCheckDetectsReplaceDrift(t *testing.T) {
	consumer, err := modfile.Parse("go.mod", readFixture(t, "consumer.go.mod"), nil)
	if err != nil {
		t.Fatalf("parse consumer fixture: %v", err)
	}
	err = selfCheck(consumer, "v0.0.0-00010101000000-000000000000", nil)
	if err == nil || !strings.Contains(err.Error(), "replace directives changed across the rewrite") {
		t.Errorf("selfCheck error = %v, want a replace-drift failure", err)
	}
}

// equalReplaceKeys compares two sorted replace-key slices.
func equalReplaceKeys(a, b []replaceKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// runUpgrade invokes the command entry point and returns its exit code and
// captured output.
func runUpgrade(args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	code = Run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// writeTempFile writes data into a fresh temp file and returns its path.
func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestRunVersionRequiredIsUsageError: no --version is a usage error naming
// the reason -- nothing is published yet, so the version is never
// discovered -- with the usage text on stderr and exit code 2.
func TestRunVersionRequiredIsUsageError(t *testing.T) {
	code, stdout, stderr := runUpgrade()
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--version is required") {
		t.Errorf("stderr %q does not explain the missing --version", stderr)
	}
	if !strings.Contains(stderr, "Usage: saasctl upgrade") {
		t.Errorf("stderr %q lacks the usage text", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

// TestRunRejectsInvalidVersionFlag: an ill-formed --version is a usage
// error carrying the version validator's message.
func TestRunRejectsInvalidVersionFlag(t *testing.T) {
	for _, bad := range []string{"v1", "1.2.3", "v1.2.3.4"} {
		code, _, stderr := runUpgrade("--version", bad)
		if code != 2 {
			t.Errorf("--version %s: code = %d, want 2", bad, code)
		}
		if !strings.Contains(stderr, `invalid release version "`+bad+`"`) {
			t.Errorf("--version %s: stderr %q lacks the validator's message", bad, stderr)
		}
	}
}

// TestRunHelpExitsZero: -h prints the command usage and exits 0.
func TestRunHelpExitsZero(t *testing.T) {
	code, stdout, stderr := runUpgrade("-h")
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(stderr, "Usage: saasctl upgrade") {
		t.Errorf("stderr %q lacks the usage text", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

// TestRunRejectsTooManyPaths: more than one go.mod argument is a usage
// error.
func TestRunRejectsTooManyPaths(t *testing.T) {
	code, _, stderr := runUpgrade("--version", target, "one.mod", "two.mod")
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "expected at most one go.mod path, got 2") {
		t.Errorf("stderr %q does not name the argument problem", stderr)
	}
}

// TestRunMissingFileIsExecutionError: an unreadable go.mod path is an
// execution error (exit 1) naming the file.
func TestRunMissingFileIsExecutionError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-go.mod")
	code, stdout, stderr := runUpgrade("--version", target, missing)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "saasctl upgrade: read "+missing) {
		t.Errorf("stderr %q does not name the unreadable file", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

// TestRunRewritesFile drives the whole command against a real go.mod on
// disk: exit 0, one stdout report line, and the file rewritten so that
// every speed require carries the target while the rest is untouched.
func TestRunRewritesFile(t *testing.T) {
	path := writeTempFile(t, "go.mod", readFixture(t, "consumer.go.mod"))
	relVersion := "v1.0.0-rc.1"
	code, stdout, stderr := runUpgrade("--version", relVersion, path)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr)
	}
	wantLine := "Rewrote 9 github.com/vislake/speed/go/* require lines to " + relVersion + " in " + path + "\n"
	if stdout != wantLine {
		t.Errorf("stdout = %q, want %q", stdout, wantLine)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten file: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(onDisk), "\n"), "\n") {
		if p, ver, ok := requireVersion(line); ok && strings.HasPrefix(p, modulePrefix) && ver != relVersion {
			t.Errorf("%s at %s after the run, want %s", p, ver, relVersion)
		}
	}
	if strings.Contains(string(onDisk), "v0.0.0-00010101000000-000000000000") {
		t.Errorf("transition-state pin survived the run:\n%s", onDisk)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(onDisk), "\n"), "\n") {
		if p, ver, ok := requireVersion(line); ok && !strings.HasPrefix(p, modulePrefix) && ver == relVersion {
			t.Errorf("non-speed require %s rewritten to %s", p, ver)
		}
	}
}

// TestRunSecondRunIsNoop: the same command against an already-upgraded
// go.mod succeeds with an "already" report and changes no byte on disk.
func TestRunSecondRunIsNoop(t *testing.T) {
	path := writeTempFile(t, "go.mod", readFixture(t, "consumer.go.mod"))
	if code, _, stderr := runUpgrade("--version", target, path); code != 0 {
		t.Fatalf("first run: code = %d, stderr: %s", code, stderr)
	}
	afterFirst, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after first run: %v", err)
	}
	code, stdout, stderr := runUpgrade("--version", target, path)
	if code != 0 {
		t.Fatalf("second run: code = %d, want 0; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "already carry "+target+"; nothing to rewrite") {
		t.Errorf("second-run stdout %q does not report the no-op", stdout)
	}
	afterSecond, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after second run: %v", err)
	}
	if !bytes.Equal(afterFirst, afterSecond) {
		t.Errorf("second run changed the file on disk")
	}
}

// TestRunNoSpeedRequiresFileIsExecutionError: upgrading a go.mod with no
// speed requires fails with exit 1 and leaves the file untouched.
func TestRunNoSpeedRequiresFileIsExecutionError(t *testing.T) {
	path := writeTempFile(t, "go.mod", readFixture(t, "thirdparty-only.go.mod"))
	code, stdout, stderr := runUpgrade("--version", target, path)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no github.com/vislake/speed/go/* requires found") {
		t.Errorf("stderr %q does not explain the failure", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(onDisk, readFixture(t, "thirdparty-only.go.mod")) {
		t.Errorf("failed run modified the file on disk")
	}
}

// TestRunMalformedFileIsExecutionError: malformed go.mod text on disk fails
// with exit 1 and a parse error, leaving the file untouched.
func TestRunMalformedFileIsExecutionError(t *testing.T) {
	broken := []byte("module example.com/app\n\ngo 1.25.0\n\nrequire (\n")
	path := writeTempFile(t, "go.mod", broken)
	code, _, stderr := runUpgrade("--version", target, path)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "parse go.mod") {
		t.Errorf("stderr %q does not name the parse failure", stderr)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(onDisk, broken) {
		t.Errorf("failed run modified the file on disk")
	}
}
