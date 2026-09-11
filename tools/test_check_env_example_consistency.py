#!/usr/bin/env python3
"""Unit suite for tools/check_env_example_consistency.py.

The fixtures synthesize a repository tree -- the host's declaration site
(a loader target with env pins and a WithRootKeyEnv call), the generated
declared-key reference (docs/config-reference.json's bootstrap_keys
entries) and the committed example -- so every reading rule, both key
sets and their comparison run without the real tree; the last case runs
the gate against the real one.
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_env_example_consistency as m  # noqa: E402

BOOTSTRAP = '''package app

import "example.com/pkgcore/config"

// envPrefix is the prefix every bootstrap variable carries.
const envPrefix = "APP_"

// hostConfig is the loader target. The comment mentioning
// WithRootKeyEnv("APP_DOC_ONLY") declares nothing; the real wiring is
// at the load call below.
type hostConfig struct {
	// DeploymentMode names APP_DEPLOYMENT_MODE.
	DeploymentMode string `config:"env=APP_DEPLOYMENT_MODE"`

	// Port names PORT, the one unprefixed variable.
	Port string `config:"env=PORT"`

	// S3Region names APP_S3_REGION.
	S3Region string `config:"env=APP_S3_REGION"`

	// Cipher_Key names APP_CONFIG_KEY.
	Cipher_Key []byte `config:"env=APP_CONFIG_KEY,derive"`
}

func loadHostConfig() (hostConfig, error) {
	hc := hostConfigDefaults()
	err := config.New(
		config.WithEnvPrefix("APP_"),
		config.WithRootKeyEnv("APP_ROOT_KEY"),
	).Load(&hc)
	return hc, err
}
'''

EXAMPLE = '''# Environment variables the reference app's bootstrap resolves;
# the loader-driven surface of internal/app/bootstrap.go.

# Deployment mode: "standalone" (default) or "distributed".
APP_DEPLOYMENT_MODE=standalone

# HTTP listen port. Defaults to 8080 when unset.
PORT=8080

# The S3 composition; the region matters to AWS S3 and Aliyun OSS:
#   APP_S3_REGION=us-east-1
#
# A prose line that names APP_CONFIG_KEY without an entry is not an
# entry.

# Key material: set APP_ROOT_KEY alone to derive the rest.
APP_ROOT_KEY=REPLACE_WITH_64_HEX_CHARS

APP_CONFIG_KEY=

# The declared key materials, under the loader-derived spellings (the
# declared path's dot as a double underscore).
APP_CONFIG__CIPHER_KEY=
APP_AUTHN__BLIND_INDEX_KEY=
APP_AUTHN__PII_CIPHER_KEY=
'''

# The root-key read spelled through a same-file constant -- the shape
# examples/reference-app/internal/app/bootstrap.go wires -- with the
# rest of the declaration surface BOOTSTRAP's.
BOOTSTRAP_CONST_ROOT_KEY = BOOTSTRAP.replace(
    'config.WithRootKeyEnv("APP_ROOT_KEY"),',
    "config.WithRootKeyEnv(rootKeyEnv),",
).replace(
    "// hostConfig is the loader target.",
    "// rootKeyEnv names the root-key variable.\n"
    'const rootKeyEnv = "APP_ROOT_KEY"\n\n'
    "// hostConfig is the loader target.",
)

# The same identifier argument with no same-file constant to resolve.
BOOTSTRAP_UNRESOLVED_ROOT_KEY = BOOTSTRAP.replace(
    'config.WithRootKeyEnv("APP_ROOT_KEY"),',
    "config.WithRootKeyEnv(rootKeyEnv),",
)

# The generated declared-key reference fixture: the bootstrap_keys
# entries docs/config-reference.json carries, one per declared key path
# (the declaring module components' BootstrapKey.Key).
CONFIG_REFERENCE = '''{
  "bootstrap_keys": [
    {
      "key": "authn.blind_index_key",
      "module": "authn",
      "env": "SPEED_AUTHN__BLIND_INDEX_KEY",
      "type": "hexkey"
    },
    {
      "key": "authn.pii_cipher_key",
      "module": "authn",
      "env": "SPEED_AUTHN__PII_CIPHER_KEY",
      "type": "hexkey"
    },
    {
      "key": "config.cipher_key",
      "module": "config",
      "env": "SPEED_CONFIG__CIPHER_KEY",
      "type": "hexkey"
    }
  ],
  "dynamic_items": []
}
'''

# The host target the declared-key fixture pairs with: the prefix const
# the derived names are spelled under, the root-key constant, and one
# pinned variable of the host's own.
BOOTSTRAP_DECLARED = '''package app

import "example.com/pkgcore/config"

// envPrefix is the prefix every bootstrap variable carries.
const envPrefix = "APP_"

// rootKeyEnv names the root-key variable.
const rootKeyEnv = "APP_ROOT_KEY"

// hostConfig is the loader target.
type hostConfig struct {
	// Port names PORT, the one unprefixed variable.
	Port string `config:"env=PORT"`
}

func loadHostConfig() (hostConfig, error) {
	hc := hostConfigDefaults()
	err := config.New(
		config.WithEnvPrefix(envPrefix),
		config.WithRootKeyEnv(rootKeyEnv),
	).Load(&hc)
	return hc, err
}
'''

EXAMPLE_DECLARED = '''# Environment variables the reference app's bootstrap resolves.

# HTTP listen port.
PORT=8080

# Key material: set APP_ROOT_KEY alone to derive the rest.
APP_ROOT_KEY=REPLACE_WITH_64_HEX_CHARS

# The three materials the declared keys resolve, under the loader-derived
# spellings (the declared path's dot as a double underscore).
APP_CONFIG__CIPHER_KEY=
APP_AUTHN__BLIND_INDEX_KEY=
APP_AUTHN__PII_CIPHER_KEY=
'''


def make_tree(files: dict[str, str]) -> pathlib.Path:
    root = pathlib.Path(
        tempfile.mkdtemp(prefix="env-example-consistency-")
    )
    for rel, text in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
    return root


def base_files(bootstrap: str = BOOTSTRAP, example: str = EXAMPLE,
               reference: str | None = CONFIG_REFERENCE):
    files = {
        m.BOOTSTRAP_REL_PATH: bootstrap,
        m.ENV_EXAMPLE_REL_PATH: example,
    }
    if reference is not None:
        files[m.CONFIG_REFERENCE_REL_PATH] = reference
    return files


class KeySetComparison(unittest.TestCase):
    def test_matching_sets_stay_silent(self):
        # Covers both declaration forms (the pins, the `,derive` option
        # and the WithRootKeyEnv literal), the declared keys' derived
        # names and both example spellings (active entries and the
        # indented commented-out example).
        self.assertEqual(m.scan(make_tree(base_files())), [])

    def test_host_key_missing_from_the_example_fires(self):
        example = EXAMPLE.replace("#   APP_S3_REGION=us-east-1\n", "")
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn(m.BOOTSTRAP_REL_PATH, findings[0])
        self.assertIn("pins APP_S3_REGION", findings[0])
        self.assertIn(m.ENV_EXAMPLE_REL_PATH, findings[0])

    def test_root_key_missing_from_the_example_fires(self):
        # APP_ROOT_KEY is no struct field; only the WithRootKeyEnv
        # literal declares it, and its absence from the example must
        # fire like any pinned variable's, the finding naming the call
        # line the literal is spelled on.
        example = EXAMPLE.replace(
            "APP_ROOT_KEY=REPLACE_WITH_64_HEX_CHARS\n", ""
        )
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn("pins APP_ROOT_KEY", findings[0])
        call_line = next(
            line_no
            for line_no, line in enumerate(BOOTSTRAP.splitlines(), start=1)
            if 'config.WithRootKeyEnv("APP_ROOT_KEY"),' in line
        )
        self.assertIn(f"{m.BOOTSTRAP_REL_PATH}:{call_line}:", findings[0])

    def test_unknown_example_key_fires(self):
        example = EXAMPLE + "\nAPP_NOT_A_HOST_VARIABLE=1\n"
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn(f"{m.ENV_EXAMPLE_REL_PATH}:", findings[0])
        self.assertIn("documents APP_NOT_A_HOST_VARIABLE", findings[0])
        self.assertIn("never declares", findings[0])

    def test_missing_bootstrap_fires(self):
        files = base_files()
        del files[m.BOOTSTRAP_REL_PATH]
        findings = m.scan(make_tree(files))
        self.assertEqual(len(findings), 1)
        self.assertIn(m.BOOTSTRAP_REL_PATH, findings[0])

    def test_missing_example_fires(self):
        files = base_files()
        del files[m.ENV_EXAMPLE_REL_PATH]
        findings = m.scan(make_tree(files))
        self.assertEqual(len(findings), 1)
        self.assertIn(m.ENV_EXAMPLE_REL_PATH, findings[0])


class RootKeyDeclarationForms(unittest.TestCase):
    def test_constant_form_resolves_the_declared_variable(self):
        # The call's argument is an identifier a same-file const binds:
        # the variable the constant names is declared, so an example
        # documenting it keeps the gate silent.
        self.assertEqual(
            m.scan(
                make_tree(base_files(bootstrap=BOOTSTRAP_CONST_ROOT_KEY))
            ),
            [],
        )

    def test_constant_form_reports_the_constant_line(self):
        # The finding's line is the constant's declaration line, where
        # the variable's name is spelled.
        example = EXAMPLE.replace(
            "APP_ROOT_KEY=REPLACE_WITH_64_HEX_CHARS\n", ""
        )
        findings = m.scan(
            make_tree(
                base_files(
                    bootstrap=BOOTSTRAP_CONST_ROOT_KEY, example=example
                )
            )
        )
        self.assertEqual(len(findings), 1)
        self.assertIn("pins APP_ROOT_KEY", findings[0])
        const_line = next(
            line_no
            for line_no, line in enumerate(
                BOOTSTRAP_CONST_ROOT_KEY.splitlines(), start=1
            )
            if 'const rootKeyEnv = "APP_ROOT_KEY"' in line
        )
        self.assertIn(f"{m.BOOTSTRAP_REL_PATH}:{const_line}:", findings[0])

    def test_unresolvable_identifier_declares_nothing(self):
        # An identifier no same-file constant resolves is not a
        # declaration: the example's entry for it fires as unknown.
        findings = m.scan(
            make_tree(base_files(bootstrap=BOOTSTRAP_UNRESOLVED_ROOT_KEY))
        )
        self.assertEqual(len(findings), 1)
        self.assertIn("documents APP_ROOT_KEY", findings[0])
        self.assertIn("never declares", findings[0])


class DeclaredKeyPaths(unittest.TestCase):
    def declared_files(self, bootstrap: str = BOOTSTRAP_DECLARED,
                       example: str = EXAMPLE_DECLARED,
                       reference: str | None = CONFIG_REFERENCE):
        return base_files(
            bootstrap=bootstrap, example=example, reference=reference
        )

    def test_derived_names_are_read_and_match(self):
        # The positive side: the reference's declared key paths plus the
        # envPrefix const spell the three derived names, which the
        # example documents.
        self.assertEqual(
            m.scan(make_tree(self.declared_files())), []
        )

    def test_derived_name_missing_from_the_example_fires(self):
        example = EXAMPLE_DECLARED.replace(
            "APP_AUTHN__PII_CIPHER_KEY=\n", ""
        )
        findings = m.scan(
            make_tree(self.declared_files(example=example))
        )
        self.assertEqual(len(findings), 1)
        self.assertIn(m.BOOTSTRAP_REL_PATH, findings[0])
        self.assertIn("pins APP_AUTHN__PII_CIPHER_KEY", findings[0])

    def test_derived_name_with_the_flat_spelling_fires(self):
        # The old flat spelling is not a second way in: the declared
        # path derives APP_AUTHN__BLIND_INDEX_KEY, so an entry for
        # APP_AUTHN_BLIND_INDEX_KEY documents a variable the host never
        # declares.
        example = EXAMPLE_DECLARED + "\nAPP_AUTHN_BLIND_INDEX_KEY=\n"
        findings = m.scan(
            make_tree(self.declared_files(example=example))
        )
        self.assertEqual(len(findings), 1)
        self.assertIn("documents APP_AUTHN_BLIND_INDEX_KEY", findings[0])

    def test_missing_reference_fires(self):
        files = self.declared_files()
        del files[m.CONFIG_REFERENCE_REL_PATH]
        findings = m.scan(make_tree(files))
        self.assertEqual(len(findings), 1)
        self.assertIn(m.CONFIG_REFERENCE_REL_PATH, findings[0])

    def test_unreadable_reference_contributes_nothing(self):
        # A document the gate cannot read end to end -- here its JSON is
        # malformed -- declares no derived name, so the example's
        # entries read as variables the host never declares.
        reference = CONFIG_REFERENCE.replace('"bootstrap_keys"', '"keys"')
        findings = m.scan(
            make_tree(self.declared_files(reference=reference))
        )
        self.assertEqual(len(findings), 3)
        for finding in findings:
            self.assertIn(m.ENV_EXAMPLE_REL_PATH, finding)
            self.assertIn("never declares", finding)


class ReadingRules(unittest.TestCase):
    def test_prose_mention_does_not_document_a_key(self):
        # Drop the active APP_CONFIG_KEY= entry but keep the prose line
        # naming it: the prose explicitly says it is not an entry, and
        # the pin must still be reported missing.
        example = EXAMPLE.replace("APP_CONFIG_KEY=\n", "")
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn("pins APP_CONFIG_KEY", findings[0])

    def test_repeated_example_entries_are_one_key(self):
        # A commented-out example and an active entry for the same key
        # (the file's own shape for the AI-gateway pair) are one
        # documented key, and a repeated unknown key fires once, at its
        # first line.
        example = EXAMPLE + (
            "\n# APP_NOT_A_HOST_VARIABLE=1\nAPP_NOT_A_HOST_VARIABLE=1\n"
        )
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        first_line = example.splitlines().index(
            "# APP_NOT_A_HOST_VARIABLE=1"
        ) + 1
        self.assertIn(
            f"{m.ENV_EXAMPLE_REL_PATH}:{first_line}:", findings[0]
        )

    def test_comment_mention_in_bootstrap_is_not_a_declaration(self):
        example = EXAMPLE + "\nAPP_DOC_ONLY=1\n"
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn("documents APP_DOC_ONLY", findings[0])

    def test_commented_unknown_key_fires_too(self):
        # The commented-out spelling is part of the key set in both
        # directions: an unknown key documented only as a commented
        # example (the file's spelling for optional groups) fires the
        # same finding its active spelling does.
        example = EXAMPLE + "\n#   APP_NOT_A_HOST_VARIABLE=1\n"
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn("documents APP_NOT_A_HOST_VARIABLE", findings[0])


class RealTree(unittest.TestCase):
    def test_repository_example_matches_the_declared_surface(self):
        root = pathlib.Path(__file__).resolve().parent.parent
        self.assertEqual(m.scan(root), [])


if __name__ == "__main__":
    unittest.main()
