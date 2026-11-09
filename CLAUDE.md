# CLAUDE.md — Atoll

## Language
- Talk to the maintainer in French.
- Write every repository file in English: docs, code, comments, commit messages and CLI messages (translatable; the CLI ships with a French translation).

## Mission
Atoll adds file-level redundancy to heterogeneous disks (SATA, USB DAS) that hold a media library (Plex, Emby, Jellyfin), **without moving existing files**.
Each file under `<disk>/media/` can have 0 to 2 full replicas on other disks, **at the same relative path and name**.
A merged view (mergerfs in phase 1, Atoll's own FUSE view from phase 2a) presents each file once: the in-sync copy with the oldest creation time.
Go now (phases 1–2, Linux only). A Rust rewrite follows as a drop-in replacement (specs.md §29), then Windows, in Rust. No distributed storage.
`specs.md` is the source of truth: read it before any non-trivial change.

## Guiding principles
- **Break glass**: stopping or uninstalling Atoll must break nothing. Existing paths stay intact, every disk remains a normal readable tree, no hidden files, and plain mergerfs at the same mount point reproduces the merged view. Reject any feature that violates this, even if it would simplify the code.
- **Never lose a user's data or edit**: when in doubt, keep both versions and report a conflict.
- **Rust-ready**: everything persisted or exchanged follows the language-neutral contracts of specs.md §29.3, and behavior is pinned by the shared conformance suite (§29.4).

## Architecture (details: specs.md §8)
- `catalog`: SQLite (WAL, `synchronous=FULL`). Rebuildable cache; the truth is on the disks.
- `disks`: identity (FS UUID or 64-bit NTFS serial), mount checks, states, SMART, sleep, remount after a USB reset.
- `rules`: ordered rules → effective policy of a file.
- `solver`: target placement, original selection and job plan. Pure, deterministic function.
- `jobs`: priority queue (replicate, rename, trash, refresh, repair, transfer).
- `ops`: journal of Atoll's own filesystem operations, written before each operation.
- `watch`: fanotify (with process attribution) or inotify, scans and rescans, grouping of events per path.
- `integrity`: BLAKE3 whole-file and 4 MiB chunk hashes, scrub, repair, quarantine, duplicates.
- `view`: mergerfs configuration and runtime branch changes (phase 1), FUSE view through cgofuse (phase 2a).
- `trash` (with the optional link guard), `notify` (Plex, Emby, Jellyfin), `alerts`, `cache` (phase 2b).
- `cmd/atoll`: CLI and `daemon`, in one binary.

## Stack and commands
- Go, latest stable release; Linux amd64 and arm64.
- SQLite: `modernc.org/sqlite` (no cgo). TOML: `github.com/pelletier/go-toml/v2`, read as a generic tree, never decoded straight into a struct (specs.md §24.3). Regex: `regexp`, limited to the subset in specs.md §29.5. Globs: in-house matcher, no third-party glob library. NFC and NFD: `golang.org/x/text/unicode/norm` (v0.43.0 or later), only through `internal/names`, run by run on invalid UTF-8. System calls (`statx`, `renameat2`, `openat`, fanotify): `golang.org/x/sys/unix`.
- BLAKE3: `github.com/zeebo/blake3`, pinned to a commit of its main branch for its arm64 backends (`docs/research/blake3.md`).
- cgo and libfuse3 only for the FUSE view (phase 2a).

```bash
make build              # go build -o bin/atoll ./cmd/atoll
go test ./...           # unit and property tests, including conformance/vectors; no root
go test -race ./...     # mandatory before any commit touching jobs, ops, watch, integrity or catalog
make conformance        # black-box runner: conformance/scenarios against a conformance build
make test-integration   # disks simulated with directories
sudo make test-loopdev  # ext4, NTFS, XFS, btrfs on loop devices; USB drops with dm-error / dm-flakey
golangci-lint run ./...
./bin/atoll --config ./dev/atoll.toml daemon --foreground
```

## Repository layout
```
cmd/atoll/             CLI and daemon
internal/catalog/      queries (the schema lives in schema/migrations/)
internal/config/       loading the configuration: schema check, values, relations (pure)
internal/disks/        identity, mount checks, states, SMART, remount
internal/layout/       paths: copies, trash, tmp, links
internal/rules/        parsing, matching, effective policy (pure)
internal/glob/         in-house glob matcher (pure)
internal/names/        exact paths, collision keys, per-filesystem compatibility (pure)
internal/solver/       placement, capacity, original selection, job plan (pure)
internal/value/        value grammars: durations, sizes, percentages, time windows (pure)
internal/jobs/         queue, atomic copy pipeline, per-path locks
internal/ops/          operation journal, crash recovery
internal/watch/        fanotify / inotify, rescans, event grouping, file identity
internal/integrity/    hashing (pure), scrub, repair, quarantine, duplicates
internal/view/         mergerfs configuration and runtime branches, FUSE view
internal/fsx/          disk I/O abstraction (real, and with fault injection)
internal/vectors/      loads conformance/vectors/ for unit tests
internal/trash/  internal/notify/  internal/alerts/  internal/cache/
api/                   openapi.yaml, schemas/ for --json outputs
schema/                migrations/NNNN_name.sql, config.schema.json
conformance/           vectors/, scenarios/, corpus/names/, runner/ (language-neutral)
test/                  integration, faults, break glass
deploy/                systemd units, sample configuration
docs/research/         research reports behind open design decisions (not contracts)
```

## Non-negotiable invariants
1. Never delete or overwrite the last verified copy of a file. Whatever a refresh or a repair overwrites goes to the trash first.
2. Under `<disk>/<root>/`, perform only the writes listed in specs.md §6.1, each journaled in `ops` before it runs. Never remove the last user copy of a file; removing any user copy needs an explicit command.
3. Write to a disk only after confirming, in the current session, that its mount path is a mount point of the expected filesystem. Do all I/O through a handle on the verified root (`openat`-style calls) and re-check the device id before each job. Never create a disk's `<root>/` folder.
4. EIO, ENOTCONN and ENODEV mean "disk unknown", never "file absent". An empty mount point is a missing disk, never a mass deletion. A missing disk is never considered empty.
5. No hidden files or folders. Nothing written outside pool disks except the catalog and the configuration.
6. A replica has exactly the same relative path and name, byte for byte, as its source. It keeps the source's modification time, as far as the target filesystem can hold it (specs.md §14), never its creation time.
7. Copy pipeline: `<workdir>/tmp/` on the target disk → chunk hashes → restore metadata → fsync → re-read without cache and verify → rename without replacing (`RENAME_NOREPLACE`, or `link` + `unlink`) → fsync folder → restore parent folder times → catalog commit. Resolve the destination at commit time, under the per-path lock.
8. Identify a disk by its FS UUID or 64-bit NTFS serial; never by `/dev/sdX`, drive letter, Windows volume GUID or USB serial. Refuse two disks with the same identifier, and two pool filesystems on one physical disk.
9. Decide content changes from hashes and change times, never from the modification time alone. Corruption requires unchanged size, modification time and change time, and a mismatch confirmed by a second read without cache. Compare times at the coarser precision of the filesystems involved.
10. A replacement (a copy replaced on one disk and removed from the others) is a modification, never a deletion. A deletion propagates only to copies unchanged since they were last in sync, and only to the trash; the mass-deletion guard pauses propagation.
11. Never propagate Atoll's own operations (recorded in `ops`) or view operations (mergerfs process) as user actions. Before treating a missing copy as deleted, look for it in Atoll's own `trash/` and `tmp/`.
12. Catalog: SQLite WAL with `synchronous=FULL`; a filesystem operation never runs before its `ops` row is committed.
13. Never two copies of a file on the same physical disk.
14. Atoll's own view presents the in-sync copy with the oldest creation time, never a stale, corrupt or unavailable copy. With mergerfs, move stale and corrupt copies out of `<root>/` (to the trash) as soon as they are found.
15. Never alter a file name. A name incompatible with a target filesystem → another disk, or the `name-incompatible` state. The logical key is the exact path, never normalized; NFC/NFD and case folding serve only to detect collisions (`lpath_key`) and to match rules (specs.md §11.3), never to name or identify a file.
16. No automatic disk adoption.
17. Every operation that moves or deletes offers `--dry-run` and asks for confirmation (`--yes`). The daemon takes no `--yes`: starting it is the consent, and the mass-deletion guard is the safety (specs.md §15.12).
18. Only the `solver` decides placement. No other module moves files on its own initiative.

## Rust-rewrite readiness
- Persisted and exchanged formats follow specs.md §29.3: SQL migration files, TOML configuration with a JSON Schema, OpenAPI for the local API, JSON schemas for `--json`, documented exit codes. Never `gob` or any other language-specific encoding.
- Paths are BLOBs in SQLite; times are integer Unix nanoseconds UTC; in JSON, `path` or `path_b64`.
- Pure core packages (`config`, `rules`, `glob`, `names`, `solver`, `value`, hashing in `integrity`) have no I/O and no global state, and load their test cases from `conformance/vectors/`.
- Every behavior change adds or updates a conformance vector or scenario before the code.
- Sort every output explicitly; never depend on map iteration order or random seeds.
- Reject regexes outside the grammar of specs.md §29.5, the common Go/Rust subset, when the configuration loads; Atoll checks it itself before the engine compiles.
- The conformance runner talks to the binary only through the CLI and the filesystem; it imports nothing from `internal/`.

## Cross-platform rules (Linux now, Windows in the Rust version)
- Logical key: the exact relative path, `/` separator; bytes on Linux, WTF-8 on Windows (keeps unpaired UTF-16 surrogates).
- `ignore_case` uses Unicode simple case folding, exactly as regex `(?i)` (data from `unicode.SimpleFold`); the NTFS upcase table serves `lpath_key` only (specs.md §11.3, §15.8). Never `strings.EqualFold`, `strings.ToLower` or `x/text/cases` on names.
- ext4, XFS, btrfs: 255 bytes per path component. Every NTFS disk follows the Windows rules, checked as a fixed table in both versions (specs.md §15.8): valid UTF-8, at most 255 bytes and 255 UTF-16 code units; characters 1–31 and `" * : < > ? \ |` forbidden; no trailing dot or space; reserved names as Windows 10 reads them (the part before the first dot, every trailing space removed: CON, PRN, AUX, NUL, CONIN$, CONOUT$, COM1–9, COM¹–³, LPT1–9, LPT¹–³). Every component is checked, folders included. NTFS names are case-insensitive. ext4 `casefold`, XFS `ascii-ci`, ntfs3 `nocase`, ntfs3 `ads` and lowntfs-3g `ignore_case` are refused at `disk add`.
- Deep trees: relative operations (`openat`) on Linux; `\\?\` prefix on Windows.
- `rename` is atomic only within one filesystem: always go through `<workdir>/tmp/` on the target disk.
- Creation time: `statx` birth time on Linux (ntfs-3g: `system.ntfs_crtime` attribute); `first_seen` when unavailable. On Windows, set it explicitly after the final rename (NTFS tunneling), and open files with `FILE_SHARE_DELETE`.
- Verification re-reads bypass the page cache (`posix_fadvise(POSIX_FADV_DONTNEED)`).
- SMART polling uses `smartctl -n standby`; rescans, scrubs and disk backups run inside the time windows.

## Testing rules
- Every fix starts with a failing test: unit test, conformance vector or scenario.
- `jobs`, `ops`, `watch`, `integrity`, `disks`: fault injection required (EIO, ENOSPC, disk disappearing, kill -9 at each pipeline step).
- USB drops are simulated with dm-error, dm-flakey or `scsi_debug` (the mount stays live and returns EIO), not only with a forced unmount.
- `solver`: property tests (never two copies on a disk, `min_free` respected, capacity matching specs.md §13, user copies never removed implicitly).
- Changes: atomic saves through mergerfs, metadata-only changes, in-place edits that keep the modification time, hard links.
- `names`: the corpus in `conformance/corpus/names/`, with the reasons each filesystem refuses each name for (specs.md §15.8); `sudo make test-loopdev` creates its accepted names on every NTFS driver present.
- Break-glass test in CI, on a pool with no stale copy and no conflict: daemon stopped and removed, plain mergerfs mounted from `mergerfs-config` → identical listing and content, no hidden file, test library unchanged.
- Failed-mount test: an empty mount point yields `missing`, never deletions, and nothing is written there.
- No test touches real disks, except `sudo make test-loopdev` or the `hwlab` hardware bench.
- FUSE, loop-device and device-mapper tests need privileges: run them on a local machine or a CI runner that allows them; cloud sessions may not.

## Conventions
- Errors: `fmt.Errorf("context: %w", err)`. No `panic` outside initialization.
- All disk I/O goes through `internal/fsx`; never call `os.*` directly from business logic.
- `context.Context` everywhere; every long loop can be cancelled.
- Structured logs (`log/slog`).
- Every destructive action writes an event to `events` BEFORE it runs.

## Ask before acting
- Changes to a contract (specs.md §29.3): SQL schema, on-disk layout (`root`, `workdir`, `trash/`, `tmp/`, `links/`), configuration keys, local API, `--json` schemas, exit codes.
- Changes to §6.1 (writes under `media/`), to the original-selection rule or to any invariant.
- A new dependency (its license must be compatible with GPL-3.0-or-later).
- Any command run on a real machine with mounted disks.
- Any ambiguity in specs.md: ask, do not invent.

## Reference
Requirements, layout, writes under `media/`, rules, redundancy strategy, change handling, CLI, contracts, conformance suite and roadmap: `specs.md`.
