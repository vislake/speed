#!/usr/bin/env python3
"""Print the backend-fragment regeneration matrix for api-contract.yml.

Reads tools/api_fragments.json -- the single machine-readable source of
truth for the platform fragment universe -- and prints, as one line of
JSON, the array GitHub Actions' matrix consumes through fromJson:
[{"name": <fragment>, "dir": <repo-relative api/ directory>}, ...] in
manifest order.

api-contract.yml's contract-gate job publishes this array as its
`fragments` output and the fragment-regeneration job's matrix is
`fromJson(needs.contract-gate.outputs.fragments)` with
`working-directory: ${{ matrix.fragment.dir }}`, so the workflow no
longer enumerates fragments for regeneration at all: a fragment joins
the matrix by joining the manifest (tools/check_api_fragments.py proves
the derivation still matches the manifest, and the trigger path filters
and the redocly join list remain gate-checked, since GitHub's `on.paths`
cannot be derived).

Standard library only; --root defaults to the current directory and
must be the repository root.
"""

from __future__ import annotations

import argparse
import json
import os
import sys

MANIFEST_REL = os.path.join("tools", "api_fragments.json")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Print the api-contract fragment-regeneration matrix (one "
            "JSON line, fromJson-compatible) derived from "
            "tools/api_fragments.json."
        )
    )
    parser.add_argument(
        "--root",
        default=".",
        help="repository root holding tools/api_fragments.json (default: "
        "the current directory)",
    )
    args = parser.parse_args(argv)
    path = os.path.join(os.path.abspath(args.root), MANIFEST_REL)
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except FileNotFoundError:
        print(f"error: {path} is missing -- the fragment manifest this "
              "matrix derives from", file=sys.stderr)
        return 2
    except json.JSONDecodeError as exc:
        print(f"error: {path} is not parsable JSON: {exc}", file=sys.stderr)
        return 2
    if not isinstance(data, dict):
        print(f"error: {path} does not hold a JSON object", file=sys.stderr)
        return 2
    frags = data.get("fragments")
    if not isinstance(frags, list) or not frags:
        print(f"error: {path} carries no non-empty 'fragments' list",
              file=sys.stderr)
        return 2
    entries = []
    for frag in frags:
        if (
            not isinstance(frag, dict)
            or not isinstance(frag.get("name"), str)
            or not isinstance(frag.get("dir"), str)
        ):
            print(f"error: {path} carries a fragment entry without a "
                  "name and dir", file=sys.stderr)
            return 2
        entries.append({"name": frag["name"], "dir": frag["dir"]})
    print(json.dumps(entries, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
