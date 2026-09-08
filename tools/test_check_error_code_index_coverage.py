#!/usr/bin/env python3
"""Unit tests for check_error_code_index_coverage.py.

Stdlib-only (unittest + tempfile), matching this directory's own
"plain executables with no third-party dependencies" convention (tools/
README.md's "Running in CI and locally" section). Run directly:

    python3 tools/test_check_error_code_index_coverage.py
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_error_code_index_coverage as m  # noqa: E402


def _build_tree(files: dict[str, str], index_rows: list[str]) -> tuple[pathlib.Path, pathlib.Path, pathlib.Path]:
    """Builds a tempdir holding the given Go sources under a "go/" root and
    an index markdown with the given code rows; returns (repo_root,
    go_root, index_path)."""
    td = pathlib.Path(tempfile.mkdtemp())
    root = td / "go"
    for rel, src in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(src, encoding="utf-8")
    index_path = td / "docs" / "error-codes.md"
    index_path.parent.mkdir(parents=True, exist_ok=True)
    index_path.write_text(
        "# Error code index\n\n"
        "| Code | Status | Message | Triggering condition | Source |\n"
        "|---|---|---|---|---|\n"
        + "".join(f"| `{c}` | 400 | m | d | `s` |\n" for c in index_rows)
        + "\n",
        encoding="utf-8",
    )
    return td, root, index_path


class CheckFixturesTests(unittest.TestCase):
    def _check(self, files: dict[str, str], index_rows: list[str]) -> tuple[dict[str, list[str]], list[str]]:
        repo_root, go_root, index_path = _build_tree(files, index_rows)
        try:
            return m.collect_findings([go_root], repo_root, index_path)
        finally:
            import shutil

            shutil.rmtree(repo_root)

    def test_inline_codes_missing_from_the_index_are_reported(self):
        # Core fixture: inline constructions -- a panic, a chained return,
        # a struct literal -- exist in the tree while the index has no
        # rows for them; an index lacking those rows must fail as a
        # finding set.
        files = {
            "jobs/queue.go": (
                "package jobs\n\n"
                "// WithWorkers sets the worker count.\n"
                "func WithWorkers(n int) {\n"
                "\tif n < 1 {\n"
                "\t\tpanic(apperr.Invalid(\"jobs.worker_count_zero\"))\n"
                "\t}\n"
                "}\n"
            ),
            "dbkit/open.go": (
                "package dbkit\n\n"
                "func Open() error {\n"
                "\treturn nil, apperr.Internal(\"dbkit.plugin_failed\").\n"
                "\t\tWithCause(nil)\n"
                "}\n"
            ),
            "sharing/errors.go": (
                "package sharing\n\n"
                "func handle() *apperr.Error {\n"
                "\treturn &apperr.Error{Code: \"sharing.unavailable\", Status: http.StatusBadGateway}\n"
                "}\n"
            ),
        }
        missing, unindexable = self._check(files, ["jobs.worker_count_zero"])
        self.assertEqual(unindexable, [])
        self.assertEqual(set(missing), {"dbkit.plugin_failed", "sharing.unavailable"})
        self.assertEqual(missing["dbkit.plugin_failed"], ["go/dbkit/open.go:4"])
        self.assertEqual(missing["sharing.unavailable"], ["go/sharing/errors.go:4"])

    def test_complete_index_passes(self):
        files = {
            "jobs/queue.go": (
                "package jobs\n\n"
                "func WithWorkers(n int) {\n"
                "\tif n < 1 {\n"
                "\t\tpanic(apperr.Invalid(\"jobs.worker_count_zero\"))\n"
                "\t}\n"
                "}\n"
            )
        }
        missing, unindexable = self._check(files, ["jobs.worker_count_zero"])
        self.assertEqual(missing, {})
        self.assertEqual(unindexable, [])

    def test_comment_text_never_fabricates_a_code(self):
        # A full-line doc comment quoting a construction, a trailing
        # comment carrying one, and a /* */ block quoting one must not
        # count; the real code on a line with a trailing comment still
        # does.
        files = {
            "a/errors.go": (
                "package a\n\n"
                "// Prefer the declared sentinel over apperr.Invalid(\"a.commented\").\n"
                "func handle() error {\n"
                "\t/* apperr.Invalid(\"a.blocked\") is not a construction */\n"
                "\treturn apperr.Invalid(\"a.real\") // apperr.Invalid(\"a.trailing\")\n"
                "}\n"
            )
        }
        missing, unindexable = self._check(files, ["a.real"])
        self.assertEqual(missing, {})
        self.assertEqual(unindexable, [])

    def test_string_literal_content_never_fabricates_a_code(self):
        # A "//"-looking URL inside a string literal is not a comment, and
        # string content that merely resembles a construction is not one
        # either -- but this fixture's real construction still counts.
        files = {
            "a/handler.go": (
                "package a\n\n"
                "func handle() error {\n"
                "\tu := \"https://example.test/x\"\n"
                "\treturn apperr.Invalid(\"a.real\")\n"
                "}\n"
            )
        }
        missing, unindexable = self._check(files, ["a.real"])
        self.assertEqual(missing, {})
        self.assertEqual(unindexable, [])

    def test_non_apperr_constructor_calls_never_count(self):
        # errors.New("..."), net.ParseCIDR("...") and friends are not
        # apperr constructions: they count only if their callee is a
        # discovered apperr-constructing helper, and errors/net are not.
        files = {
            "a/module.go": (
                "package a\n\n"
                "import \"errors\"\n\n"
                "func NewModule() error {\n"
                "\treturn errors.New(\"a: module requires a database handle\")\n"
                "}\n"
            ),
            "a/net.go": (
                "package a\n\n"
                "var blocked = mustParseCIDRs(\"100.64.0.0/10\")\n\n"
                "func mustParseCIDRs(cidrs ...string) []string {\n"
                "\treturn append([]string{}, cidrs...)\n"
                "}\n"
            ),
        }
        missing, unindexable = self._check(files, [])
        self.assertEqual(missing, {})
        self.assertEqual(unindexable, [])

    def test_helper_wrapped_codes_are_reported_and_sanctioned_body_is_not(self):
        # A declaration routed through an apperr-constructing helper
        # carries a code (rateLimited convention). The helper's own
        # parameterized apperr.Invalid(code) is the sanctioned exception
        # to the non-literal rule: it is not reported as unindexable,
        # because the helper's literal call sites carry the codes.
        files = {
            "a/errors.go": (
                "package a\n\n"
                "var (\n"
                "\tErrInvitationRateLimited = rateLimited(\"a.invitation_rate_limited\")\n"
                ")\n\n"
                "// rateLimited returns an *apperr.Error carrying HTTP 429.\n"
                "func rateLimited(code string) *apperr.Error {\n"
                "\terr := apperr.Invalid(code)\n"
                "\terr.Status = http.StatusTooManyRequests\n"
                "\treturn err\n"
                "}\n"
            )
        }
        missing, unindexable = self._check(files, [])
        self.assertEqual(missing, {"a.invitation_rate_limited": ["go/a/errors.go:4"]})
        self.assertEqual(unindexable, [])

    def test_package_level_non_literal_builder_argument_is_unindexable(self):
        # A package-level declaration building a code from a variable or
        # concatenation (apperr.Invalid(prefix + suffix)) constructs a code
        # no index can carry: refused. (Inside a function such a call sits
        # in an apperr-constructing helper's own body -- the sanctioned
        # parameterized form -- so package scope is where this class
        # fires.)
        files = {
            "a/errors.go": (
                "package a\n\n"
                "var ErrX = apperr.Invalid(prefix + \"suffix\")\n"
            )
        }
        missing, unindexable = self._check(files, [])
        self.assertEqual(missing, {})
        self.assertEqual(len(unindexable), 1)
        self.assertIn("go/a/errors.go:3", unindexable[0][1])
        self.assertIn("apperr.Invalid(prefix", unindexable[0][0])

    def test_helper_called_with_non_literal_argument_is_unindexable(self):
        # A call to an apperr-constructing helper with a non-literal code
        # argument (rateLimited(someVar)) builds a code whose value the
        # index cannot know: refused, while the helper's own parameterized
        # body stays sanctioned (the helper test above pins that half).
        files = {
            "a/errors.go": (
                "package a\n\n"
                "// rateLimited returns an *apperr.Error carrying HTTP 429.\n"
                "func rateLimited(code string) *apperr.Error {\n"
                "\terr := apperr.Invalid(code)\n"
                "\treturn err\n"
                "}\n\n"
                "func handle(codeVar string) *apperr.Error {\n"
                "\treturn rateLimited(codeVar)\n"
                "}\n"
            )
        }
        missing, unindexable = self._check(files, [])
        self.assertEqual(missing, {})
        self.assertEqual(len(unindexable), 1)
        self.assertIn("go/a/errors.go:10", unindexable[0][1])
        self.assertIn("rateLimited(codeVar)", unindexable[0][0])

    def test_multiline_literal_argument_is_seen(self):
        # A code literal on its own continuation line below the builder
        # call is within this checker's bounded multi-line net (the
        # generator indexes only same-line literals -- this is the
        # blind-spot form the checker must catch red if it ever lands).
        files = {
            "a/queue.go": (
                "package a\n\n"
                "func With() {\n"
                "\tpanic(apperr.Invalid(\n"
                "\t\t\"a.worker_count_zero\"))\n"
                "}\n"
            )
        }
        missing, unindexable = self._check(files, [])
        self.assertEqual(missing, {"a.worker_count_zero": ["go/a/queue.go:4"]})
        self.assertEqual(unindexable, [])

    def test_test_files_are_out_of_scope(self):
        files = {
            "a/errors.go": "package a\n\nvar ErrA = apperr.NotFound(\"a.not_found\")\n",
            "a/errors_test.go": (
                "package a\n\n"
                "func TestX(t *testing.T) {\n"
                "\tpanic(apperr.Invalid(\"a.test_only_code\"))\n"
                "}\n"
            ),
        }
        missing, unindexable = self._check(files, ["a.not_found"])
        self.assertEqual(missing, {})
        self.assertEqual(unindexable, [])

    def test_generated_files_are_out_of_scope(self):
        files = {
            "a/errors.go": "package a\n\nvar ErrA = apperr.NotFound(\"a.not_found\")\n",
            "a/server.gen.go": 'package a\n\nvar _ = apperr.Invalid("a.generated_code")\n',
        }
        missing, unindexable = self._check(files, ["a.not_found"])
        self.assertEqual(missing, {})
        self.assertEqual(unindexable, [])


class IndexParsingTests(unittest.TestCase):
    def test_index_codes_parse_rows(self):
        with tempfile.TemporaryDirectory() as td:
            p = pathlib.Path(td) / "error-codes.md"
            p.write_text(
                "# t\n\n"
                "| Code | Status | Message | Triggering condition | Source |\n"
                "|---|---|---|---|---|\n"
                "| `foo.a` | 400 | m | d | `a.go:1` |\n"
                "| `bar.b` | 404 | m | d | `b.go:1` |\n",
                encoding="utf-8",
            )
            self.assertEqual(m.index_codes(p), {"foo.a", "bar.b"})


class CommentMaskTests(unittest.TestCase):
    def _mask(self, src: str) -> str:
        return m.mask_comments(src)

    def test_full_line_trailing_and_block_comments_are_masked(self):
        out = self._mask(
            '// apperr.Invalid("a.line")\n'
            'x := 1 // apperr.Invalid("a.trailing")\n'
            '/* apperr.Invalid("a.block")\n'
            '   apperr.Invalid("a.block2") */\n'
            'y := 2\n'
        )
        self.assertNotIn('"a.line"', out)
        self.assertNotIn('"a.trailing"', out)
        self.assertNotIn('"a.block', out)
        self.assertNotIn('"a.block2"', out)
        self.assertIn("x := 1", out)
        self.assertIn("y := 2", out)

    def test_strings_are_not_comment_contexts(self):
        out = self._mask('u := "https://example.test/a" // c\nv := 1\n')
        # The URL's "//" must survive masking (it is string content) while
        # the trailing comment is blanked.
        self.assertIn("https://example.test/a", out)
        self.assertNotIn("// c", out)

    def test_length_is_preserved_so_line_numbers_hold(self):
        src = "// c1\n\nx := 1\n"
        out = self._mask(src)
        self.assertEqual(len(out), len(src))
        self.assertEqual(out.splitlines()[-1], "x := 1")


if __name__ == "__main__":
    unittest.main()
