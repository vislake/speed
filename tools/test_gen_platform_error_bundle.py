#!/usr/bin/env python3
"""Unit tests for gen_platform_error_bundle.py.

Stdlib-only (unittest + tempfile), matching this directory's own
"plain executables with no third-party dependencies" convention (tools/
README.md's "Running in CI and locally" section) -- no pytest, no
fixtures directory, no network. The filters, refusals and bytes each get
a planted case, and the red line the whole tool exists to hold -- a
catalog id with no apperr construction behind it (an invitation email, a
notification template, an SMS body) must never reach the bundle -- is
pinned from both sides: a temp-tree case and the real committed
artifacts. zh-CN fixture text is written with \\uXXXX escapes because
tools/ is an English-only source tree (root CLAUDE.md's Language Rule;
tools/test_check_i18n_keys.py's own suite sets the same precedent).

Run directly:

    python3 tools/test_gen_platform_error_bundle.py
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import gen_platform_error_bundle as m  # noqa: E402

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent

# The two Go constructions the temp trees plant: one code with catalog
# copy behind it, one without.
YES_MARKER = 'ErrYes = apperr.Invalid("foo.has_copy")\n'
NO_MARKER = 'ErrNo = apperr.Invalid("foo.no_copy")\n'

# zh-CN fixture strings, written as escapes (see the module docstring):
# "has copy", "you have been invited", "at most {{.max_length}} chars",
# "{{.Count}} items", "one", "many", "the code named like the form",
# "same", "another id", "first", "second".
ZH_COPY = "\u6709\u6587\u6848\u3002"
ZH_INVITED = "\u4f60\u5df2\u88ab\u9080\u8bf7\u3002"
ZH_AT_MOST = "\u6700\u591a {{.max_length}} \u4e2a\u5b57\u7b26\u3002"
ZH_AT_MOST_I18NEXT = "\u6700\u591a {{max_length}} \u4e2a\u5b57\u7b26\u3002"
ZH_COUNT = "{{.Count}} \u4e2a\u3002"
ZH_COUNT_I18NEXT = "{{Count}} \u4e2a\u3002"
ZH_ONE = "\u4e00\u4e2a\u3002"
ZH_MANY = "\u591a\u4e2a\u3002"
ZH_SAME_CODE = "\u540c\u540d\u4ee3\u7801\u3002"
ZH_SAME = "\u540c\u4e0a"
ZH_OTHER = "\u522b\u7684\u3002"
ZH_FIRST = "\u4e00\u3002"
ZH_SECOND = "\u4e8c\u3002"


class BundleTestCase(unittest.TestCase):
    """A temp repo tree with the layout the generator scans: go/ roots
    and locales/<language>.toml catalogs."""

    def tree(self, go_source: str = "", en: str = "", zh: str = "") -> pathlib.Path:
        td = tempfile.TemporaryDirectory()
        self.addCleanup(td.cleanup)
        root = pathlib.Path(td.name)
        files: dict[str, str] = {}
        if go_source:
            files["go/foo/errors.go"] = "package foo\n\n" + go_source
        if en:
            files["go/foo/locales/en-US.toml"] = en
        if zh:
            files["go/foo/locales/zh-CN.toml"] = zh
        for rel, content in files.items():
            path = root / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content, encoding="utf-8")
        (root / "go").mkdir(exist_ok=True)
        return root

    def build(self, root: pathlib.Path):
        return m.build_bundle([root / "go", root / "examples"], root)


class FilterTests(BundleTestCase):
    def test_code_with_catalog_copy_lands_in_the_bundle(self):
        root = self.tree(
            go_source=YES_MARKER,
            en='"foo.has_copy" = "It has copy."\n',
            zh=f'"foo.has_copy" = "{ZH_COPY}"\n',
        )
        bundles, counts = self.build(root)
        self.assertEqual(bundles["en-US"], {"foo.has_copy": "It has copy."})
        self.assertEqual(bundles["zh-CN"], {"foo.has_copy": ZH_COPY})
        self.assertEqual(counts["in_bundle"], 1)

    def test_content_id_without_a_census_code_stays_out(self):
        # The red line: a catalog id no Go source constructs (an
        # invitation email subject, a notification template, an SMS
        # body) has no code for a client to look it up with, so the
        # bundle must not carry it -- the frontend cannot be handed
        # backend-only content.
        root = self.tree(
            go_source=YES_MARKER,
            en='"foo.has_copy" = "It has copy."\n'
            '"foo.invitation.subject" = "You have been invited."\n',
            zh=f'"foo.has_copy" = "{ZH_COPY}"\n'
            f'"foo.invitation.subject" = "{ZH_INVITED}"\n',
        )
        bundles, counts = self.build(root)
        for language in m.LANGUAGES:
            self.assertNotIn("foo.invitation.subject", bundles[language])
        self.assertEqual(counts["excluded_content"], 1)
        self.assertEqual(counts["in_bundle"], 1)

    def test_census_code_without_catalog_copy_stays_out(self):
        root = self.tree(
            go_source=YES_MARKER + NO_MARKER,
            en='"foo.has_copy" = "It has copy."\n',
            zh=f'"foo.has_copy" = "{ZH_COPY}"\n',
        )
        bundles, counts = self.build(root)
        for language in m.LANGUAGES:
            self.assertNotIn("foo.no_copy", bundles[language])
        self.assertEqual(counts["census_no_catalog"], 1)


class InterpolationTests(BundleTestCase):
    def test_go_template_placeholder_normalizes_to_i18next(self):
        root = self.tree(
            go_source=YES_MARKER,
            en='"foo.has_copy" = "At most {{.max_length}} characters."\n',
            zh=f'"foo.has_copy" = "{ZH_AT_MOST}"\n',
        )
        bundles, _ = self.build(root)
        self.assertEqual(
            bundles["en-US"], {"foo.has_copy": "At most {{max_length}} characters."}
        )
        self.assertEqual(bundles["zh-CN"], {"foo.has_copy": ZH_AT_MOST_I18NEXT})

    def test_template_action_shape_is_refused(self):
        root = self.tree(
            go_source=YES_MARKER,
            en='"foo.has_copy" = "{{if .Name}}hi{{end}}"\n',
            zh=f'"foo.has_copy" = "{ZH_COPY}"\n',
        )
        with self.assertRaises(m.BundleRefused) as caught:
            self.build(root)
        self.assertIn("is not translatable mechanically", str(caught.exception))

    def test_field_chain_shape_is_refused(self):
        root = self.tree(
            go_source=YES_MARKER,
            en='"foo.has_copy" = "{{.user.name}}"\n',
            zh=f'"foo.has_copy" = "{ZH_COPY}"\n',
        )
        with self.assertRaises(m.BundleRefused):
            self.build(root)

    def test_unterminated_placeholder_is_refused(self):
        root = self.tree(
            go_source=YES_MARKER,
            en='"foo.has_copy" = "at most {{.max_length chars"\n',
            zh=f'"foo.has_copy" = "{ZH_COPY}"\n',
        )
        with self.assertRaises(m.BundleRefused) as caught:
            self.build(root)
        self.assertIn("never closed", str(caught.exception))

    def test_lone_closing_braces_are_refused(self):
        root = self.tree(
            go_source=YES_MARKER,
            en='"foo.has_copy" = "close those }} braces"\n',
            zh=f'"foo.has_copy" = "{ZH_COPY}"\n',
        )
        with self.assertRaises(m.BundleRefused):
            self.build(root)


class PluralTests(BundleTestCase):
    def test_plural_table_becomes_suffixed_leaves(self):
        root = self.tree(
            go_source='ErrCounted = apperr.Invalid("foo.counted")\n',
            en='["foo.counted"]\none = "{{.Count}} thing."\nother = "{{.Count}} things."\n',
            zh=f'["foo.counted"]\none = "{ZH_COUNT}"\nother = "{ZH_COUNT}"\n',
        )
        bundles, counts = self.build(root)
        self.assertEqual(counts["in_bundle"], 1)
        self.assertEqual(
            bundles["en-US"],
            {
                "foo.counted_one": "{{Count}} thing.",
                "foo.counted_other": "{{Count}} things.",
            },
        )

    def test_missing_form_is_padded_from_the_languages_other_form(self):
        # zh-CN selects only the `other` category, so its catalog ships
        # only that form; the bundle still carries `_one` in both
        # languages, because bundles must register with identical leaf
        # sets -- the zh-CN `_one` is padded from zh-CN's own `other`.
        root = self.tree(
            go_source='ErrCounted = apperr.Invalid("foo.counted")\n',
            en='["foo.counted"]\none = "{{.Count}} thing."\nother = "{{.Count}} things."\n',
            zh=f'["foo.counted"]\nother = "{ZH_COUNT}"\n',
        )
        bundles, _ = self.build(root)
        self.assertEqual(
            sorted(bundles["zh-CN"]), ["foo.counted_one", "foo.counted_other"]
        )
        self.assertEqual(bundles["zh-CN"]["foo.counted_one"], ZH_COUNT_I18NEXT)

    def test_translation_key_is_the_v1_other_synonym(self):
        root = self.tree(
            go_source='ErrCounted = apperr.Invalid("foo.counted")\n',
            en='["foo.counted"]\ntranslation = "{{.Count}} things."\n',
            zh=f'["foo.counted"]\ntranslation = "{ZH_COUNT}"\n',
        )
        bundles, _ = self.build(root)
        self.assertEqual(bundles["en-US"], {"foo.counted_other": "{{Count}} things."})

    def test_metadata_only_table_is_refused(self):
        root = self.tree(
            go_source='ErrCounted = apperr.Invalid("foo.counted")\n',
            en='["foo.counted"]\ndescription = "documents nothing renderable"\n',
            zh=f'["foo.counted"]\ndescription = "{ZH_SAME}"\n',
        )
        with self.assertRaises(m.BundleRefused) as caught:
            self.build(root)
        self.assertIn("no CLDR category form", str(caught.exception))

    def test_shape_disagreement_across_languages_is_refused(self):
        root = self.tree(
            go_source='ErrCounted = apperr.Invalid("foo.counted")\n',
            en='["foo.counted"]\none = "one."\nother = "many."\n',
            zh=f'"foo.counted" = "{ZH_ONE}"\n',
        )
        with self.assertRaises(m.BundleRefused) as caught:
            self.build(root)
        self.assertIn("disagree on the message shape", str(caught.exception))

    def test_leaf_collision_after_plural_expansion_is_refused(self):
        root = self.tree(
            go_source='ErrCounted = apperr.Invalid("foo.counted")\n'
            'ErrOne = apperr.Invalid("foo.counted_one")\n',
            en='"foo.counted" = { one = "one.", other = "many." }\n'
            '"foo.counted_one" = "the code named like the form."\n',
            zh=f'"foo.counted" = {{ one = "{ZH_ONE}", other = "{ZH_MANY}" }}\n'
            f'"foo.counted_one" = "{ZH_SAME_CODE}"\n',
        )
        with self.assertRaises(m.BundleRefused) as caught:
            self.build(root)
        self.assertIn("declared twice", str(caught.exception))


class RefusalTests(BundleTestCase):
    def test_empty_translation_is_refused(self):
        root = self.tree(
            go_source=YES_MARKER,
            en='"foo.has_copy" = ""\n',
            zh=f'"foo.has_copy" = "{ZH_COPY}"\n',
        )
        with self.assertRaises(m.BundleRefused) as caught:
            self.build(root)
        self.assertIn("is empty", str(caught.exception))

    def test_empty_plural_form_is_refused(self):
        root = self.tree(
            go_source='ErrCounted = apperr.Invalid("foo.counted")\n',
            en='["foo.counted"]\none = ""\nother = "many."\n',
            zh=f'["foo.counted"]\none = "{ZH_ONE}"\nother = "{ZH_MANY}"\n',
        )
        with self.assertRaises(m.BundleRefused):
            self.build(root)

    def test_missing_zh_id_is_refused(self):
        root = self.tree(
            go_source=YES_MARKER,
            en='"foo.has_copy" = "It has copy."\n',
            zh=f'"foo.other_id" = "{ZH_OTHER}"\n',
        )
        with self.assertRaises(m.BundleRefused) as caught:
            self.build(root)
        self.assertIn("foo.has_copy", str(caught.exception))

    def test_duplicate_id_across_catalogs_is_refused(self):
        td = tempfile.TemporaryDirectory()
        self.addCleanup(td.cleanup)
        root = pathlib.Path(td.name)
        files = {
            "go/foo/errors.go": "package foo\n\n" + YES_MARKER,
            "go/foo/locales/en-US.toml": '"foo.has_copy" = "first."\n',
            "go/bar/locales/en-US.toml": '"foo.has_copy" = "second."\n',
            "go/foo/locales/zh-CN.toml": f'"foo.has_copy" = "{ZH_FIRST}"\n',
            "go/bar/locales/zh-CN.toml": f'"foo.has_copy" = "{ZH_SECOND}"\n',
        }
        for rel, content in files.items():
            path = root / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content, encoding="utf-8")
        with self.assertRaises(m.BundleRefused) as caught:
            self.build(root)
        self.assertIn("already declared", str(caught.exception))


class OutputTests(unittest.TestCase):
    def test_bundle_json_has_the_errors_section_and_sorted_flat_keys(self):
        rendered = m.render_bundle_json({"b.code": "second.", "a.code": "first."})
        self.assertEqual(
            rendered,
            '{\n  "errors": {\n    "a.code": "first.",\n    "b.code": "second."\n  }\n}\n',
        )
        self.assertIsInstance(json.loads(rendered)["errors"]["a.code"], str)

    def test_render_is_byte_deterministic(self):
        leaves = {"foo.has_copy": "text", "foo.counted_one": "one"}
        self.assertEqual(
            m.render_bundle_json(leaves),
            m.render_bundle_json(dict(reversed(list(leaves.items())))),
        )

    def test_stale_outputs_reports_absent_and_drifted_files(self):
        with tempfile.TemporaryDirectory() as name:
            out_dir = pathlib.Path(name)
            rendered = {"zh-CN": "zh\n", "en-US": "en\n"}
            self.assertEqual(len(m.stale_outputs(out_dir, rendered)), 2)
            (out_dir / "zh-CN.json").write_text("zh\n", encoding="utf-8")
            (out_dir / "en-US.json").write_text("stale\n", encoding="utf-8")
            stale = m.stale_outputs(out_dir, rendered)
            self.assertEqual(len(stale), 1)
            self.assertTrue(stale[0].endswith("en-US.json"))

    def test_stale_outputs_is_empty_when_files_match(self):
        with tempfile.TemporaryDirectory() as name:
            out_dir = pathlib.Path(name)
            rendered = {"zh-CN": "zh\n", "en-US": "en\n"}
            for language, text in rendered.items():
                (out_dir / f"{language}.json").write_text(text, encoding="utf-8")
            self.assertEqual(m.stale_outputs(out_dir, rendered), [])


class CommittedArtifactTests(unittest.TestCase):
    """The real tree: the committed bundle is what a fresh render
    produces, holds the app modules' codes (the --roots default covers
    examples/), and holds none of the content ids the bundle exists to
    exclude."""

    def _fresh_render(self) -> dict[str, dict[str, str]]:
        bundles, _ = m.build_bundle(
            [REPO_ROOT / "go", REPO_ROOT / "examples"], REPO_ROOT
        )
        return bundles

    def test_committed_bundle_matches_a_fresh_render(self):
        out_dir = REPO_ROOT / "web/packages/i18n/src/platform-errors/locales"
        rendered = {
            language: m.render_bundle_json(leaves)
            for language, leaves in self._fresh_render().items()
        }
        self.assertEqual(m.stale_outputs(out_dir, rendered), [])

    def test_app_module_codes_from_examples_are_in_the_bundle(self):
        bundles = self._fresh_render()
        self.assertIn("notes.text_required", bundles["en-US"])
        self.assertIn("notes.text_required", bundles["zh-CN"])

    def test_content_ids_are_absent_from_the_committed_bundle(self):
        bundles = self._fresh_render()
        excluded = (
            "org.invitation.subject",
            "authn.sms.verification_code",
            "notification.contact.verify_code.email.subject",
            "pkgcore.seed.plural_other",
            "org.default_workspace_name",
        )
        for language in m.LANGUAGES:
            for code in excluded:
                self.assertNotIn(code, bundles[language], language)


if __name__ == "__main__":
    unittest.main()
