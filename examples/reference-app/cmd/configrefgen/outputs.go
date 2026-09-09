package main

// outputs.go owns the write and --check paths of the three committed
// outputs. The drift gate compares the committed bytes against a fresh
// rendering -- never a git diff, so an output that is missing entirely fails
// the same as one that is stale.

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

// renderOutputs assembles the three committed artifacts.
func renderOutputs(root string, doc *document, bootRows []bootstrapVar) []outputFile {
	markdown := doc.renderMarkdown()
	envExample := renderEnvExample(bootRows)
	jsonOut := doc.marshalJSON()
	return []outputFile{
		{path: "docs/config-reference.md", content: markdown},
		{path: "docs/config-reference.json", content: jsonOut},
		{path: ".env.example", content: envExample},
	}
}

// writeOutputs writes the three artifacts and reports what was written.
func writeOutputs(root string, doc *document, bootRows []bootstrapVar) int {
	for _, out := range renderOutputs(root, doc, bootRows) {
		abs := filepath.Join(root, out.path)
		// #nosec G301 -- the generator writes committed documentation
		// (docs/config-reference.* and .env.example) that every repository
		// reader must be able to read; 0755 is the repository's own
		// committed directory mode, and the path is the fixed artifact
		// location below the repository root, never caller input.
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
func checkOutputs(root string, doc *document, bootRows []bootstrapVar) int {
	stale := false
	for _, out := range renderOutputs(root, doc, bootRows) {
		abs := filepath.Join(root, out.path)
		// #nosec G304 -- reading the committed artifact back is the drift
		// gate's whole job; the path is one of the fixed artifact names
		// below the repository root, never caller input.
		current, err := os.ReadFile(abs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s is missing or unreadable; run go run ./cmd/configrefgen from examples/reference-app/ (%v)\n", out.path, err)
			stale = true
			continue
		}
		if string(current) != out.content {
			fmt.Fprintf(os.Stderr, "%s is out of date; run go run ./cmd/configrefgen from examples/reference-app/\n", out.path)
			stale = true
		}
	}
	if stale {
		return 1
	}
	fmt.Printf("docs/config-reference.md, docs/config-reference.json and .env.example are up to date (%d dynamic item(s), %d bootstrap variable(s)).\n", dynamicCount(doc), bootstrapCount(doc))
	return 0
}

func dynamicCount(doc *document) int {
	n := 0
	for _, it := range doc.Items {
		if it.Layer == "dynamic" {
			n++
		}
	}
	return n
}

func bootstrapCount(doc *document) int {
	n := 0
	for _, it := range doc.Items {
		if it.Layer == "bootstrap" {
			n++
		}
	}
	return n
}
