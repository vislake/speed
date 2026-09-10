package main

// outputs.go owns the write and --check paths of the committed outputs. The
// drift gate compares the committed bytes against a fresh rendering -- never a
// git diff, so an output that is missing entirely fails the same as one that
// is stale.

import (
	"fmt"
	"os"
	"path/filepath"
)

// outputFile is one committed artifact the generator owns.
type outputFile struct {
	// path is the file path, relative to the repository root.
	path string
	// content is the deterministic bytes the generator renders.
	content string
}

// renderOutputs assembles the committed artifacts: the reference in both
// spellings, the JSON counterpart of the config-file example, and the
// documentation site's copy of the reference.
func renderOutputs(root string, doc *document) ([]outputFile, error) {
	jsonExample, err := deriveConfigExampleJSON(root)
	if err != nil {
		return nil, err
	}
	return []outputFile{
		{path: "docs/config-reference.md", content: doc.renderMarkdown()},
		{path: "docs/config-reference.json", content: doc.marshalJSON()},
		{path: "config.example.json", content: jsonExample},
		{path: sitePagePath, content: doc.sitePage()},
	}, nil
}

// writeOutputs writes the artifacts and reports what was written.
func writeOutputs(root string, doc *document) int {
	outputs, err := renderOutputs(root, doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configrefgen: %v\n", err)
		return 1
	}
	for _, out := range outputs {
		abs := filepath.Join(root, out.path)
		// #nosec G301 -- the generator writes committed documentation that
		// every repository reader must be able to read; 0755 is the
		// repository's own committed directory mode, and the path is the
		// fixed artifact location below the repository root, never caller
		// input.
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "configrefgen: %v\n", err)
			return 1
		}
		// #nosec G306 -- same rationale as the MkdirAll above: committed
		// documentation files are world-readable by design (0644).
		if err := os.WriteFile(abs, []byte(out.content), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "configrefgen: write %s: %v\n", out.path, err)
			return 1
		}
		fmt.Printf("wrote %s\n", out.path)
	}
	return 0
}

// checkOutputs compares the committed artifacts against fresh renderings and
// exits nonzero when any differs or is missing, printing the first
// difference.
func checkOutputs(root string, doc *document) int {
	outputs, err := renderOutputs(root, doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configrefgen: %v\n", err)
		return 1
	}
	stale := false
	for _, out := range outputs {
		abs := filepath.Join(root, out.path)
		// #nosec G304 -- reading the committed artifact back is the drift
		// gate's whole job; the path is one of the fixed artifact names
		// below the repository root, never caller input.
		current, err := os.ReadFile(abs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s is missing or unreadable; run go run . from tools/configrefgen/ (%v)\n", out.path, err)
			stale = true
			continue
		}
		if string(current) != out.content {
			fmt.Fprintf(os.Stderr, "%s is out of date; run go run . from tools/configrefgen/\n", out.path)
			stale = true
		}
	}
	if stale {
		return 1
	}
	c := doc.counts()
	fmt.Printf("all %d committed artifacts are up to date (%d declared key(s), %d dynamic item(s)).\n",
		len(outputs), c.declared, c.dynamic)
	return 0
}
