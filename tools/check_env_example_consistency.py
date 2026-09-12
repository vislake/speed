#!/usr/bin/env python3
"""Env-example consistency gate: the reference app host's declared
environment surface and its committed .env.example are one key set.

examples/reference-app/.env.example is the reference app's committed
carrier for the variables its host reads: its own header states that it
documents ConfigFromEnv's loader-driven bootstrap surface, and readers
copy it to .env or apply it through a shell / a process manager. The
host side of that surface is declared in three forms:

  * the loader target's env pins -- examples/reference-app/internal/app/
    bootstrap.go's hostConfig struct tags the host's own variables with
    config:"env=NAME" (further loader options after the name do not
    change the variable name), so the tag is each variable's
    declaration site;
  * APP_ROOT_KEY -- the one variable that is not a struct field at all:
    the loader reads it through the WithRootKeyEnv option loadHostConfig
    wires. The call's argument names the variable either as a string
    literal or as an identifier a same-file const declaration binds to
    one; an identifier with no such constant declares nothing;
  * the declared key materials -- each declaring module's component
    carries its BootstrapKeys declaration, the assembly's loader
    resolves them, and each key path's environment variable is derived
    under the host's prefix: config.EnvName's rule, the prefix then the
    key uppercased with each level of nesting spelled as a double
    underscore, so config.cipher_key reads APP_CONFIG__CIPHER_KEY under
    APP_. The prefix is read from the loadHostConfig call's
    WithEnvPrefix option, its argument the same two forms the
    WithRootKeyEnv argument takes (a string literal, or an identifier a
    same-file const declaration binds to one). The declared key paths
    are read from docs/config-reference.json's bootstrap_keys entries,
    the generated reference of exactly the declarations the components
    carry.

Nothing else in the app's executable code reads the environment (the
app's own unit suite pins that surface, unittest/
bootstrap_direct_reads_test.go), so the pins, that call and the
declarations' derived names are the host's whole declared set.

The example side is read line by line in both spellings the file
actually uses for keys: an active KEY= entry and a commented-out
example (``# APP_X=...`` -- the indented ``#   APP_X=...`` variant
included). A prose mention of a name is not an entry.
"""

from __future__ import annotations

import json
import pathlib
import re
import sys

# The host's declaration site, the declared-key reference, and the
# example it must agree with, relative to the repository root.
BOOTSTRAP_REL_PATH = "examples/reference-app/internal/app/bootstrap.go"
CONFIG_REFERENCE_REL_PATH = "docs/config-reference.json"
ENV_EXAMPLE_REL_PATH = "examples/reference-app/.env.example"

# One struct-tag literal: config:"env=NAME" with any further loader
# options (",derive") after the name.
CONFIG_TAG = re.compile(r'config:"([^"]*)"')

# The root-key read: WithRootKeyEnv(NAME), the name spelled either as
# a string literal (group 1) or as an identifier (group 2) the file's
# own const declaration binds to the name's literal.
ROOT_KEY_ENV = re.compile(
    r'WithRootKeyEnv\(\s*'
    r'(?:"([A-Za-z_][A-Za-z0-9_]*)"|([A-Za-z_][A-Za-z0-9_]*))'
    r'\s*\)'
)

# The loader-prefix read: WithEnvPrefix(NAME), the prefix spelled in
# the same two forms ROOT_KEY_ENV reads. The anchor is the call the
# loader is actually built with, never the constant's identifier name:
# the prefix stays derivable however the same-file constant behind the
# argument is named or exported.
ENV_PREFIX_CALL = re.compile(
    r'WithEnvPrefix\(\s*'
    r'(?:"([A-Za-z_][A-Za-z0-9_]*)"|([A-Za-z_][A-Za-z0-9_]*))'
    r'\s*\)'
)

# One same-file const binding a call argument -- the root-key name or
# the loader prefix -- may name: const NAME = "STRING", the value an
# environment variable name or the prefix.
ROOT_KEY_CONST = re.compile(
    r'\bconst\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*"([A-Za-z_][A-Za-z0-9_]*)"'
)

# A documented key: NAME= at the head of a line's content (the value
# may be empty), once any leading whitespace and the comment marker of
# the commented-out example spelling are stripped.
EXAMPLE_KEY = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)=")


def declared_key_paths(reference_text: str) -> list[str]:
    """The declared bootstrap key paths the generated reference lists.

    Every bootstrap_keys entry's "key" is a declared key path (the
    declaring module component's BootstrapKey.Key, character for
    character); an unreadable document contributes nothing, and the
    example's entries then read as variables the host never declares,
    which is the drift the gate is for.
    """
    try:
        document = json.loads(reference_text)
    except json.JSONDecodeError:
        return []
    entries = document.get("bootstrap_keys")
    if not isinstance(entries, list):
        return []
    paths: list[str] = []
    for entry in entries:
        if isinstance(entry, dict) and isinstance(entry.get("key"), str):
            paths.append(entry["key"])
    return paths


def declared_host_keys(text: str, key_paths: list[str]) -> dict[str, int]:
    """The variables bootstrap.go's host surface declares, name -> first
    declaration line.

    Three declaration forms are read: a struct tag's env pin
    (config:"env=NAME"; any loader options after the comma-separated
    name are not part of the variable), the WithRootKeyEnv call,
    whose argument is the name as a string literal or an identifier
    the file's own const declaration binds to one (an identifier with
    no such constant in the file declares nothing, and the line
    reported for the resolved identifier form is the constant's
    declaration line), and the declared key paths' derived names --
    one per path in key_paths, reported at the line spelling the
    prefix: the WithEnvPrefix argument's call line for the literal
    form, the bound constant's declaration line for the identifier
    form. A WithEnvPrefix argument the file's own constants do not
    resolve derives nothing, and the example's derived-spelling
    entries then read as variables the host never declares. Full-line
    ``//`` comments are skipped -- a comment may state a variable's
    name, but only the pins, the call sites and the declarations name
    one, and the line numbers reported for the findings point at
    code."""
    lines = text.splitlines()
    consts: dict[str, tuple[str, int]] = {}
    for line_no, line in enumerate(lines, start=1):
        if line.strip().startswith("//"):
            continue
        match = ROOT_KEY_CONST.search(line)
        if match and match.group(1) not in consts:
            consts[match.group(1)] = (match.group(2), line_no)

    keys: dict[str, int] = {}
    prefix: tuple[str, int] | None = None
    for line_no, line in enumerate(lines, start=1):
        if line.strip().startswith("//"):
            continue
        for tag in CONFIG_TAG.finditer(line):
            for option in tag.group(1).split(","):
                if option.startswith("env="):
                    name = option[len("env="):].strip()
                    if name and name not in keys:
                        keys[name] = line_no
        root_key = ROOT_KEY_ENV.search(line)
        if root_key is not None:
            if root_key.group(1) is not None:
                name, decl_line = root_key.group(1), line_no
            elif root_key.group(2) in consts:
                name, decl_line = consts[root_key.group(2)]
            else:
                name = None
            if name is not None and name not in keys:
                keys[name] = decl_line
        if prefix is None:
            prefix_call = ENV_PREFIX_CALL.search(line)
            if prefix_call is not None:
                if prefix_call.group(1) is not None:
                    prefix = (prefix_call.group(1), line_no)
                elif prefix_call.group(2) in consts:
                    prefix = consts[prefix_call.group(2)]

    # The declared key materials' derived names: one per declared key
    # path, reported at the line spelling the prefix. A WithEnvPrefix
    # argument the file's constants do not resolve derives nothing, and
    # the example's entries then read as variables the host never
    # declares.
    if prefix is None:
        return keys
    for path in key_paths:
        # config.EnvName(prefix, key): the prefix, then the key uppercased
        # with each nesting level (config.KeyDelimiter ".") spelled as the
        # environment separator (config.EnvSeparator, a double underscore).
        name = prefix[0] + path.upper().replace(".", "__")
        keys.setdefault(name, prefix[1])
    return keys


def documented_example_keys(text: str) -> dict[str, int]:
    """The keys .env.example documents, name -> first documenting line.

    Both key spellings the file uses count: an active ``KEY=`` line and
    a commented-out example (``# APP_X=...``, the indented variant
    included). A prose line that merely names a variable does not."""
    keys: dict[str, int] = {}
    for line_no, line in enumerate(text.splitlines(), start=1):
        entry = line.strip()
        if entry.startswith("#"):
            entry = entry[1:].strip()
        match = EXAMPLE_KEY.match(entry)
        if match and match.group(1) not in keys:
            keys[match.group(1)] = line_no
    return keys


def scan(root: pathlib.Path) -> list[str]:
    findings: list[str] = []
    bootstrap = root / BOOTSTRAP_REL_PATH
    reference = root / CONFIG_REFERENCE_REL_PATH
    example = root / ENV_EXAMPLE_REL_PATH
    for path, rel, what in (
        (bootstrap, BOOTSTRAP_REL_PATH, "the host's declaration site"),
        (reference, CONFIG_REFERENCE_REL_PATH, "the declared-key reference"),
        (example, ENV_EXAMPLE_REL_PATH, "the committed example"),
    ):
        if not path.is_file():
            findings.append(
                f"{rel}: {what} is missing -- the gate compares the "
                "host's declared environment surface against the "
                "committed example, and all of these are committed "
                "artifacts"
            )
    if findings:
        return findings

    host = declared_host_keys(
        bootstrap.read_text(encoding="utf-8"),
        declared_key_paths(reference.read_text(encoding="utf-8")),
    )
    documented = documented_example_keys(
        example.read_text(encoding="utf-8")
    )

    for name, line_no in sorted(host.items(), key=lambda item: item[1]):
        if name not in documented:
            findings.append(
                f"{BOOTSTRAP_REL_PATH}:{line_no}: pins {name}, but "
                f"{ENV_EXAMPLE_REL_PATH} documents no such key -- add an "
                "entry there (an active KEY= line or a commented-out "
                "example; the file counts both)"
            )
    for name, line_no in sorted(documented.items(), key=lambda item: item[1]):
        if name not in host:
            findings.append(
                f"{ENV_EXAMPLE_REL_PATH}:{line_no}: documents {name}, "
                "which the host never declares -- remove the entry, or "
                "wire the variable in the loader target so the host "
                f"reads it ({BOOTSTRAP_REL_PATH})"
            )
    return findings


def main() -> int:
    root = pathlib.Path(__file__).resolve().parent.parent
    findings = scan(root)
    for finding in findings:
        print(f"check_env_example_consistency: {finding}")
    if findings:
        print(
            f"check_env_example_consistency: {len(findings)} finding(s)"
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
