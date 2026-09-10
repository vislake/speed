package main

// main_test.go is the execution test of the generator's own entry path:
// run() driven end to end over a throwaway repository root. The pins are
// the command's contract -- a write pass exits 0 and lands the four
// artifacts under the root it was told, --check exits 0 over outputs it
// just wrote (the drift gate's green answer) and exits 1 once an
// artifact is missing or stale (its red answer). The root the writes
// land in is a temp directory, so the generated artifacts land somewhere
// the committed tree never sees.

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeRepoRoot builds the throwaway repository root one run() drive
// writes into: the temp root itself for the artifacts.
func fakeRepoRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// TestRunWritesAndChecksTheOutputsEndToEnd drives run() through the
// generator's full lifecycle against one fake root: a write pass must
// exit 0 with every artifact present and non-empty at its documented
// root-relative path; a --check pass over those fresh outputs must exit 0
// (nothing is stale); and once an artifact is deleted and another tampered
// with, a further --check pass must exit 1 -- the drift gate's whole job is
// refusing a tree whose committed bytes do not match a fresh rendering, and
// this is that refusal executing against a real schema host.
func TestRunWritesAndChecksTheOutputsEndToEnd(t *testing.T) {
	root := fakeRepoRoot(t)

	// The JSON example is derived from config.example.yaml, which lives at the
	// real repository root rather than in the throwaway one: the fake root
	// carries only the generated artifacts, so the derivation source is linked
	// in.
	if err := os.Symlink(filepath.Join(repoRootFromTest(t), configExampleYAMLPath), filepath.Join(root, configExampleYAMLPath)); err != nil {
		t.Fatalf("symlink %s: %v", configExampleYAMLPath, err)
	}

	if code := run([]string{"--repo-root", root}); code != 0 {
		t.Fatalf("run (write pass) = %d, want 0", code)
	}
	for _, artifact := range []string{
		"docs/config-reference.md",
		"docs/config-reference.json",
		configExampleJSONPath,
		sitePagePath,
	} {
		info, err := os.Stat(filepath.Join(root, artifact))
		if err != nil {
			t.Errorf("artifact %s missing after the write pass: %v", artifact, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("artifact %s is empty after the write pass", artifact)
		}
	}

	if code := run([]string{"--repo-root", root, "--check"}); code != 0 {
		t.Fatalf("run (fresh --check) = %d, want 0 over outputs the write pass just produced", code)
	}

	// Make the tree stale on two distinct refusal paths at once: one
	// artifact gone entirely, another holding different bytes.
	if err := os.Remove(filepath.Join(root, "docs", "config-reference.md")); err != nil {
		t.Fatalf("remove config-reference.md: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "config.example.json"),
		[]byte("{\"deploymentmode\": \"distributed\"}\n"),
		0o644,
	); err != nil {
		t.Fatalf("tamper config.example.json: %v", err)
	}
	if code := run([]string{"--repo-root", root, "--check"}); code != 1 {
		t.Fatalf("run (stale --check) = %d, want 1 when an artifact is missing and another is out of date", code)
	}
}

// TestRun_ReportsAUsageErrorOnAnUnknownFlag pins the flag-parsing refusal:
// an argument the command does not define exits 2 without touching any
// output.
func TestRun_ReportsAUsageErrorOnAnUnknownFlag(t *testing.T) {
	if code := run([]string{"--no-such-flag"}); code != 2 {
		t.Fatalf("run with an unknown flag = %d, want 2", code)
	}
}

// TestRun_WithoutRepoRootFlag_RefusesWhenNoneIsFound pins the no-flag run
// mode's refusal: with no go.work in any ancestor of the working directory
// there is no root to write into, and the command exits 2 rather than
// guessing one. The working directory is a temp directory, which sits outside
// any repository.
func TestRun_WithoutRepoRootFlag_RefusesWhenNoneIsFound(t *testing.T) {
	t.Chdir(t.TempDir())
	if code := run(nil); code != 2 {
		t.Fatalf("run with no repository root above the working directory = %d, want 2", code)
	}
}

// TestFindRepoRoot_DiscoversTheAncestorCarryingGoWork pins the discovery the
// no-flag run mode depends on: a directory carrying go.work is its own
// answer, and a nested directory resolves to the ancestor that carries it.
func TestFindRepoRoot_DiscoversTheAncestorCarryingGoWork(t *testing.T) {
	root := t.TempDir()
	// The fixture only has to exist: findRepoRoot stats the file and never
	// parses it.
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("// repository-root marker for findRepoRoot\n"), 0o644); err != nil {
		t.Fatalf("write go.work: %v", err)
	}
	nested := filepath.Join(root, "examples", "reference-app")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", nested, err)
	}

	// The expectation is the physical spelling os.Getwd reports: findRepoRoot
	// walks up from that spelling, and a temp directory may be reached through
	// a symlink on some platforms.
	t.Chdir(root)
	physicalRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if got, err := findRepoRoot(); err != nil || got != physicalRoot {
		t.Fatalf("findRepoRoot at the root = %q, %v; want %q, nil", got, err, physicalRoot)
	}

	t.Chdir(nested)
	if got, err := findRepoRoot(); err != nil || got != physicalRoot {
		t.Fatalf("findRepoRoot at a nested directory = %q, %v; want %q, nil", got, err, physicalRoot)
	}
}

// TestFindRepoRoot_ReportsAFailureWithNoGoWorkAbove pins the walk's
// termination: with no go.work in any ancestor, discovery fails cleanly
// instead of looping at the filesystem root.
func TestFindRepoRoot_ReportsAFailureWithNoGoWorkAbove(t *testing.T) {
	t.Chdir(t.TempDir())
	if got, err := findRepoRoot(); err == nil {
		t.Fatalf("findRepoRoot outside a repository = %q, want an error", got)
	}
}
