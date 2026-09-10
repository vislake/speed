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
