#!/usr/bin/env python3
"""Toolchain drift gate for the root .mise.toml.

The developer toolchain is pinned with mise; the root .mise.toml
carries the versions. CI cannot read .mise.toml
directly -- actions/setup-go's go-version-file resolves go.mod, go.work,
go.sum or .go-version only, and setup-node reads web/.nvmrc -- so every
.mise.toml version is a MIRROR of an authoritative source elsewhere in
the repository, and this gate (wired into fast-check's repo-checks job)
fails when a mirror drifts from its source. The sources, one per tool:

  task          the Taskfile.yml header comment -- "task 3.53.1 (the
                version verified against this file)" -- the one tool
                whose only pin lives there (scanned over the header's
                first 40 lines)
  go            go.work's `go` directive, which actions/setup-go reads
                (.github/actions/setup-go-env/action.yml)
  node          web/.nvmrc, which the shared setup-node-env action reads
  pnpm          web/package.json's packageManager field
  golangci-lint GOLANGCI_VERSION in .github/actions/setup-go-env/
                action.yml
  hugo          HUGO_VERSION in .github/actions/setup-hugo-env/
                action.yml

Bump the authoritative source and the mirror together; this gate exists
because they are separate files and will drift without it.

Exit codes: 0 = every mirror equals its source; 1 = drift (a mirror
differs from its source, or a report printed below that starts with
MISMATCH); 2 = infrastructure error (a source file missing or
unparsable, or an expected tool absent from .mise.toml). Paths in the
report are relative to --root.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import tomllib

# Tools .mise.toml is expected to pin, with the authoritative source each
# mirrors. The value is a (file, reader, what) triple used in the report.
TASK_HEADER_LIMIT = 40  # the header comment's task pin lives in these lines


def _infra(message: str) -> None:
    """Report an infrastructure failure and exit 2.

    The exit-code contract (module docstring): 2 = infrastructure error
    (a source file missing or unparsable, a tool absent from .mise.toml),
    and only 1 = drift. SystemExit alone cannot carry the distinction --
    sys.exit("message") exits 1 -- so the message is printed to stderr
    first and the bare code raised.
    """
    print(message, file=sys.stderr)
    raise SystemExit(2)


def _read_mise_tools(root: str) -> dict[str, str]:
    path = os.path.join(root, ".mise.toml")
    try:
        with open(path, "rb") as fh:
            data = tomllib.load(fh)
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the mise config this gate guards")
    except tomllib.TOMLDecodeError as exc:
        _infra(f"error: {path} is not parsable TOML: {exc}")
    tools = data.get("tools")
    if not isinstance(tools, dict):
        _infra(f"error: {path} has no [tools] table")
    return {str(k): str(v) for k, v in tools.items()}


def _read_task_pin(root: str) -> str:
    path = os.path.join(root, "Taskfile.yml")
    try:
        with open(path, encoding="utf-8") as fh:
            # Read up to TASK_HEADER_LIMIT lines one at a time, keeping
            # what was actually read: a file shorter than the limit (a
            # freshly scaffolded one, say) must still report a pin that
            # lives in its readable lines. The old form -- a list
            # comprehension of next(fh) calls that raised StopIteration
            # mid-read and discarded the lines already collected -- turned
            # a short Taskfile whose pin sat on line 1 into "pin not
            # found".
            lines: list[str] = []
            for _ in range(TASK_HEADER_LIMIT):
                line = fh.readline()
                if not line:
                    break
                lines.append(line)
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the source of the task pin")
    m = re.search(r"\btask\s+(\d+\.\d+(?:\.\d+)?)", "".join(lines))
    if not m:
        _infra(
            f"error: no 'task <version>' pin found in the first "
            f"{TASK_HEADER_LIMIT} lines of {path}"
        )
    return m.group(1)


def _read_go_version(root: str) -> str:
    path = os.path.join(root, "go.work")
    try:
        with open(path, encoding="utf-8") as fh:
            text = fh.read()
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the source of the go pin")
    m = re.search(r"^go\s+(\d+\.\d+(?:\.\d+)?)", text, re.MULTILINE)
    if not m:
        _infra(f"error: no 'go' directive found in {path}")
    return m.group(1)


def _read_nvmrc(root: str) -> str:
    path = os.path.join(root, "web", ".nvmrc")
    try:
        with open(path, encoding="utf-8") as fh:
            version = fh.read().strip()
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the source of the node pin")
    if not version:
        _infra(f"error: {path} is empty -- the source of the node pin")
    return version


def _read_package_manager(root: str) -> str:
    path = os.path.join(root, "web", "package.json")
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the source of the pnpm pin")
    except json.JSONDecodeError as exc:
        _infra(f"error: {path} is not parsable JSON: {exc}")
    field = data.get("packageManager")
    if not isinstance(field, str):
        _infra(f"error: {path} has no packageManager string field")
    m = re.match(r"^pnpm@(\d+\.\d+(?:\.\d+)?)", field)
    if not m:
        _infra(f"error: {path}'s packageManager is not 'pnpm@<version>': {field}")
    return m.group(1)


def _read_golangci_version(root: str) -> str:
    path = os.path.join(root, ".github", "actions", "setup-go-env", "action.yml")
    try:
        with open(path, encoding="utf-8") as fh:
            text = fh.read()
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the source of the golangci-lint pin")
    m = re.search(r'^\s*GOLANGCI_VERSION:\s*"([^"]+)"', text, re.MULTILINE)
    if not m:
        _infra(f"error: no GOLANGCI_VERSION found in {path}")
    return m.group(1)


def _read_hugo_version(root: str) -> str:
    path = os.path.join(root, ".github", "actions", "setup-hugo-env", "action.yml")
    try:
        with open(path, encoding="utf-8") as fh:
            text = fh.read()
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the source of the hugo pin")
    m = re.search(r'^\s*HUGO_VERSION:\s*"([^"]+)"', text, re.MULTILINE)
    if not m:
        _infra(f"error: no HUGO_VERSION found in {path}")
    return m.group(1)


SOURCES = [
    ("task", _read_task_pin, "Taskfile.yml header comment"),
    ("go", _read_go_version, "go.work's go directive"),
    ("node", _read_nvmrc, "web/.nvmrc"),
    ("pnpm", _read_package_manager, "web/package.json's packageManager"),
    ("golangci-lint", _read_golangci_version, "setup-go-env's GOLANGCI_VERSION"),
    ("hugo", _read_hugo_version, "setup-hugo-env's HUGO_VERSION"),
]


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Fail when a version pinned in the root .mise.toml no longer mirrors "
            "its authoritative source (Taskfile.yml header, go.work, web/.nvmrc, "
            "web/package.json, setup-go-env's GOLANGCI_VERSION, setup-hugo-env's "
            "HUGO_VERSION)."
        )
    )
    parser.add_argument(
        "--root",
        default=".",
        help="repository root to check (default: current directory); "
        "paths in the report are relative to it",
    )
    args = parser.parse_args(argv)
    root = os.path.abspath(args.root)
    if not os.path.isdir(root):
        print(f"error: --root is not a directory: {args.root}", file=sys.stderr)
        return 2

    mise = _read_mise_tools(root)
    drifted = 0
    missing = 0
    for tool, reader, source_name in SOURCES:
        if tool not in mise:
            print(f"toolchain: MISSING     {tool} is absent from .mise.toml")
            missing += 1
            continue
        mirror = mise[tool]
        source = reader(root)
        if mirror == source:
            print(
                f"toolchain: ok          {tool}: .mise.toml {mirror} == "
                f"{source_name} ({source})"
            )
        else:
            print(
                f"toolchain: MISMATCH    {tool}: .mise.toml pins {mirror} but "
                f"{source_name} says {source}"
            )
            drifted += 1

    if missing:
        # An expected tool absent from .mise.toml is an infrastructure
        # failure per the module docstring's exit-code contract: the gate
        # exists to prove the mirrors cannot drift, and a mirror that does
        # not exist cannot be compared.
        print(
            f"error: {missing} expected tool(s) absent from .mise.toml -- "
            "every tool this gate's SOURCES list pins must appear in the "
            "[tools] table (see the .mise.toml header)",
            file=sys.stderr,
        )
        return 2
    if drifted:
        print(
            f"error: {drifted} tool version(s) drifted -- bump the authoritative "
            "source and .mise.toml together (see the .mise.toml header)",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
