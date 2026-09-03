# tools/release — the lockstep release machinery (M0)

`docs/internal/02-repo-and-release.md` and `docs/internal/18-cicd.md`
define the lockstep release: every Go module and every npm package shares
ONE version number and releases together, and a single command must be
able to publish all of them at that version (the M0 exit condition,
`docs/internal/15-roadmap.md`). This directory is the M0 deliverable for
that command, and it is deliberately an OFFLINE, verification-only round:

- `lockstep-release.py` — the coordinator. Derives the publishable set at
  runtime (go.work use entries under `go/` for the Go half;
  `web/packages/*` for the npm half), prints the full one-version plan in
  its default mode, and exits 0 only when the plan is consistent. Real
  publishing is scheduled for the v1.0 release at M4; no mode of the
  coordinator (or of `release.yml`) can publish for real.
- `test_lockstep_release.py` — the coordinator's unittest suite, run by
  `--self-test`, by `python3 -m unittest discover -s tools/release`, or
  directly. Its sandbox proof builds a scratch git repository from the
  live tree's module metadata and asserts that the gated apply mode
  creates exactly one version's tag set there and nothing anywhere else.
- `testdata/` — go.mod fixtures for the first-release replace-cleanup
  engine. The engine is a pure function exercised ONLY against these
  fixtures; no operational mode of the coordinator calls it.

The npm half of the lockstep configuration lives in `web/.changeset/`
(its fixed version group), not in this directory; the coordinator
verifies that the fixed group covers exactly the packages that exist.

## How to run

```
python3 tools/release/lockstep-release.py v1.2.0    # offline verification plan
python3 tools/release/lockstep-release.py --self-test
```

The root `Taskfile.yml`'s `release:plan` task wraps the first form;
`.github/workflows/release.yml` runs both forms on its version input.

## Absolute prohibitions — every mode, including autonomous agents

- **Never run the replace-cleanup engine against a live go.mod.** The
  engine (`first_release_replace_cleanup`) is exercised only against the
  fixtures under `testdata/`. The live tree keeps its pre-release
  transition state (sibling `replace` lines, `0.0.0` and zero
  pseudo-versions) until the first real release at M4, which is when the
  cleanup runs for real.
- **Never edit a module go.mod replace line or a web package.json
  version field.** The tree is deliberately transitional until M4; a
  premature cleanup or bump breaks `go mod tidy` consumers or publishes
  nothing while pretending otherwise.
- **Never pass `--apply` without `--allow-local-tag-creation`, and never
  use the hatched mode against anything but a scratch checkout.** The
  tags it creates are local and never pushed; pushing is M4's job.
- **Never hand-maintain a module or package list.** The go.work use
  block is the Go source of truth and `web/packages/*` plus
  `web/.changeset/config.json` the npm one; lists derived anywhere else
  drift silently.
- **Never weaken a gate to make a plan pass.** A failing preflight means
  the tree is not release-consistent; fix the tree (register the module,
  extend the fixed group, pick a new version), not the check.

## When something changes in the tree

- **A Go module joins:** its go.work `use` entry is its release
  registration — the coordinator's drift check fails loudly until the
  entry lands. No tag list to update anywhere.
- **An npm package joins or leaves:** update the fixed group in
  `web/.changeset/config.json` in the same change — the coordinator's
  coverage check fails loudly otherwise.
- **The module count changes:** nothing in this directory hard-codes it;
  the closing aggregate line of the plan ("21 Go modules + 3 packages ->
  v0.3.0") derives from the runtime discovery.
- **A release-version rule changes:** the pattern
  `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$` lives in two places that
  must stay in step: `VERSION_PATTERN` in `lockstep-release.py` and the
  version-input validation step of `.github/workflows/release.yml`.

## Adding a self-test

Fixture-based engine tests: add a directory under `testdata/` with a
`go.mod`, then a `CleanupEngineTest` case reading it through
`_read_fixture`. New behaviors get cases in the existing test classes;
keep every assertion offline (temp directories and scratch git
repositories only — a self-test that touches the live tree's tags,
config or version fields is a bug).
