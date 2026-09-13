<!--
Template for a plan document: <plans>/<task-slug>.md in the main checkout, where
the slug says what the work is and is the same string that names the workflow and
appears in every report. The file is untracked and never lives inside a
workflow's working tree -- that tree is removed when the workflow finishes.
Delete every comment block before the workflow starts.

One file per task. It is the single coordination surface between the workflow
doing the work and the coder watching it: the agents write, the coder only
reads. Section order is fixed so that a monitoring session can scan any plan
document the same way.

**Language: write the prose in the language of the session.** The task
statement, the item descriptions, the acceptance criteria and the log entries
are written in whatever language the work is being discussed in -- this file is
English because the skill is, not because the plan has to be.

**The structure is not translated.** Section headings, the Work Items table
header, the Stage and Status vocabularies and the item ids stay exactly as they
appear here: they are the keys every reader and the checker match on, and a
translated heading is an unreadable plan to both.

Progress has exactly one source of truth in this file:

  - The Work Items table holds current state. Nowhere else repeats it -- the
    detail blocks below carry what does not change (what an item touches, what
    it must satisfy), never its status.
  - The Progress Log holds what happened. The table says where things stand;
    the log says how they got there, including the failures and the rework.
  - How fresh the file is comes from the file's own modification time, not from
    a timestamp copied into the text where it can go stale.

Statuses are a closed vocabulary:
  planned -> coding -> review -> fixing -> done, plus blocked.

The last move is the one that gets skipped. An item that is coded, reviewed and
merged still reads review until somebody writes done, and the acceptance
criteria under a row that never says done are never checked by anyone -- so a
plan can be archived with every box unticked and nothing will have said so. The
last stage makes that final move for every item, and check_plan.py --final is
what refuses the archive until it has.

**A status move is written before the work it announces, never after it.**
Starting W1 means: edit W1's row to coding, save this file, and only then open
the first source file. The same order holds for every later move. This is a
rule about sequence, not a reminder to keep notes -- "I will fill it in when
the item is done" empties the one window onto a running workflow for exactly
the stretch of time anyone would have looked through it, and the finished file
reads the same either way, so nothing catches it afterwards.
-->

# <Task name>

**Stage**: <plan | build | land | done>
**Branch**: <branch the workflow works on>

## Task

<The task statement, restated as it was accepted. What is being asked for, and
what "done" means for the task as a whole. No solution design here -- that is
what the work items are.>

## Work Items

<!--
The decomposition must partition the files it touches: two items that write the
same file are one item, or they are serialized through Depends on. Parallel
agents writing one file is the accident this table exists to prevent.

Owns: the paths (or path globs) this item alone may write.
Depends on: item IDs that must reach done first, or "--".
Updated: YYYY-MM-DD HH:MM, touched in the same edit that changes Status --
which happens before the work that status describes, never after it.

This table is what the coder monitoring the workflow reads, and while the work
runs it is nearly all it has. A row that says planned is a claim that nothing
has been written for that item yet, and someone is acting on that claim now.
-->

| ID | Item | Owns | Depends on | Status | Updated |
|---|---|---|---|---|---|
| W1 | <short title> | <paths this item owns> | -- | planned | <YYYY-MM-DD HH:MM> |
| W2 | <short title> | <paths this item owns> | W1 | planned | <YYYY-MM-DD HH:MM> |

### W1 — <short title>

**Changes**: <what this item does, in a sentence or two.>

**Acceptance**:
- [ ] <A criterion that can fail. "Works correctly" cannot fail; "rejects a
      negative amount with ErrInvalidAmount" can.>
- [ ] <The test that proves it, named by file and case.>

**Reviewer**: <who checked this item -- never who wrote it. One review round
that covered several items is named identically in each of them; that is the
normal shape, not a shortcut.>

### W2 — <short title>

**Changes**: <...>

**Acceptance**:
- [ ] <...>

**Reviewer**: <...>

## Progress Log

<!--
Appended as the work happens, newest last. One line per event that changes
someone's picture of the task: a status move, a review verdict, a failure, a
decision taken. Not a narration of every edit.

A status move and its log line are written in the same edit, before the work
they announce -- the table says where the item stands, this says how it got
there, and they never disagree by being saved at different times.

The coder's progress report is assembled from this log, so an event that never
lands here never reaches the user.
-->

- `<YYYY-MM-DD HH:MM>` `W1` <what happened>

## Open Findings

<!--
Review findings not yet resolved. A finding the author disputes stays here with
both positions recorded -- dropping it silently is what this table prevents.

State: open | fixed | disputed | accepted-as-is | moved: <task-slug>

Archiving requires fixed, accepted-as-is or moved. Use moved for a finding that
is real but belongs to a decision this task does not own, and name the task that
will settle it -- that slug is the only link between the two. open and disputed
are states for work in flight: left in an archived plan, which is untracked and
read by nobody, they are findings thrown away.
-->

| ID | Item | Finding | Raised by | State |
|---|---|---|---|---|
| F1 | W1 | <what is wrong and why it matters> | <reviewer> | open |

## Verification

<!--
Run over the whole result once everything is coded and reviewed, before the last
stage begins. Record the command as it was actually run and what it actually
returned. A command that was not run is recorded as not run -- never as assumed
to pass.

The review rounds go here too, one block each, below the table. A round reads a
diff rather than an item, so this is where it fits; what it read, what it found
and how it ruled belong to it, and an item's Reviewer line names it. A name no
round and no log entry accounts for is a name, not a review -- check_plan.py
--final says so.
-->

| Command | Result | When |
|---|---|---|
| <the project's test command> | <pass / fail: what failed> | <YYYY-MM-DD HH:MM> |
| <the project's lint command> | <pass / fail: what failed> | <YYYY-MM-DD HH:MM> |

### <review round> — <reviewer>, <verdict>

<What this round read, what it found, and where the findings went.>

**Warnings**: <every warning surfaced, and for each: fixed, or why it stands and
what the follow-up is. "None" only when the output genuinely had none.>

## Deliberate Omissions

<!--
Anything inside the task's scope that is not being delivered, and why. Empty is
a valid answer; silence is not -- an omission the user learns about at merge
time is a defect in this document.
-->

- <what was left out> — <why, and what would have to change to do it>

## Outcome

<!--
Filled at the last stage, immediately before archiving -- together with the
final status moves, the ticks, and the disposition of every finding. A recorded
commit with the Stage still reading build is the contradiction that says this
section was filled and the rest of the file was not.
-->

**Landed**: <merge commit sha, or "not landed: reason">
