#!/usr/bin/env python3
"""Unit tests for check_docs_site.py's link-resolution check.

Stdlib-only (unittest + tempfile), matching this directory's own "plain
executables with no third-party dependencies" convention (tools/README.md's
"Running in CI and locally" section) -- no pytest, no fixtures directory, no
network, and no real `hugo` build: these tests exercise `_check_links`
directly against a hand-built fake public/ tree, so they run anywhere
Python runs. Run directly:

    python3 tools/test_check_docs_site.py

Regression coverage: a real minified Hugo build (`hugo --minify --gc`, the
flag every build in this repo runs) strips the quotes from any href/src
value containing no whitespace/quote/angle-bracket character -- which is
true of nearly every internal link this site emits, e.g.
`<link href=/speed/favicon.png>`. A link-resolution regex that only matches
quoted attribute values therefore matches nothing on a real minified build
and silently no-ops the whole check. test_unquoted_broken_link_is_a_violation
reproduces that exact failure mode: it fails before the unquoted-value
regex branch was added, and passes after.
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_docs_site as m  # noqa: E402


class CheckLinksTests(unittest.TestCase):
    def _run(self, page_html: str, base_path: str = "/speed") -> list[str]:
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            public_dir = root / "public"
            public_dir.mkdir()
            (public_dir / "docs").mkdir()
            (public_dir / "docs" / "quickstart").mkdir()
            page = public_dir / "docs" / "quickstart" / "index.html"
            page.write_text(page_html, encoding="utf-8")
            # A real target the page may legitimately link to.
            (public_dir / "docs" / "modules").mkdir()
            (public_dir / "docs" / "modules" / "index.html").write_text(
                "<html></html>", encoding="utf-8"
            )
            return m._check_links(public_dir, [page], root, base_path)

    def test_unquoted_broken_link_is_a_violation(self):
        # This is exactly the shape a real `hugo --minify` build emits: no
        # quotes around the href value at all.
        violations = self._run(
            "<html><body>"
            "<a href=/speed/nonexistent-page/>broken</a>"
            "</body></html>"
        )
        self.assertEqual(len(violations), 1)
        self.assertIn("nonexistent-page", violations[0])

    def test_unquoted_valid_link_is_not_a_violation(self):
        violations = self._run(
            "<html><body>"
            "<a href=/speed/docs/modules/>modules</a>"
            "</body></html>"
        )
        self.assertEqual(violations, [])

    def test_quoted_broken_link_is_still_a_violation(self):
        # The pre-fix quoted-only regex already caught this shape; keep it
        # covered so the fix does not regress the quoted case.
        violations = self._run(
            '<html><body>'
            '<a href="/speed/nonexistent-page/">broken</a>'
            '</body></html>'
        )
        self.assertEqual(len(violations), 1)
        self.assertIn("nonexistent-page", violations[0])

    def test_unquoted_link_outside_base_path_is_a_violation(self):
        violations = self._run(
            "<html><body>"
            "<a href=/nonexistent-page/>escaped</a>"
            "</body></html>"
        )
        self.assertEqual(len(violations), 1)
        self.assertIn("rooted outside this site's own baseURL path", violations[0])

    def test_unquoted_src_attribute_is_checked_too(self):
        violations = self._run(
            "<html><body>"
            "<img src=/speed/icons/missing.svg>"
            "</body></html>"
        )
        self.assertEqual(len(violations), 1)
        self.assertIn("missing.svg", violations[0])

    def test_unquoted_link_terminated_by_other_attribute(self):
        # A real Hugo build often has more attributes after href/src with no
        # quotes anywhere, e.g. `<link rel=icon href=/speed/x.png>` or an
        # <a> tag with a trailing class attribute.
        violations = self._run(
            "<html><body>"
            "<a href=/speed/nonexistent-page/ class=book-icon>broken</a>"
            "</body></html>"
        )
        self.assertEqual(len(violations), 1)
        self.assertIn("nonexistent-page", violations[0])


if __name__ == "__main__":
    unittest.main()
