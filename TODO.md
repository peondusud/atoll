# To do

Open work, in a suggested order. Finished work is in [DONE.md](DONE.md), lessons in [LESSONS.md](LESSONS.md). Items marked **decision** change a contract (specs.md §29.3) or an invariant: ask the maintainer first (CLAUDE.md).

## Names (specs.md §30)

To decide before the solver, the scanner and `atoll names check` need them:

- **decision**, before phase 2a: should Atoll's own view refuse, with an error, a rename that a copy's filesystem refuses, instead of following §15.3?
- Pin in `test/loopdev` what mergerfs and ntfs3 do with a refused name or an `ads` stream (`docs/research/name-compatibility.md`), re-reading through a cold cache, so that a release that changes it fails a test. The measurements so far were throwaway scripts in qemu; `make test-qemu` now runs `test/loopdev` on both kernels, but its guest has no mergerfs yet.
- Measure on the maintainer's Windows machine, with their go-ahead, what a report says is not established: which reserved names Windows 11 still refuses, and what Windows does with `Film.mkv` beside `film.mkv` on an NTFS volume (a VHD).
- NTFS 8.3 short names: never decide that a file exists with `stat` alone, and treat an `EEXIST` without a visible exact entry as name-incompatible.
- `disk add`: refuse ext4 `casefold`, XFS `ascii-ci`, ntfs3 `nocase`, ntfs3 `ads` and lowntfs-3g `ignore_case`, which the mount check of §10 already reports. Watch `FS_CASEFOLD_FL` on folders under `<root>/`.
- The `ntfs` driver of Linux 7.3 (§22): measure its name rules, what it does with `:`, and its times, should anyone want it. Until then a disk mounted with it is `readonly`.

## Pure core still without vectors (specs.md §29.4)

- **Solver, phase 1b** (§13.3): moving copies when rebalancing (`atoll balance`), the hysteresis, and the reachable capacity as a dry run of that plan (`atoll capacity`), which needs the moves to mean what it says. Moving an Atoll copy also fixes a file whose copies cover too few domains, which stays `degraded` until then.
- **Solver, phase 2b** (§12.4): `colocate` and the `ordered` and `percent` strategies. Until then the caller gives the plan `most-free` and no `colocate` (§12.2): `solver.Placement` has neither.
- **BLAKE3**: run `conformance/vectors/blake3.json` on real arm64 and AVX-512 hardware, measure throughput there, and move `zeebo/blake3` to a tagged release when one holds the arm64 backends (`docs/research/blake3.md`).

## Contracts not yet defined (specs.md §29.3)

Every item here is a **decision**:

- `api/openapi.yaml` and `api/schemas/` for the `--json` outputs.

## Phase 1a, not started (specs.md §28)

- Configuration in the shell (§24.3, §10):
  - in `atoll rules check`, the rules that no file reaches or that an earlier one hides (§11.3);
  - `--help`, the French translation of the CLI, and human output that writes durations as the configuration does: `atoll rules explain` prints `settle_ns = 600000000000`;
  - the fallbacks of §12.2 for the modes, when jobs act on them: `rules.Policy` holds what the configuration asks for, and `atoll scan` already gives the plan only what has an effect;
  - `disk add` and `disk remove` editing the file: every other byte kept, the result re-read before it replaces the file.
- **decision**: a nested btrfs subvolume below the root has its own inode numbers, so `file_key` does not identify a file there, and what happens to a hard-linked file can write another file's row, which then reads as changed. Also, nothing can move from it to the trash, as `rename` gives `EXDEV`. Should a walk treat a folder on another device like a folder on another mount?
- `catalog rebuild` must start `ops.id` past the ids in each disk's trash and in each `tmp/`, as must `atoll init` over a catalog that went missing, so that a new operation's folder or `<op id>.part` does not meet an old one (§14.2, §17): both restart `sqlite_sequence`. Until then such a meeting costs one `io-error`, and the next run takes another id.
- Memory of the daemon on 200,000 files (§26): `atoll scan` and every pass of `daemon --until-idle` measured stay under the limit, the highest at about 187 MiB, a scan after a rename of a folder holding everything. A daemon that runs for good, with its watcher, will hold more.
- What the mount check of §10 leaves to do for disks:
  - the check every 30 s in the daemon, which must wake no disk (§18), and the alert when a disk becomes `missing` or `readonly`;
  - `Atoll/disk.toml` and `atoll disk show`, which could show `time_precision_ns`;
  - at `disk add`: the block-device topology (R-06), another filesystem with the same identifier (§10), and the warning about `chattr +i`;
  - the remount after a USB reset (§10).
- `disk add` and `disk remove`, and the other `atoll disk` commands. `disk remove` must refuse a disk with `planned` operations: on a disk no longer in the configuration they wait for good, and every command that writes exits with 4.
- The scan in the daemon: at startup, after an overflow, every `maintenance.rescan_every`, and hashing new files inside `maintenance.window` (§15.1, §18). `atoll scan` hashes only with `--full`.
- The modes `immediate`, which needs the watcher to know a file closed and stable, and `scheduled`, which runs inside `[replication].window`: until then both fall back to `deferred`, which `daemon --until-idle` runs, settle counted from when a file was first seen (§12.2, §12.3, §14.1). The `manual_alert_after` alert for a `manual` file left unprotected.
- What the copy pipeline leaves for later (§14): one copy at a time, in one pass, is all the daemon runs, so `max_parallel_per_disk` waits for its loop; the creation time set after the rename on Windows (§7).
- A folder renamed on one disk into the old name of one that waits there (`mv Dx N; mv D Dx`, while a copy below Dx on another disk has a clock running, is held by the guard, or cannot be read): what only other disks hold below Dx stays there, in the folder now named Dx (specs.md §15.12, `daemon-folder-rotation-while-waiting`). The fix holds back what lands in that old name, folders and copies moved in from elsewhere, until the one that waits is decided. It needs that wait known before the files of a pass are decided, and must not block a cycle, where each folder lands in the other's old name.
- The owner applied to the other copies (§15.4, Limits), which the maintainer chose to leave for later when the mode came: it would take `copies.uid` and `copies.gid`, recorded as `copies.mode` is, and operations `chown` journaled with their reprise. Until then a `chown` is a metadata change that applies nothing. The grouping of events per path comes with the watcher (§15.1).
- What modifications leave as limits (§15.4, §15.12): an overwriting move (`mv P Q`, Q there) and a swap of two names on one disk, and a copy replaced by anything but a regular file, which are only reported. `deletes.direct = "restore"` (§15.5), which the copy pipeline now allows, still propagates nothing.
- What pull request #3 left of renames and deletions: judge a port written from the documents on the 269 new scenarios, which needs a port of the daemon, and settle three gaps no vector reaches (specs.md §15.12): a file reaching its own path as a later target, a behind copy first seen after the file left, and whether a file both not due and held says wait or held. The decisions taken there without the maintainer, listed in [DONE.md](DONE.md), are to confirm.
- A volume that ntfs3 left with names that disagree (§17.2) shows only as a warning in the log and exit 4 on every run on that disk: say what to do, which is a repair of the volume, in an alert once alerts exist, and in `atoll status`.
- The rest of the trash (§17): `[trash].max_size` and its purging order, the `.chunks` folders with `repair`, `restore --to` with the copy pipeline, the link guard, the alert for a name the driver refuses, the reasons other than `surplus`, `deleted` and `renamed`, and removing Atoll's own folders below the root once they are empty (§6.1). A folder's attributes other than its mode and owner wait for the pipeline.
- Detection-only scrub. `files.state` holds one state, so a file both `corrupt` and in conflict shows `corrupt`, and `repair` (phase 1b) must look for the conflict again before it clears that.
- `mergerfs-config` and runtime branches.
- `atoll names check`: the CLI over `names.Check`, reading the catalog, with the JSON of `api/schemas/names-check.schema.json`.
- ntfy alerts, among them the two guards of a disk tripped: for now only the `guards` of a pass, exit code 3 and `atoll guard list` say so.
- The operation journal (`ops`) covers the trash, the renames of propagation, files and folders, and the copies (`create`) so far (§14.2, §17.1, §17.2). Each new kind of operation needs its own reprise, with kill -9 at each step and fault injection.
- The break-glass test in CI (`test/ci/run.sh`), which waits for `mergerfs-config`. The failed-mount test runs in `test/loopdev` (`TestAFailedMountIsAMissingDisk`).
