#!/usr/bin/env python3
"""Unit tests for check_host_core_parity.py's rules.

Stdlib-only (unittest + tempfile), matching this directory's
conventions. Run directly:

    python3 tools/test_check_host_core_parity.py

The real tree passes the gate clean, so these planted fixtures are the
rules' living proof, in the same shape as the sibling checker suites:

  * two identical copies (modulo the template marker) stay silent -- the
    positive side, proving the gate is not simply firing on everything;
  * a content drift in either direction fires, naming both files;
  * a missing copy fires;
  * a marker on the app copy (or a missing marker on the canonical copy)
    fires;
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

import check_host_core_parity as m  # noqa: E402

CORE_BODY = '''// Package hostcore is the shared host kernel.
package hostcore

// AuthnAPIPath is authn's own mount point.
const AuthnAPIPath = "/api/v1/authn"

// PreAuthAllowlist returns the tenancy options exempting the pre-auth
// surface.
func PreAuthAllowlist() []tenancy.MiddlewareOption { return nil }
'''
CANONICAL = "//go:build ignore\n\n" + CORE_BODY


def make_tree(files: dict[str, str]) -> pathlib.Path:
    root = pathlib.Path(tempfile.mkdtemp(prefix="hostcore-parity-"))
    for (rel, text) in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
    return root


def base_files(canonical: str = CANONICAL, app: str = CORE_BODY, extra=None):
    files = {
        m.CANONICAL_REL_PATH: canonical,
        m.APP_COPY_REL_PATH: app,
    }
    if extra:
        files.update(extra)
    return files


class CopyParity(unittest.TestCase):
    def test_identical_copies_stay_silent(self):
        self.assertEqual(m.scan(make_tree(base_files())), [])

    def test_content_drift_fires_with_a_diff(self):
        root = make_tree(base_files(app=CORE_BODY.replace("/api/v1/authn", "/api/v1/authn2")))
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("drifted apart", findings[0])
        self.assertIn(m.APP_COPY_REL_PATH, findings[0])

    def test_missing_app_copy_fires(self):
        files = base_files()
        del files[m.APP_COPY_REL_PATH]
        findings = m.scan(make_tree(files))
        self.assertEqual(len(findings), 1)
        self.assertIn("copy is missing", findings[0])

    def test_marker_on_the_app_copy_fires(self):
        # The mismatch also surfaces as a content diff (the app copy is
        # the marked file, not the marker-stripped one) -- both findings
        # name the same defect, so this asserts on content, not count.
        root = make_tree(base_files(app=CANONICAL))
        findings = m.scan(root)
        self.assertTrue(
            any("must not carry the template marker" in f for f in findings),
            findings,
        )

    def test_missing_marker_on_the_canonical_copy_fires(self):
        root = make_tree(base_files(canonical=CORE_BODY))
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("must open with", findings[0])


class NoForkRules(unittest.TestCase):
    def test_forked_declaration_in_the_app_fires(self):
        root = make_tree(
            base_files(
                extra={
                    "examples/reference-app/internal/app/server.go": (
                        "package app\n\n// AuthnAPIPath re-declares the shared name.\n"
                        "const AuthnAPIPath = \"/api/v1/authn\"\n"
                    ),
                }
            )
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
            base_files(
                extra={
                    "go/saasctl/internal/template/project/cmd/server/server.go": (
                        "package main\n\nfunc PreAuthAllowlist() {}\n"
                    ),
                }
            )
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("declares PreAuthAllowlist", findings[0])

    def test_use_is_not_a_declaration(self):
        root = make_tree(
            base_files(
                extra={
                    "examples/reference-app/internal/app/server.go": (
                        "package app\n\n"
                        "import \"example.com/app/internal/hostcore\"\n\n"
                        "func build() { _ = hostcore.PreAuthAllowlist() }\n"
                    ),
                }
            )
        )
        self.assertEqual(m.scan(root), [])

    def test_sentinel_in_a_non_test_file_fires(self):
        root = make_tree(
            base_files(
                extra={
                    "examples/reference-app/internal/app/server.go": (
                        "package app\n\nfunc serve() { srv.ListenAndServe() }\n"
                    ),
                }
            )
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("ListenAndServe", findings[0])

    def test_sentinel_in_a_test_file_stays_silent(self):
        root = make_tree(
            base_files(
                extra={
                    "examples/reference-app/flowtests/probe_test.go": (
                        "package flowtests\n\n"
                        "func TestProbe(t *testing.T) { mux.HandleFunc(\"/healthz\", h) }\n"
                    ),
                }
            )
        )
        self.assertEqual(m.scan(root), [])


if __name__ == "__main__":
    unittest.main()
