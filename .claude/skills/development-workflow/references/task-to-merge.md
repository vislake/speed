# Task to Merge

How one change travels, from a branch to a merged commit. **This is the universal
standard: it applies to every role, every session and every task.** Nothing in it
is conditional on who is doing the work or on how the work is organized.

The entry point ([SKILL.md](../SKILL.md)) carries the premise, the configuration
and the role split.

---

## 1. Branch Discipline

- **DO NOT develop on the default branch.** Branch first, always — including for
  a one-line fix. A change made directly on the default branch has no place to
  fail safely and cannot be abandoned.
  - **The exception is the user asking for it.** Where to work is theirs to
    decide; once they have said so, it is simply the instruction, and raising the
    objection again on the next change is noise, not diligence.
  - **Honour the scope they gave it, and never widen it.** A request aimed at one
    change covers that change; a statement that this repository is worked
    directly on its default branch covers the repository.
  - **The exception suspends the branch and nothing else.** Verification, the
    bug-fix test rule and the warning rule all still hold, and they carry more
    weight here: there is no branch to throw away, so whatever goes wrong has to
    be fixed forward.
- **One branch carries one logical task.** Unrelated work found along the way
  becomes its own task, not an extra commit on this branch.
- **Branch from the current tip of the target branch**, not from an old local
  state. Fetch first.
- **Land the branch quickly.** A branch that cannot land within a few days is
  too large: split the task. Long-lived branches accumulate conflicts faster than
  they accumulate value. Where a project fixes a limit, that limit governs.
- **Use an isolated working tree for work that must not disturb the main
  checkout** — parallel tasks, long builds, anything a second session might touch.
  Create it wherever the project and the tooling put working trees; that location
  is theirs to decide, not this standard's.
- **DO NOT touch another session's branch or working tree.** Coordinate through
  the task, never by editing someone else's tree.

## 2. Before Changing Anything

- **Read the current state before proposing the change.** The code is the
  authority on what exists; a document, a comment or a memory may be stale.
- **State the assumption you are acting on** when the task admits more than one
  reading, and keep going. Stop and ask only when proceeding either way would be
  unsafe or would waste the work if the reading is wrong.

**Splitting, moving or renaming demands a reference sweep.** Moving a file,
splitting a package, renaming a symbol or changing a directory breaks a class of
references that no compiler and no import graph will catch, because they are not
expressed as imports. They surface one CI run at a time, over days.

Run **two sweeps** over the whole repository — one for the old path, one for the
old symbol — and check at least these surfaces:

- **CI configuration**: trigger path filters *and* the step bodies that name
  scripts, arguments or members.
- **Lint and rule configuration**: allowlists, denylists, `exclude` globs, import
  restrictions. A stale entry becomes a dead exemption; a missing one turns a
  legitimate site red.
- **Manifests and generated indexes**: any file listing paths, plus every
  generated artifact that embeds a path. Regenerate those and re-check; **never
  hand-edit a generated file.**
- **Container and packaging builds**: `COPY` lists, ignore files, include globs.
- **Test fixtures** belonging to rules and checkers — a fixture that ages with
  the rule makes the rule silently stop matching real code.
- **Prose**: documentation, module guides, READMEs, skill files that cite the old
  path.

**Acceptance is per hit, not per sweep.** List every match and its disposition —
updated, or verified unaffected — and check the reverse direction too: the new
path and the new symbol must appear everywhere they now belong. "Sweep done" with
no itemized list does not count.

## 3. Implementation

- **Contract before implementation.** Where a change crosses a generated or
  declared contract — an API spec, a schema, an interface others implement —
  change the contract first, regenerate, and let the compiler enumerate what to
  fix. Writing the implementation first and back-filling the contract discards
  the only mechanical guarantee the contract offers.
- **Stage the work: finish the coding across all stages, then verify once.**
  Running the full suite after every small edit costs far more time than it
  returns. Write the code for every stage, then run verification once over the
  whole result, then merge. This does not license skipping verification — it
  moves it, and section 5 still has to pass.
- **DO NOT widen the task.** Deliver what was asked. A real problem found outside
  the scope is stated and recorded, not silently fixed in the same branch.
- **DO NOT narrow it either.** Part of the scope turning out to be blocked means
  finishing everything else in full and saying plainly what was left out and why.

## 4. Tests

- **Every bug fix ships with a test that reproduces the bug** — failing before
  the fix, passing after. A test that passes against the unfixed code proves
  nothing and does not satisfy this rule.
- **The test targets the actual defect**, not something adjacent to it.
- **If a test genuinely cannot be added**, say so explicitly before calling the
  work done: the bug, why no test is possible now, and the follow-up plan. Get
  acknowledgment. Silence is not an option, and autonomous execution grants no
  exemption from this rule.
- **New behaviour ships with the test that covers it**, in the same branch.
- Where tests live is the project's coding standard's decision, not this one's.

## 5. Warnings and Verification

**Warnings are first-class defects.** Compiler, linter, deprecation, console,
accessibility, race-detector — all of them.

- **DO NOT silence a warning to clean up the output.** Suppressing, filtering or
  narrowing a check to make it quiet is a defect on top of the original one.
- **A warning that cannot be fixed now is stated explicitly** — what it says, why
  it stands, what the follow-up is — and acknowledged. Never swallowed.

**The definition of done**, and none of it is optional:

- The project's test and lint commands were **run** and **passed**. Not "should
  pass", not "unchanged code so unaffected".
- **DO NOT reach green by weakening the check** — skipping a test, loosening an
  assertion, adding an ignore directive, lowering a threshold. The check exists
  to fail; make the code satisfy it.
- **Report the outcome faithfully.** Tests failing means saying so, with the
  output. A step skipped means saying which and why. Work finished and verified
  is stated plainly, without hedging.

## 6. Merging

- **Rebase onto the target branch before merging.** Conflicts are resolved on the
  branch, never on the target.
- **Fast-forward merges only.** A merge that cannot fast-forward gets rebased
  again; never resolve it with a merge commit. History stays linear, which is
  what keeps bisecting and "which release contains this" answerable.
- **DO NOT rewrite history that is already on the default branch**, and never
  force-push a branch another session or person is working on.
- **The merge is the moment of truth**: it happens after section 5 passes, not
  before, and not "while the checks finish".
- Commit messages follow the project's commit convention; this document does not
  restate it.

## 7. Checklist

- [ ] Configuration read from `CLAUDE.md` ([entry point](../SKILL.md), section 2), not assumed
- [ ] Work happened on a branch off the current target tip — or directly on the
      default branch because the user asked for that, within the scope they gave
- [ ] One logical task on this branch; unrelated findings recorded, not smuggled in
- [ ] Split/move/rename: both sweeps run, every hit itemized with its disposition, reverse direction checked
- [ ] Contract changed before the implementation, where a contract was involved
- [ ] Every bug fix carries a test that failed before the fix
- [ ] No warning suppressed; anything deferred stated explicitly and acknowledged
- [ ] The project's test and lint commands were run and passed
- [ ] No check was weakened to reach green
- [ ] Rebased onto the target; merged fast-forward; history linear
