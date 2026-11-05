# To do

Open work, in a suggested order. Finished work is in [DONE.md](DONE.md), lessons in [LESSONS.md](LESSONS.md). Items marked **decision** change a contract (specs.md §29.3) or an invariant: ask the maintainer first (CLAUDE.md).

## In progress

- Act on the independent review of name compatibility (a spec-only Rust port, the corpus replayed on ntfs3 and lowntfs-3g, and a review of the spec and corpus). Fix what it finds, vectors and corpus first.

## Names (specs.md §30)

To decide before the solver, the scanner and `atoll names check` need them:

- **decision**: case collisions on NTFS targets.
  - Should a collision with the target's existing and planned siblings block the copy, or only be reported?
  - Folders also collide, and the whole-path `lpath_key` misses that: `Films/a.mkv` against `films/b.mkv`. Add a key for each prefix, or a key on `entries`?
- **decision**: a user rename to a name that another copy's filesystem refuses (§15.3, §6.1). What happens to the copy left at the old name?
- **decision**: the trash layout `<logical path>.chunks/` cannot hold a file name of 249 bytes or more (§17).
- **decision**: total path length:
  - mergerfs opens `<branch>/<path>` as one string of at most 4096 bytes;
  - Windows applications without long-path support stop at 259 UTF-16 units.

  Should either be a refusal or a warning?
- **decision**: the output of `atoll names check`:
  - JSON schema, sort key, exit code;
  - which paths and filesystems it covers;
  - the precedence between `name-incompatible` and `degraded`.
- NTFS 8.3 short names: never decide that a file exists with `stat` alone, and treat an `EEXIST` without a visible exact entry as name-incompatible.
- Mount checks:
  - ntfs3 must use `iocharset=utf8`;
  - report its `ads` option (on by default from Linux 7.3) and `nocase`;
  - a listed entry whose `lstat` fails is a name the driver cannot read, never a missing file.
- `disk add`: refuse ext4 `casefold`, XFS `ascii-ci` and NTFS `nocase`. Watch `FS_CASEFOLD_FL` on folders under `<root>/`.

## Pure core still without vectors (specs.md §29.4)

- **Capacity formula** (§13.1): `capacity.json` can be written now. Decide two points first:
  - rounding to whole bytes (floor);
  - k greater than the number of disks (capacity 0).

  Later: what s_i is (raw capacity or minus `min_free`), the capacity with strict domains, and the capacity reachable without moving originals.
- **decision**: **original selection** (§7). Open points:
  - comparing creation times at the coarser precision, and its rounding;
  - whether surplus copies, and copies on standby, readonly or draining disks, are eligible;
  - a "previous original" input for conflicts.
- **Rules → effective policy** (§11.2, §11.3, §12.2):
  - ordered rules, where the first match wins;
  - `[defaults]` and the `strategy` presets;
  - the properties of §11.2.

  Needs the configuration schema below.
- **Solver** (§12.4, §13.3): placement, hard constraints (name compatibility included), the job plan, and property tests.
- **decision**: **BLAKE3** (§16): choose a Go implementation with SIMD after a throughput benchmark. It is a new dependency, and its license must be compatible with GPL-3.0-or-later.

## Contracts not yet defined (specs.md §29.3)

Every item here is a **decision**:

- `schema/config.schema.json`: the JSON Schema of the TOML configuration (§24). Unknown keys are errors.
- `schema/migrations/NNNN_name.sql`: the catalog schema (§9), including `meta.lpath_key_version`.
- `api/openapi.yaml` and `api/schemas/` for the `--json` outputs.
- The format of `conformance/scenarios/` and the runner. The runner refuses any scenario today.

## Phase 1a, not started (specs.md §28)

- Disk identity and mounts: FS UUID or NTFS serial, mount-point checks, states.
- `disk add` and `disk remove`.
- Scan.
- File and chunk hashes.
- `deferred` and `manual` modes, with `--now` and `--force`.
- Propagation of renames, deletions and modifications, with grouping and the mass-deletion guard.
- Trash.
- Detection-only scrub.
- `mergerfs-config` and runtime branches.
- `status`.
- ntfy alerts.
- The operation journal (`ops`) and crash recovery, with fault injection.
- The break-glass test and the failed-mount test in CI.

## Questions for the maintainer

- Should Claude watch pull request #2 (reviews and CI)?
