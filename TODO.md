# To do

Open work, in a suggested order. Finished work is in [DONE.md](DONE.md), lessons in [LESSONS.md](LESSONS.md). Items marked **decision** change a contract (specs.md §29.3) or an invariant: ask the maintainer first (CLAUDE.md).

## Names (specs.md §30)

To decide before the solver, the scanner and `atoll names check` need them:

- **decision**, before phase 2a: should Atoll's own view refuse, with an error, a rename that a copy's filesystem refuses, instead of following §15.3?
- Pin in `test/loopdev` what mergerfs and ntfs3 do with a refused name or an `ads` stream (`docs/research/name-compatibility.md`), re-reading through a cold cache, so that a release that changes it fails a test. The measurements so far were throwaway scripts in qemu.
- Measure on the maintainer's Windows machine, with their go-ahead, what a report says is not established: which reserved names Windows 11 still refuses, and what Windows does with `Film.mkv` beside `film.mkv` on an NTFS volume (a VHD).
- NTFS 8.3 short names: never decide that a file exists with `stat` alone, and treat an `EEXIST` without a visible exact entry as name-incompatible.
- Mount checks:
  - how to detect lowntfs-3g `-o ignore_case`, which mount information does not show (the FUSE daemon's command line?);
  - ntfs3 must use `iocharset=utf8`;
  - a disk already in the pool that shows `ads` in `/proc/mounts` becomes `readonly`, with an alert (§10);
  - report `nocase`;
  - a listed entry whose `lstat` fails is a name the driver cannot read, never a missing file.
- `disk add`: refuse ext4 `casefold`, XFS `ascii-ci`, ntfs3 `nocase`, ntfs3 `ads` and lowntfs-3g `ignore_case`. Watch `FS_CASEFOLD_FL` on folders under `<root>/`.

## Pure core still without vectors (specs.md §29.4)

- **Rules → effective policy** (§11.2, §11.3, §12.2):
  - ordered rules, where the first match wins;
  - `[defaults]` and the `strategy` presets;
  - the properties of §11.2.

  Needs the configuration schema below.
- **Solver** (§12.4, §13.3): placement, hard constraints (name compatibility, NTFS collisions through `names.TargetNames`, and the branch path limit through `names.FitsBranch`), the job plan, the reachable capacity as a dry run of that plan, and property tests.
- **decision**: **BLAKE3** (§16): choose a Go implementation with SIMD after a throughput benchmark. It is a new dependency, and its license must be compatible with GPL-3.0-or-later.

## Contracts not yet defined (specs.md §29.3)

Every item here is a **decision**:

- `schema/config.schema.json`: the JSON Schema of the TOML configuration (§24). Unknown keys are errors.
- `schema/migrations/NNNN_name.sql`: the catalog schema (§9), including `meta.lpath_key_version`.
- `api/openapi.yaml` and `api/schemas/` for the `--json` outputs.
- The format of `conformance/scenarios/` and the runner. The runner refuses any scenario today.

## Phase 1a, not started (specs.md §28)

- Disk identity and mounts: FS UUID or NTFS serial, mount-point checks, states.
- Reading times: creation times (§7) through `statx`, or `system.ntfs_crtime` on ntfs-3g and lowntfs-3g, and `solver.CreationTimeNS`; modification and change times through `solver.TimeNS`; reading the time back in the copy pipeline (§14).
- `disk add` and `disk remove`.
- Scan.
- File and chunk hashes.
- `deferred` and `manual` modes, with `--now` and `--force`.
- Propagation of renames, deletions and modifications, with grouping and the mass-deletion guard, including renames to a name a disk refuses (§15.3).
- Trash, including the `.chunks` folders and copies whose name the driver refuses (§17).
- Detection-only scrub.
- `mergerfs-config` and runtime branches.
- `status`, with the state of `solver.ShownState`.
- `atoll names check`: the CLI over `names.Check`, reading the catalog, with the JSON of `api/schemas/names-check.schema.json`.
- ntfy alerts.
- The operation journal (`ops`) and crash recovery, with fault injection.
- The break-glass test and the failed-mount test in CI.
