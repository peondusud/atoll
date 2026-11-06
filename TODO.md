# To do

Open work, in a suggested order. Finished work is in [DONE.md](DONE.md), lessons in [LESSONS.md](LESSONS.md). Items marked **decision** change a contract (specs.md §29.3) or an invariant: ask the maintainer first (CLAUDE.md).

## Names (specs.md §30)

To decide before the solver, the scanner and `atoll names check` need them:

- **decision**, before phase 2a: should Atoll's own view refuse, with an error, a rename that a copy's filesystem refuses, instead of following §15.3?
- Pin in `test/loopdev` what mergerfs and ntfs3 do with a refused name or an `ads` stream (`docs/research/name-compatibility.md`), re-reading through a cold cache, so that a release that changes it fails a test. The measurements so far were throwaway scripts in qemu.
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
- `catalog rebuild` must start `ops.id` past the ids in each disk's trash, so that a new operation's folder cannot meet an old one (§17): rebuilding empties `sqlite_sequence`.
- Memory of the daemon on 200,000 files (§26): `atoll scan` stays under the limit, and a daemon holds more than a scan does.
- What the mount check of §10 leaves to do for disks:
  - **decision** (§30): what confirms a disk before a session of writes (invariant 3). Should it read the head past the page cache, which wakes the disk, as the writes will?
  - a directory handle on the verified root, and the device id checked again before each job (invariant 3), with the first command that writes;
  - the check every 30 s in the daemon, which must wake no disk (§18), and the alert when a disk becomes `missing` or `readonly`;
  - `Atoll/disk.toml`, `atoll disk show`, and the inode size in the ext4 superblock, which gives `time_precision_ns`;
  - at `disk add`: the block-device topology (R-06), another filesystem with the same identifier (§10), and the warning about `chattr +i`;
  - the remount after a USB reset (§10).
- The conformance build (§29.4): `daemon --until-idle`, with the first jobs. It reads its disks and its clock from `ATOLL_CONFORMANCE` already.
- Reading the time back in the copy pipeline (§14).
- `disk add` and `disk remove`, and the other `atoll disk` commands.
- The scan in the daemon: at startup, after an overflow, every `maintenance.rescan_every`, and hashing new files inside `maintenance.window` (§15.1, §18). `atoll scan` hashes only with `--full`.
- `time_precision_ns` when `disks` is written (§9), which the comparison of times across filesystems will need.
- `deferred` and `manual` modes, with `--now` and `--force`.
- Propagation of renames, deletions and modifications, with grouping and the mass-deletion guard, including renames to a name a disk refuses (§15.3). Until then `atoll scan` reports them in `changed` and `absent`, and the catalog does not follow.
- Trash, including the `.chunks` folders and copies whose name the driver refuses (§17).
- Detection-only scrub. `files.state` holds one state, so a file both `corrupt` and in conflict shows `corrupt`, and `repair` (phase 1b) must look for the conflict again before it clears that.
- `mergerfs-config` and runtime branches.
- `atoll names check`: the CLI over `names.Check`, reading the catalog, with the JSON of `api/schemas/names-check.schema.json`.
- ntfy alerts.
- The operation journal (`ops`) and crash recovery, with fault injection.
- The break-glass test and the failed-mount test in CI.
