#!/usr/bin/env python3
"""Unit tests for gen_error_code_index.py.

Stdlib-only (unittest + tempfile), matching this directory's own
"plain executables with no third-party dependencies" convention (tools/
README.md's "Running in CI and locally" section) -- no pytest, no fixtures
directory, no network. Run directly:

    python3 tools/test_gen_error_code_index.py
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import gen_error_code_index as m  # noqa: E402


class ScanGoFileTests(unittest.TestCase):
    def _scan(self, go_source: str, helpers: set[str] | None = None) -> list[m.ErrorEntry]:
        with tempfile.TemporaryDirectory() as td:
            path = pathlib.Path(td) / "errors.go"
            path.write_text(go_source, encoding="utf-8")
            return m._scan_go_file(path, "errors.go", helpers or set())

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
        self.assertEqual(e.kind, "declared")

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
        self.assertEqual(entries[0].kind, "declared")

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

    def test_function_body_assignment_is_indexed_as_an_inline_construction(self):
        # A reassignment inside a function body constructs a real code even
        # though it binds no package-level variable -- "a code is indexed
        # iff it is constructed in Go source with a literal code argument"
        # -- so it yields an INLINE entry (ident ""), never a phantom
        # DECLARED entry: the code is real, the declaration is not. This is
        # the regression test for the round that closed the inline blind
        # spot: the pre-fix scanner dropped every such code silently.
        entries = self._scan(
            'package foo\n\n'
            'func handle() error {\n'
            '\tif broken {\n'
            '\t\terr = apperr.Invalid("foo.inline_assignment")\n'
            '\t\treturn err\n'
            '\t}\n'
            '\treturn nil\n'
            '}\n'
        )
        self.assertEqual(len(entries), 1)
        e = entries[0]
        self.assertEqual(e.code, "foo.inline_assignment")
        self.assertEqual(e.status, 400)
        self.assertEqual(e.ident, "")
        self.assertEqual(e.kind, "inline")
        self.assertEqual(e.source, "errors.go:5")

    def test_multiple_var_blocks_all_contribute(self):
        # Two var (...)-blocks in one file (the gofmt shape when a module
        # groups its sentinels twice, e.g. before and after a long doc
        # comment) each contribute their entries; the closer of the first
        # block must not suppress the second.
        entries = self._scan(
            'package foo\n\n'
            'var (\n'
            '\tErrA = apperr.NotFound("foo.a")\n'
            ')\n'
            '\n'
            'var (\n'
            '\tErrB = apperr.Invalid("foo.b")\n'
            ')\n'
        )
        self.assertEqual([e.code for e in entries], ["foo.a", "foo.b"])
        self.assertEqual([e.status for e in entries], [404, 400])

    def test_inline_panic_construction_is_indexed_with_enclosing_doc(self):
        # The jobs pattern this round exists for: a With* option function
        # refuses an unhonourable value with panic(apperr.Invalid("code")),
        # and the code's triggering condition is the function's own doc
        # comment. Fails before the fix (the panic line produced no entry).
        entries = self._scan(
            'package foo\n\n'
            '// WithWorkers sets the worker count. A value below 1 is\n'
            '// refused at option time with a coded panic: a queue with no\n'
            '// workers would silently process nothing.\n'
            'func WithWorkers(n int) {\n'
            '\tif n < 1 {\n'
            '\t\tpanic(apperr.Invalid("foo.worker_count_zero"))\n'
            '\t}\n'
            '}\n'
        )
        self.assertEqual(len(entries), 1)
        e = entries[0]
        self.assertEqual(e.code, "foo.worker_count_zero")
        self.assertEqual(e.status, 400)
        self.assertEqual(e.kind, "inline")
        self.assertEqual(e.ident, "")
        self.assertIn("WithWorkers sets the worker count.", e.doc)
        self.assertIn("silently process nothing.", e.doc)

    def test_inline_return_with_chained_continuation_is_indexed(self):
        # The dbkit pattern: "return nil, apperr.Internal("code")." with the
        # .WithParam/.WithCause chain on the following lines -- the builder
        # call and its literal code still share one line.
        entries = self._scan(
            'package foo\n\n'
            '// Open opens a connection already wired with safeguards.\n'
            'func Open() error {\n'
            '\tif err := use(); err != nil {\n'
            '\t\treturn nil, apperr.Internal("foo.plugin_failed").\n'
            '\t\t\tWithParam("dialect", "sqlite").\n'
            '\t\t\tWithCause(err)\n'
            '\t}\n'
            '\treturn nil\n'
            '}\n'
        )
        self.assertEqual(len(entries), 1)
        e = entries[0]
        self.assertEqual(e.code, "foo.plugin_failed")
        self.assertEqual(e.status, 500)
        self.assertEqual(e.kind, "inline")
        self.assertEqual(e.doc, "Open opens a connection already wired with safeguards.")

    def test_inline_adjacent_comment_beats_enclosing_doc(self):
        # A comment directly above the construction is the tightest
        # triggering-condition evidence; the enclosing function's doc is
        # only the fallback.
        entries = self._scan(
            'package foo\n\n'
            '// Open opens a connection.\n'
            'func Open() error {\n'
            '\t// A plugin install failure leaves the handle unusable.\n'
            '\treturn apperr.Internal("foo.plugin_failed")\n'
            '}\n'
        )
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0].doc, "A plugin install failure leaves the handle unusable.")

    def test_inline_construction_without_any_doc_keeps_empty_doc(self):
        # An undocumented inline code still gets indexed -- an empty
        # triggering-condition column is never a reason to drop the code
        # (that would preserve the blind spot in another form).
        entries = self._scan(
            'package foo\n\n'
            'func handle() error {\n'
            '\treturn apperr.Invalid("foo.bare_inline")\n'
            '}\n'
        )
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0].code, "foo.bare_inline")
        self.assertEqual(entries[0].kind, "inline")
        self.assertEqual(entries[0].doc, "")

    def test_comment_text_never_indexes(self):
        # A full-line comment quoting a construction, and a trailing
        # comment carrying one, must not invent codes; the code part of a
        # line with a trailing comment is still indexed.
        entries = self._scan(
            'package foo\n\n'
            '// Declared here rather than as apperr.Internal("foo.commented")\n'
            'func handle() error {\n'
            '\treturn apperr.Invalid("foo.real") // apperr.Invalid("foo.trailing")\n'
            '}\n'
        )
        self.assertEqual([e.code for e in entries], ["foo.real"])

    def test_inline_struct_literal_is_indexed(self):
        entries = self._scan(
            'package foo\n\n'
            'func handle() *apperr.Error {\n'
            '\treturn &apperr.Error{Code: "foo.structy", Status: http.StatusTooManyRequests}\n'
            '}\n'
        )
        self.assertEqual(len(entries), 1)
        e = entries[0]
        self.assertEqual(e.code, "foo.structy")
        self.assertEqual(e.status, 429)
        self.assertEqual(e.kind, "inline")

    def test_declaration_line_is_not_double_indexed(self):
        # The inline scan must not re-index a line the declaration scan
        # already claimed: one construction, one entry.
        entries = self._scan(
            'package foo\n\nvar ErrA = apperr.Invalid("foo.a")\n'
        )
        self.assertEqual(len(entries), 1)

    def test_helper_wrapped_declaration_is_indexed_only_for_discovered_helpers(self):
        source = (
            'package foo\n\n'
            'var (\n'
            '\t// ErrRateLimited reports a 429 refusal.\n'
            '\tErrRateLimited = rateLimited("foo.rate_limited")\n'
            ')\n'
        )
        # The callee is not a discovered apperr-constructing helper: no
        # entry -- this is regexp.MustCompile/net.ParseCIDR territory and a
        # phantom row must never come from it.
        self.assertEqual(self._scan(source), [])
        entries = self._scan(source, helpers={"rateLimited"})
        self.assertEqual(len(entries), 1)
        e = entries[0]
        self.assertEqual(e.ident, "ErrRateLimited")
        self.assertEqual(e.code, "foo.rate_limited")
        self.assertEqual(e.status, 429)
        self.assertEqual(e.doc, "ErrRateLimited reports a 429 refusal.")
        self.assertEqual(e.kind, "declared")

    def test_inline_helper_call_is_indexed_for_discovered_helpers(self):
        entries = self._scan(
            'package foo\n\n'
            'func handle() *apperr.Error {\n'
            '\treturn rateLimited("foo.rate_inline")\n'
            '}\n',
            helpers={"rateLimited"},
        )
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0].code, "foo.rate_inline")
        self.assertEqual(entries[0].status, 429)
        self.assertEqual(entries[0].kind, "inline")


class DiscoverHelpersTests(unittest.TestCase):
    def _discover(self, *go_sources: str) -> set[str]:
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            for i, src in enumerate(go_sources):
                pkg = root / f"pkg{i}"
                pkg.mkdir()
                (pkg / "errors.go").write_text(src, encoding="utf-8")
            return m.discover_apperr_helpers([root], root)

    def test_body_construction_marks_the_function_a_helper(self):
        helpers = self._discover(
            'package foo\n\n'
            '// rateLimited returns an *apperr.Error carrying HTTP 429.\n'
            'func rateLimited(code string) *apperr.Error {\n'
            '\terr := apperr.Invalid(code)\n'
            '\terr.Status = http.StatusTooManyRequests\n'
            '\treturn err\n'
            '}\n'
        )
        self.assertEqual(helpers, {"rateLimited"})

    def test_comment_mention_alone_is_not_body_evidence(self):
        helpers = self._discover(
            'package foo\n\n'
            '// rateLimited would return an apperr.Error, but this one\n'
            '// parses CIDRs instead -- apperr.Invalid("never.built") in a\n'
            '// doc comment is not a construction.\n'
            'func mustParseCIDRs(cidrs ...string) []string {\n'
            '\treturn cidrs\n'
            '}\n'
        )
        self.assertEqual(helpers, set())

    def test_no_apperr_in_body_is_not_a_helper(self):
        helpers = self._discover(
            'package foo\n\n'
            'func mustParseCIDRs(cidrs ...string) []string {\n'
            '\treturn append([]string{}, cidrs...)\n'
            '}\n'
        )
        self.assertEqual(helpers, set())


class CollectEntriesTests(unittest.TestCase):
    def _collect(self, go_sources: dict[str, str]) -> list[m.ErrorEntry]:
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            for rel, src in go_sources.items():
                path = root / rel
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(src, encoding="utf-8")
            return m.collect_entries([root], root, set())

    def test_declared_and_inline_entries_across_files(self):
        entries = self._collect(
            {
                "a/errors.go": 'package a\n\nvar ErrA = apperr.NotFound("a.not_found")\n',
                "b/queue.go": 'package b\n\nfunc With() {\n\tpanic(apperr.Invalid("b.panic"))\n}\n',
            }
        )
        self.assertEqual([e.code for e in entries], ["a.not_found", "b.panic"])
        self.assertEqual([e.kind for e in entries], ["declared", "inline"])


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

    def test_declared_row_wins_over_inline_row_for_the_same_code(self):
        # The same code constructed both as a named declaration and inline
        # (authn.internal_error is the real-tree shape) renders one row
        # carrying the declaration's own doc comment.
        entries = [
            m.ErrorEntry(ident="ErrInternal", code="foo.internal", status=500, source="z/errors.go:5", doc="declaration doc", kind="declared"),
            m.ErrorEntry(ident="", code="foo.internal", status=500, source="a/handler.go:9", doc="handler doc", kind="inline"),
        ]
        rendered = m.render_markdown(entries)
        self.assertEqual(rendered.count("`foo.internal`"), 1)
        self.assertIn("declaration doc", rendered)
        self.assertNotIn("handler doc", rendered)
        self.assertIn("`z/errors.go:5`", rendered)

    def test_inline_row_without_doc_gets_the_inline_marker_not_the_undocumented_one(self):
        entries = [
            m.ErrorEntry(ident="ErrX", code="foo.x", status=400, source="a.go:1", doc=""),
            m.ErrorEntry(ident="", code="foo.y", status=400, source="b.go:1", doc="", kind="inline"),
        ]
        rendered = m.render_markdown(entries)
        self.assertIn("_(undocumented)_", rendered)
        self.assertIn("_(inline construction, no doc comment nearby)_", rendered)

    def test_missing_message_gets_the_class_neutral_marker(self):
        # The entryless cell marker must not assert a class: it lands on
        # boot-time wiring refusals AND request-time refusals whose rows
        # the header's two-class definition covers (an entryless
        # request-time code reaches the client as its structured code, so
        # "not user-facing" -- the old single-class gloss -- would be
        # false on it). Both kinds of row render the same neutral fact.
        for kind in ("declared", "inline"):
            entries = [m.ErrorEntry(ident="", code="foo.a", status=400, source="a.go:1", doc="d", kind=kind)]
            rendered = m.render_markdown(entries)
            self.assertIn("_(no locale message -- the catalog carries no copy)_", rendered)
            self.assertNotIn("not user-facing", rendered)

    def test_header_defines_entryless_semantics_by_class(self):
        # The Message-column gloss must name both entryless classes -- a
        # boot-time wiring refusal that never reaches an end user, and a
        # request-time refusal that reaches one only as its structured
        # code (consult's entryless 400s) -- never the old single-class
        # claim ("never rendered to an end user, typically a boot-time
        # wiring refusal") that an indexed request-time row falsifies.
        rendered = m.render_markdown([])
        self.assertIn("entryless code is one the catalog carries no copy for", rendered)
        self.assertIn("boot-time wiring refusal never reaches an end user", rendered)
        self.assertIn("request-time refusal with no entry still reaches the client", rendered)
        self.assertNotIn("never rendered to an end user", rendered)

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

    def test_footer_reports_the_real_site_and_dedupe_counts(self):
        # The footer must state the real numbers -- how many construction
        # sites (named declarations and inline apperr calls alike) the
        # table documents, how many unique codes, and how many duplicate
        # sites the one-row-per-code collapse removed.
        entries = [
            m.ErrorEntry(ident="ErrA", code="foo.a", status=400, source="a.go:1", doc="doc a"),
            m.ErrorEntry(ident="ErrA2", code="foo.a", status=400, source="a2.go:1", doc="doc a again"),
            m.ErrorEntry(ident="", code="foo.a", status=400, source="queue.go:1", doc="", kind="inline"),
            m.ErrorEntry(ident="ErrB", code="bar.b", status=404, source="b.go:1", doc="doc b"),
        ]
        rendered = m.render_markdown(entries)
        self.assertIn("4 construction site(s) -- named declarations and inline apperr calls alike -- collapsed to 2 code(s)", rendered)
        self.assertIn("(2 duplicate site(s) removed)", rendered)

    def test_code_counts_helper(self):
        entries = [
            m.ErrorEntry(ident="ErrA", code="foo.a", status=400, source="a.go:1"),
            m.ErrorEntry(ident="ErrA2", code="foo.a", status=400, source="a2.go:1"),
            m.ErrorEntry(ident="ErrB", code="foo.b", status=400, source="b.go:1"),
            m.ErrorEntry(ident="ErrC", code="bar.c", status=404, source="c.go:1"),
        ]
        self.assertEqual(m.code_counts(entries), (4, 3, 1))
        self.assertEqual(m.code_counts([]), (0, 0, 0))


class MachineOutputTests(unittest.TestCase):
    """The docs/error-codes.json twin the reference-app shell's
    codes-alignment suite reads back: one row per code with the SAME
    collapse as the Markdown table, file/line split, ident and kind
    carried per row."""

    def _entries(self):
        return [
            m.ErrorEntry(ident="ErrA", code="foo.a", status=400, source="a.go:1", doc="doc a"),
            m.ErrorEntry(ident="ErrA2", code="foo.a", status=400, source="a2.go:1", doc="doc a again"),
            m.ErrorEntry(ident="", code="foo.a", status=400, source="queue.go:1", doc="", kind="inline"),
            m.ErrorEntry(ident="ErrB", code="bar.b", status=404, source="b.go:1", doc="doc b"),
        ]

    def test_machine_json_mirrors_markdown_collapse(self):
        # The declared ErrA row wins over its inline twin, exactly as
        # the table renders it; both artifacts derive from winning_rows.
        entries = self._entries()
        rows = json.loads(m.render_machine_json(entries))["rows"]
        self.assertEqual([r["code"] for r in rows], ["bar.b", "foo.a"])
        foo = [r for r in rows if r["code"] == "foo.a"][0]
        self.assertEqual(foo["file"], "a.go")
        self.assertEqual(foo["line"], 1)
        self.assertEqual(foo["ident"], "ErrA")
        self.assertEqual(foo["kind"], "declared")

    def test_machine_json_is_valid_json_with_expected_keys(self):
        entries = self._entries()
        data = json.loads(m.render_machine_json(entries))
        self.assertEqual(data["generated_by"], "tools/gen_error_code_index.py")
        row = data["rows"][0]
        for key in ("code", "module", "status", "file", "line", "ident", "kind"):
            self.assertIn(key, row)

    def test_winning_rows_prefers_declared_over_inline_earliest_source(self):
        entries = self._entries()
        winners = m.winning_rows(entries)
        self.assertEqual([e.code for e in winners], ["foo.a", "bar.b"])

    def test_inline_source_splits_into_file_and_line(self):
        entries = [
            m.ErrorEntry(ident="", code="foo.inline", status=500, source="go/x/queue.go:77", kind="inline"),
        ]
        rows = json.loads(m.render_machine_json(entries))["rows"]
        self.assertEqual(rows[0]["file"], "go/x/queue.go")
        self.assertEqual(rows[0]["line"], 77)
        self.assertEqual(rows[0]["ident"], "")


if __name__ == "__main__":
    unittest.main()
