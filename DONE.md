# Done

Finished work, newest first. Each item names its commits; unless it says otherwise, they are on `claude/funny-allen-jyq7pa` (pull request #2). Open work is in [TODO.md](TODO.md), lessons in [LESSONS.md](LESSONS.md).

## Names made durable on NTFS (specs.md §14, §15.12, §17.1, §17.2)

Commit "Sync what a move names before its folder", on `propagate-renames-deletions` (pull request #3). Found by running `test/loopdev`'s rename tests in qemu, which the propagation work had left for later.

- The maintainer's decisions: what a name leads to is fsynced before its folder on every filesystem, after every move and every folder made, and the copy pipeline does the same: invariant 7 now says "fsync the file at its new name, then its folder".
- What it does: `fsx.Dir.SyncEntry` opens what a name leads to and fsyncs it. A rename of a file or a folder, a move to or from the trash, the link path, a move back, and a folder made each sync it before the folder that holds it. A reprise syncs what it finds at the target first, the file again once it takes the other name away, and the file at the source before an undo. A failed fsync loses the disk like any other. A name that no longer leads to what moved has nothing to sync. What Atoll may not open keeps the sync of its folder alone, with a warning.
- Why, measured in qemu on ntfs3, Linux 6.12 and 7.3, with a device that fails right after the target folder's fsync: on NTFS a file or a folder keeps its names in its own record too, which ntfs3 does not write when only the folder is synced. A file renamed and a folder renamed were gone under both names, the folder with a file no other disk held. An entry of the trash and a file restored answered `ESTALE` with their operations `done`, and a folder of the trash just made answered `EINVAL`. The tests of a dying device skipped those three, as a name and its record that disagree are a repair's to judge.
- With the fsync, every test of a dying device passes on ext4, XFS, ntfs3 and ntfs-3g, twice on each kernel, with a rename into another folder added, and so does the rest of `test/loopdev` but `TestNameCorpus`, which finds no `go.mod` in the guest. The fsync costs about 1 ms a move, and about 1.5 ms while another file is written. `syncfs` after each folder fsync also fixed the rename, at 35 to 120 ms a move while another file was written.
- One case stays, on ntfs3: a move to or from the trash that the device interrupts between the syncs of its two folders leaves both names leading to the file, with one link, and `unlink` of either answers `ENOENT` and removes nothing (measured). The reprise took that `ENOENT` for a name gone, recorded the move done, and left the other name there, for a later purge to remove the name the file really has.
- Decided without the maintainer, to confirm:
  - such a reprise now concludes nothing: the operation stays `planned` with a warning, every run on that disk exits with 4, and only a repair of the volume can say which name is the file's;
  - a folder made is synced too, as the trash's showed the same fault.
- Verified: `go test ./...`, the race detector, lint on the default, conformance and loopdev builds, 443 scenarios, and `test/loopdev` in qemu on both kernels. The first new tests failed on the code before, in `fsx`, `jobs`, `trash` and `test/loopdev`. Of 25 mutations that take a new sync or check away or change how its errors read, 24 are caught; the last ignores an open of a folder that the next call opens again, and fails on.

## Propagation: `atoll daemon --until-idle` and `atoll guard` (specs.md §15.1, §15.3, §15.6, §15.10, §15.12, §17.2, §25)

Commits "Propagate renames and deletions, with atoll daemon and atoll guard", "Hold a folder back while one renamed inside it waits", "Test what the mutation runs left standing, and fix a dry run", "State a folder rotation while one waits as a limit", "Make the propagation schemas say what their descriptions say", "Read below a folder by its paths, not by its disk", "Bring every pass on 200,000 files under the 200 MB of §26", "Write down what a port from the documents alone could not tell" and "Tell an edit from what was linked by each of size, time and inode alone", on `propagate-renames-deletions` (pull request #3).

- The maintainer's decisions:
  - the command is `daemon --until-idle`, which does all the work that is due once and exits, so a timer can run it until the watcher exists. It takes no `--yes`: starting it is the consent, and the mass-deletion guard is the safety (invariant 17 now says so);
  - no modifications yet: a copy `edited` or `replaced` is only reported until the copy pipeline (§14);
  - a deletion sends to the trash only the copies that hold the same content;
  - the copies below a folder follow it only when its rename is real;
  - a copy that moved is read again before it counts as verified, and the chunks go with the hash;
  - the guard holds the copies of its disk seen before it tripped;
  - a rotation on one disk while the folder that left waits is a stated limit (below).
- What it does: each pass looks at every disk, gives each copy a verdict (`copy_verdict.json`), and decides a file once every copy that moved or went has done so for `deletes.debounce` (`propagation.json`). A rename made on one disk is carried to the others; a folder renamed is one operation on each other disk where it can be, and its copies follow one by one where it cannot. A deletion sends the other copies to the trash. `atoll guard list`, `confirm --disk` and `reject --disk` answer the guard of §15.6 (`guard.json`). Renames and folders are journaled like the trash, with their reprise and crash points.
- Contracts, as agreed: `schema/migrations/0002_propagation.sql` (`gone_ns`, `gone_kind`, `old_lpath` and `old_lpath_key` on copies, `file_key` and `btime_ns` on entries), `api/schemas/daemon`, `guard-list` and `guard-answer`, `moved` and `path_taken` in `scan`, `old_path` in `status`.
- Eleven adversarial reviews of the code, as each round left it, confirmed 62, 44, 32, 32, 23, 22, 13, 18, 16, 7 and 2 findings, each fixed with a failing test first or stated in the spec as a limit. None since the sixth round lost data; the one in the sixth sent a copy to the trash. The eleventh found a folder decided while one renamed inside it waited, which left what only another disk held below the inner one there for good, and a dry run that gave a queued trash job the guard of another source.
- Mutations: 381 of 390 caught on the code before the third commit. With the run before it, 20 mutants were left. 15 now have a test, each seen to fail under its mutant. One was a bug: since a fix of the eleventh round, a dry run hid the trash job of a refused rename once its file was decided deleted, which the real pass keeps and lists. 3 are equivalent: in `cannotLeave` a copy that stays always has its row on its file, in the dry run's guard a gone copy is never something left beside itself, and a folder around one that waits for its own copies counts the same copies. One is left, tied to the limit below.
- A limit found while testing the mutants, stated with the maintainer: `mv Dx N; mv D Dx` on one disk, while Dx waits for a copy below it on another disk, gives D's entries Dx's name, so the next look reads Dx as a folder made at N, and what only other disks hold below Dx stays there, in the folder now named Dx. Nothing is lost (`daemon-folder-rotation-while-waiting`). The fix is in [TODO.md](TODO.md). Without a wait the rotation works, and so does a swap of two folders on one disk.
- Decided without the maintainer, to confirm:
  - a copy found moved, then gone, restarts its clock;
  - `guard confirm` restarts the clock of the rows it leaves out, and `guard reject` also takes the rows of copies that have not yet followed a rename;
  - a file's hash goes when no copy left is verified;
  - a disk the configuration no longer lists gets no line and no exit 4 for what waits on it, until `disk remove` exists;
  - a moved copy on a disk out of reach waits without being counted in `waiting`, as counting it would change what that field means;
  - on NTFS, a collision with a name that has no row yet is a stated limit: the next walk reports it;
  - the dry run's differences from a real pass are a class of limits in §15.12;
  - a trash job queued for a deletion stands when its file is decided again, and the job of a refused rename stands beside a deletion's: two clauses in the Deletion row of §15.12;
  - a disk's folder renames are decided in the order of their jobs. Nothing says in what order two disks acted, so across disks a copy decided below a folder's old name is below it like any other, and the next look finds it `moved`.
- Verified: `go test ./...`, the race detector, lint on both builds, 443 scenarios (269 new) and every vector. 2,352 real outputs against their schemas, 809 of them from `daemon` and 28 from `guard`, which refuse 303 broken ones. 18 of those passed at first, each ruled out by a description, §10 or §15.6: a disk's reason as its state says, the format of `next_due` and `up_to`, `up_to` as `done` says, and no paths on a guard not tripped. The schemas now enforce them, and nothing a command prints changes.
- Memory and time, measured again on 200,000 files with two copies each: the worst passes peaked at about 212 and 211 MiB, past the 200 MB of §26 (190.7 MiB), against 205 and 208 MiB before the last seven rounds, and a scan after a rename of a folder holding everything at 195 MiB. The peak was in Go's heap, and up to 25 MiB above what sampling saw: lists of every file grown entry by entry, each for a moment twice its size. Sized once, with no job list for a plan whose jobs nothing runs, one string where the catalog gives a path twice, a compact row for each copy not where its row says, and a scan that lets go of what its looks found before its plan, the highest is now about 187 MiB for that scan and about 181 MiB for a pass. The time had grown, unseen by any test: the passes after every copy of a disk went took about two minutes each, against 7 to 13 s. Four queries that read below a folder were led by the disk, and so read every copy of that disk, once for each of the 800 folders it lost, and a fifth read the old path of every copy of every disk. They now read by the folder's paths, a test pins their plans, and those passes are back at about 8, 7 and 15 s.
- What the first commits left, done since:
  - `test/loopdev`'s rename tests ran in qemu on Linux 6.12 and 7.3, and found ntfs3 losing a renamed file: the section above.
  - The older mutation lists ran again on the code as it is: 1,006 mutations. Six survived their own list's judge; judged by every test, three were caught, and the three left were real: on the link path to the trash, an edit that kept the modification time, an edit of the same size, and a replacement of the same size and time each passed for what was linked. Each now has a case in `trash`, and without the inode the replacement lost its only name. Fourteen mutations no longer compiled; rewritten to compile, each is caught.
  - A port of the three propagation vector files, written in Python from the documents alone, passes their 188 cases. It had to guess in 14 places, each now a sentence of specs.md or of the vectors' README, and a second read by the port found each settled. Three gaps no vector reaches are in [TODO.md](TODO.md), and so is a port judged on the scenarios, which needs a port of the daemon.

## The journal and the trash: `atoll prune` and `atoll trash` (specs.md §6.1, §9, §10, §15.10, §17, §25)

Commit "Send surplus copies to the trash, with atoll prune and atoll trash", on `main`. The first code that writes to a disk.

- The maintainer's decisions:
  - `prune` and `trash` come first, ahead of the copy pipeline, as they need only a move inside one filesystem;
  - a session confirms each disk by reading its head past the page cache, once, before it first reads or writes that disk. The handle comes first, so that what is confirmed is what the handle leads to;
  - a copy goes only if the copies that stay reach the level, unchanged and verified, and the level is the larger of the file's row and what the configuration says now;
  - a verified copy with another hash loses `verified_ns`, or `prune` would count it as good and send the good one to the trash;
  - an exclusive `flock` on the catalog for every command that writes;
  - a reprise tells a file edited in place from an intact one, and starts the retention of what it finishes again;
  - a restore looks first and journals nothing for `path-taken` or `name-collision`. A missing folder is an operation of its own, `kind = mkdir`, and the root is never made;
  - a purge keeps another file at the entry's place, with `changed`, and so does a restore;
  - another root, workdir or id is refused while a disk holds copies, entries, trash or planned operations;
  - a walk reports a name that `lstat`, `openat` or `readlink` cannot reach in `unreadable`, with no row, and the disk stays `online`.
- What it does: one journaled operation per copy, in `internal/catalog/ops.go`, `internal/ops` and `internal/trash`, moved by `renameat2` with `RENAME_NOREPLACE` or by `link`, `fsync` and `unlink`, then synced. A reprise resolves what a crash left from a table of what each name holds. Crash points in the conformance build (`crash_at`) make a `kill -9` at each step a plain scenario.
- Measured on ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g, on Linux 6.12 and 7.3, in throwaway probes and now in `test/loopdev`:
  - the move keeps the inode, the modification time and the creation time;
  - ntfs-3g and lowntfs-3g give `EINVAL` for `RENAME_NOREPLACE` whatever the name, so only `link` can tell a refused name;
  - on a device that fails, XFS answers the move with success, and only the first `fsync` after it gives `EIO`: a file can be back at its source after a remount. So nothing is `done` on the strength of the move alone;
  - ntfs3 has no journal of its own: a device that fails during a write can leave a name and its record that disagree, which it answers with `ESTALE` or `EINVAL`. So the test of a dying device skips those cases on ntfs3, where only a repair can say more.
- Four adversarial reviews, on the code as each round left it: 85 findings confirmed, then 48, 33 and 3. What they found, and each with a failing test first:
  - in the third round, on the link path, a source's name gone during the `fsync` made Atoll unlink the file's last name, while a crash one line earlier kept the file;
  - in the third round too, an edit in place during the `fsync`s before a move, which keeps the inode, reached the trash unseen. The copy is now compared with its row right before the move, and a reprise syncs before it looks, so that nothing long comes between a look and what it decides;
  - a move called `undone` with what it left not durable, so that the next move went through a folder that could vanish;
  - a reprise that removed a source's name without looking again after a long `fsync`;
  - `--older-than 0s` purging what its own reprise had just finished, which its dry run had not shown;
  - hard links: every name of a file moves together, so the rows of the others follow, unless they already disagreed, which is a change nobody has looked at.
- A port written from the documents alone, never reading the Go code, passes all 174 scenarios and every vector. Its failures along the way were mostly gaps in the documents, which is how the event levels, the order of a reprise and several exit codes came to be written down.
- Verified: `go test ./...`, the race detector, lint and every scenario. 351 real outputs against their schemas, which refuse 196 broken ones. 301 mutations of this work, and the 432 of the older lists, all caught, and 2 more that did not compile. The older lists ran before the last small fixes, which touch none of what they test. Left out of the lists as equivalent: the two guards that only save a query, and a kind check that only an inode reused by a folder could reach. `test/loopdev` passes on Linux 6.12 and 7.3, on each driver, with ntfs3 skipped where a dying device left its volume in need of a repair. Of 13 mutations judged on real mounts, 10 were caught; the fsync of the target folder removed, in either direction, survives there, as the next fsync catches the same failure on these drivers, and a unit test pins the order; and one mount check is pinned by a unit test.
- Not measured: real disks, and disks asleep.

## `atoll status` (specs.md §7, §15.11, §25)

Commit "Report what the catalog holds, with atoll status", on `main`.

- The maintainer's decisions:
  - it reads the catalog and the configuration and touches no disk, so it says what the last scan saw, and `atoll disk list` is what looks now;
  - `--path` keeps a file or a folder by whole components, in the counts and in `files`, and a path the catalog does not hold is an empty list with exit code 0;
  - a file is at its level by what can be read now, so a copy on a `missing` disk does not count, even before `health.missing_grace`, unlike in the plan. The JSON holds integers only, so no rounding can differ between two versions, and the share of bytes correctly placed waits for `balance`;
  - dates at a fixed width (§29.3), so both versions write the same bytes;
  - with no eligible copy, a file keeps its original, as it keeps its size and date (§7). A scan used to write NULL;
  - `corrupt` and `conflict` now come before `unavailable` in the order of §4.
- A bug in the previous commit, found by writing the scenario for the rule above: `files.state` is all that remembers a conflict, and `unavailable` came first. So when every disk of a file in conflict went away, the file became `unavailable`, then `ok` when they returned, with copies whose content differs. Nothing acted on it yet, and a later phase could have taken those copies for identical.
- Found along the way:
  - holding the files of a large `--path` would have gone far past §26, so they are written as they are read, before the counts, which only the end knows. 200,000 files in JSON peak at about 21 MiB. A catalog that fails half way therefore leaves an unfinished output, and a script goes by the exit code;
  - §25 said a flag given twice is a usage error, and `--json`, `--full` and `--size` accepted it, in every command. Only a scenario was missing;
  - pages are read apart, so a scan running meanwhile can show a copy on a disk not yet in the list, which would have crashed the command;
  - a port written from the documents disagreed with Go on the original of a file with no readable copy, which is how that rule was found.

## The first scan: `atoll init` and `atoll scan` (specs.md §9, §15.10, §25)

Commit "Record what the disks hold, with atoll init and atoll scan", on `main`.

- The maintainer's decisions:
  - a copy that changed or went away is only reported, in `changed` and `absent`, until propagation exists: no state in the catalog would be true for it;
  - `atoll init` is the only one to create a catalog, so a mistyped path cannot start again from nothing in silence;
  - the same size on two disks is `ok`, unverified, until `--full` reads them, and another size is a `conflict` at once;
  - a walk is written whole or not at all, and the rules of `--json`, ids and retired disks are in §15.10 and §9.
- `internal/watch` walks a disk through handles and compares it with the catalog, `internal/catalog` holds the queries, and `internal/fsx` the handles, with `Fault` for injecting errors. Every scan gives each file its original, its policy and what the plan says, so the solver now runs on real data. Its jobs are not kept, as nothing runs them yet.
- The conformance build reads its clock and gives its disks a capacity, and the runner gets `expect_catalog`, so the scenarios compare the catalog itself.
- Found along the way:
  - the first version peaked at 432 MiB on 200,000 files, past §26. Rows kept while the plan ran, a map of known copies, and each string copied before being shared were most of it. A merge in the order of paths, reading by pages and a soft limit bring the first scan to about 180 MiB and the next to about 145 MiB, and it got faster: 12 s against 19 s;
  - a file whose times move while it is read gives a hash of nothing in particular, so `--full` compares them before and after;
  - a port written from the documents showed that what the walk did not reach was reported `absent`, and through another mount `--full` would have read a file of that mount.
- Not done: propagation, jobs, `daemon --until-idle`, the scan in the daemon, hashing inside `maintenance.window`, all in [TODO.md](TODO.md).

## Disk identity and the mount check (specs.md §10, §25)

Commit "Identify disks and check their mounts, with atoll disk list", on `main`.

- The measurements are in [docs/research/disk-identity.md](docs/research/disk-identity.md): both kernels, every driver, each shape of mount, dm-error, and USB disks really unplugged in qemu.
- The maintainer's decisions:
  - the identifier comes from the superblock, with no external tool, which takes root or the `disk` group;
  - what changes name rules (`ads`, `nocase`, `iocharset`, `casefold`, `ascii-ci`, `ignore_case`), the `ntfs` driver of Linux 7.3 and a `ro` mount make a disk `readonly`. No `media/`, a bind of a folder and a btrfs subvolume make it `missing`;
  - `atoll disk list` shows it, with `api/schemas/disk-list.schema.json`. The clock of the conformance build and `daemon --until-idle` wait for `scan`, their first use;
  - a new replica keeps the date of its source (§14): taking the original's would change invariant 6 and §9, and leave the copies that differ as they are.
- `internal/disks` has the mount table, the superblock and the check, pinned by `mountinfo.json`, `superblock.json` and `disk_check.json`, and the observer of each build. The delivered binary has no way to take a folder for a disk, and a test builds it and tries.
- The runner has `unmount` and `mount` steps, for a failed mount.
- Found along the way:
  - an unplugged ext4 keeps answering `statx` and `getdents`, from the cache: only the device node tells;
  - lowntfs-3g `ignore_case` shows nowhere, so a check asks for the root in the other case and compares inodes;
  - Linux 7.3 has a third NTFS driver, reached by `mount -t ntfs` where no helper exists;
  - Docker Desktop's 9p writes `path=C:\` in its options, which the first parser took for a broken table, so one mount that is none of the pool's would have made every disk `missing`. Now a backslash that escapes nothing stays, and a line that cannot be read says nothing about the others. Only the test that runs the delivered binary found it;
  - on XFS `ascii-ci` the root answers in the other case too, which hid the feature behind `ignore-case` until the real test ran;
  - checks that did nothing went: the mount-root attribute, as the entry says all of it, the length of a line, as a short one has no separator, and a test for a relative source, as it leads nowhere anyway.
- Not done: the confirmation before writes, the handle on the verified root, the daemon's check, `Atoll/disk.toml`, the topology check and the remount, all in [TODO.md](TODO.md).

## Conformance scenarios and the first CLI (specs.md §25, §29.4)

Commit "Run conformance scenarios, with rules check and rules explain", on `main`.

- The format is in [conformance/scenarios/README.md](conformance/scenarios/README.md), and `conformance/runner` runs it.
- The maintainer's decisions:
  - a test build (`-tags conformance`) takes folders as disks, so the delivered binary has no way to skip a mount check, and real mounts stay with `test-loopdev`;
  - that build reads the time from a control file, and `daemon --until-idle` does what is due and stops, so nothing waits and Go and Rust reach the same ids. Changes reach scenarios through rescans, and the watcher is tested apart;
  - one JSON file per scenario, with tree and JSON expected exactly. `${ANY}` stands for the wording of a message and a `*` component for an operation's id, and the runner always looks for hidden names and for anything outside `media` and `Atoll`;
  - `atoll rules check` and `atoll rules explain` come first, as they need only the configuration.
- `cmd/atoll` has those two commands and `--config`, with `api/schemas/rules-check.schema.json` and `rules-explain.schema.json`.
- No code reads the control file yet, as nothing needs a disk: that comes with disk identity, and the file steps have only run against a fake binary so far and in two scenarios.
- Found along the way:
  - to Go, `time.LoadLocation("Local")` is the machine's zone, so `timezone = "Local"` would have loaded in Go and failed in Rust. It is now refused;
  - §24.4 had no list of what a later phase brings. It lived only in `phase.go`, so no other version could reproduce those warnings. The table is now in §24.4, and two scenarios pin every row of it, asked for and not;
  - nothing said that `--size` needs its unit, or that a usage error comes before the configuration is read;
  - a field on the wrong step, such as `tree` on `atoll`, would have checked nothing, so the runner refuses it. One `*` entry now takes one file, so it cannot cover two operations.
- Not covered: the default path `/etc/atoll/atoll.toml`, which a scenario must not touch.

## Catalog schema (specs.md §9)

Commit "Define the catalog schema", on `main`.

- `schema/migrations/0001_initial.sql`, and `catalog.Open`, which sets and checks the pragmas of §9 and applies the migrations.
- The maintainer's decisions:
  - chunk hashes in a table of their own: changing the state of a 100 GB file writes 5 KB instead of 780 KB;
  - `STRICT` tables and `CHECK` on the closed lists: through the Go driver, a string written to a `BLOB` column of an ordinary table is stored as `TEXT` with no error;
  - `disks` is a copy of the configuration, rewritten at each start;
  - no `chunk_size`, a `job_chunks` table instead of JSON, and the lists of job states and event levels.
- Found while writing it: `ops.kind` had no `chmod`, though §6.1 propagates permissions; and `files.id` and `ops.id` name things on disks, so they use `AUTOINCREMENT` and never come back.
- Vectors: `catalog.json`, on `catalog_fixture.sql`: plain SQL, which any SQLite must agree on.

## Placement plan (specs.md §12.4, §13.4)

Commit "Plan which copies to create and how to mark the others", on `main`.

- `solver.MakePlan` gives the jobs (`refresh`, then `replicate`), the marks of the copies (`surplus`, back to `ok`, canceled) and the states of each file. It moves and removes nothing.
- The maintainer's decisions:
  - placement only for now: moves, hysteresis and reachable capacity come with `balance` and `capacity` (phase 1b);
  - with `spread_mode = "strict"`, the domains to cover are those of every data disk not `failed`, `retired` or `draining`, so a file waits for a missing bay rather than doubling up on the other;
  - `overflow` concerns the rule's `tags` and `domains` only;
  - the details of §13.4: the order of files and of disks, which copies count, what is kept beyond the level.
- Report: [docs/research/placement-order.md](docs/research/placement-order.md). Where room is short, all orders protect the same bytes; smallest first protects 98% of files at 60% full, against 68% in the library's order and 6.5% largest first.
- Two things the plan found along the way:
  - a refresh writes a copy, so it takes the same name checks as creating, which §17 already required for a name the driver refuses;
  - a plan for 200,000 files first allocated about 270 MB. `names.TargetNames` is now a tree of folders, and a plan holds 76 to 97 MB on top of its input, in about 1 s at worst.
- Vectors: `plan.json`. Property tests in `internal/solver`: no two copies on a disk, `min_free` respected, no copy past the level, marks never away from the last readable or user copy, the order of the input changes nothing, a plan fed back adds nothing, and nothing past the capacity of §13.1.

## Effective policy of a file (specs.md §11.3, §12.4, §24.2)

Commit "Compute the effective policy of a file", on `main`.

- `rules.Effective` takes the first rule that matches and fills what it leaves out from the pool-wide values. `Policy.Sources` says where each value comes from, for `atoll rules explain`. The policy holds what the configuration asks for: the fallbacks of §12.2 are for the code that acts on it.
- The maintainer's decisions:
  - `settle` and `cache` never go with a size condition, as they are read before the size is known;
  - `tags_mode = "strict"` covers `tags` and `domains`, and a disk must satisfy both lists;
  - `strict` from `[placement]` asks nothing of a rule with no list, and `strict` written on such a rule is an error.
- While the size is not known, a rule with a size condition does not match, following §19.
- Vectors: `policy.json`, including the example of §24.1.
- Classifying one file takes 1.8 µs on an i7-12700K with the 10 rules of that example, so 200,000 files take about 0.4 s.

## Configuration schema and loader (specs.md §24)

Commit "Define the configuration schema and load it", on `main`.

- Report: [docs/research/toml.md](docs/research/toml.md). go-toml v2.4.3 and Rust's `toml` 1.2.0 agree on all 746 files of the official corpus; BurntSushi v1.6.0 accepts 16 invalid files they refuse. All three accept TOML 1.1 syntax.
- The maintainer's decisions:
  - `github.com/pelletier/go-toml/v2`, and TOML 1.1 in the contract;
  - `integrity.chunk_size`, `trash.keep_overwritten`, `placement.move_originals` and `read.striping` removed: each contradicted the spec or served nothing;
  - a setting of a later phase is accepted with a warning, and `view.engine = "atoll"` is an error until phase 2a;
  - a `[maintenance]` table for the window, the rescans and the catalog backups, which the other windows follow;
  - rule properties keep their sections of §24, in the order rule, section, preset, default;
  - identifiers in lowercase ASCII, and a required, unique `label` on each rule;
  - `0s` and `0%` refused where they would remove a protection or stop a task.
- `schema/config.schema.json` is the one list of keys, types, allowed values and defaults: `internal/config` reads it at load time. Decoding into a struct cannot tell `Pool_ID` from `pool_id` in either Go library, so the loader checks the generic tree first.
- Vectors: `config.json`. `deploy/atoll.toml` is the example of §24.1, byte for byte.
- A port written in Rust from the spec alone, without the Go code, passes the same vectors. It found the sentences that did not settle a case (a second byte order mark, the case of `\uXXXX`, `catalog` equal to a mount, and others), which were then fixed.

Effective policy, and everything that reads the file from disk, is still open: [TODO.md](TODO.md).

## BLAKE3 library and hashes (specs.md §16, §29.7)

Commit "Hash files and chunks with zeebo/blake3", on `main`.

- Report: [docs/research/blake3.md](docs/research/blake3.md). On one core, zeebo is 36% to 42% faster than lukechampine, with 2 allocations against 10,756 per 64 MiB.
- The maintainer's decisions:
  - `github.com/zeebo/blake3`, main branch, pinned to commit `28a397d`, for its NEON and SVE2 backends;
  - an empty file has no chunks.
- Contract: `conformance/vectors/blake3.json` holds 17 cases, whose hashes come from the official `b3sum`, 10 lengths also checked against the official test vectors.
- Code: `integrity.Hasher`, one read feeding both hashes, with no I/O. Nothing calls it outside its tests yet.
- Not measured: AVX-512, and throughput on real arm64.

## Capacity: `min_free`, strict domains, reachable (specs.md §12.4, §13)

Commit "Define the capacity with min_free and strict domains", on `main`.

- The maintainer's decisions:
  - a disk's size in the formula is its capacity minus `min_free`, for the disks the solver can write to;
  - with fewer copies than domains, each copy goes to a different domain, and a disk with no `domain` forms its own;
  - the reachable capacity is a dry run of the solver, plus the formula on the space left. It waits for the solver.
- Derived: with strict domains, the formula applies to the domains' sizes when k < m; otherwise the capacity is the smaller of the formula and the smallest domain.
- Contract: `conformance/vectors/min_free.json` holds 23 cases, and `capacity_strict.json` 28. `capacity.json` gains the three §13.2 cases with `min_free`.
- Code: `value.MinFree` and `solver.StrictCapacity`. Nothing calls them outside their tests yet.
- Tests:
  - a property test places one-byte files by trying every choice on small pools, and compares with the formula;
  - the cases were recomputed outside Go, in Python with exact integers, from what the numbers mean.

## Copies that differ at `disk add` and `catalog rebuild` (specs.md §4, §9, §15.7, §18)

Commit "Define copies that differ when a disk is added or the catalog rebuilt", on `main`.

- The maintainer's decisions:
  - copies with the same content and different modification times are in sync, each keeps its own time, and nothing is written to them;
  - `files.mtime_ns` holds the original's time;
  - at `catalog rebuild`, different content is a `conflict`, whatever the times.
- Only a later change is applied to the other copies, which makes the times equal again.
- No code and no vector: this lives in the scan and the catalog, which do not exist yet. The scenario is listed in §27, and the scenario format is still to be defined.

## Modification and change times (specs.md §9, §14)

Commit "Define what happens to times a disk or the catalog cannot hold", on `main`.

- Measured on Linux 6.12.115 and 7.3.1:
  - ext4 and XFS clamp a time without an error: a copy to ext4 of an NTFS file dated 1601 holds 1901, and `cp` exits 0;
  - Go's `UnixNano` wraps: 1601 becomes a date in 2185.
- The maintainer's decisions:
  - the copy is made with the time the disk keeps, which is recorded per copy; it stays in sync and the difference is never applied to the other copies. Invariant 6 now says so;
  - a time out of range is stored at the nearest limit, and Atoll sets times from a copy's `stat`, never from the catalog.
- Contract: `conformance/vectors/time_ns.json` holds 26 cases.
- Code: `solver.TimeNS`, with `CreationTimeNS`, in `times.go`. Nothing calls them outside their tests yet.

## Choosing the original (specs.md §7)

Commit "Define the choice of the original", on `main`.

- Measured per driver, on Linux 6.12.115 and 7.3.1: report in [docs/research/file-times.md](docs/research/file-times.md). ext4 reports a crtime of 0 as 1970, and ntfs3 a FILETIME of 0 as 1601, which does not fit in `btime_ns`.
- The maintainer's decisions:
  - creation times are compared in exact nanoseconds;
  - a date of exactly 0, or out of range, counts as absent;
  - every copy that is in sync and can be read is eligible, `surplus` and `draining` included;
  - a file in conflict keeps its previous original while that copy is eligible.
- Contract: `conformance/vectors/original.json` holds 37 cases, and `creation_time.json` 23. `absent` joins the vector flags.
- Code: `solver.SelectOriginal` and `solver.CreationTimeNS`. Disks and the catalog do not exist yet, so nothing calls them outside their tests.
- Left open: modification and change times out of range, in `TODO.md`.

## The name report and the state a file shows (specs.md §4, §15.9)

Commit "Define the name report and the state a file shows", on `main`.

- The maintainer's decisions:
  - one finding per name to fix, the shortest path with the problem, in a flat sorted list;
  - `ext4` and `ntfs` are always checked, whatever the pool holds;
  - exit code 0 whenever the check ran, with or without findings;
  - the name states come before `degraded` in the order of states.
- Also stated:
  - placement and job order read the copies, never the state;
  - the report reads the catalog only, so it wakes no disk;
  - collisions are listed between siblings; a difference in a parent is covered by the finding on that parent;
  - a well-formed logical path is defined in §4.
- Contract: `api/schemas/names-check.schema.json`, the first of the `--json` schemas. `conformance/vectors/names_check.json` holds 56 cases, and `file_state.json` 27.
- Code: `names.Check` and `solver.ShownState`. The CLI and the catalog do not exist yet, so nothing calls them outside their tests, and nothing writes this JSON yet.

## Total path length (specs.md §12.4, §15.8)

Commit "Limit the total path length on mergerfs branches", on `main`.

- Measured:
  - with mergerfs 2.40.2, a copy past 4095 bytes on its branch is listed and `stat` gives `ENOENT`, so it protects nothing, and a deletion or a rename through the view leaves it behind;
  - on Windows 11 with `LongPathsEnabled` = 1, six built-in tools work at 259 UTF-16 units and fail at 260, while `cmd /c type` and PowerShell work at 300.
- The maintainer's decisions:
  - a path past the branch limit is a hard constraint of the solver;
  - a path past the Windows limit is only listed by `atoll names check`.
- Contract: `conformance/vectors/branch_path.json` holds 24 cases, and `windows_path.json` 22.
- Code: `names.FitsBranch` and `names.FitsWindows`. The solver and `atoll names check` do not exist yet, so nothing calls them outside their tests.
- Tests:
  - the cases were generated and checked outside Go, in PowerShell with .NET byte and UTF-16 counts;
  - 24 code mutations each make a vector fail, and 14 mutations of the vector shape each make the test fail.

## ntfs3 `ads` and chunks in the trash (specs.md §10, §15.2, §17)

Commit "Refuse ntfs3 ads and fix the chunk layout in the trash", on `main`.

- Measured in qemu on ntfs3, on Linux 6.12.115 and 7.3.1, re-reading after dropping the caches:
  - on 7.3.1, with `ads` on by default and with or without `windows_names`, a rename through mergerfs onto `m:n` succeeds and replaces the content of the file `m`, even when that was its only copy;
  - `ads=0` stops it, and 7.3.1 then behaves as 6.12, which behaves as ntfs-3g;
  - ntfs3 holds names that differ only by case.
- The maintainer's decisions:
  - ntfs3 with `ads` is refused at `disk add`, and a disk found with it later becomes `readonly`, with an alert;
  - overwritten chunks go to `Atoll/trash/<date>/<op-id>.chunks/<logical path>/<index>`, which holds a file name of 255 bytes.
- Also stated in §17: the index format, and a copy whose name the driver refuses, which cannot reach the trash: it stays, is reported, and is never refreshed or repaired.
- No code: the trash and `disk add` are not written yet. The tests to write are listed in §27.

## Renames to a name a disk refuses (specs.md §15.2, §15.3)

Commit "Decide what happens on a rename a disk refuses", on `main`.

- Measured: when one branch refuses a rename and another accepts it, mergerfs returns success and removes the source from the branch that refused. A file loses that copy, with no trash. A folder that is not empty stays at the old name, and the merged view shows both folders. §15.2 said that renames reach every copy; it now states this.
- The maintainer's decisions:
  - the copy left at the old name goes to the trash, with the new reason `renamed`, and the solver looks for another compatible disk;
  - `windows_names` stays recommended, with its cost stated in §15.8.
- Also stated in §15.3 and §6.1:
  - a copy changed meanwhile stays, as a `conflict`;
  - the renamed copy counts as a user copy, and the user's rename counts as the command that removes a user copy;
  - the mass-deletion guard counts these copies;
  - `renamed` is purged as late as `deleted`.
- No code: the check already exists (`names.CheckPath`, `names.TargetNames`), and propagation is not written yet. The scenario is listed in §27, and the scenario format is still to be defined.

## Name collisions on NTFS targets (specs.md §15.7)

Commit "Refuse name collisions on NTFS targets", on `main`.

- The maintainer's decisions:
  - a colliding copy is blocked: a hard constraint of the solver, not only a report;
  - `entries` gets an `lpath_key` column, so folders are looked up the same way as files.
- Also stated in §15.7:
  - names already side by side on an NTFS disk, created outside Atoll, are only reported;
  - a case-only rename does not collide with its own old name;
  - between two new names, the first in the plan's order gets the disk.
- Contract: `conformance/vectors/ntfs_collision.json` holds 39 cases.
- Code: `names.TargetNames`. The solver and the catalog do not exist yet, so nothing calls it outside its tests.
- Tests:
  - the cases were checked outside Go, in PowerShell with .NET normalization and an upcase built from the rule of §15.8;
  - a property test compares it with a pairwise comparison, on names mixing case, NFC and NFD forms and invalid UTF-8;
  - 11 code mutations each make a vector fail, and 11 mutations of the vector shape each make the test fail.
- Measured: ntfs-3g and, in a later measurement, ntfs3 hold both names, so the drivers do not protect against collisions. Not measured: what Windows does with such names.

## Capacity formula (specs.md §13.1)

Commit "Add the capacity formula and its vectors", on `main`.

- The maintainer's decisions:
  - the result is rounded down to whole bytes;
  - with more copies than disks, the capacity is 0.
- Also stated in §13.1: k is at least 1, no size is negative, and the total fits in a signed 64-bit integer. Any other input is an error.
- Contract: `conformance/vectors/capacity.json` holds 42 cases.
- Code: `solver.Capacity`, with integers only.
- Tests:
  - the values were checked outside Go, in PowerShell with exact integers, against the formula and against the largest x such that k copies of x bytes fit;
  - a property test compares the function with that second definition on 20,000 small pools;
  - of 20 code mutations, 19 make a vector fail. The last, a function that sorts its own input, makes the property test fail.

## One spelling for vector flags (specs.md §29.4)

Commit "Refuse vector flags other than true", on `main`.

- `internal/vectors` refuses `error` and `no_key` with any value but `true`, in every vector file and in the corpus. Only `name_compat.json` refused `"error": false` before.
- `window.json`: an error case can no longer carry `"at": ""`.
- No vector file changed: none used a refused spelling.
- Mutation checks on all nine vector files that have a flag: a flag written as false, an error case with an expected result, and a case with neither. Each test fails on each mutation.

## Name compatibility per filesystem (specs.md §15.8)

Commits `f7b6502`, `10c1c2b`, `b9fbeb3`, and the follow-up after the review.

- Researched:
  - the Windows 10 and 11 rules;
  - ntfs3 on Linux 7.2 and 7.3, measured in qemu;
  - ntfs-3g and lowntfs-3g;
  - ext4, including `casefold`;
  - XFS, including `ascii-ci`, and btrfs.

  Report: [docs/research/name-compatibility.md](docs/research/name-compatibility.md).
- The maintainer's decisions:
  - every NTFS disk follows the Windows rules, as a fixed table in both versions;
  - reserved names as Windows 10 reads them, with `CONIN$` and `CONOUT$`;
  - at most 255 bytes per component on NTFS too;
  - ext4 `casefold`, XFS `ascii-ci` and NTFS `nocase` are refused at `disk add`;
  - every component is checked, folders included;
  - a closed list of six reason codes.
- Contract:
  - `conformance/corpus/names/names.json` holds 101 names with their reasons on ext4, xfs, btrfs and ntfs;
  - `conformance/vectors/name_compat.json` holds 27 whole-path cases;
  - vector files must not hold an unpaired surrogate escape. Keys must be lowercase ASCII and unique, integers plain, and base64 written exactly as the standard encoder writes it, because Go's decoder would otherwise accept what Rust's rejects.
- Code:
  - `internal/names/compat.go`: `ComponentReasons`, `CheckPath`;
  - `internal/vectors` loads the corpus and checks the text rules.
- Tests:
  - an independent Python model agrees on 300,000 random names;
  - mutation checks;
  - fuzzing;
  - `sudo make test-loopdev` creates the corpus on real ext4, XFS and btrfs loop images, and on NTFS with every driver present (ntfs3, ntfs-3g, lowntfs-3g), with and without `windows_names`;
  - an independent review: two spec-only ports agreed with Go on 5.4 million names and paths, and the corpus was replayed on ntfs3 (Linux 7.2 and 7.3), ntfs-3g and lowntfs-3g. The review found lowntfs-3g `ignore_case`, now a refused variant.

## Regex grammar and weight (specs.md §29.5)

Commits `9579074` to `a4e82fa`.

- A positive grammar, the common subset of Go `regexp` and Rust `regex`, which Atoll checks itself in both versions. It is limited to 1000 bytes in NFC form, with Go's nested-repeat product rule.
- A weight limit of 1000:
  - every element that adds to the automaton weighs at least 1 per copy, and a property weighs 100;
  - Rust keeps `size_limit` at 10 MiB and sets `nest_limit` to 1000;
  - the costliest regex found needs 0.41 of the limit.
- A pattern with more than 30 non-starters in a row is rejected. They are counted in the NFKD of each character, exactly where Go's NFC inserts U+034F.
- Class pairs `--`, `&&` and `~~` are rejected anywhere in a class. The grammar applies to the NFC form, and errors quote the typed text.
- Verified by several rounds of independent review:
  - spec-only Rust validators;
  - 4.5 million Go/Rust engine pairs;
  - 5.2 million weight comparisons;
  - adversarial searches for the costliest regexes in Rust.
- Documented and kept as is, by the maintainer's choice:
  - Rust needs an 8 MiB thread to compile the deepest regexes in debug builds;
  - with `ignore_case`, a pathological regex can take about 0.7 s to compile.

## `lpath_key` (specs.md §15.7, §15.8)

Commits `aa7da56`, `5cd9ab7`, `31e7278`, `5fed1ed`, `63e9cf3`.

- The key is NFC of the NTFS upcase table, plus `ς` → `Σ`, applied to NFD.
  - The table is generated from Unicode 5.1 data: 973 entries.
  - It was checked byte for byte against `$UpCase` from Windows 10 images, and from a Windows 11 volume read by the maintainer.
- `meta.lpath_key_version` names the language, the library, its release and its Unicode version: `go golang.org/x/text v0.43.0 17.0.0`. Any difference recomputes every key.

## NFC and case folding in rule matching (specs.md §11.3)

Commits `7db3410` to `34a644d`.

- Research report: [docs/research/nfc-and-case-folding.md](docs/research/nfc-and-case-folding.md).
- NFC comes from `golang.org/x/text/unicode/norm` v0.43.0, applied run by run on invalid UTF-8, only through `internal/names`.
- `ignore_case` uses Unicode simple case folding, exactly as regex `(?i)`.
- Rule conditions are implemented: `path`, `filename` and `ext` (a list), with globs and regexes.
- Vectors: `nfc.json`, `match.json`, `lpath_key.json`.

## Earlier work

Commits `b1cdf1d` to `e384a9c`.

- The specification, CLAUDE.md, the project skeleton, the license, and the `api/`, `schema/` and `deploy/` folders.
- The conformance vector format.
- Value grammars: durations, sizes, percentages, time windows.
- The in-house glob matcher.
