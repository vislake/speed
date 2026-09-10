#!/usr/bin/env python3
"""Unit tests for check_host_composition.py's rules.

Stdlib-only (unittest + tempfile), matching this directory's
conventions. Run directly:

    python3 tools/test_check_host_composition.py

The real tree passes the gate clean, so these planted fixtures are the
rules' living proof, in the same shape as the sibling checker suites:

  * a host that merely USES the kernel (imports the platform module) stays
    silent -- the positive side, proving the gate is not simply firing on
    everything;
  * a forked declaration of a shared identifier in either host fires,
    while a test file's own use of the identifiers does not;
  * a re-grown kernel sentinel in a non-test file fires, while the same
    text in a test file stays silent.
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_host_composition as m  # noqa: E402


def make_tree(files: dict[str, str]) -> pathlib.Path:
    root = pathlib.Path(tempfile.mkdtemp(prefix="host-composition-"))
    for (rel, text) in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
    return root


class NoForkRules(unittest.TestCase):
    def test_a_host_importing_the_kernel_stays_silent(self):
        root = make_tree(
            {
                "examples/reference-app/internal/app/server.go": (
                    "package app\n\n"
                    "import speedapp \"github.com/vislake/speed/go/app\"\n\n"
                    "func build() { _ = speedapp.PreAuthAllowlist() }\n"
                ),
                "go/saasctl/internal/template/project/cmd/server/main.go": (
                    "package main\n\n"
                    "import speedapp \"github.com/vislake/speed/go/app\"\n\n"
                    "func run() { _ = speedapp.ServeUntilShutdown }\n"
                ),
            }
        )
        self.assertEqual(m.scan(root), [])

    def test_forked_declaration_in_the_app_fires(self):
        root = make_tree(
            {
                "examples/reference-app/internal/app/server.go": (
                    "package app\n\n// AuthnAPIPath re-declares the shared name.\n"
                    "const AuthnAPIPath = \"/api/v1/authn\"\n"
                ),
            }
        )
        findings = m.scan(root)
        # Two findings, both true: the declaration fork, and the
        # kernel-owned path literal it reintroduces.
        self.assertTrue(
            any("declares AuthnAPIPath" in f for f in findings), findings
        )
        self.assertTrue(
            any('"/api/v1/authn"' in f for f in findings), findings
        )

    def test_forked_declaration_in_the_template_fires(self):
        root = make_tree(
            {
                "go/saasctl/internal/template/project/cmd/server/server.go": (
                    "package main\n\nfunc PreAuthAllowlist() {}\n"
                ),
            }
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("declares PreAuthAllowlist", findings[0])

    def test_forked_lifecycle_declaration_fires(self):
        root = make_tree(
            {
                "examples/reference-app/cmd/server/main.go": (
                    "package main\n\nfunc ServeUntilShutdown() error { return nil }\n"
                ),
            }
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("declares ServeUntilShutdown", findings[0])

    def test_sentinel_in_a_non_test_file_fires(self):
        root = make_tree(
            {
                "examples/reference-app/internal/app/server.go": (
                    "package app\n\nfunc serve() { srv.ListenAndServe() }\n"
                ),
            }
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("ListenAndServe", findings[0])

    def test_sentinel_in_a_test_file_stays_silent(self):
        root = make_tree(
            {
                "examples/reference-app/flowtests/probe_test.go": (
                    "package flowtests\n\n"
                    "func TestProbe(t *testing.T) { mux.HandleFunc(\"/healthz\", h) }\n"
                ),
            }
        )
        self.assertEqual(m.scan(root), [])


if __name__ == "__main__":
    unittest.main()
