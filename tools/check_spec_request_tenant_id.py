#!/usr/bin/env python3
"""API-spec checker: no request may carry a caller-supplied tenant_id.

docs/internal/18-cicd.md's architecture-discipline table row "the API
layer must not accept an externally supplied tenant_id" (root
CLAUDE.md's multi-tenant isolation rule: "Do not accept a caller-
supplied tenant_id at the API layer. The tenant comes from the access
token claims, never from request parameters, headers or bodies") is
enforced here over the OpenAPI fragments this repository ships: every
spec file under go/*/api/openapi.yaml and
examples/reference-app/internal/*/api/openapi.yaml is scanned, and a
request that can carry a tenant_id -- a requestBody schema property
named tenant_id, or a parameter named tenant_id -- is a finding.

The one sanctioned exception is go/authn's own pre-auth surface, and it
is sanctioned in the spec itself: authn's header records that "where a
request NAMES a tenant at all (login, phone-SMS login, social callback,
tenant switch) it is a REQUEST for that tenant's first access token,
never a grant" -- resolveTenant re-verifies membership on every call,
the field is always optional, and no access token exists yet to carry
the tenant claim on those operations. The exception is encoded as the
ALLOWED_TENANT_NAMING_SCHEMAS allowlist below (authn's four request
schemas, by name), so a FUTURE operation that adds a tenant_id request
property -- in authn or anywhere else -- still fails unless it reuses
one of those exact names, and a request schema named like one of them
in any other module's spec is treated as a finding anyway when its
module is not authn's (see module_allowlist below).

What the scanner understands (documented because it is a structural
line scanner over hand-written YAML, not a YAML parser -- the same
textual-scanner convention check_repo_isolation.py documents for Go):

  * An operation is a method key (get/post/put/patch/delete) with its
    path key. Its requestBody and parameters regions are its children
    by indentation.
  * A requestBody that references '#/components/schemas/NAME' pulls
    that schema into the request-referenced set; references INSIDE a
    request-referenced schema's own block are followed transitively,
    so a nested object schema that carries tenant_id is caught through
    the chain.
  * A property declaration is a line whose whole content is
    "tenant_id:"; prose (descriptions saying "there is no tenant_id
    field anywhere", example values) is not a property line and is
    ignored. A requestBody with an inline schema (no $ref) is covered
    by the same line rule inside the requestBody region.
  * A parameter named tenant_id inside a parameters region is a
    finding; shared component parameters ($ref inside parameters) are
    not resolved -- no fragment ships any today, and the residual is
    recorded in this header rather than silently scanned.
  * Response schemas are not requests: a schema referenced only from
    responses may legitimately carry tenant_id (authn's
    AuthnPrincipal), and does not fire.

Usage:
    python3 tools/check_spec_request_tenant_id.py [--root DIR]

Exit codes: 0 = clean; 1 = at least one finding; 2 = usage or walk
error. Standard library only, Python >= 3.11.
"""

from __future__ import annotations

import argparse
import pathlib
import re
import sys

# The sanctioned tenant-naming request schemas (go/authn's pre-auth
# surface only) -- see the module docstring and go/authn/api/
# openapi.yaml's own header for the "request, never a grant" reasoning.
ALLOWED_TENANT_NAMING_SCHEMAS = {
    "AuthnLoginWithPasswordRequest",
    "AuthnLoginWithSMSCodeRequest",
    "AuthnSocialCallbackRequest",
    "AuthnSwitchTenantRequest",
}

# A tenant-naming schema is sanctioned only inside authn's own spec; the
# same name in another module's spec is a finding (module path
# suffix, compared against the file's path).
AUTHN_SPEC_SUFFIX = "go/authn/api/openapi.yaml"

METHODS = {"get", "post", "put", "patch", "delete", "options", "head"}

SCHEMA_REF = re.compile(
    r"\$ref:\s*['\"]?#/components/schemas/([A-Za-z0-9_]+)['\"]?"
)
# Parameter list items are "- name: X" lines.
PARAM_NAME = re.compile(r"^(?:-\s*)?name:\s*(\S+)")


def parse_spec(text: str, spec_path: str) -> list[tuple[int, str]]:
    """Scan one spec's text for request-context tenant_id declarations.
    Returns a list of (line_number, message) findings. Line numbers are
    1-based, matching editor navigation."""
    lines = text.splitlines()
    findings: list[tuple[int, str]] = []
    # Indent of a line, or None for blank/comment-only lines.
    indents: list[int | None] = []
    for raw in lines:
        stripped = raw.lstrip(" ")
        if not stripped or stripped.startswith("#"):
            indents.append(None)
        else:
            indents.append(len(raw) - len(stripped))

    # Operation segments: (start_line, end_line) of each op block.
    # A method key sits at indent 4 under a path key at indent 2.
    op_segments: list[tuple[int, int]] = []
    path_depth = 2
    method_depth = 4
    start = None
    for i, raw in enumerate(lines):
        stripped = raw.strip()
        indent = indents[i]
        if indent is None:
            continue
        is_method = (
            indent == method_depth
            and raw[:method_depth].strip() == ""
            and stripped.rstrip(":").lower() in METHODS
        )
        if is_method:
            if start is not None:
                op_segments.append((start, i))
            start = i
        elif start is not None and indent <= method_depth:
            op_segments.append((start, i))
            start = None
        # A path key at indent 2 closes an op that follows it.
        if indent == path_depth and stripped.startswith("/") and start is None:
            pass
    if start is not None:
        op_segments.append((start, len(lines)))

    def region_lines(
        seg_start: int, seg_end: int, key: str
    ) -> list[tuple[int, str, int]]:
        """(line_no, stripped_text, indent) of the region under an
        op-child key named `key` inside an op segment."""
        for i in range(seg_start, seg_end):
            indent = indents[i]
            if indent is None or indent != method_depth + 2:
                continue
            stripped = lines[i].strip()
            if stripped == key + ":":
                region: list[tuple[int, str, int]] = []
                for j in range(i + 1, seg_end):
                    ind = indents[j]
                    if ind is None:
                        continue
                    if ind <= method_depth + 2:
                        break
                    region.append((j, lines[j].strip(), ind))
                return region
        return []

    # schemas: name -> block [(line_no, text)] collected under the
    # components/schemas tree. Schema names sit at indent 4; the
    # section ends at the next indent-2 key or EOF.
    schema_blocks: dict[str, list[tuple[int, str]]] = {}
    in_schemas = False
    current_schema: str | None = None
    for i, raw in enumerate(lines):
        indent = indents[i]
        if indent is None:
            continue
        stripped = raw.strip()
        if indent == 2 and stripped == "schemas:":
            in_schemas = True
            current_schema = None
            continue
        if not in_schemas:
            continue
        if indent <= 2:
            break
        if indent == 4:
            name = stripped.rstrip(":")
            if name and not name.startswith("-"):
                current_schema = name
                schema_blocks.setdefault(name, [])
                continue
        if indent > 4 and current_schema is not None:
            schema_blocks[current_schema].append((i, stripped))

    # Which schemas do operations pull into their requests?
    request_schemas: set[str] = set()
    op_owner: dict[str, str] = {}  # schema name -> operation id, first seen
    for (seg_start, seg_end) in op_segments:
        op_id = None
        for i in range(seg_start, seg_end):
            indent = indents[i]
            if indent is None:
                continue
            stripped = lines[i].strip()
            if indent == method_depth + 2 and stripped.startswith(
                "operationId:"
            ):
                op_id = stripped.split(":", 1)[1].strip()
                break
        for (line_no, stripped, _ind) in region_lines(
            seg_start, seg_end, "requestBody"
        ):
            for match in SCHEMA_REF.finditer(stripped):
                name = match.group(1)
                request_schemas.add(name)
                op_owner.setdefault(name, op_id or "<unknown operation>")
        for (line_no, stripped, _ind) in region_lines(
            seg_start, seg_end, "parameters"
        ):
            match = PARAM_NAME.match(stripped)
            if match and match.group(1) == "tenant_id":
                findings.append(
                    (
                        line_no + 1,
                        "%s: parameter named tenant_id on a request "
                        "surface" % (op_id or "<unknown operation>"),
                    )
                )

    # Transitive closure over schema-internal refs, so a request schema
    # whose nested object schema carries tenant_id is caught.
    frontier = list(request_schemas)
    seen: set[str] = set()
    while frontier:
        name = frontier.pop()
        if name in seen:
            continue
        seen.add(name)
        for (_line_no, stripped) in schema_blocks.get(name, []):
            for match in SCHEMA_REF.finditer(stripped):
                if match.group(1) not in seen:
                    frontier.append(match.group(1))

    # Property declarations inside request-referenced schemas.
    for name in sorted(seen):
        allowed = name in ALLOWED_TENANT_NAMING_SCHEMAS
        if allowed and not spec_path.endswith(AUTHN_SPEC_SUFFIX):
            allowed = False
        for (line_no, stripped) in schema_blocks.get(name, []):
            if stripped == "tenant_id:":
                if allowed:
                    continue
                findings.append(
                    (
                        line_no + 1,
                        "schema %s (request body of %s) declares a "
                        "tenant_id property; the tenant comes from the "
                        "access token claims, never from a request body%s"
                        % (
                            name,
                            op_owner.get(name, "<unknown operation>"),
                            "" if name not in ALLOWED_TENANT_NAMING_SCHEMAS
                            else " -- this name is sanctioned only inside "
                            "authn's own spec",
                        ),
                    )
                )

    # Inline requestBody schemas: a tenant_id property line inside the
    # requestBody region itself.
    for (seg_start, seg_end) in op_segments:
        for (line_no, stripped, _ind) in region_lines(
            seg_start, seg_end, "requestBody"
        ):
            if stripped == "tenant_id:":
                findings.append(
                    (
                        line_no + 1,
                        "inline requestBody schema declares a tenant_id "
                        "property",
                    )
                )

    findings.sort(key=lambda item: item[0])
    return findings


def spec_files(root: pathlib.Path) -> list[pathlib.Path]:
    """Every backend OpenAPI fragment: module specs under go/*/api/ and
    the reference app's own fragment directories (internal/*/api). The
    layout is the api:gen convention -- one openapi.yaml per fragment
    directory -- so the walk derives from the tree, never a manifest."""
    files: list[pathlib.Path] = []
    for candidate in root.glob("go/*/api/openapi.yaml"):
        files.append(candidate)
    for candidate in root.glob(
        "examples/reference-app/internal/*/api/openapi.yaml"
    ):
        files.append(candidate)
    return sorted(files)


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--root",
        default=None,
        help="repository root (default: the tools/ directory's parent)",
    )
    args = parser.parse_args(argv)
    root = (
        pathlib.Path(args.root).resolve()
        if args.root
        else pathlib.Path(__file__).resolve().parent.parent
    )
    total_findings = 0
    for spec in spec_files(root):
        try:
            text = spec.read_text(encoding="utf-8")
        except OSError as err:
            print("check_spec_request_tenant_id: %s" % err, file=sys.stderr)
            return 2
        findings = parse_spec(text, str(spec))
        for (line_no, message) in findings:
            print(
                "%s:%d: %s"
                % (spec.relative_to(root), line_no, message)
            )
        total_findings += len(findings)
    if total_findings:
        print(
            "check_spec_request_tenant_id: %d finding(s) across %d spec "
            "file(s)" % (total_findings, len(spec_files(root))),
            file=sys.stderr,
        )
        return 1
    print(
        "check_spec_request_tenant_id: %d spec file(s) clean"
        % len(spec_files(root))
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
