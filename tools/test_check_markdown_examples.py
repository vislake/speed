#!/usr/bin/env python3
"""Unit tests for check_markdown_examples.py.

Stdlib-only (unittest + tempfile), matching this directory's own "plain
executables with no third-party dependencies" convention (tools/README.md's
"Running in CI and locally" section) -- no pytest, no fixtures directory,
no network. Run directly:

    python3 tools/test_check_markdown_examples.py

This suite needs `go` and `gofmt` on PATH (the same toolchain the script
itself requires) but never touches the network and never depends on this
repository's own go/* modules existing on disk: every complete-block test
below is a self-contained snippet with no github.com/vislake/speed/...
imports, so `go mod tidy`/`go build` resolve against nothing but the
standard library.

Regression coverage: two real documentation bugs this script's own first
run against the live repository found and fixed (go/dbkit/AGENTS.md and
go/admin/AGENTS.md each used a bare `...` as an "elided code" placeholder
in a position where Go's grammar requires a real token) are reproduced
directly as test_ellipsis_placeholder_in_statement_position_is_a_violation
and test_ellipsis_placeholder_in_call_position_is_a_violation: both fail
under every wrapping strategy before the fix and would be caught by any
future regression of the same shape.
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import textwrap
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_markdown_examples as m  # noqa: E402


class InScopeTests(unittest.TestCase):
    def test_agents_md_anywhere_is_in_scope(self):
        self.assertTrue(m._in_scope("go/notification/AGENTS.md"))
        self.assertTrue(m._in_scope("AGENTS.md"))
        self.assertTrue(m._in_scope("web/packages/ui-kit/AGENTS.md"))

    def test_go_module_readme_is_in_scope(self):
        self.assertTrue(m._in_scope("go/dbkit/README.md"))

    def test_web_package_readme_is_in_scope(self):
        self.assertTrue(m._in_scope("web/packages/ui-kit/README.md"))

    def test_root_readme_is_in_scope(self):
        self.assertTrue(m._in_scope("README.md"))

    def test_non_root_non_module_readme_is_out_of_scope(self):
        # A README that is neither the root one, nor under go/, nor under
        # web/packages/ -- e.g. an example app's own README -- carries no
        # compile-harness obligation here.
        self.assertFalse(m._in_scope("examples/reference-app/README.md"))

    def test_adr_is_in_scope(self):
        self.assertTrue(m._in_scope("docs/adr/0001-something.md"))

    def test_docs_internal_is_out_of_scope_even_as_agents_md(self):
        self.assertFalse(m._in_scope("docs/internal/AGENTS.md"))

    def test_docs_internal_prose_is_out_of_scope(self):
        self.assertFalse(m._in_scope("docs/internal/07-platform-services.md"))

    def test_unrelated_markdown_is_out_of_scope(self):
        self.assertFalse(m._in_scope("go/dbkit/CHANGELOG.md"))


class ExtractGoBlocksTests(unittest.TestCase):
    def test_single_block_extracted_with_line_numbers(self):
        text = "intro\n\n```go\npackage p\n```\n\nmore text\n"
        blocks = m.extract_go_blocks(text)
        self.assertEqual(len(blocks), 1)
        self.assertEqual(blocks[0].body, "package p")
        self.assertEqual(blocks[0].start_line, 3)
        self.assertEqual(blocks[0].end_line, 5)

    def test_multiple_blocks_extracted_independently(self):
        text = "```go\npackage a\n```\ntext\n```go\npackage b\n```\n"
        blocks = m.extract_go_blocks(text)
        self.assertEqual(len(blocks), 2)
        self.assertEqual(blocks[0].body, "package a")
        self.assertEqual(blocks[1].body, "package b")

    def test_non_go_fence_is_ignored(self):
        text = "```python\nprint(1)\n```\n"
        self.assertEqual(m.extract_go_blocks(text), [])

    def test_gotemplate_fence_is_not_mistaken_for_go(self):
        # A "```go" prefix match without a strict end-of-line anchor would
        # wrongly swallow a ```gotemplate or ```go-like fence tag.
        text = "```gotemplate\n{{ .Foo }}\n```\n"
        self.assertEqual(m.extract_go_blocks(text), [])

    def test_skip_marker_immediately_above_fence(self):
        text = "<!-- markdown-example: no-parse-check -->\n```go\nnonsense !!!\n```\n"
        blocks = m.extract_go_blocks(text)
        self.assertEqual(len(blocks), 1)
        self.assertTrue(blocks[0].skip)

    def test_skip_marker_tolerates_one_blank_line(self):
        text = "<!-- markdown-example: no-parse-check -->\n\n```go\nnonsense !!!\n```\n"
        blocks = m.extract_go_blocks(text)
        self.assertTrue(blocks[0].skip)

    def test_no_skip_marker_means_not_skipped(self):
        text = "some prose\n```go\npackage p\n```\n"
        blocks = m.extract_go_blocks(text)
        self.assertFalse(blocks[0].skip)

    def test_skip_marker_paragraphs_above_does_not_count(self):
        # The escape hatch must be the *nearest* line, not merely present
        # somewhere earlier in the file -- otherwise one marker could
        # silently blanket every later block.
        text = (
            "<!-- markdown-example: no-parse-check -->\n"
            "\nSome unrelated paragraph in between.\n\n"
            "```go\npackage p\n```\n"
        )
        blocks = m.extract_go_blocks(text)
        self.assertFalse(blocks[0].skip)


class ClassifyTests(unittest.TestCase):
    def test_package_clause_is_complete(self):
        self.assertEqual(m.classify("package foo\n\nfunc F() {}"), "complete")

    def test_leading_comment_then_package_is_still_complete(self):
        self.assertEqual(
            m.classify("// Package foo does things.\npackage foo\n"), "complete"
        )

    def test_bare_statement_is_fragment(self):
        self.assertEqual(m.classify("x := 1\nreturn x"), "fragment")

    def test_bare_signature_is_fragment(self):
        self.assertEqual(m.classify("Migrations() dbkit.MigrationSet"), "fragment")

    def test_empty_block_is_fragment(self):
        self.assertEqual(m.classify(""), "fragment")

    def test_leading_comment_mentioning_package_word_is_not_complete(self):
        # "// package pkgcore" as a comment (docs/adr/0002's own shape) must
        # not be mistaken for a real package clause -- only an actual
        # `package` keyword line counts.
        self.assertEqual(
            m.classify("// package pkgcore\ntype TenantID string\n"), "fragment"
        )


class ModuleDirsForBlockTests(unittest.TestCase):
    def test_direct_module_import(self):
        body = 'import "github.com/vislake/speed/go/pkgcore"\n'
        self.assertEqual(m._module_dirs_for_block(body), {"go/pkgcore"})

    def test_subpackage_import_maps_to_owning_module(self):
        body = 'import "github.com/vislake/speed/go/pkgcore/i18n"\n'
        self.assertEqual(m._module_dirs_for_block(body), {"go/pkgcore"})

    def test_multiple_distinct_modules(self):
        body = (
            'import (\n'
            '\t"github.com/vislake/speed/go/dbkit"\n'
            '\t"github.com/vislake/speed/go/pkgcore"\n'
            ')\n'
        )
        self.assertEqual(m._module_dirs_for_block(body), {"go/dbkit", "go/pkgcore"})

    def test_reference_app_import(self):
        body = 'import "github.com/vislake/speed/examples/reference-app/internal/notes"\n'
        self.assertEqual(m._module_dirs_for_block(body), {"examples/reference-app"})

    def test_no_speed_imports_yields_empty_set(self):
        body = 'import "context"\n'
        self.assertEqual(m._module_dirs_for_block(body), set())


class CheckFragmentTests(unittest.TestCase):
    def setUp(self):
        import shutil

        self.gofmt = shutil.which("gofmt")
        if self.gofmt is None:
            self.skipTest("gofmt not on PATH")

    def test_bare_method_signature_parses_under_toplevel(self):
        # A body-less function/method signature is legal top-level Go
        # syntax (the shape used for assembly-implemented functions), so
        # this needs no interface wrapping at all.
        ok, strategy, _ = m.check_fragment(self.gofmt, "func Migrations() dbkit.MigrationSet")
        self.assertTrue(ok)
        self.assertEqual(strategy, "top-level")

    def test_interface_method_only_signature_needs_interface_wrap(self):
        ok, strategy, _ = m.check_fragment(self.gofmt, "Migrations() dbkit.MigrationSet")
        self.assertTrue(ok)
        self.assertEqual(strategy, "interface-body")

    def test_statement_sequence_parses_under_func_body(self):
        body = textwrap.dedent(
            """\
            x, err := f()
            if err != nil {
                return err
            }
            _ = x
            """
        )
        ok, strategy, _ = m.check_fragment(self.gofmt, body)
        self.assertTrue(ok)
        self.assertEqual(strategy, "func-body")

    def test_bare_import_parses_under_toplevel(self):
        ok, strategy, _ = m.check_fragment(
            self.gofmt, 'import _ "github.com/vislake/speed/go/dbkit/dialect/sqlite"'
        )
        self.assertTrue(ok)
        self.assertEqual(strategy, "top-level")

    def test_ellipsis_placeholder_in_statement_position_is_a_violation(self):
        # Regression test for the real go/dbkit/AGENTS.md bug this script's
        # first run against the live repo found: a bare `...` standing in
        # for "handle it" inside a block is not valid Go anywhere a
        # statement is expected, and must fail every wrapping.
        body = (
            "if err := db.Where(cond).First(&got).Error; err != nil { ... }"
        )
        ok, strategy, err = m.check_fragment(self.gofmt, body)
        self.assertFalse(ok)
        self.assertEqual(strategy, "")
        self.assertTrue(err)

    def test_ellipsis_placeholder_in_call_position_is_a_violation(self):
        # Regression test for the real go/admin/AGENTS.md bug: a bare `...`
        # standing in for "more arguments here" after a comma inside a call
        # is not valid Go (variadic `...` must follow an actual expression,
        # e.g. `slice...`, never a bare comma).
        body = "tenancy.Middleware(authn.NewPrincipalResolver(), ...)(mux)"
        ok, strategy, err = m.check_fragment(self.gofmt, body)
        self.assertFalse(ok)
        self.assertEqual(strategy, "")

    def test_struct_field_list_needs_struct_wrap(self):
        body = 'Email      string `gorm:"serializer:email_enc"`'
        ok, strategy, _ = m.check_fragment(self.gofmt, body)
        self.assertTrue(ok)
        # A single tagged field also happens to parse as a bare top-level
        # var-ish construct in some wrappings; assert only that some
        # wrapping among the struct/const/var family accepts it, since the
        # exact winning strategy is not the property under test here.
        self.assertIn(strategy, {"top-level", "struct-body", "const-block", "var-block"})

    def test_genuinely_unparseable_fragment_fails_everywhere(self):
        ok, strategy, err = m.check_fragment(self.gofmt, "{{{ not go at all ]]]")
        self.assertFalse(ok)
        self.assertEqual(strategy, "")
        self.assertTrue(err)


class CheckCompleteBlockTests(unittest.TestCase):
    def setUp(self):
        import shutil

        if shutil.which("go") is None:
            self.skipTest("go not on PATH")

    def test_self_contained_valid_program_builds_clean(self):
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            block = m.GoBlock(1, 3, "package p\n\nfunc F() int { return 1 }\n", False)
            violations = m.check_complete_block(root, "fake.md", block, keep_temp=False)
        self.assertEqual(violations, [])

    def test_broken_program_is_reported_as_a_violation(self):
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            block = m.GoBlock(
                1, 3, "package p\n\nfunc F() int { return undefinedThing() }\n", False
            )
            violations = m.check_complete_block(root, "fake.md", block, keep_temp=False)
        self.assertEqual(len(violations), 1)
        self.assertIn("fake.md:1", violations[0])
        self.assertIn("undefinedThing", violations[0])

    def test_import_of_nonexistent_module_is_reported_without_attempting_a_build(self):
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            body = (
                'package p\n\n'
                'import "github.com/vislake/speed/go/doesnotexist"\n\n'
                'func F() { doesnotexist.Foo() }\n'
            )
            block = m.GoBlock(1, 5, body, False)
            violations = m.check_complete_block(root, "fake.md", block, keep_temp=False)
        self.assertEqual(len(violations), 1)
        self.assertIn("go/doesnotexist", violations[0])


class MainExitCodeTests(unittest.TestCase):
    def test_missing_root_is_infrastructure_error(self):
        rc = m.main(["--root", "/this/path/does/not/exist/at/all"])
        self.assertEqual(rc, 2)

    def test_survey_mode_on_empty_tree_exits_zero(self):
        with tempfile.TemporaryDirectory() as td:
            rc = m.main(["--root", td, "--survey"])
        self.assertEqual(rc, 0)

    def test_clean_tree_exits_zero(self):
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            (root / "AGENTS.md").write_text(
                "# Fake\n\n```go\npackage p\n\nfunc F() {}\n```\n", encoding="utf-8"
            )
            rc = m.main(["--root", td])
        self.assertEqual(rc, 0)

    def test_tree_with_a_broken_fragment_exits_one(self):
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            (root / "AGENTS.md").write_text(
                "# Fake\n\n```go\nnot go at all {{{ ]]]\n```\n", encoding="utf-8"
            )
            rc = m.main(["--root", td])
        self.assertEqual(rc, 1)

    def test_skip_marker_lets_a_broken_fragment_pass(self):
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td)
            (root / "AGENTS.md").write_text(
                "<!-- markdown-example: no-parse-check -->\n"
                "```go\nnot go at all {{{ ]]]\n```\n",
                encoding="utf-8",
            )
            rc = m.main(["--root", td])
        self.assertEqual(rc, 0)


if __name__ == "__main__":
    unittest.main()
