#!/usr/bin/env python3
"""Fragment-enumeration drift gate for .github/workflows/api-contract.yml.

The workflow names the backend-fragment universe in several coherent
places at once: the trigger path filters (the pull_request and push
`paths` blocks name each fragment's api/ directory), the thirteen
oapi-codegen regeneration steps (one `cd <dir>` + pinned oapi-codegen
run per fragment), and the redocly `join` input list (the fragments
merged into build/openapi/speed.yaml, whose order is load-bearing: the
merged document is committed and diff-gated, and the input order is what
join renders). The fragment set grew from six to thirteen over
successive rounds with the sites' consistency held only by comments in
that file saying "keep the two in lockstep" -- an enumeration that can
drift silently, and one that did.

tools/api_fragments.json is the single machine-readable source of truth
for the fragment list. This gate reads that manifest, the live tree and
the workflow's legs, and fails (exit 1) on any of these disagreements:

  * a fragment registered in the manifest whose api/ directory does not
    exist on disk with its openapi.yaml and oapi-codegen.yaml;
  * an api/ directory on disk carrying openapi.yaml and
    oapi-codegen.yaml that the manifest does not register (the
    tree-scan signature of a backend fragment; anything else that looks
    like one must be registered or moved);
  * a registered fragment missing from the pull_request or the push
    trigger path filter (each fragment's filter row is exactly its
    `<dir>/**`), or a path-filter row of fragment shape (`.../api/**`)
    naming a directory the manifest does not register;
  * a registered fragment with no oapi-codegen regeneration step in the
    workflow, or a regeneration step regenerating a directory the
    manifest does not register;
  * the redocly join step's input list disagreeing with the manifest's
    merged fragments (the fragments carrying a `merge_rank`, in rank
    order -- rank order is the join order, and the join order shapes
    the committed merged document);
  * tools/api_fragments.json, tools/check_api_fragments.py or
    tools/test_check_api_fragments.py missing from either trigger path
    filter (a change to the manifest or to this gate must re-run the
    job that consumes them).

The tree scan skips .git/, .claude/ (nested worktree checkouts), node_modules/,
vendor/ and __pycache__/. A fragment added to the tree that touches no
trigger path at all -- no manifest row, no workflow row, no Taskfile row
-- does not run this job (api-contract.yml's path filter cannot name a
directory it does not know); the moment its author touches any trigger
path (the Taskfile api:gen leg, the workflow itself, the manifest), this
gate runs and catches an omission. What this gate deliberately does not
read: Taskfile.yml's api:gen and api:merge legs enumerate the same
universe for the local regeneration command, and keeping them in step
with the manifest stays a code-review concern (the workflow steps'
"the same command as Taskfile's api:gen task" comments) -- this gate
guards the workflow, whose scope the manifest was written for.

The workflow is read by a purpose-written reader for the YAML subset
those files use (block maps, block sequences, plain scalars, `|` literal
blocks, `#` comments). The reader fails loudly (exit 2, infrastructure
error) on any shape it does not understand, so a workflow restructure
that outgrows the reader goes red with a message naming the update,
never silently skips a leg.

Exit codes: 0 = every leg agrees with the manifest; 1 = drift (any
disagreement above); 2 = infrastructure error (manifest or workflow
missing or unreadable, or a workflow shape this reader does not
understand). Companion planted-drift suite: tools/test_check_api_fragments.py.
"""

from __future__ import annotations

import argparse
import json
import os
import sys

WORKFLOW_REL = os.path.join(".github", "workflows", "api-contract.yml")
MANIFEST_REL = os.path.join("tools", "api_fragments.json")
# Files whose change must re-run this job: the manifest this gate reads,
# and the gate itself (plus its suite). The gate checks their presence in
# both trigger path filters so that wiring cannot silently regress.
SELF_ROWS = [
    "tools/api_fragments.json",
    "tools/check_api_fragments.py",
    "tools/test_check_api_fragments.py",
]
OAPI_TOOL = "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
CONFIG_ARGS = "-config oapi-codegen.yaml openapi.yaml"
FRAGMENT_SPEC = "openapi.yaml"
FRAGMENT_CONFIG = "oapi-codegen.yaml"
SKIP_DIRS = {".git", ".claude", "node_modules", "vendor", "__pycache__", ".venv"}


def _infra(message: str) -> None:
    """Report an infrastructure failure and exit 2.

    The exit-code contract (module docstring): 2 = infrastructure error
    (manifest or workflow missing or unreadable, a workflow shape this
    reader does not understand), and only 1 = drift. SystemExit alone
    cannot carry the distinction -- sys.exit("message") exits 1 -- so
    the message is printed to stderr first and the bare code raised.
    """
    print(message, file=sys.stderr)
    raise SystemExit(2)


class _ShapeError(Exception):
    """The workflow uses a YAML shape the subset reader does not cover."""


def _strip_comment(text: str) -> str:
    """Cut a trailing ` # comment` off a plain scalar value."""
    idx = text.find(" #")
    return text[:idx].rstrip() if idx != -1 else text


def _read_yaml(path: str) -> dict:
    """Read the YAML subset this repository's workflow files use.

    Covers exactly: block maps, block sequences (plain-scalar items and
    maps whose first key rides the item line), plain scalars (trailing
    ` # comment` stripped), `|` literal blocks, full-line `#` comments
    and blank lines between structural entries. The literal reader
    consumes raw lines, so a content line inside a `run: |` block is
    never mistaken for a comment. Any other shape raises _ShapeError;
    the caller converts it into the infrastructure-error exit code.
    """
    with open(path, encoding="utf-8") as fh:
        raw = fh.read()
    lines: list[tuple[int, str]] = []
    for raw_line in raw.splitlines():
        if not raw_line.strip():
            lines.append((0, ""))
            continue
        indent = len(raw_line) - len(raw_line.lstrip(" "))
        lines.append((indent, raw_line[indent:]))
    pos = [0]

    def _advance() -> None:
        while pos[0] < len(lines):
            ind, txt = lines[pos[0]]
            if not txt or txt.startswith("#"):
                pos[0] += 1
            else:
                break

    def _look() -> tuple[int, str] | None:
        _advance()
        if pos[0] < len(lines):
            return lines[pos[0]]
        return None

    def _parse_node(indent: int) -> object:
        nxt = _look()
        if nxt is None or nxt[0] != indent:
            raise _ShapeError(
                f"expected a block at indent {indent}, found none"
            )
        if nxt[1].startswith("- "):
            return _parse_seq(indent)
        return _parse_map(indent)

    def _read_literal(parent_indent: int) -> str:
        chunks: list[str] = []
        while pos[0] < len(lines):
            ind, txt = lines[pos[0]]
            if not txt:
                chunks.append("")
                pos[0] += 1
                continue
            if ind > parent_indent:
                chunks.append(txt)
                pos[0] += 1
                continue
            break
        return "\n".join(chunks)

    def _parse_map(indent: int) -> dict:
        out: dict[str, object] = {}
        while True:
            entry = _look()
            if entry is None:
                break
            ind, txt = entry
            if ind < indent:
                break
            if ind > indent:
                raise _ShapeError(
                    f"line deeper than indent {indent} where a map entry "
                    f"was expected"
                )
            if txt.startswith("- "):
                raise _ShapeError(
                    f"sequence item at indent {indent} where a map entry "
                    f"was expected"
                )
            if ":" not in txt:
                raise _ShapeError(f"map entry without ':' at indent {indent}")
            key, _, rest = txt.partition(":")
            key = key.strip()
            rest = rest.strip()
            pos[0] += 1
            if rest == "|":
                out[key] = _read_literal(indent)
            elif rest:
                out[key] = _strip_comment(rest)
            else:
                child = _look()
                if child is not None and child[0] > indent:
                    out[key] = _parse_node(child[0])
                else:
                    out[key] = None
        return out

    def _looks_like_map_start(text: str) -> bool:
        key, _, _rest = text.partition(":")
        return bool(key) and all(
            c.isalnum() or c in "._-" for c in key
        ) and not key[0].isdigit() and text.startswith(key + ":")

    def _parse_seq(indent: int) -> list:
        out: list[object] = []
        while True:
            item = _look()
            if item is None:
                break
            ind, txt = item
            if ind < indent:
                break
            if ind > indent:
                raise _ShapeError(
                    f"line deeper than indent {indent} where a sequence "
                    f"item was expected"
                )
            if not txt.startswith("- "):
                raise _ShapeError(
                    f"map entry at indent {indent} where a sequence item "
                    f"was expected"
                )
            body = txt[2:].strip()
            pos[0] += 1
            if not body:
                child = _look()
                if child is not None and child[0] > indent:
                    out.append(_parse_node(child[0]))
                else:
                    out.append(None)
            elif _looks_like_map_start(body):
                key, _, rest = body.partition(":")
                rest = rest.strip()
                if rest == "|":
                    raise _ShapeError(
                        "inline 'key: |' item shape is not covered"
                    )
                item_map: dict[str, object] = {}
                if rest:
                    item_map[key] = _strip_comment(rest)
                else:
                    child = _look()
                    if child is not None and child[0] > indent:
                        item_map[key] = _parse_node(child[0])
                    else:
                        item_map[key] = None
                nxt = _look()
                if nxt is not None and nxt[0] > indent:
                    tail = _parse_map(nxt[0])
                    item_map.update(tail)
                out.append(item_map)
            else:
                out.append(_strip_comment(body))
        return out

    doc = _parse_node(0)
    if not isinstance(doc, dict):
        raise _ShapeError("top level is not a map")
    return doc


def _read_workflow(root: str) -> dict:
    path = os.path.join(root, WORKFLOW_REL)
    try:
        return _read_yaml(path)
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the workflow this gate guards")
    except _ShapeError as exc:
        _infra(
            f"error: {path} uses a YAML shape this gate does not "
            f"understand ({exc}). Update tools/check_api_fragments.py to "
            f"read the new shape -- the drift gate must fail loudly, "
            f"never silently skip a leg."
        )


def _read_manifest(root: str, problems: list[str]) -> list[dict]:
    path = os.path.join(root, MANIFEST_REL)
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the fragment manifest this gate reads")
    except json.JSONDecodeError as exc:
        _infra(f"error: {path} is not parsable JSON: {exc}")
    if not isinstance(data, dict):
        _infra(f"error: {path} does not hold a JSON object")
    frags = data.get("fragments")
    if not isinstance(frags, list) or not frags:
        problems.append(f"{path}: 'fragments' must be a non-empty list")
        return []
    clean: list[dict] = []
    seen_names: set[str] = set()
    seen_dirs: set[str] = set()
    seen_ranks: dict[int, str] = {}
    for idx, entry in enumerate(frags):
        if not isinstance(entry, dict):
            problems.append(f"{path}: fragments[{idx}] is not an object")
            continue
        name = entry.get("name")
        frag_dir = entry.get("dir")
        if not isinstance(name, str) or not name:
            problems.append(f"{path}: fragments[{idx}] lacks a 'name'")
            continue
        if not isinstance(frag_dir, str) or not frag_dir:
            problems.append(f"{path}: fragment '{name}' lacks a 'dir'")
            continue
        if (
            frag_dir.startswith(("/", "./"))
            or ".." in frag_dir.split("/")
            or not frag_dir.endswith("/api")
        ):
            problems.append(
                f"{path}: fragment '{name}': 'dir' must be a "
                f"repo-relative path ending in '/api', not '{frag_dir}'"
            )
            continue
        if name in seen_names:
            problems.append(f"{path}: duplicate fragment name '{name}'")
            continue
        if frag_dir in seen_dirs:
            problems.append(f"{path}: duplicate fragment dir '{frag_dir}'")
            continue
        rank = entry.get("merge_rank")
        if rank is not None and (
            not isinstance(rank, int) or isinstance(rank, bool) or rank < 1
        ):
            problems.append(
                f"{path}: fragment '{name}': 'merge_rank' must be a "
                f"positive integer when present, not {rank!r}"
            )
            continue
        if rank is not None and rank in seen_ranks:
            problems.append(
                f"{path}: fragment '{name}' and '{seen_ranks[rank]}' "
                f"share merge_rank {rank}"
            )
            continue
        seen_names.add(name)
        seen_dirs.add(frag_dir)
        if rank is not None:
            seen_ranks[rank] = name
        clean.append(
            {
                "name": name,
                "dir": frag_dir,
                "merge_rank": rank,
            }
        )
    return clean


def _scan_fragment_dirs(root: str) -> list[str]:
    """Every api/ directory holding openapi.yaml + oapi-codegen.yaml.

    The on-disk signature of a backend fragment this workflow exists to
    regenerate; the manifest must register exactly this set.
    """
    found: list[str] = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
        if os.path.basename(dirpath) != "api":
            continue
        if FRAGMENT_SPEC in filenames and FRAGMENT_CONFIG in filenames:
            found.append(os.path.relpath(dirpath, root))
    return found


def _path_rows(doc: dict, trigger: str) -> list[str]:
    node = doc.get("on")
    if not isinstance(node, dict):
        _infra(f"error: {WORKFLOW_REL} has no readable 'on' block")
    block = node.get(trigger)
    if not isinstance(block, dict):
        _infra(f"error: {WORKFLOW_REL} has no readable 'on.{trigger}' block")
    rows = block.get("paths")
    if not isinstance(rows, list) or not all(isinstance(r, str) for r in rows):
        _infra(
            f"error: {WORKFLOW_REL} 'on.{trigger}.paths' is not a list of "
            f"strings -- the path-filter leg this gate reads"
        )
    return rows


def _job_steps(doc: dict) -> list[dict]:
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        _infra(f"error: {WORKFLOW_REL} has no readable 'jobs' block")
    job = jobs.get("api-contract")
    if not isinstance(job, dict):
        _infra(f"error: {WORKFLOW_REL} has no readable 'jobs.api-contract' job")
    steps = job.get("steps")
    if not isinstance(steps, list) or not all(isinstance(s, dict) for s in steps):
        _infra(
            f"error: {WORKFLOW_REL} 'jobs.api-contract.steps' is not a list "
            f"of steps -- the step legs this gate reads"
        )
    return steps


def _regen_steps(steps: list[dict]) -> list[tuple[str, str, str]]:
    """(step name, run, regenerated dir or None) for every oapi-codegen step."""
    out: list[tuple[str, str, str | None]] = []
    for step in steps:
        run = step.get("run")
        if not isinstance(run, str) or OAPI_TOOL not in run:
            continue
        name = step.get("name")
        name = name if isinstance(name, str) and name else "(unnamed step)"
        dirs = [
            line.strip()[3:].strip()
            for line in run.splitlines()
            if line.strip().startswith("cd ")
        ]
        out.append((name, run, dirs[0] if len(dirs) == 1 else None))
    return out


def _merge_inputs(steps: list[dict]) -> list[str] | None:
    """The redocly join input paths in input order, or None if no join step."""
    for step in steps:
        run = step.get("run")
        if not isinstance(run, str) or "dlx redocly join" not in run:
            continue
        inputs: list[str] = []
        for line in run.splitlines():
            text = line.strip().rstrip("\\").rstrip()
            if text.startswith("-o "):
                break
            if text.endswith("/" + FRAGMENT_SPEC) or text == FRAGMENT_SPEC:
                inputs.append(text)
        return inputs
    return None


def check(root: str) -> tuple[list[str], int, int]:
    """Check every leg against the manifest; return (problems, fragments, merged)."""
    problems: list[str] = []
    frags = _read_manifest(root, problems)
    manifest_dirs = {f["dir"] for f in frags}

    for frag in frags:
        for needed in (FRAGMENT_SPEC, FRAGMENT_CONFIG):
            path = os.path.join(root, frag["dir"], needed)
            if not os.path.isfile(path):
                problems.append(
                    f"fragment '{frag['name']}' ({frag['dir']}): {needed} "
                    f"is missing -- a fragment registered in "
                    f"{MANIFEST_REL} must carry its spec and its generator "
                    f"config"
                )

    for frag_dir in _scan_fragment_dirs(root):
        if frag_dir not in manifest_dirs:
            problems.append(
                f"{frag_dir}: looks like an api fragment (it holds "
                f"{FRAGMENT_SPEC} and {FRAGMENT_CONFIG}) but is not "
                f"registered in {MANIFEST_REL} -- register it there (and "
                f"wire it into every leg below) or move it if it is not "
                f"one"
            )

    doc = _read_workflow(root)
    path_rows = {
        trigger: _path_rows(doc, trigger) for trigger in ("pull_request", "push")
    }

    for frag in frags:
        row = frag["dir"] + "/**"
        for trigger in ("pull_request", "push"):
            if row not in path_rows[trigger]:
                problems.append(
                    f"fragment '{frag['name']}' ({frag['dir']}): missing "
                    f"from the {trigger} path filter (expected row "
                    f"'{row}')"
                )
    for trigger, rows in path_rows.items():
        for row in rows:
            if row.endswith("/api/**"):
                frag_dir = row[: -len("/**")]
                if frag_dir not in manifest_dirs:
                    problems.append(
                        f"the {trigger} path filter names {row}, but "
                        f"{MANIFEST_REL} registers no fragment at {frag_dir}"
                    )
    for self_row in SELF_ROWS:
        for trigger in ("pull_request", "push"):
            if self_row not in path_rows[trigger]:
                problems.append(
                    f"{self_row} is missing from the {trigger} path "
                    f"filter -- a change to the fragment manifest or to "
                    f"this gate must re-run this job"
                )

    steps = _job_steps(doc)
    regen = _regen_steps(steps)
    if not regen:
        _infra(
            f"error: no oapi-codegen regeneration step found in "
            f"{WORKFLOW_REL} -- the workflow no longer matches the shape "
            f"this gate reads; update tools/check_api_fragments.py"
        )
    regen_dirs: set[str] = set()
    for step_name, run, regen_dir in regen:
        if CONFIG_ARGS not in run:
            problems.append(
                f"regeneration step '{step_name}': run does not pass "
                f"'{CONFIG_ARGS}' -- the invocation shape this gate reads"
            )
        if regen_dir is None:
            problems.append(
                f"regeneration step '{step_name}': no single 'cd <fragment "
                f"dir>' line found in its run -- the shape this gate "
                f"reads is a run that starts with 'cd <dir>' and passes "
                f"'{CONFIG_ARGS}'"
            )
            continue
        regen_dirs.add(regen_dir)
    for frag in frags:
        if frag["dir"] not in regen_dirs:
            problems.append(
                f"fragment '{frag['name']}' ({frag['dir']}): no "
                f"oapi-codegen regeneration step regenerates it"
            )
    for step_name, _run, regen_dir in regen:
        if regen_dir is not None and regen_dir not in manifest_dirs:
            problems.append(
                f"regeneration step '{step_name}' regenerates {regen_dir}, "
                f"but {MANIFEST_REL} registers no fragment there"
            )

    actual = _merge_inputs(steps)
    if actual is None:
        _infra(
            f"error: no redocly 'join' step found in {WORKFLOW_REL} -- the "
            f"workflow no longer matches the shape this gate reads; "
            f"update tools/check_api_fragments.py"
        )
    merged = sorted(
        (f for f in frags if f["merge_rank"] is not None),
        key=lambda f: (f["merge_rank"], f["name"]),
    )
    expected = [f"{f['dir']}/{FRAGMENT_SPEC}" for f in merged]
    if actual != expected:
        problems.append(
            f"the redocly join step's input list does not match the "
            f"manifest's merged fragments: expected "
            f"[{', '.join(expected)}] (manifest merge_rank order) but "
            f"got [{', '.join(actual)}]"
        )

    return problems, len(frags), len(merged)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Fragment-enumeration drift gate: .github/workflows/"
            "api-contract.yml names the backend-fragment universe in its "
            "path filters, its oapi-codegen regeneration steps and its "
            "redocly join input list; tools/api_fragments.json is the "
            "single source of truth, and this gate fails when the tree, "
            "the manifest and any of those legs disagree. Exit 0 clean, "
            "1 drift, 2 infrastructure error."
        )
    )
    parser.add_argument(
        "--root",
        default=".",
        help="repository root (default: the current directory)",
    )
    args = parser.parse_args(argv)
    problems, n_frags, n_merged = check(args.root)
    if problems:
        for problem in problems:
            print(f"api-fragments: drift    {problem}")
        print(
            "api-fragments: fix tools/api_fragments.json and the "
            f"{WORKFLOW_REL} legs together -- see the module docstring of "
            "tools/check_api_fragments.py for the checked invariants"
        )
        return 1
    print(
        "api-fragments: ok    "
        f"{n_frags} fragments in {MANIFEST_REL} match the tree, the "
        f"trigger path filters, the {n_frags} regeneration steps and the "
        f"redocly join (merged: {n_merged})"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
