#!/usr/bin/env python3
"""Platform error-copy bundle generator.

The frontend resolves an API failure to text by looking the apperr code
up in its i18n catalog (`t('errors.' + code)`); the framework packages
each hand-maintain the codes they know, while the backend modules already
publish exactly that text in their own go-i18n catalogs
(`<module>/locales/{zh-CN,en-US}.toml`, whose TOML key IS the apperr
code). This tool mechanically derives one platform-wide client bundle
from those catalogs, so the words a user sees for a backend refusal come
from the same source the backend itself renders.

What the bundle is: the intersection of two sets, computed structurally,
with no name list anywhere --

  - the *census*: every apperr code constructed in Go source under
    --roots, read through tools/gen_error_code_index.py's own public
    functions (imported rather than re-implemented, and never read back
    from that tool's generated artifacts, which could be stale); and
  - the *catalog ids*: every message id the modules' en-US.toml files
    declare.

An id the catalog carries but the census does not is backend-only
*content* -- invitation emails, notification templates, SMS bodies,
default workspace names, demo copy -- and stays out by construction:
there is no code for a client to look it up with, so the frontend must
never be handed it. A census code with no catalog id stays out the same
way: the backend never declares client copy for it (a boot-time wiring
refusal, or a request-time refusal rendered from the client's own
fallback), and this generator does not invent one.

Value conversion, the whole of it:

  - go-i18n's `{{.name}}` placeholder normalizes to i18next's
    `{{name}}`; any other `{{...}}` shape (a template action, a field
    chain, an unterminated group) refuses the run with exit 2, naming the
    id and the shape -- a form this tool cannot translate mechanically is
    a decision for a human, never a rendered guess;
  - a plural message table (`key = { one = "...", other = "..." }`, or
    its `["id"]` section spelling) becomes `key_<category>` leaves,
    the i18next suffixed-leaf convention @speed/i18n's registerNamespace
    resolves counts with. The forms of every language are unioned and
    each language's leaves padded from its own `other` form (the CLDR
    catch-all), because bundles must carry identical leaf sets and a
    count whose form is absent would render the raw key. Reserved
    metadata keys (description, id, hash, leftdelim, rightdelim) are
    dropped: they document a message, they are not copy.
  - an empty translation refuses the run: @speed/i18n's registration
    refuses an "" leaf (it renders as silence and never fires the
    missing-key discipline), so the bundle must never carry one.

Output: one JSON bundle per language under --out-dir, shaped as
`{"errors": {"<module>.<code>": "<text>", ...}}` -- the `errors` section
the packages' resolvers already navigate, with each code as a flat
dotted key, which is the leaf path i18next's own deepFind resolves
`t('errors.' + code)` to. Keys are sorted and the bytes are
deterministic, so the committed artifacts are reproducible and --check
compares them against a fresh render.

Usage:
    python3 tools/gen_platform_error_bundle.py [--roots go examples] [--out-dir web/packages/i18n/src/platform-errors/locales] [--check]

--check exits nonzero (printing which files are stale) instead of
writing, for the CI wiring in docs-check.yml. Exit 2 is a refused run
(an unsupported interpolation shape, a catalog id the zh-CN side lacks, a
duplicate id across catalogs, an unreadable catalog), never a rendered
guess.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys
import tomllib

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import gen_error_code_index as census  # noqa: E402

# The bundle's language pair: the repository's pair discipline
# (tools/check_i18n_keys.py's TOML_PAIR), every locale directory ships
# exactly these two files.
LANGUAGES = ("zh-CN", "en-US")

DEFAULT_OUT_DIR = "web/packages/i18n/src/platform-errors/locales"

# The CLDR cardinal categories, in resolution order -- the same order
# @speed/i18n's registerNamespace validates families in, and the order
# the unioned form set below is emitted in.
PLURAL_CATEGORIES = ("zero", "one", "two", "few", "many", "other")

# The keys go-i18n reserves on a plural message table (described
# case-insensitively, mirroring tools/check_i18n_keys.py's own reserved
# set): the category keys are the forms, "translation" is the v1 synonym
# for "other", and the rest is message metadata that documents a message
# rather than rendering.
RESERVED_METADATA_KEYS = frozenset(
    {"description", "id", "hash", "leftdelim", "rightdelim"}
)

# The one placeholder shape this tool translates: a simple field
# reference ("{{.max_length}}"). Anything else -- a template action, a
# chained field, an unterminated group -- refuses the run.
_INTERPOLATION_INNER_RE = re.compile(r"\.[A-Za-z_]\w*\Z")


class BundleRefused(Exception):
    """Raised when the tree holds a shape this generator must not
    translate mechanically. main prints the message (which names the id
    or file at fault) and exits 2 -- a refused run, never a rendered
    guess."""


def collect_catalog_messages(
    roots: list[pathlib.Path], language: str
) -> tuple[dict[str, object], dict[str, str]]:
    """Every catalog id of one language, with the file each id came from.

    Walks every locales/<language>.toml under the roots and returns
    id -> raw value plus id -> defining file (for refusal messages). A
    catalog file that cannot be parsed, or an id two catalogs both
    declare, refuses the run: the merged client bundle must be one
    unambiguous lookup per code.
    """
    messages: dict[str, object] = {}
    sources: dict[str, str] = {}
    for root in roots:
        for path in sorted(root.rglob(f"locales/{language}.toml")):
            try:
                with path.open("rb") as f:
                    data = tomllib.load(f)
            except (tomllib.TOMLDecodeError, OSError) as exc:
                raise BundleRefused(
                    f"{path}: cannot read the {language} catalog: {exc}"
                ) from exc
            for key, value in data.items():
                if key in messages:
                    raise BundleRefused(
                        f"{path}: the catalog id {key!r} is already declared "
                        f"by {sources[key]}; a code resolves to one message"
                    )
                messages[key] = value
                sources[key] = str(path)
    return messages, sources


def normalize_interpolation(text: str, site: str) -> str:
    """go-i18n's `{{.name}}` placeholder as the i18next `{{name}}` the
    frontend renders, with every other `{{...}}` shape refused (exit 2).

    The scan is deliberately strict about the whole string, not only
    about well-formed groups: an unterminated `{{` or a lone `}}` is a
    malformed template that would render literally in one engine and not
    the other, so it refuses the run rather than shipping.
    """
    out: list[str] = []
    index = 0
    while True:
        start = text.find("{{", index)
        if start == -1:
            tail = text[index:]
            if "}}" in tail:
                raise BundleRefused(
                    f"{site}: the closing braces at {text!r} have no opening "
                    f"'{{{{' -- fix the placeholder in the catalog"
                )
            out.append(tail)
            return "".join(out)
        out.append(text[index:start])
        end = text.find("}}", start + 2)
        if end == -1:
            raise BundleRefused(
                f"{site}: the '{{{{' at {text!r} is never closed"
            )
        inner = text[start + 2 : end]
        if not _INTERPOLATION_INNER_RE.match(inner):
            raise BundleRefused(
                f"{site}: the placeholder {{{{...}}}} shape {text[start : end + 2]!r} "
                "is not translatable mechanically; only a simple field "
                "reference ({{.name}}) normalizes to i18next's {{name}}"
            )
        out.append("{{" + inner[1:] + "}}")
        index = end + 2


def plural_forms(value: object, site: str) -> dict[str, str] | None:
    """The CLDR category -> text forms of one catalog value, or None when
    the value is a plain single-form string.

    A table whose keys all belong to the reserved set is one plural
    message (tools/check_i18n_keys.py's own reading of the shape); its
    category keys are the forms, "translation" is normalized to "other",
    and the metadata keys are dropped. A table with any other key, a
    non-string form, a table that declares no form at all, and two keys
    naming the same category all refuse the run.
    """
    if isinstance(value, str):
        return None
    if not isinstance(value, dict):
        raise BundleRefused(
            f"{site}: the value is {type(value).__name__}, not a message string "
            "or a plural message table"
        )
    forms: dict[str, str] = {}
    for key, text in value.items():
        lowered = key.lower()
        if lowered == "translation":
            lowered = "other"
        if lowered in RESERVED_METADATA_KEYS:
            continue
        if lowered not in PLURAL_CATEGORIES:
            raise BundleRefused(
                f"{site}: the table key {key!r} is neither a CLDR plural "
                "category nor reserved message metadata; a grouping table "
                "under a message id is not a shape this bundle translates"
            )
        if lowered in forms:
            raise BundleRefused(
                f"{site}: the table declares the {lowered!r} form twice"
            )
        if not isinstance(text, str):
            raise BundleRefused(
                f"{site}: the {lowered!r} plural form is {type(text).__name__}, "
                "not a string"
            )
        if text == "":
            raise BundleRefused(
                f"{site}: the {lowered!r} plural form is empty; an empty leaf "
                "renders as silence and @speed/i18n's registration refuses it"
            )
        forms[lowered] = normalize_interpolation(text, f"{site} ({lowered})")
    if not forms:
        raise BundleRefused(
            f"{site}: the plural table carries no CLDR category form"
        )
    return forms


def render_code_leaves(
    code: str,
    raw_by_language: dict[str, object],
    sources: dict[str, str],
) -> dict[str, dict[str, str]]:
    """The leaves one bundle code contributes, per language:
    {language: {leaf name: text}}.

    A single-form message contributes its own key in every language; a
    plural table contributes `key_<category>` leaves, with each
    language's form set padded up to the union of all languages' forms
    (from its own `other`, the CLDR catch-all) -- bundles carry identical
    leaf sets, so a form one language's counts can select must ship in
    every language's bundle even where that language never selects it.
    One language declaring a string where another declares a table
    refuses the run: the leaf sets could never agree.
    """
    singles: dict[str, str] = {}
    plurals: dict[str, dict[str, str]] = {}
    for language in LANGUAGES:
        site = sources[language]
        value = raw_by_language[language]
        forms = plural_forms(value, site)
        if forms is None:
            text = value
            if text == "":
                raise BundleRefused(
                    f"{site}: the translation for {code!r} is empty; an empty "
                    "leaf renders as silence and @speed/i18n's registration "
                    "refuses it"
                )
            singles[language] = normalize_interpolation(text, site)
        else:
            plurals[language] = forms
    if singles and plurals:
        declared = {
            language: ("table" if language in plurals else "string")
            for language in LANGUAGES
        }
        raise BundleRefused(
            f"{code!r}: the languages disagree on the message shape "
            f"({', '.join(f'{language} {kind}' for language, kind in declared.items())}); "
            "a plural table in one language and a plain string in the other "
            "can never register as one leaf set"
        )
    if singles:
        return {
            language: {code: text} for language, text in singles.items()
        }
    union = [
        category
        for category in PLURAL_CATEGORIES
        if any(category in plurals[language] for language in LANGUAGES)
    ]
    leaves: dict[str, dict[str, str]] = {}
    for language in LANGUAGES:
        forms = plurals[language]
        fallback = forms.get("other")
        if fallback is None:
            fallback = next(
                forms[category] for category in union if category in forms
            )
        leaves[language] = {
            f"{code}_{category}": forms.get(category, fallback)
            for category in union
        }
    return leaves


def build_bundle(
    roots: list[pathlib.Path], repo_root: pathlib.Path
) -> tuple[dict[str, dict[str, str]], dict[str, int]]:
    """The per-language bundle documents and the run's entry counts.

    The census runs over the same roots the error-code index generator
    defaults to, so the app modules under examples/ contribute their codes
    exactly as they do to the index. Returns {language: bundle} where a
    bundle is the `errors` section's key -> text map, plus the counts
    (bundle size, content ids excluded from it, census codes no catalog
    declares).
    """
    helpers = census.discover_apperr_helpers(roots, repo_root)
    entries = census.collect_entries(roots, repo_root, helpers)
    codes = {entry.code for entry in census.winning_rows(entries)}
    if not codes:
        raise BundleRefused(
            "the census is empty: --roots must name the trees that construct "
            "apperr codes (the default is go and examples)"
        )

    catalogs: dict[str, dict[str, object]] = {}
    sources: dict[str, dict[str, str]] = {}
    for language in LANGUAGES:
        catalogs[language], sources[language] = collect_catalog_messages(
            roots, language
        )

    in_bundle = sorted(code for code in codes if code in catalogs["en-US"])
    excluded_content = sorted(set(catalogs["en-US"]) - codes)
    census_no_catalog = sorted(codes - set(catalogs["en-US"]))

    missing_zh = [code for code in in_bundle if code not in catalogs["zh-CN"]]
    if missing_zh:
        raise BundleRefused(
            "the zh-CN catalogs lack ids the en-US catalogs declare, so the "
            "bundle's languages would not carry the same key set: "
            + ", ".join(
                f"{code} ({sources['en-US'][code]})" for code in missing_zh[:10]
            )
            + (" and more" if len(missing_zh) > 10 else "")
        )

    bundles: dict[str, dict[str, str]] = {language: {} for language in LANGUAGES}
    for code in in_bundle:
        leaves_by_language = render_code_leaves(
            code,
            {language: catalogs[language][code] for language in LANGUAGES},
            {language: sources[language][code] for language in LANGUAGES},
        )
        for language in LANGUAGES:
            for leaf, text in leaves_by_language[language].items():
                if leaf in bundles[language]:
                    raise BundleRefused(
                        f"{language}: the leaf {leaf!r} is declared twice -- a "
                        "plural key's suffixed form collides with another id"
                    )
                bundles[language][leaf] = text

    counts = {
        "in_bundle": len(in_bundle),
        "excluded_content": len(excluded_content),
        "census_no_catalog": len(census_no_catalog),
    }
    return bundles, counts


def render_bundle_json(leaves: dict[str, str]) -> str:
    """The committed bytes of one language bundle: the `errors` section
    with each code as a flat dotted key, sorted, deterministically
    indented, non-ASCII text literal (the resource is user-facing copy,
    not an escape stream)."""
    return (
        json.dumps(
            {"errors": dict(sorted(leaves.items()))},
            indent=2,
            ensure_ascii=False,
        )
        + "\n"
    )


def stale_outputs(
    out_dir: pathlib.Path, rendered: dict[str, str]
) -> list[str]:
    """The bundle files that are absent or differ from a fresh render --
    the --check drift condition, shared with the unit suite so the gate
    is tested rather than assumed."""
    stale: list[str] = []
    for language in LANGUAGES:
        path = out_dir / f"{language}.json"
        current = path.read_text(encoding="utf-8") if path.exists() else ""
        if current != rendered[language]:
            stale.append(str(path))
    return stale


def main() -> int:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument(
        "--roots",
        nargs="+",
        default=["go", "examples"],
        help="Directories to scan for Go sources (the census) and locales/ catalogs (relative to the repo root).",
    )
    parser.add_argument(
        "--out-dir",
        default=DEFAULT_OUT_DIR,
        help="Output directory for the per-language bundle files (relative to the repo root).",
    )
    parser.add_argument(
        "--check",
        action="store_true",
        help="Exit nonzero if the outputs are not already up to date, instead of writing them.",
    )
    args = parser.parse_args()

    repo_root = pathlib.Path(__file__).resolve().parent.parent
    roots = [repo_root / r for r in args.roots]
    out_dir = repo_root / args.out_dir

    try:
        bundles, counts = build_bundle(roots, repo_root)
    except BundleRefused as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    rendered = {language: render_bundle_json(bundles[language]) for language in LANGUAGES}

    if args.check:
        stale = [
            str(pathlib.Path(p).relative_to(repo_root))
            for p in stale_outputs(out_dir, rendered)
        ]
        if stale:
            print(
                f"{', '.join(stale)} out of date; run: python3 tools/gen_platform_error_bundle.py",
                file=sys.stderr,
            )
            return 1
        print(
            f"platform error bundles are up to date ({counts['in_bundle']} in bundle, "
            f"{counts['excluded_content']} content id(s) excluded, "
            f"{counts['census_no_catalog']} census code(s) without catalog copy)."
        )
        return 0

    out_dir.mkdir(parents=True, exist_ok=True)
    for language in LANGUAGES:
        (out_dir / f"{language}.json").write_text(rendered[language], encoding="utf-8")
    print(
        f"wrote {args.out_dir}/{{{','.join(LANGUAGES)}}}.json "
        f"({counts['in_bundle']} in bundle, {counts['excluded_content']} content "
        f"id(s) excluded, {counts['census_no_catalog']} census code(s) without "
        "catalog copy)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
