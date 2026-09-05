#!/usr/bin/env python3
"""Unit tests for gen_error_code_index.py.

Stdlib-only (unittest + tempfile), matching this directory's own
"plain executables with no third-party dependencies" convention (tools/
README.md's "Running in CI and locally" section) -- no pytest, no fixtures
directory, no network. Run directly:

    python3 tools/test_gen_error_code_index.py
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import gen_error_code_index as m  # noqa: E402


class ScanGoFileTests(unittest.TestCase):
    def _scan(self, go_source: str) -> list[m.ErrorEntry]:
        with tempfile.TemporaryDirectory() as td:
            path = pathlib.Path(td) / "errors.go"
            path.write_text(go_source, encoding="utf-8")
            return m._scan_go_file(path, "errors.go")

    def test_var_block_declaration(self):
        entries = self._scan(
            'package foo\n\n'
            'var (\n'
            '\t// ErrNotFound reports a missing widget.\n'
            '\tErrNotFound = apperr.NotFound("foo.not_found")\n'
            ')\n'
        )
        self.assertEqual(len(entries), 1)
        e = entries[0]
        self.assertEqual(e.ident, "ErrNotFound")
        self.assertEqual(e.code, "foo.not_found")
        self.assertEqual(e.status, 404)
        self.assertEqual(e.doc, "ErrNotFound reports a missing widget.")
        self.assertEqual(e.source, "errors.go:5")

    def test_standalone_var_declaration_with_var_keyword(self):
        entries = self._scan(
            'package foo\n\n'
            '// ErrTextRequired reports an empty text field.\n'
            'var ErrTextRequired = apperr.Invalid("notes.text_required")\n'
        )
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0].ident, "ErrTextRequired")
        self.assertEqual(entries[0].code, "notes.text_required")
        self.assertEqual(entries[0].status, 400)

    def test_unexported_identifier_is_captured(self):
        entries = self._scan(
            'package foo\n\n'
            'var errInternal = apperr.Internal("foo.internal_error")\n'
        )
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0].ident, "errInternal")
        self.assertEqual(entries[0].status, 500)

    def test_struct_literal_429_shape(self):
        entries = self._scan(
            'package foo\n\n'
            '// ErrRateLimited reports a 429.\n'
            'var ErrRateLimited = &apperr.Error{Code: "foo.rate_limited", Status: http.StatusTooManyRequests}\n'
        )
        self.assertEqual(len(entries), 1)
        e = entries[0]
        self.assertEqual(e.ident, "ErrRateLimited")
        self.assertEqual(e.code, "foo.rate_limited")
        self.assertEqual(e.status, 429)
        self.assertEqual(e.doc, "ErrRateLimited reports a 429.")

    def test_chained_with_param_does_not_break_the_match(self):
        entries = self._scan(
            'package foo\n\n'
            'var ErrScoped = apperr.Invalid("foo.scoped").\n'
            '\tWithParam("field", "x")\n'
        )
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0].code, "foo.scoped")

    def test_no_preceding_comment_yields_empty_doc(self):
        entries = self._scan('package foo\n\nvar ErrX = apperr.Invalid("foo.x")\n')
        self.assertEqual(entries[0].doc, "")

    def test_module_property_is_the_code_prefix(self):
        entries = self._scan('package foo\n\nvar ErrX = apperr.Invalid("aigateway.x")\n')
        self.assertEqual(entries[0].module, "aigateway")

    def test_module_property_falls_back_to_whole_code_with_no_dot(self):
        entries = self._scan('package foo\n\nvar ErrX = apperr.Invalid("bare")\n')
        self.assertEqual(entries[0].module, "bare")

    def test_test_files_and_generated_files_are_excluded_by_caller(self):
        # _is_excluded is what collect_entries consults; scanning happens
        # only for files that pass it, so this test pins the predicate
        # directly rather than round-tripping through collect_entries.
        self.assertTrue(m._is_excluded(pathlib.Path("errors_test.go")))
        self.assertTrue(m._is_excluded(pathlib.Path("notes-server.gen.go")))
        self.assertTrue(m._is_excluded(pathlib.Path("foo_gen.go")))
        self.assertFalse(m._is_excluded(pathlib.Path("errors.go")))


class CollectMessagesTests(unittest.TestCase):
    def test_flat_string_message_is_looked_up_by_code(self):
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            locales = root / "mymodule" / "locales"
            locales.mkdir(parents=True)
            (locales / "en-US.toml").write_text(
                '"mymodule.not_found" = "The widget was not found."\n',
                encoding="utf-8",
            )
            messages = m.collect_messages([root])
        self.assertEqual(messages["mymodule.not_found"], "The widget was not found.")

    def test_plural_table_falls_back_to_other_form(self):
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            locales = root / "mymodule" / "locales"
            locales.mkdir(parents=True)
            (locales / "en-US.toml").write_text(
                '["mymodule.count"]\n'
                'one = "1 item"\n'
                'other = "{{.count}} items"\n',
                encoding="utf-8",
            )
            messages = m.collect_messages([root])
        self.assertEqual(messages["mymodule.count"], "{{.count}} items")

    def test_missing_locale_file_is_silently_absent(self):
        with tempfile.TemporaryDirectory() as td:
            messages = m.collect_messages([pathlib.Path(td)])
        self.assertEqual(messages, {})


class RenderMarkdownTests(unittest.TestCase):
    def test_groups_by_module_and_deduplicates_repeated_codes(self):
        entries = [
            m.ErrorEntry(ident="ErrA", code="foo.a", status=400, source="a.go:1", doc="doc a"),
            m.ErrorEntry(ident="ErrB", code="bar.b", status=404, source="b.go:1", doc="doc b"),
            # A second declaration of "foo.a" (a rare re-export) must not
            # produce a second row.
            m.ErrorEntry(ident="ErrA2", code="foo.a", status=400, source="a2.go:1", doc="doc a again"),
        ]
        rendered = m.render_markdown(entries)
        self.assertIn("## bar", rendered)
        self.assertIn("## foo", rendered)
        self.assertEqual(rendered.count("`foo.a`"), 1)
        self.assertIn("doc a", rendered)

    def test_missing_message_gets_the_not_user_facing_marker(self):
        entries = [m.ErrorEntry(ident="ErrA", code="foo.a", status=500, source="a.go:1", doc="d")]
        rendered = m.render_markdown(entries)
        self.assertIn("not user-facing", rendered)

    def test_pipe_characters_in_message_or_doc_are_escaped(self):
        entries = [
            m.ErrorEntry(
                ident="ErrA", code="foo.a", status=400, source="a.go:1",
                doc="a | b", message="c | d",
            )
        ]
        rendered = m.render_markdown(entries)
        self.assertIn("a \\| b", rendered)
        self.assertIn("c \\| d", rendered)


if __name__ == "__main__":
    unittest.main()
