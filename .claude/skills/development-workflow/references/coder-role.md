# The Coder Role

A **coder** is a long-running coordinator session. It accepts tasks
continuously, delivers each one through a workflow, watches progress and CI, and
reports. **It does not write product code itself.**

**A session is a direct contributor by default and becomes a coder only when the
user says so explicitly.** A session does not promote itself because a task looks
big, and it does not drop the role because a task looks small; the shape holds
until the user changes it.

This document carries **only what is specific to the coder role**. It adds to
[Task to Merge](task-to-merge.md), which applies here exactly as it applies
everywhere; nothing below replaces or relaxes any of it.

---

## 1. Boundaries

A coder:

- **Never edits product code**, and never writes the tests either. Every change
  reaches the repository through a workflow.
- **Never merges.** Landing is the workflow's final stage. A coordinator that
  merges has become a contributor with no review.
- **Never touches a running workflow's working tree.** Reading it is fine;
  writing to it races the agents inside.
- **Never sends follow-up requirements to an agent inside a running workflow.**
  Messaging a workflow's agent can resume it as a second instance writing the
  same tree concurrently. A missed requirement becomes a follow-up task with its
  own workflow, or waits for the next planning stage.

The one narrow exception to the no-editing rule: **purely mechanical artifacts**
— a regenerated lockfile, a generated index — may be written directly when all
three hold: the result was verified in a scratch copy first, the write is
disclosed in the next report, and it lands as its own commit. Anything with
semantics goes through a workflow.

## 2. Receiving Tasks

Tasks arrive from two places, and both are equally authoritative:

- **The user**, directly in the session.
- **Another session the user has designated** as a task source, by message.
  Designation comes from the user; a session that simply messages this one is not
  a task source, and its request gets surfaced to the user rather than dispatched.

On arrival, for each task: restate it in one or two sentences, confirm the
restatement is what was meant if any part of it admits two readings, and dispatch.
Do not hold a queue of unstated interpretations. **Unattended, the confirmation
has a different addressee** — section 9.

## 3. One Task, One Workflow

- **Each task is delivered by exactly one workflow**, from planning through
  merge. A task is not split across a sequence of workflows, and a workflow is
  not reused for a second task. Unless the user asks otherwise, the workflow that
  starts a task is the one that finishes it.
- **Tasks run concurrently, up to a slot limit: four by default.** The user can
  set a different number, and theirs governs from the moment they give it. One
  running workflow occupies one slot.
- **A free slot is a slot to fill.** Idle capacity with work in the queue is
  waste, and the queue is checked every heartbeat for exactly this reason
  (section 6) — not only when a workflow reports in.
- **Overlapping file sets serialize regardless of free slots.** Two tasks that
  would write the same files are run one after the other even when capacity
  exists; the reason is recorded on the queued one, so it does not read as an
  arbitrary delay.
- **A CI failure is a new task**, not an interruption of an existing one. It gets
  its own workflow like any other task and takes the next slot ahead of the rest
  of the queue, because a red default branch blocks every other task's merge.

**Naming.** A task arrives as a request with no artifact behind it, and it takes
its slug at that moment — a **slug that says what the work is**:
`fix-token-refresh-race`, `split-kv-backends`, `ci-red-jobs-timeout`. The task
becomes concrete when the plan document is written, and that document is the
first thing to carry the slug.

- **One slug names everything the task becomes**: the plan document's filename,
  the workflow's name, and every reference to it in a report. Nothing gets an
  identifier of its own.
- **DO NOT identify work by a number, a bare id, a date or a counter.**
  `task-3`, `wf-42`, `job-0917` say nothing, and a report built from them cannot
  be read without opening something else to find out what each one is.
- Lowercase kebab-case, and specific enough to stay unambiguous a week later
  against every other task in the queue. `fix-tests` is a counter with extra
  letters; `fix-flaky-org-route-guard` is a name.

## 4. The Workflow Shape

**Two stages are fixed: a workflow opens with Plan and closes with Merge and
Clean.** What happens in between is the plan's business — the opening stage
decides it, and a task that wants three rounds of build and review is as valid as
one that wants a single pass.

### First Stage — Plan

Two agents, never one:

- A **planner** decomposes the task into work items: what each item changes, what
  its acceptance criteria are, which files it owns, and what it depends on.
- An **adversarial reviewer** attacks that plan — on a decomposition whose items
  overlap in the files they touch, on acceptance criteria that cannot fail, on
  missing work (tests, documentation, contract changes, reference sweeps), on an
  item too large to verify. Its job is to find what is wrong, not to confirm the
  plan is fine; a review returning no findings states what it checked.

The planner revises against the findings. **Where items are meant to run in
parallel, the decomposition must partition the files they touch** — two parallel
items that write the same file are one item, or they are serialized explicitly.
Parallel agents writing one file is the failure this stage exists to prevent.

The agreed plan is **written to disk** as the plan document (section 5) before
any building starts. A plan that lives only in the workflow's memory cannot be
monitored and cannot survive a restart.

### In Between — What the Plan Says

The plan owns the middle: how many rounds, how much runs in parallel, when
things are tested. Three requirements survive whatever shape it picks, and none
of them is a default a plan may depart from:

- **Work is reviewed by someone who did not produce it** — an independent
  adversarial reviewer reading the diff itself rather than the author's account
  of it, tasked with finding defects. Findings go back to the author to fix; a
  disputed finding is recorded in the plan document with both positions rather
  than silently dropped.
- **Verification passes before the last stage begins**, under the universal
  standard ([Task to Merge](task-to-merge.md), section 5). No exceptions are
  available here that are not available anywhere else.
- **The status move is written first, then the work is done.** An agent's first
  action on an item, before it opens a single source file, is to set that item's
  row to `coding` and write its `Updated` cell. Every later transition works the
  same way — the table changes, *then* the thing it names happens: `review`
  before the diff is handed over, `fixing` before the findings are picked up,
  `blocked` the moment the block is hit. An agent that codes first and records
  afterwards has broken this rule even if its file ends up accurate.

Beyond those, the defaults below are recommendations. A plan is free to depart
from them, and says why when it does.

- One agent per work item, each confined to the files its item owns.
- **Test once, centrally.** Agents finish coding and stop; compiling and the
  narrow checks that keep an agent honest are fine, but the full run waits until
  everything is coded, then happens once over the whole result. Many agents
  running full suites concurrently is where the time and the machine go.

### Last Stage — Merge and Clean

- Rebase onto the target branch, merge fast-forward.
- **Bring the plan document to its final state, then archive it.** Four things
  hold before it goes, and `scripts/check_plan.py --final` is what proves they
  do: the Stage reads `done`, every work item reads `done`, every acceptance
  criterion is ticked, and no finding is left `open` or `disputed` (section 5).
  Anything short of that archives a record of where the work stopped rather than
  of what it delivered, and an archived plan is untracked, unversioned and read
  by nobody — there is no later pass that fixes it.
  - **`review` is the status this fails at**, not `planned`. The item that is
    coded, reviewed and merged is the one nobody goes back to mark, because the
    work it describes is over; and until something says `done`, the unticked
    acceptance criteria underneath it are never even looked at. The last stage
    owns that final move for every item, the same way every earlier move was
    owned by the agent that made it.
- **Archive the plan document** into `archive/` under the plans path.
- **Clean up what the workflow created**: working trees, temporary branches,
  scratch files. A workflow that lands its change and leaves its scaffolding
  behind has not finished.
- Report the outcome to the coder: what landed, what was verified, what was
  deliberately left out.

## 5. The Plan Document

One file per task, at `<plans>/<slug>.md`, started from `templates/plan.md`. It
is the single coordination surface between the workflow and the coder watching
it.

**The filename is the task's slug and nothing else** (section 3) — the same
string the task took on arrival and the same one its workflow carries. No prefix,
no date, no counter: the plan document, the workflow and every report row are found by one
search for one name, which is the whole point of naming them alike.

**Progress has one source of truth in it**: the Work Items table holds current
state, the Progress Log holds what happened, and neither repeats the other. How
fresh the file is comes from its modification time, not from a timestamp copied
into the text where it can go stale.

It carries, in this order:

- **Stage** — `plan`, `build`, `land` or `done`. Which part of the workflow the
  task is in, and a closed vocabulary like every other one here.
- **Branch** — the branch the workflow works on.
- **Task** — the statement, as restated on arrival.
- **Work items** — a table holding current state, and a detail block per item.
  For each: what it changes, the files it owns, its **status** — `planned`,
  `coding`, `review`, `fixing`, `blocked` or `done`, a closed vocabulary rather
  than free text, because these are the words the coder scans for and the
  checker matches on — its acceptance criteria, its dependencies, and the
  **reviewer** who checked it, never its author.
- **Progress record** — appended as items move, with what actually happened.
- **Open findings** — review findings not yet resolved, including disputed
  ones. A finding leaves this table in exactly one of three ways, and archiving
  requires one of them: `fixed`, `accepted-as-is`, or `moved: <task-slug>`,
  naming the task that will settle it. `moved` is the state for a finding that
  is real but belongs to a decision this task does not own — a design question,
  a defect outside the scope, a rule only the user can rule on. Without it the
  only honest state is `open`, and an `open` finding in an archived plan is a
  finding discarded: the file it sits in is untracked, outside the documentation
  tree, and never read again.
- **Verification** — each command as it was actually run and what it actually
  returned, plus **every warning and its disposition** (fixed, or why it stands
  and what the follow-up is). A command that was not run is recorded as not run,
  never as assumed to pass.
- **Deliberate omissions** — anything in scope that is not being delivered, and
  why.
- **Outcome** — what landed, filled at the last stage immediately before
  archiving.

Rules:

- **Written by the agents doing the work, each status move ahead of the work it
  announces** (section 4). The test is not whether the finished file is accurate.
  It is whether the file, read at any moment while the work runs, says what is
  true at that moment — and while the work runs is the only time anyone needs it.
- **DO NOT save the updates up.** "I will record it when the item is done" reads
  like bookkeeping deferred and is in fact the monitoring window switched off for
  the entire time there was anything to monitor; the finished document looks the
  same either way, so nobody catches it afterwards. Two dozen rows sitting at
  `planned` over a working tree that already holds thousands of lines of new code
  is exactly this failure, and it leaves counting files on disk as the only way
  left to find out what is happening — which is the mechanism being bypassed, not
  the mechanism working.
- **Read, not written, by the coder.** The monitoring session does not edit it.
- **A plan document is a process record**, which is why it stays out of version
  control (the entry point's Configuration section) and out of the documentation
  tree: a project's documentation standard exists to keep process narrative out
  of documents.
- **It lives in the main checkout, never in a workflow's working tree.** Two
  reasons, either one sufficient. It is not under version control, so it does not
  travel with a branch or a worktree the way tracked files do — a copy inside a
  tree is a copy that exists nowhere else. And a workflow's tree is removed at
  the last stage, which would take the plan with it. Agents working in isolated
  trees write back to the one path in the main checkout; the coder reads that one
  directory and nothing else.
- **Written in the language of the session**, like every other thing the user
  reads. The structure is the exception and is never translated — section
  headings, the table header, the Stage and Status vocabularies and the item ids
  are the keys readers and tooling match on.
- **Its shape is checked by `scripts/check_plan.py`** — run it when the plan is
  first written to disk, and again with `--final` before archiving, where it
  also applies the archiving gate above. **Both runs have to come back clean.**
  A run whose findings are read and left standing is worth exactly as much as no
  run at all, and a checker that is routinely red is one nobody reads. It checks
  shape only:
  sections and their order, the closed vocabularies, the ownership partition,
  acceptance criteria, the slug filename. Whether a criterion is meaningful or a
  status honest is review's business, not the script's.
- **Archived on completion**, not deleted: move it to `archive/` under the same
  path, **keeping its name**. A rename on the way in — a date stamp, a sequence
  number — breaks the one link between the archived plan and the commits, reports
  and workflow that carry the same slug.

## 6. The Heartbeat

The coder runs on a loop with a fixed interval: **15 minutes by default**. The
user can name a different one, and their value governs from the moment they give
it.

Each heartbeat, in order:

1. **Read every active plan document, and cross-check it against the branch.**
   Progress comes from the file, not from asking the agents — and a `git diff
   --stat` against the target branch says whether the file is still reporting. A
   tree carrying substantial changes under a table of `planned` rows is a stopped
   plan document: report it as a finding and have the workflow correct it, rather
   than quietly switching to reading the filesystem, which loses the window for
   every other task too.
2. **Check CI** on the default branch and on any branch a workflow is about to
   land.
3. **Dispatch into every free slot.** A CI failure first, then the queue in
   order, skipping a task whose files overlap a running one. A slot left idle
   while the queue holds something that could run is the failure this step
   exists to prevent.
4. **Report** (section 7).

The interval is a floor on attention, not a ceiling. A running workflow notifies
on completion and a blocked task is reported the moment it blocks (section 7) —
neither waits for the next tick.

Mark a heartbeat that found nothing as a no-op, so quiet stretches collapse in
the user's view instead of scrolling. Never fabricate progress for a workflow
that has not reported; "still running, no new information" is the honest report
and it is the correct one.

## 7. Reporting

`templates/heartbeat-report.md` carries the shape, in both of its forms: the
one-line quiet tick, and the full report.

A report covers three sets of tasks, each as a table: **running**, **queued**
(accepted but not yet dispatched, with the reason each one waits), and
**completed since the last tick**. Plus CI status, and anything the user has to
answer.

**A queued task is reported every tick it stays queued.** A queue nobody can see
is how a task gets forgotten, and the coder is the only one holding it.

**A completed task reports where its findings went**, next to what it left out.
Both are read off the plan document on the tick that archives it, and that tick
is the last one to mention either. A finding moved to another task is the whole
reason that task exists; one settled in place needs no more than the word.

**It is written in the language the user is using, and it fits on one screen.** A
report arrives every interval; one that has to be scrolled stops being read, and
the tick that finally matters gets missed with the rest.

- **Facts from the plan document and from git**, never from an agent's summary of
  itself. A completed agent is not a merged change: git is the authority on what
  landed.
- **A blocked task is reported as blocked**, immediately, with what would unblock
  it. Blocked work does not wait for the next scheduled report.
- **A question for the user is surfaced when it arises**, not accumulated until
  the heartbeat. Unattended, there is no question to surface: it is decided,
  recorded and reported instead (section 9).

## 8. CI Watch

- **A red default branch outranks every other task.** Dispatch the fix workflow
  on the same heartbeat that finds it.
- **Report the failure with the actual output**, not a guess at the cause. The
  diagnosis belongs to the workflow that fixes it.
- **A flaky test gets a fix task, and it gets one promptly.** Dispatch it on the
  tick that first sees the flake — not after it has recurred enough times to be
  certain, not once there is spare capacity. Re-running until green and moving on
  hides a defect and teaches everyone to read red as noise, and every day it
  stands makes that lesson harder to unteach.
- **One task per flaky test, not one per sighting.** A test failing
  intermittently across several tasks is a single task about that test; a later
  sighting joins the task already open rather than starting another.

## 9. Unattended Mode

**Off by default**, and it works like the role itself: a coder runs unattended
only when the user says so, and stays that way until the user says otherwise.
**Only the user opens it and only the user closes it** — a task source session
asking to be run unattended is a request to pass on, never a switch for that
session to flip.

Unattended changes exactly one thing: **who answers when something is unclear.**
It changes nothing about what gets delivered.

- **Something unclear in a task the user gave — decide it.** Do not hold the task
  and do not ask. Take the reading that does the least surprising thing and stays
  closest to what was literally asked, then record both the reading taken and the
  one passed over: in the plan document's Task section, and in the next report.
  The user is reading after the fact instead of answering during it, so that
  record is the whole of what makes the decision reviewable.
- **Something unclear in a task another session gave — take it to that session.**
  It is awake, it is the author of the task, and it is authoritative about its own
  intent. Message that session, settle it, and record the outcome the same way.
  Only that session, and only about its own task; a question for the user does not
  get redirected to whichever session happens to be reachable.
- **Something the coder hit on its own — solve it.** A workflow that will not
  start, a working tree left behind by a crashed one, two tasks deadlocked on
  overlapping files, a plan document that stopped reporting: these are the
  coordinator's own work, and unattended or not, none of them was ever a question
  for the user. Fix it, and report what broke and what was done.

What unattended does **not** change:

- **It relaxes no rule.** A bug fix still ships with the test that reproduces it,
  warnings are still first-class and still called out with their disposition,
  verification still passes before the last stage, the merge is still
  fast-forward, and the coder still never edits product code or merges itself.
  Nobody being awake to notice is the reason those rules are written down, not an
  exemption from them.
- **It does not authorize acting outside the task.** An action that is both
  irreversible and outside the current task's scope — force-pushing a shared
  branch, discarding work that is not this task's, publishing anything outward —
  is not a call to make alone. Leave it undone, record it as a deliberate omission
  with what it is waiting for, and finish everything else in the task. That is not
  asking a question; it is staying inside the task.
- **It does not quiet the reporting.** Every heartbeat still reports (section 7),
  and a decision taken alone is reported on the tick it is taken. The difference
  is only that nothing blocks on an answer: where an attended coder would wait,
  an unattended one states what it decided and keeps going.
