#!/usr/bin/env python3
"""Unit tests for check_i18n_keys.py's duplicate-leaf-id defensive rule.

Stdlib-only (unittest + tempfile), matching this directory's own "plain
executables with no third-party dependencies" convention (tools/README.md's
"Running in CI and locally" section). Run directly:

    python3 tools/test_check_i18n_keys.py

Regression coverage: the module docstring's defensive rule promises that
a file defining the same leaf id under two different paths (two sections
both carrying a key of the same name) is reported as an error rather than
compared -- pairing across languages would be ambiguous. extract_message_ids
always collected the full dotted paths for exactly this check, but
load_toml_ids discarded them, so the guard was never real: a zh-CN/en-US
pair where BOTH files hid the same leaf under different sections was
judged matching and passed. test_duplicate_leaf_under_two_sections_is_
refused fails before the fix (exit 0, judged matching) and passes after
(exit 1); test_unique_leaves_across_sections_still_match pins that honest
grouping keeps passing.
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_i18n_keys as m  # noqa: E402


class DuplicateLeafIdTests(unittest.TestCase):
    def _run_on(self, zh: str, en: str) -> int:
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            locales = root / "mod" / "locales"
            locales.mkdir(parents=True)
            (locales / "zh-CN.toml").write_text(zh, encoding="utf-8")
            (locales / "en-US.toml").write_text(en, encoding="utf-8")
            return m.main(["--root", str(root)])

    def test_duplicate_leaf_under_two_sections_is_refused(self):
        # Both files hide the SAME leaf id ('x') under two different
        # sections. Key sets are identical, so the pre-fix loader judged
        # the pair matching; the defensive rule must instead refuse the
        # file -- the translator of one language cannot tell which 'x'
        # the other language's id means.
        zh = '[a]\nx = "one"\n\n[b]\nx = "two"\n'
        en = '[a]\nx = "one"\n\n[b]\nx = "two"\n'
        self.assertEqual(self._run_on(zh, en), 1)

    def test_duplicate_leaf_refusal_names_the_file(self):
        zh = '[a]\nx = "\u4e00"\n\n[b]\nx = "\u4e8c"\n'
        en = '[a]\nx = "one"\n\n[b]\nx = "two"\n'
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            locales = root / "mod" / "locales"
            locales.mkdir(parents=True)
            (locales / "zh-CN.toml").write_text(zh, encoding="utf-8")
            (locales / "en-US.toml").write_text(en, encoding="utf-8")
            import io
            import contextlib
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                code = m.main(["--root", str(root)])
            self.assertEqual(code, 1)
            self.assertIn("defined under 2 different paths", buf.getvalue())
            self.assertIn("'a.x'", buf.getvalue())
            self.assertIn("'b.x'", buf.getvalue())

    def test_unique_leaves_across_sections_still_match(self):
        # Grouping with every leaf under exactly one path is the honest
        # grouped shape the id semantics tolerate: it keeps matching.
        zh = '[a]\nx = "\u4e00"\n\n[b]\ny = "\u4e8c"\n'
        en = '[a]\nx = "one"\n\n[b]\ny = "two"\n'
        self.assertEqual(self._run_on(zh, en), 0)

    def test_duplicate_leaf_across_a_nested_group_and_a_flat_key_is_refused(self):
        # One 'x' as a flat message id and another 'x' nested under a
        # grouping section are two different paths to the same leaf.
        zh = '"x" = "flat"\n\n[group]\nx = "nested"\n'
        en = '"x" = "flat"\n\n[group]\nx = "nested"\n'
        self.assertEqual(self._run_on(zh, en), 1)

    def test_matching_sections_on_one_side_only_still_fails_as_key_drift(self):
        # The genuine drift case the guard must not swallow: only one
        # language nests an extra leaf -- still reported (as a key-set
        # mismatch, not a duplicate-path refusal).
        zh = '[a]\nx = "\u4e00"\n\n[b]\ny = "\u4e8c"\n'
        en = '[a]\nx = "one"\n'
        self.assertEqual(self._run_on(zh, en), 1)


if __name__ == "__main__":
    unittest.main()
