# The TLA+ model

Atoll's worst bugs are interleavings: Atoll killed between two syncs of a move while a USB disk vanishes and a pass propagates a deletion. Tests check the orders someone thought of; TLC, the model checker of TLA+, explores every reachable state of a small pool and gives the shortest trace to each violation (the maintainer, 2026-11-19, `specs.md` §27). The model is language-neutral, as the vectors are: the Rust version inherits it.

## Running it

```bash
make model-check                         # every instance, each against what it expects
test/tla/run.sh Copy.cfg Live.cfg        # some of them
make model-flaws                         # the black-box scenarios of the flaws still open: none now
```

`test/tla/run.sh` needs only Docker: it runs TLC in `eclipse-temurin:21.0.13_9-jre-noble`, with 2 workers, a 2 GiB heap and 2 CPUs, so that it leaves the machine to whatever else runs there. It downloads `tla2tools.jar` 1.7.4 once into `bin/tla/`, git-ignored, and checks its SHA-256. Each instance's log, with the full trace of a violation, goes to `bin/tla/<instance>.log`. With `--inside`, it runs TLC where it is, in that image, as the CI's `tla` job and `test/ci/run.sh` do.

The first line of each `.cfg` says what TLC must find there: `\* expect: pass`, or `\* expect: <invariant>` for a flaw of `specs.md` that the model found and that waits for the maintainer's remedy. An instance that finds anything else fails `make model-check`, a flaw fixed included: its expectation then becomes `pass`.

## Files

| File | What it holds |
|---|---|
| `Replication.tla` | The model: variables, the actions of Atoll and of the world, the invariants. Each action names the section and step of `specs.md` it formalizes. |
| `MC.tla` | What the instances name and a `.cfg` file cannot hold: the physical disks and the copies at the start. |
| `*.cfg` | The instances TLC checks, each with what it expects. |
| `flaws/` | A black-box scenario for each flaw still open, in the format of `../scenarios/README.md`, which fails until the flaw is fixed and then moves to `../scenarios/`; the folder is gone while no flaw is open. |
| `fingerprints.json`, `fingerprints_test.go` | The SHA-256 of each section of `specs.md` the model formalizes, and the test that compares them (below). |
| `states.json`, `states.md`, `states_test.go` | The statecharts of a disk's states and a copy's, each transition with the section of `specs.md` that says it and the past bugs it caused, by their entry of `DONE.md`; `states.md` is drawn from `states.json`, and the test keeps both equal to the closed lists of `schema/migrations/` and `specs.md` §4. They show that no transition enters the copy state `unavailable`. |

## The model

**The world.** Disks hold objects, each standing for an inode with its creation time (the identity of §15.1), with a content (a version number), a change count (what size, modification and change time say), and a count of what moved the change time alone: a rename, or a write by a program that keeps the size and the dates. Each disk has its names twice: as the kernel shows them, and as they would survive a power cut, so that what a sync makes durable is explicit. The names are a file's logical path, a part `<op-id>.part` in `<workdir>/tmp/`, and an entry `<date>/<op-id>/…` in `<workdir>/trash/`. A disk is up, or gone: answering `EIO`, or an empty mount point; mounted again it is a new mount (a new mount id) and has lost what it had not made durable, as XFS brings a rename back after a remount (§17.1). A user writes into a copy (a new version, same inode), or rewrites it keeping its size and dates (a new version, same inode, only the change time moved), replaces it (an atomic save: a new inode), deletes it, or makes a file where none was, on any disk at any moment, durably, with what came before on that disk. Atoll is killed (kill -9: the catalog keeps what it committed, the process loses the rest), or the power is cut (every disk keeps only what was durable).

**Atoll.** One process that runs passes, step by step, as the conformance build does, one copy at a time (§29.4): the mount check of every disk and the session's confirmation, which binds the session's handles to that mount (§10); the reprise of each disk's planned operations, in two steps, what it does on the disk then its commit (§17.2, §14.2 Reprise); a part in `tmp/` no job owns, dropped (§14.2); the walk of each disk, then its confirmation, and the verdicts there, edited, replaced, gone, own and unknown (§15.10, §15.12); the decisions Deletion, Kept and Gone (§15.12), with peers, which wait while a job of the file is running (§14.1); the modifications, `modified`, `metadata` and `conflict`, with their waits (§15.4); a stale copy's trash job queued again (§15.4); the trash jobs and the operation `trash` step by step, with `link` and `unlink` on a disk without `RENAME_NOREPLACE` (§15.12, §17.1); the plan, which keeps or cancels pending copies, counts the copies, finds a source, and places a copy where no disk on the same physical disk holds one (§13.4, §14.1); the copies, the operation `create` step by step: journaled, the part made and its inode recorded, copied, the source looked at again, synced, verified, renamed without replacing anything and looked at, the change time that look found recorded in the job (`placed`), the file then its folder synced, the folder of `tmp/` synced, the commit (§14, §14.2). A disk that fails at any step leaves its operation planned for the reprise. A decision may always wait, which stands for the debounce, `settle` and the guards.

**The catalog.** Files (their hash as the version it describes, conflict), copies (state `ok`, `stale` or `pending`, inode, change count, `verified_ns`, `created_by`, `gone_kind`, `first_seen_ns` as a clock), the jobs `replicate` and `trash`, the journal `ops` (`planned`, `done`, `undone`), and each disk's state from the last mount check.

**The invariants**, with the numbers of `CLAUDE.md`'s non-negotiable invariants:

| Invariant | Checks |
|---|---|
| `NoUserDataLost` | every version a user wrote, or the library held at the start, is still durably on some disk, in the library or a trash, missing disks included, unless a user's own action took its last copy there |
| `Inv1_LastVerifiedCopyKept` | 1: the version the catalog vouches for is still on some disk; Atoll never makes a name hold something else |
| `Inv4_UnknownNeverAbsent` | 4: nothing a walk of a disk gone (`EIO`, or an empty mount point, mounted again or not) found is recorded |
| `Inv7_PipelineOrder` | 6 and 7: no copy of Atoll's at a name of the library without its content durable and verified, and no commit of a copy or of a move to the trash before the names are durable |
| `Inv10_DeletionOnlyUnchangedToTrash` | 10: a deletion sends to the trash only a copy unchanged since it was in sync, and never the latest version a user wrote and did not delete (a replacement is no deletion) |
| `Inv11_OwnOpsNotUsers` | 11: no copy that Atoll's own trash or `tmp/` holds is decided gone, and no copy Atoll made sits in the library unknown to the catalog, where the next walk would take it for a user's new file |
| `Inv12_JournalFirst` | 12: every part in a `tmp/` and every entry in a trash, visible or durable, is named by a committed operation of that disk |
| `Inv13_OnePhysicalDisk` | 13: no plan queues a copy where a disk on the same physical disk holds a copy of the file |
| `ReplicaInSync` | a copy Atoll made, that the catalog calls `ok` and that nobody touched since its row was written, holds the version the catalog says is current (G2), but for one a reprise recorded unverified as its change time may be a write's (§14.2, the limit of flaw 1) |
| `DoubtUnverified` | a copy a reprise recorded so is never vouched for: it stays unverified until a read takes it again |
| `EventuallySettled` | liveness, with fairness: a file below its level reaches it unless in conflict, and no operation stays planned |

## Instances

Measured with `make model-check` on the maintainer's machine, 2 workers, on 2026-11-20, with the change time's own count and the record step of §14.2, while other work ran there: distinct states, and about 16 minutes for all of them (about 6 on 2026-11-19, the machine idle).

| Instance | Expects | What it explores | States | Time |
|---|---|---|---|---|
| `Copy` | pass | a file replicated on 3 disks, two on one physical disk; one user action, one kill, one power cut, one disk lost | 3,862,283 | 3 min 33 s |
| `Propagate` | pass | a file at its level; two user actions, one kill, one disk lost | 2,509,457 | 1 min 59 s |
| `PowerCut` | pass | `Propagate` with a power cut in place of the kill | 2,865,298 | 2 min 19 s |
| `Link` | pass | `Propagate` on disks without `RENAME_NOREPLACE` | 2,852,554 | 2 min 38 s |
| `TwoFiles` | pass | a file to replicate and one at its level, in the same passes | 1,316,595 | 1 min 31 s |
| `Live` | pass | liveness: one user action, one kill, one disk lost and mounted again in time | 457,332 | 1 min 5 s |
| `FlawReplicaEdited` | pass | flaw 1, fixed: its trace, with the remedy | 3,972 | 2 s |
| `FlawReplicaRewritten` | pass | the limit of flaw 1, closed: a write that moves only the change time, with Atoll killed or the disk lost after the rename | 85,696 | 7 s |
| `FlawJobDroppedGone` | pass | flaw 2, fixed: its trace through a deletion, with the remedy | 7,919 | 4 s |
| `FlawJobDroppedPlan` | pass | flaw 2, fixed: its trace through the plan, with the remedy | 85,696 | 8 s |

Both flaws are fixed, their remedies `specs.md`'s, and every instance takes `specs.md` as it is: `Fixes` is empty. The `Flaw…` instances stay, each the smallest pool that showed its flaw.

## Flaws found in specs.md

Both were real: each trace was replayed on the conformance build, and each had a black-box scenario in `flaws/` that failed (`make model-flaws`) until its remedy, the maintainer's to decide; the model's `Fixes` holds the one proposed for a flaw still open, so that the instances that expect `pass` look past it. Once a remedy is in `specs.md`, the model takes it whatever `Fixes` says, the flaw's name leaves `Fixes`, its instance expects `pass`, and its scenario moves to `../scenarios/`. Both are fixed now.

### Flaw 1: a write into a new replica before its row is written is recorded as verified

Fixed on 2026-11-19, with the remedy the maintainer approved (below): `specs.md` §14.2, `conformance/vectors/create_row.json`, `FlawReplicaEdited.cfg` expecting `pass`, and `../scenarios/replica-edited-before-commit.json`.

`specs.md` §14.2 commits a copy "with what a look at its new name finds", and its reprise, for a target `same` and a part `absent`, "finishes as above, with the hashes the job recorded". The look comes after the rename has put the copy in the library: whatever a user writes into it between the rename and the look is recorded as the copy's own state, `verified_ns` set against the hash of the version it was made from.

1. A file of version 1 on disk 1; the plan queues its replica on disk 2.
2. The operation `create` runs up to `source-synced`: the copy is at its name on disk 2, every sync done.
3. A user writes version 2 into it, in place (directly on disk 2, or through mergerfs, whose first branch may be disk 2).
4. The commit records the copy `ok`, verified, with the size and times of version 2, while the file's hash is version 1's (`ReplicaInSync` broken).
5. Every later look finds the copy as its row says: the edit is never propagated. A scrub then reads a mismatch with unchanged size and times, `corrupt`, and repairs the user's edit away from disk 1, the chunks it held in the trash (against G4); or a later change on disk 1 makes it stale and sends it to the trash.

A crash at `source-synced` makes the window as long as Atoll is down: the reprise records what is there when it runs. In the code: `internal/ops/create.go` (`PlacePart`, whose `movedFrom` looks after the syncs) and `internal/ops/recover.go` (`recoverCreate`, "what is there now is what gets recorded"), then `catalog.FinishCreate`. Reproduced by `FlawReplicaEdited.cfg` and `replica-edited-before-commit.json`, which failed until the remedy.

The remedy (`create-look`, approved by the maintainer on 2026-11-19): the row takes what the part was when it was verified, before the rename (inode, size, modification time), so that a write since reads as `edited` at the next look; the reprise likewise records the part's size and modification time from the job, never what it finds. As it landed: the job records with its whole-file hash what the part was then, its size, times, mode, owner and links (`part_look`), and the row takes it all, but for the change time, which the rename moved: that one comes from a look right after the rename, or in a reprise from its look, and only when the look found nothing else moved; otherwise the part's, which the next look finds moved, later than the row says, a modification and never a `conflict` (§15.4). In the model, whose gen stands for size, times and change time at once, and whose rename and the look after it are one step, the row takes gen 0. What it left: a write that keeps the size, the times, the mode and the owner moves only the change time, which a reprise could not tell from the rename's, and took; the model, where every edit moved the size, could not see it either.

**Its limit, closed** (the maintainer, 2026-11-20, "Les deux"; `specs.md` §14.2, `conformance/vectors/create_reprise.json`, `FlawReplicaRewritten.cfg`, and `../scenarios/replica-edited-keeping-dates-after-record.json` and `-before-record.json`). The model now counts apart what moves the change time alone (`cg`): the rename, the name in `tmp/` a reprise takes away, and a user's `rewrite`, a write that keeps the size and the dates. (b) Right after the rename and its look, the job records the change time that look found, in a commit of its own (`CopyRecord`, `placed`), and a reprise that finds another reads a write since: the row is the part's, and the next walk finds the copy `edited`. (a) A reprise that finds no record, the crash or the disk lost having come between the rename and that commit (`moved`), and a change time other than the part's when it was verified, records the copy unverified: the row's `doubt`, no column, which `ReplicaInSync` looks past and `DoubtUnverified` keeps unverified; its write is then for a read, which the model leaves out, to find a `conflict`. Each was taken out once to see TLC fail: without (b), a rename, its record, a rewrite, a crash and the reprise record the write as the copy (`ReplicaInSync`); without (a), the same with the disk lost before the record. The normal run's window stays: the look comes in the step of the rename, which here is the width of a `stat`, and on a disk without `RENAME_NOREPLACE` the look comes after the `unlink`, so that a rewrite between the link and that look is taken (What it leaves out).

### Flaw 2: a job running is dropped while its operation is still planned

Fixed on 2026-11-19, with the remedy the maintainer approved (below): `specs.md` §9, §13.4, §14.1 and §15.12, `conformance/vectors/plan.json` and `propagation.json` (`running`), `FlawJobDroppedGone.cfg` and `FlawJobDroppedPlan.cfg` expecting `pass`, and `../scenarios/replica-of-deleted-file.json` and `replica-of-old-version.json`.

`specs.md` §14.1 says that a job whose operation the reprise could not conclude "stays running, and does not run: what is at its target may be its own copy, and nothing is decided of it until that operation is". But two decisions drop such a job: the plan cancels a pending copy that no longer counts, "its row goes, with its job" (§13.4, §14.1), as one on a disk `missing` does; and the decision Gone deletes the file, with its jobs (§15.12, §9). Back, the disk's reprise finishes the operation with no job (§14.2), which `catalog.FinishCreate` records as an `ok` copy by Atoll, never verified, when the file still has a row, and not at all when it has none.

Through a deletion (`FlawJobDroppedGone.cfg`, `replica-of-deleted-file.json`):

1. A file of version 1 on disk 1; its replica's operation reaches `target-synced` on disk 2, which is then lost (`EIO`): the operation stays planned, the job running.
2. The user deletes the file's copy on disk 1.
3. A pass walks disk 1: gone. Disk 2 is missing, and its pending copy is no copy: the decision is Gone, and the file goes with its running job.
4. Disk 2 comes back. Its reprise finds the copy at its name and finishes the operation: no job, no file, nothing recorded.
5. Atoll's copy sits in the library unknown to the catalog (`Inv11_OwnOpsNotUsers` broken): the next walk records it as a user's new file, and the file the user deleted is back, with no trash.

Through the plan (`FlawJobDroppedPlan.cfg`, `replica-of-old-version.json`): the same lost disk; the plan cancels its pending copy and the running job; the user writes version 2 into the copy on disk 1, and the modification, which waits for a running job (§15.4), finds none and is decided; back, disk 2's reprise records its copy of version 1 as `ok`, by Atoll, at the file's level (`ReplicaInSync` broken): one real copy of version 2, where the catalog counts two, until a scrub reads it.

In the code: `internal/solver/plan.go` (`counts`: a pending copy on a disk `missing` is canceled), `catalog.cancelPending` (deletes the job whatever its state), the foreign key of `jobs` (`ON DELETE CASCADE`), and `internal/ops/recover.go` (`recoverCreate` with no job).

The remedy (`running-job`, approved by the maintainer on 2026-11-19): what §14.1 says, everywhere: a pending copy whose job is running is kept, and counts, as a copy on a disk not yet past `missing_grace` does; and a file with a job running waits for its decisions, as its modifications already do (§15.4), until the reprise concludes the operation. The other, a reprise that finds no job sending the copy to the trash, was not chosen. As it landed: the caller tells the plan which pending copies have their job running; such a copy counts as an `ok` copy on its disk would, so no longer past `missing_grace`, nor on a disk `draining`, `failed` or `retired`, holds no room, and is never canceled, by the level, the room, the file leaving or its disk; every decision of its file waits, Rename included, but not a file that only follows a folder renamed; and nothing else takes the file, the job or its pending copy away: a file whose last copy goes otherwise keeps that pending copy and its job (`catalog.dropFileIfEmpty`), as the model's `Emptied` does, and a modification keeps it too.

## Modelling mistakes, found by TLC and fixed

TLC's first traces also showed where the model was wrong, not `specs.md`:

- a user deleting a fresh copy after Atoll synced it read as a commit before the syncs: the check now asks that a name still holding what Atoll put there be durable;
- a part in `tmp/` counted as a copy of the user's version, so a file deleted while its replica was being made read as lost by Atoll: only the library and the trash count (§17, Limits);
- Atoll kept writing to a disk lost and mounted again through the handles of the old mount, which answer `EIO` in reality (§10): a session now holds the mount it confirmed;
- a check of Atoll's own moves outlived a remount that put the copy back, then deleted by the user: it now looks where the file is;
- the liveness instance ran out of operation ids, after which nothing can be journaled: it has more.

## How sensitive it is

Each of these known-bad designs, put into the model one at a time during its writing, was caught by the invariant it breaks: the walk's confirmation without the mount id (`Inv4_UnknownNeverAbsent`), the commit of a copy right after its rename (`Inv7_PipelineOrder`), a move to the trash without the look right before it (`Inv10_DeletionOnlyUnchangedToTrash`), the source's folder synced before the trash entry's (`NoUserDataLost`), the plan without the physical disks (`Inv13_OnePhysicalDisk`), a rename that replaces what took the target (`NoUserDataLost`), and a reprise of `trash` that finishes whatever both names hold (`Inv11_OwnOpsNotUsers`). Two were not: a deletion propagated to a copy found edited is undone by two later defences of `specs.md` (the modification of the same pass drops the trash job, and the look right before the move refuses an edited copy); and without the verdict `own`, nothing changes here, as the journal's planned operations make every copy Atoll is moving `unknown` to a walk, and `own` needs a catalog rebuilt, which the model leaves out.

## What it leaves out

Names and paths (one logical path a file), renames, folders and their operations, cycles and exchanges; NTFS, collisions and refused names; the guards (a decision may always wait, which covers what they hold, not what they count); `deletes.direct = "restore"`; the link guard; restores, purges and `prune`; scrub and repair; Atoll's own view and its journal; the SSD cache and its transfers; copies at once; the catalog rebuilt; metadata-only changes and the operations `chmod`, `chown`, `utimens`, `xattr`; an edit that keeps size and modification time, which only the change time tells, everywhere but in `FlawReplicaRewritten`, where it is a user's only action: the reprise of a move to the trash (§17.2, `same`/`same`) and the look before the `unlink` of a move to the trash on a disk without `RENAME_NOREPLACE` compare size and modification time alone, as the `link` moved the change time, and a copy made on such a disk is looked at after its `unlink`, so that a rewrite between its link and that look is taken (§14.2); the time and its windows. A user acts durably, and with what came before on that disk.

The next models worth writing, by risk: the view's journal `view_ops` and the parts it writes in parallel, taken in by a pass (§21.2); renames and folder renames across disks, with the reprise of `rename` (§15.12); the SSD cache's transfer, the one removal of a user copy no command asks for (§19); the link guard (§17); copies at once on several disks (§14).

## Keeping the model and specs.md in step

`fingerprints.json` lists each section of `specs.md` the model formalizes, with the SHA-256 of its text: from its heading to the next heading of the same level or a higher one, so a section holds its subsections; CRLF read as LF, each line without its trailing spaces and tabs, headings inside fenced code blocks not counting, blank lines at the end dropped, each line ended by LF (its `normalisation` says so in full). `go test ./conformance/models` fails when a section no longer has that text, and says to reread the model against it, update the model or confirm it unchanged, and put the new fingerprint in the same commit (`CLAUDE.md`, "Rust-rewrite readiness"). It also fails when the model cites a section it pins nowhere. Each section pinned says so in a sentence that points here.
