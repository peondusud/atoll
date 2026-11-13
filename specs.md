# specs.md — Atoll

> Functional and technical specification — v0.4, November 4, 2026.
> **Atoll**: every disk an island, every file a twin. Command `atoll`, visible work folder `Atoll/` on each disk. Functionally close to StableBit DrivePool, without reusing its name, code or user interface.
> Decisions in §2 are validated; open questions are listed in §30.

## 1. Context and goals

A home media library (Plex, Emby or Jellyfin; MKV and MP4 videos from a few MB to over 100 GB) is spread across heterogeneous JBOD disks: internal SATA disks (ThinkNAS enclosure around a Lenovo M920q, ASM1166 controller) and a USB DAS (TerraMaster D4-320). The disks are already full, and existing Plex libraries depend on their current paths.

Atoll adds file-level redundancy **without moving existing files**. Each file can have 0, 1 or 2 full replicas on other disks, stored at the **same relative path**, so every disk remains a normal, self-sufficient folder tree. A merged view, in the style of mergerfs, presents each file once. Atoll also provides integrity checking, a trash, rules by path, file name, extension and size, and notifications to media servers.

Measurable goals:

- **G1 — Break glass.** Stopping or uninstalling Atoll changes no existing path. Plain mergerfs mounted over the same disks at the same mount point reproduces the merged view.
- **G2 — Fault tolerance.** A file with N replicas survives the loss of N disks and stays visible in the merged view without any manual action.
- **G3 — Readability.** Beyond that, every surviving disk is readable on its own: whole files, standard filesystems, the same folder tree, no proprietary format.
- **G4 — Integrity.** Silent corruption is detected by scrubs and repaired whenever a healthy copy exists; a user's edit is never mistaken for corruption.
- **G5 — Disk sleep.** Reading a file wakes a single disk; replication, scrubs, rescans and rebalancing are batched into time windows.

## 2. Validated decisions

| # | Decision |
|---|---|
| D1 | DrivePool model: pool of heterogeneous disks, full file copies, no parity in v1. |
| D2 | Linux first (Debian 13 / openmediavault 8, SMB share); Windows PCs map the share as a network drive. A native Windows engine and pool drive (WinFsp) come after the Rust rewrite; member disks stay accessible. |
| D3 | Break glass: current disk paths stay valid; user copies never move without an explicit action. |
| D4 | No hidden files on the disks: outside the managed root, everything Atoll writes lives in a visible folder. |
| D5 | The managed root is a folder inside each disk, `media/` by default. |
| D6 | 0 to 2 replicas per file (x1 to x3). |
| D7 | Mirror layout: a replica keeps the same relative path and name under `media/` on its own disk. |
| D8 | The merged view presents the in-sync copy with the oldest creation time. |
| D9 | File and folder renames and moves are handled without re-copying, including extended names, except on a disk that refuses the new name (§15.3). |
| D10 | No automatic disk adoption. |
| D11 | Ordered rules on path, file name, extension (glob or regex) and minimum or maximum size. |
| D12 | BLAKE3 hash of the whole file and of each chunk; duplicate report based on hashes. |
| D13 | Trash with retention. |
| D14 | Filesystems: NTFS and ext4; XFS and btrfs optional. |
| D15 | SSD cache in a later phase, designed in from the start, with a configurable residency time before transfer to HDD. |
| D16 | No distributed storage: one pool = one machine. |
| D17 | Configurable redundancy strategy (presets and rules); notifications to Plex, Emby and Jellyfin. |
| D18 | Go for phases 1 and 2, then a Rust rewrite as a drop-in replacement; every persisted or exchanged format is language-neutral (§29). |

## 3. Non-goals

- Distributed or multi-machine storage, network replication.
- Block RAID, striping, object storage, storage-level deduplication.
- SnapRAID-style parity (possible study after v1, as a rule mode).
- Managing files outside a disk's `media/` root.
- Encryption (use LUKS or BitLocker underneath Atoll).
- Replacing backups: the trash recovers from mistakes, not from ransomware.
- Hosting databases or virtual machines, including Plex's own database.
- Media servers reading member disks directly once replicas exist: they would see every replicated file twice.

## 4. Glossary

- **Disk**: a mounted filesystem that belongs to the pool. Exactly one pool filesystem per physical disk.
- **Root** (`root`): the managed folder inside a disk, `media/` by default.
- **Work folder** (`workdir`): visible folder `Atoll/`, sibling of `media/`, holding `trash/`, `tmp/` and, optionally, `links/`, `disk.toml`, `manifest.b3sum` and `catalog/`.
- **Logical path**: exact path relative to the root, `/` separator, kept byte for byte on Linux and as WTF-8 on Windows, never normalized (e.g. `Films/Avatar (2009)/Avatar.mkv`, §15.8). It is well-formed when it has no empty component, no `.` or `..` component and no NUL byte.
- **Copy**: an instance of a logical path on one disk, at `<disk>/media/<logical path>`.
- **User copy** / **Atoll copy**: a copy that existed before Atoll indexed it, or that a user or an application wrote or modified, is a user copy; a copy Atoll created (replica, refresh, transfer) is an Atoll copy. Stored as `created_by`. Protection decisions rely on it (§6.1).
- **Current version**: the size and hash that every copy of a file must match, and the modification time of its original (§7). A copy's own modification time may differ: where its filesystem cannot hold that time (§14), or where it already differed when Atoll first saw the copy (§18).
- **In-sync copy**: a copy that matches the current version.
- **Original**: the in-sync copy with the oldest creation time. The merged view presents it (§7). It drives presentation only, never protection.
- **Replica**: any other in-sync copy.
- **Level**: number of wanted replicas, 0 to 2, displayed as x1 to x3 (number of copies).
- **Merged view**: a single tree that presents each logical path once: mergerfs in phase 1, Atoll's own view from phase 2.
- **Failure domain**: group of disks likely to fail together (same DAS, same power supply, same controller).
- **Tag**: label on a disk (`sata`, `usb`, `ssd`…), used by rules; an identifier (§29.5).
- **Residency**: time a file stays on the SSD cache before its transfer to HDD.
- **File states**: `ok`, `pending` (replicas to create), `degraded` (fewer replicas than wanted), `unprotected` (x1, or manual mode waiting), `unavailable` (no reachable copy), `corrupt`, `conflict`, `name-incompatible`, `name-collision` (§15.7). A file shows one state, the first that applies in this order: `corrupt`, `conflict`, `unavailable`, `name-incompatible`, `name-collision`, `degraded`, `pending`, `unprotected`, `ok`; it is `ok` when no other applies. `corrupt` and `conflict` come first as `files.state` is all that remembers them, while a scan works out the others again: behind `unavailable`, an outage of a file's disks would make the catalog forget that its copies differ. So a file `corrupt` whose copies also turn out to differ stays `corrupt`. The state serves display and alerts; placement and job order read the copies, never the state.
- **Copy states**: `ok`, `pending`, `stale` (does not match the current version), `corrupt`, `unavailable`, `surplus` (beyond the wanted level).
- **Disk states**: `online`, `standby` (spun down), `missing`, `readonly`, `draining`, `failed`, `retired`. The mount check gives the first four (§10); `draining` and `failed` come from a disk's `state` in the configuration, which the disk commands set (§18); `retired` is a disk the configuration no longer lists (§9).

## 5. Requirements

| ID | Requirement | Level | Phase |
|---|---|---|---|
| R-01 | Atoll never moves, renames, deletes or rewrites a user copy except through the writes listed in §6.1 | MUST | 1a |
| R-02 | Stopping or uninstalling Atoll leaves every disk readable, and plain mergerfs at the same mount point reproduces the merged view | MUST | 1a |
| R-03 | No hidden file or folder on pool disks; writes under `media/` are limited to §6.1, everything else goes to the visible work folder | MUST | 1a |
| R-04 | 0 to 2 replicas per file, set by rule | MUST | 1a |
| R-05 | A replica has exactly the same relative path and name as its source | MUST | 1a |
| R-06 | Never two copies of a file on the same physical disk | MUST | 1a |
| R-07 | Disks of different sizes and filesystems in one pool | MUST | 1a |
| R-08 | Explicit addition of an already filled disk, with no move or reformat; files with the same content at the same path are adopted as existing copies | MUST | 1a |
| R-09 | No automatic disk adoption | MUST | 1a |
| R-10 | File and folder renames and moves propagated to all copies without re-copying, except on a disk that refuses the new name (§15.3) | MUST | 1a |
| R-11 | Extended names (Unicode, long names, special characters) handled without silent alteration | MUST | 1a |
| R-12 | BLAKE3 hash of the whole file and of each 4 MiB chunk (§16) | MUST | 1a |
| R-13 | Scheduled scrub with corruption detection | MUST | 1a |
| R-14 | Repair from a verified healthy copy, per chunk or whole file, keeping what it overwrites in the trash | SHOULD | 1b |
| R-15 | Duplicate report based on hashes | MUST | 1b |
| R-16 | Ordered rules: path, file name, extension (glob or regex), minimum and maximum size | MUST | 1a |
| R-17 | Deferred and manual replication modes, a command to force replication (1a); immediate and scheduled modes (1b) | MUST | 1a, 1b |
| R-18 | A deletion detected on one disk sends the other copies to the trash, with a mass-deletion guard; a replacement is never mistaken for a deletion | MUST | 1a |
| R-19 | Notifications to Plex, Emby and Jellyfin (partial scan of the affected folder) | MUST | 1b |
| R-20 | `--dry-run` and confirmation for every operation that moves or deletes | MUST | 1a |
| R-21 | Failure domains and disk tags | SHOULD | 1b |
| R-22 | SMART and temperature monitoring without waking sleeping disks; alerts through ntfy, Gotify or SMTP | SHOULD | 1a (ntfy), 1b |
| R-23 | ext4 and NTFS | MUST | 1a |
| R-24 | Generated mergerfs configuration and runtime branch management (phase 1 and break glass) | MUST | 1a |
| R-25 | Own merged view on Linux (FUSE): original presented, every entry served from the catalog, trash on delete | SHOULD | 2a |
| R-26 | SSD cache: landing, configurable residency, verified transfer to HDD | SHOULD | 2b (designed in phase 1) |
| R-27 | UPS through NUT: pause and clean shutdown | SHOULD | 2b |
| R-28 | `ordered` and `colocate` strategies | SHOULD | 2b |
| R-29 | Windows engine (service) and pool drive (WinFsp), in Rust | SHOULD | 4 |
| R-30 | Health evacuation | SHOULD | 4 |
| R-31 | XFS and btrfs | MAY | 4 |
| R-32 | Web interface | MAY | 5 |
| R-33 | openmediavault plugin | MAY | 5 |
| R-34 | Language-neutral contracts and a shared conformance suite (§29) | MUST | 1a |
| R-35 | Rust implementation that replaces the Go one without data migration and passes the conformance suite | SHOULD | 3 |

## 6. On-disk layout (mirror)

```
/mnt/t1/                               ThinkNAS disk (ext4); on Windows: D:\
├── media/                             managed root: same tree on every disk
│   ├── Films/Avatar (2009)/Avatar.mkv              original (created 2021)
│   └── Séries/The Expanse/S01/…                    replicas (originals on u1)
├── Atoll/                             visible work folder, outside media/
│   ├── trash/2026-10-26/4812/Films/…               deleted or overwritten copies
│   ├── trash/2026-10-26/4813.chunks/Films/…        chunks a repair overwrote
│   ├── tmp/                                        copies in progress
│   ├── links/                                      optional link guard (§17)
│   ├── disk.toml                                   identity (optional)
│   ├── manifest.b3sum                              hashes checkable without Atoll (optional)
│   └── catalog/                                    catalog backups (optional)
└── Other data/                        outside the root: ignored

/mnt/u1/                               D4-320 disk (NTFS)
└── media/
    ├── Films/Avatar (2009)/Avatar.mkv              replica (created 2026, same path)
    └── Séries/The Expanse/S01/…                    originals
```

- Every copy of a file sits at the same relative path under `media/` on its disk.
- Each disk stays a normal, self-sufficient tree: readable on its own, with no Atoll format.
- The merged view shows each logical path once. Media servers and the SMB share read the merged view; reading member disks directly would show every replicated file twice (two versions of the same item in Plex).
- When a disk dies, its files stay visible in the merged view through their replicas, with no manual action (in phase 1, Atoll removes the dead disk's branch from the running mergerfs, §21.1).
- Break glass: mount plain mergerfs over the `media/` folders at the same mount point to get the same view. `atoll mergerfs-config` prints the matching configuration.
- Atoll never creates a disk's `media/` root, and creates `Atoll/` only after the disk's identity is confirmed (§10). On an unmounted disk the root does not exist, so neither mergerfs nor Atoll can write to the system disk by mistake.
- The names `media` and `Atoll` are configurable, globally (`[layout]`) or per disk (`root` and `workdir` on a `[[disk]]`).

### 6.1 Writes under `media/`

Atoll performs only these writes under a disk's `media/`, each journaled in `ops` before it runs:

1. Create a replica and its missing parent folders (§14).
2. Refresh a stale copy: the stale copy goes to the trash, then a new copy is created (§15.4).
3. Propagate a detected rename or move of a file or folder, and make its missing parent folders (§15.3, §15.12).
4. Move a copy to the trash: propagated deletion, a copy left at the old name by a rename its disk refuses, surplus (`prune`), rebalancing, quarantine of a corrupt copy (§15.3, §15.5, §16, §17).
5. Repair a corrupt copy in place, chunk by chunk, or replace it (§16).
6. Restore a file from the trash (`trash restore`).
7. Apply a metadata-only change (modification time, permissions) made on one copy to the others (§15.4).
8. Restore parent folders' modification times after any of the above; remove empty folders Atoll created itself.
9. On explicit commands only: `disk remove --evacuate`, `disk replace`, `balance --move-originals`, `conflicts resolve`.
10. From phase 2: writes through Atoll's own view (new files, deletions to the trash) and SSD-cache transfers. From phase 4: health evacuation.

Removal rules: `prune`, rebalancing and evacuation remove Atoll copies first and never remove the last user copy of a file. Removing any user copy requires an explicit command and goes to the trash; a deletion or a rename the user made on another copy counts as one (§15.3, §15.5).

## 7. Choosing the original

- The original is the in-sync copy with the oldest creation time:
  - Linux: `statx` birth time (ext4 with 256-byte inodes, XFS, btrfs, ntfs3); ntfs-3g and lowntfs-3g expose none through `statx`, so their `system.ntfs_crtime` extended attribute is read instead: 8 little-endian bytes, a FILETIME (100 ns units since 1601-01-01 UTC);
  - Windows: NTFS creation time.
- A creation time is absent, `copies.btime_ns` is NULL and `first_seen` stands in for it, when the filesystem or driver exposes none (ext4 with 128-byte inodes), when it is exactly 0, or when it does not fit in a signed 64-bit count of nanoseconds, from -2^63 to 2^63-1 (before 1677-09-21 or after 2262-04-11). Such dates are not real, and the oldest would always win: ext4 reports a crtime of 0 as 1970-01-01, and ntfs3 a FILETIME of 0 as 1601-01-01.
- Eligible copies: copy state `ok` or `surplus`, on a disk `online`, `standby`, `readonly` or `draining`. These hold the same bytes and can be read, so the original does not change when a disk sleeps; it changes when a draining disk leaves.
- Each copy's creation time, or its `first_seen` where that is absent, is compared in exact nanoseconds, not at the coarser precision of §9, which serves times that must match. Here nothing has to match: creation times are never copied, and the choice is among identical copies. Ties: earliest `first_seen`, then disk order in the configuration (`disks.position`).
- A file in `conflict` (§15.7) keeps its previous original (`files.original_disk`) as long as that copy is eligible, so adding a disk with an older version of different content does not change what is presented. Otherwise, or after `catalog rebuild` has lost it, the rule above applies. Outside a conflict the previous original is ignored.
- With no eligible copy, a file keeps `files.original_disk`, as it keeps its size and modification time, which are that copy's. So a file in `conflict` whose disks all went away finds the same original when they return.
- Stale, corrupt or unavailable copies are never presented by Atoll's own view, even when they are older.
- Atoll preserves a replica's modification time, as far as its filesystem can hold it (§14), but never its creation time, so a replica is younger than its source. On Windows, NTFS "tunneling" can give a file re-created under the same name within 15 seconds its predecessor's creation time; the Windows version therefore sets the creation time explicitly after the final rename.
- `files.mtime_ns` holds the original's modification time, so it follows the original to another disk. It needs no history, and `catalog rebuild` finds the same value.
- The view returns the original's metadata (size, times, attributes). It reads data from the original by default; with `read.policy = "prefer-spinning"`, it may read any in-sync copy on a disk that is already spinning (identical content).
- If the original's disk goes missing, the oldest remaining in-sync copy is presented. When the disk returns in sync, its copy becomes the original again.
- A refresh or a `file` repair gives a copy a new creation time, so the original may move to another disk; a `chunk` repair keeps it (§16). Protection decisions never depend on the original (§6.1).
- Phase 1 (mergerfs, `category.search=ff`): the copy on the first branch holding the file is presented, not necessarily the oldest. Atoll keeps stale and corrupt copies out of `media/` as soon as it finds them (§15.4, §16), so mergerfs serves current content. mergerfs exposes no creation time through its view (measured), so Atoll reads it on the disks.

## 8. Architecture

```
atoll CLI ──── local API (Unix socket / named pipe) ──── web UI (phase 5)
                              │
                              ▼
atoll daemon (same binary)
  disks      identity (FS UUID / 64-bit NTFS serial), mounts, states, SMART, sleep
  catalog    SQLite (WAL, synchronous=FULL): files, copies, entries, hashes, jobs, ops, events
  rules      ordered rules → effective policy of a file
  solver     target placement, original selection, job plan (pure function)
  jobs       priority queue: replicate, rename, trash, refresh, repair, transfer
  ops        journal of Atoll's own filesystem operations
  watch      fanotify / inotify, rescans, grouping of events per path
  integrity  BLAKE3 file and chunk hashes, scrub, repair, duplicates
  trash      per-disk trash, retention, restore, optional link guard
  view       mergerfs config and runtime branches (phase 1), FUSE (phase 2a), WinFsp (phase 4, Rust)
  notify     Plex, Emby, Jellyfin (partial scans)
  alerts     ntfy, Gotify, SMTP; Prometheus /metrics (phase 2b)
  cache      SSD tier: landing, residency, transfer (phase 2b)
                              │
          ┌───────────────────┼───────────────────┐
       /mnt/t1             /mnt/t2      …      /mnt/u1        ext4, NTFS, XFS, btrfs
```

Principles:

- The **solver** alone decides placement. No other module moves files on its own initiative. This is the lesson of DrivePool's stacked balancers, which conflict to the point that a third-party plugin exists only to merge them. In phase 1, mergerfs still places new files written through it; Atoll constrains it through the generated configuration (§21.1).
- The **catalog** is a rebuildable cache. The truth is on the disks: every copy sits at its logical path under `media/`.
- Every filesystem operation Atoll performs is **journaled** in `ops` before it runs, durably (`synchronous=FULL`). The watcher uses this journal to tell Atoll's own operations from user actions; crash recovery uses it to finish or undo them.
- The SSD cache is a **disk role** (`role = "cache"`) known to the solver from phase 1, even though transfers arrive in phase 2b.
- The merged view **reads** the catalog and leaves every placement decision to the solver.

## 9. Data model (SQLite)

The catalog schema is `schema/migrations/`: plain SQL, with a comment on why each table is as it is. This section gives what a schema cannot say.

| Table | Holds |
|---|---|
| `meta` | `pool_id`, and `lpath_key_version`: what computed `lpath_key` (§15.8): the language, the normalization library, its release and its Unicode version, such as `go golang.org/x/text v0.43.0 17.0.0` or `rust unicode-normalization 0.1.25 17.0.0`; compared for equality only |
| `disks` | a copy of the disks of the configuration, plus state, free space and SMART |
| `files` | one row per managed file: its current version, level, rule, mode and state |
| `file_chunks` | the chunk hashes of a file (§16) |
| `copies` | one row per copy of a file |
| `entries` | everything else the view must list: folders, symlinks, ignored files |
| `ops` | the journal of Atoll's own filesystem operations |
| `trash` | what each disk's trash holds |
| `jobs`, `job_chunks` | the queue, and the chunk hashes a copy in progress has read (§14) |
| `events` | what happened, written before any destructive action |
| `alerts` | each alert standing: the condition's key, when it began, when it was last sent, and the count it was sent with (§23) |
| `scrub_cursor` | where the scrub of each disk stopped when its budget ran out: the copy, the next chunk to read, and the size and times its row had then (§16) |

- **Pragmas**, set on every connection, never in a migration: `journal_mode = WAL`, `synchronous = FULL` (a committed `ops` row survives a power cut), `foreign_keys = ON`. Atoll checks that they took effect when it opens the catalog: a filesystem that cannot do WAL fails there, not later.
- **Migrations**: `schema/migrations/NNNN_name.sql`, numbered from 1 with no gap. `PRAGMA user_version` holds the version. Each file runs in its own transaction with its `user_version`, so a failure leaves the catalog at the version before it, and with `PRAGMA defer_foreign_keys = ON`, so that foreign keys are checked at its commit: a table made again keeps the rows that refer to it. A catalog of a higher version than the binary is refused: going back to an older Atoll takes `catalog rebuild`. A file holds no pragma and no transaction statement.
- **`disks`** is rewritten from the configuration at each start, and the configuration wins (§10). A disk whose `state` is `failed` is `failed`, whatever its mount check says, and nothing is measured on it; one whose `state` is `draining` is `draining` while its mount check finds it `online`, and `missing` or `readonly` as the check says otherwise (§18). A retired disk keeps its row, so its identifier and its history stay: a disk the configuration no longer lists becomes `retired`, placed after the others by `id`, and comes back if it is listed again.
  - A disk whose `id` now has another `uuid` is refused while it holds copies, entries, entries of the trash or `planned` operations, and so is a `uuid` that another `id` has: what the catalog knows about them would be read against another disk. `disk replace` is the way to do that. Another `root` or `workdir` is refused likewise, as the catalog's paths are relative to them.
- **`meta`**: `atoll init` writes `pool_id` and `lpath_key_version`. A catalog that does not exist, or whose `pool_id` is not the configuration's, is refused, so a mistyped path or an unmounted folder cannot start again from an empty catalog in silence.
- **Chunk hashes have their own table**, as SQLite rewrites a whole row on each `UPDATE`. Measured on 200,000 files and 20 TB with `modernc.org/sqlite`, WAL and `synchronous = FULL`: changing the state of a 100 GB file writes 780 KB to the WAL and takes 4 to 5 ms with its hashes in `files`, against 5 KB and 1.3 ms here. The catalog is about 280 MB. The chunk size is fixed (§16), so the catalog does not store it.
- **Tables are `STRICT`, and closed lists have a `CHECK`** (§29.3). Through the Go driver, a Go string written to a `BLOB` column of a table that is not strict is stored as `TEXT` with no error. A new state therefore takes a migration that rebuilds the table: about 0.7 s for `files` or `copies` at this size. `events.kind` has no `CHECK`, as each module adds its own; its list is in this specification.
- **Deleting**: a file with copies cannot be deleted, and neither can a disk with copies or entries, so a bug cannot lose that knowledge in passing. Deleting a file deletes its chunk hashes and jobs. `ops` and `trash` have no foreign key to `disks`: the journal outlives a disk.
- **`ops`**: `path` is the logical path, `path2` the location inside `<workdir>/trash/` (§17), and `file_key` the identity of the file moved, its inode in decimal. For `kind = rename`, `path` is where it sat and `path2` the target, both logical paths on that disk, and `file_key` the inode of the file or folder (§15.12). For `kind = create`, `path2` is the name of the copy in progress inside `<workdir>/tmp/`, and `file_key` its inode (§14.2). An operation of the link guard (§17) holds in `link` the id of the file whose link in `<workdir>/links/` it is about: `kind = link` makes that link from the copy at `path`, `file_key` being its inode; `kind = unlink` removes it; and `kind = trash` sends it to the trash at `path2`, in place of a copy at `path`. `link` is NULL for every other operation. An operation is `planned` from before it starts until it is known to be finished or not to have happened (§17.2): `done` or `undone`.
- **`events.kind`**: `trash`, `restore`, `purge`, `rename`, `replicate`, `utimens`, `chmod`, `chown` and `rmdir`, each written with its `planned` operation, and `op-undone`, written when an operation finds that a name no longer holds what it expected (§17.1, §17.2, §15.12). `copy-gone`, for a copy that disappeared, `copy-kept`, for a file a deletion did not reach, and `modified`, for a file whose copies a modification made stale (§15.4). `guard`, `guard-confirm`, `guard-reject` and `guard-clear`, and `modify-guard`, `modify-guard-confirm` and `modify-guard-reject` (§15.6). `corrupt`, for a copy the scrub found corrupt, written before its row says so (§16). `op-undone`, `copy-kept`, `guard`, `modify-guard` and `corrupt` have the level `warning`, and the others `info`. `data` is a JSON object with `disk`, `path` or `path_b64` for what names a file, `op` for what comes with an operation, `reason` for `trash`, with `link` for one of the link guard (§17), `source` for `replicate`, `mtime_ns` for `utimens`, `mode` for `chmod`, as an integer, `uid` and `gid` for `chown`, as integers, `gone_ns`, `nothing_left` and `file`, the id of the file, for `copy-gone`, `answered` for a `modified` that a confirm answered (§15.6), and `up_to` for `guard-confirm`, `guard-reject`, `modify-guard-confirm` and `modify-guard-reject`, in integer nanoseconds.
- **`events`** only grows, as it is the history: no row is ever removed. What a pass reads of it, at every pass and for each disk, goes through an index, never over the whole log: the last answer to each guard, the guard that stands since, and what each counts within its window (§15.6), by `events_by_kind`, on `kind` then `ts_ns`. Over a million events, a pass with nothing to do took 0.8 s without that index, and 64 ms with it, as over none.
- **`copies.gone_ns`** says since when a look has found that the name no longer holds this copy, and **`gone_kind`** what the last look found (§15.12). **`copies.old_lpath`**, NULL for nearly all, says that this copy has not followed its file's rename yet, and still sits at that path on its disk; **`old_lpath_key`** is its key. So a rename waiting for a disk is one column, and its target is always `files.lpath`, resolved when it runs. Every reader of a copy's place reads `COALESCE(old_lpath, files.lpath)`.
- **`copies.mode`** is the copy's mode as its disk holds it: the permission bits, setuid, setgid and sticky included (0 to `07777`), without the file type. A look, an operation or the copy pipeline records it with the copy's times (§14, §15.4). It is NULL until known: the migration that added it leaves it NULL, and the next look that finds such a copy `there`, unchanged since its row, records the mode it holds, which cannot have moved since, as a `chmod` moves the change time (§15.10). A stale copy that becomes pending loses it, with the rest of what its disk held.
- **`copies.uid`** and **`copies.gid`** are the copy's owner as its disk holds it: its user and group ids. A look, an operation or the copy pipeline records them with its mode. They are NULL until known: the migration that added them leaves them NULL, and the next look that finds such a copy `there`, unchanged since its row, records the owner it holds, which cannot have moved since, as a `chown` moves the change time (§15.10). A stale copy that becomes pending loses them.
- **`entries.file_key`** and **`btime_ns`** identify a folder, as a rename keeps both: they tell a folder renamed from files moved out one by one (§15.12).
- **`jobs`** remembers what cannot run now: `kind = trash`, with `reason` the `trash.reason` it will give and `state` `queued`, or `canceled` for a refused rename the user rejected, which keeps it from being proposed again; and `kind = rename` for a folder, `file_id` NULL, `payload` `{"from_b64", "to_b64"}`. `kind = replicate` is a copy to make, with its pending copy (§14.1); its `payload` is `{"part", "blake3", "replaces"}`, the `.part` it copies into and, once verified, the whole-file hash in hex, and for a copy `atoll balance --move-originals` makes in place of a user copy, the disk of that copy (§13.3), and `job_chunks` holds the hash of each chunk copied so far. A job that ran is deleted.
- **Ids that name something on a disk never come back**: `files.id` names `Atoll/links/<file id>`, and `ops.id` names a folder in the trash (§17). A new catalog, from `atoll init` or a rebuild, starts each past the highest its disks name: its operations past those of `trash/` and `tmp/`, its files past those of `links/`.
- All INTEGER times are Unix nanoseconds, UTC. A time that must match across disks is compared at the coarser precision of the filesystems involved (`time_precision_ns`), so an ext4 → NTFS replica still matches: two times are the same at a precision p when each, divided by p and rounded down, toward the past, gives the same number, which is what a filesystem that holds only multiples of p keeps of a time (`conformance/vectors/same_time.json`). Creation times are never copied, so §7 compares them exactly; and a copy's times are compared exactly with its own row, which recorded what that filesystem held.
- **`disks.time_precision_ns`** is how finely the filesystem holds a time: 1 for XFS and btrfs, 100 for NTFS, and for ext4 1, or 10^9 when its inodes are 128 bytes, which leave no room for nanoseconds (§10). Each mount check that reads the head of the device records it, as it records the disk's state; a check that does not keeps what an earlier one recorded. A disk never seen has none, and its times compare exactly, which at worst applies a time again.
- A modification or change time outside the range of a signed 64-bit count of nanoseconds, from -2^63 to 2^63-1 (before 1677-09-21 or after 2262-04-11), is stored at the nearest limit, the same way in both versions. Reading it again gives the same value, so it is never taken for a change. A creation time is absent instead (§7). Atoll sets a time from a copy's `stat`, never from the catalog, so a stored limit never reaches a disk.
- `files.lpath_key` and `entries.lpath_key` are indexed: the collision checks of §15.7 look names up by key.
- `copies.file_key` is indexed by disk, for the copies that have one (`copies_by_inode`): what the names of a file record when one of them changes, and what those names are, are looked up by inode, once for each operation on a file with more than one name, as every copy has with the link guard on (§17). Without it each lookup read every copy of the disk, and the first pass with the guard on 200,000 files a disk, which makes a link for each copy, had not ended after 8 hours (§26).
- `trash` is indexed by path, then by operation (`trash_by_path`): its list is read in that order, a page at a time, and an entry is found by the path it left and the operation that sent it to the trash. Without it the list sorted the whole trash for each page, and each purge, restore and undone operation read every entry to find its own.
- Chunk hash volume: 20 TB ÷ 4 MiB × 32 bytes ≈ 150 MB, one BLOB per file.
- Catalog location: `catalog` in the configuration, by default `/var/lib/atoll/catalog.db` (Linux), `%ProgramData%\Atoll\catalog.db` (Windows).
- Daily catalog backup on the system disk and, with `maintenance.catalog_on_disks`, in each disk's `Atoll/catalog/`, inside `maintenance.window` (§21.6). A backup is SQLite's `VACUUM INTO`, from one snapshot, into `<catalog folder>/backups/catalog-<UTC time, YYYYMMDDTHHMMSSZ>.db` through `<name>.part`, checked whole before it is renamed: SQLite's integrity check, its foreign keys, and this pool's `pool_id`. The seven newest are kept. With `catalog_on_disks`, it is then copied to each disk `online` and not `failed`, as `Atoll/catalog/catalog.db`, through a confirmed handle, by way of `catalog.db.new`, synced, then renamed in place of the one before.
- Rebuild (`atoll catalog rebuild`): from the manifests when present, otherwise through a full rescan with re-hashing. Since every copy sits at the same path, a rescan alone tells which copies belong together. No data is lost, but `first_seen`, recorded conflicts and corruption history are. So copies with different hashes become conflicts, whatever their sizes and modification times, as nothing is left to say which one is current. Copies with the same hash are in sync, each with its own modification time. The new catalog is made beside the old one, at `<catalog>.rebuild`, as `atoll init` would, its operations started past the highest id the disks' `trash/` and `tmp/` name, so that a new one never meets an old one's folder or part, and its files past the highest id their `links/` name, so that an old link never names a new file; then filled by a full scan. The old catalog's write-ahead log is moved into it when SQLite can, and set aside with it otherwise, as a log left beside the new catalog would be played into it. The two then trade names in one step (`renameat2` with `RENAME_EXCHANGE`), so that a command waiting for the lock always finds a catalog, and the old one is kept, never removed, as `<catalog>.old-<UTC time, YYYYMMDDTHHMMSSZ>`. It works when the old catalog cannot be opened: of a later version, damaged, or gone. What the disks' trash holds is left on them, and the new catalog does not list it.

## 10. Disk identity and mounts

- Identifier: the filesystem UUID (ext4, XFS, btrfs) or the 64-bit NTFS serial number, readable on every system that can mount the filesystem, without writing anything to the disk.
- Atoll reads it from the superblock, with no external tool, so both versions read the same bytes. Offsets count from the start of the device, and a value "as a UUID" is 16 bytes, in order, as 8-4-4-4-12 lowercase hexadecimal digits:

  | `fs` | Magic | Identifier | Also read |
  |---|---|---|---|
  | `ext4` | the bytes `53 EF` at 1080 | 16 bytes at 1128, as a UUID | `casefold`: bit `0x20000` of the little-endian 32-bit value at 1120. `time_precision_ns`: 10^9 when the little-endian 32-bit revision at 1100 is 0, whose inodes are 128 bytes, or the little-endian 16-bit inode size at 1112 is 128 or less; 1 otherwise |
  | `xfs` | `XFSB` at 0 | 16 bytes at 32, as a UUID | `ascii-ci`: bit `0x4000` of the big-endian 16-bit value at 100. `time_precision_ns`: 1 |
  | `btrfs` | `_BHRfS_M` at 65600 | 16 bytes at 65568, as a UUID | `time_precision_ns`: 1 |
  | `ntfs` | `NTFS` and four spaces at 3 | the little-endian 64-bit value at 72, as 16 uppercase hexadecimal digits | `time_precision_ns`: 100 |

  Only the layout of the disk's `fs` is read, so what an earlier filesystem left at another offset changes nothing. A head too short for a field holds no such filesystem. Reading a device takes root or the `disk` group. `blkid` without `-p` answers from its cache, and gave the UUID of the image that was on that loop device before; `FS_IOC_GETFSUUID` needs no privilege, but only ext4 and XFS have it (`docs/research/disk-identity.md`).
- Two disks with the same identifier: both are refused until one identifier is regenerated (`tune2fs -U`, `xfs_admin -U`, `btrfstune -u`, `ntfslabel --new-serial`). A clone is not the only way there: two NTFS volumes that `mkntfs` makes in the same second get the same serial (measured).
- Never identify a disk by `/dev/sdX`, drive letter, Windows volume GUID (specific to each machine) or USB serial number (often fake behind a bridge).
- `disk add` checks the block-device topology and refuses two pool filesystems on the same physical disk (R-06). It also refuses the variants that change name rules (§15.8): ext4 with the `casefold` feature, XFS with `ascii-ci`, NTFS mounted with ntfs3 `nocase` or with lowntfs-3g `ignore_case`. And it refuses NTFS mounted with ntfs3 `ads`, which can destroy another file (§15.2).
- Before any write, Atoll confirms in the same session that the mount path is a mount point of the expected filesystem (device and identifier). It then works through a directory handle opened on the verified root (`openat`-style calls) and re-checks the device id before each job. It writes nothing to an unconfirmed disk.
- A **confirmation** is a mount check that does not find the disk `missing`, then a read of the head past the page cache (`O_DIRECT`), 68 KiB aligned to 4 KiB, whose identifier must be the disk's. A failure makes the disk `missing`, with the reason the mount check gives for the same answer: `device-gone`, `device-unreadable`, `wrong-filesystem` or `wrong-identifier`, or `not-mounted` when the mount table no longer has an entry for the path. A session confirms each disk it reads or writes, once, before it first does so; and a scan each disk it walks, before it records what that walk found (§15.10). It opens its handle on the top of the disk first, and after the confirmation that handle must have the mount id and device of the mount path, so that an unmount in between cannot pass an empty mount point as the disk. It wakes that disk, as the writes will; the disks it does not need stay asleep. Measured on a device that fails (dm-error, on ext4, XFS, ntfs3 and ntfs-3g, Linux 6.12 and 7.3): the mount check still answers from the cache, the read gives `EIO` on all four, and without it ntfs-3g accepts `mkdir`, `link` and `unlink`, and only `fsync` of the target folder says `EIO`.
- A **mount check** asks these questions in order and stops at the first that fails, which gives the disk's state and its reason, from a closed list (§29.3). It needs Linux 5.8 or later: a kernel that does not give the mount id fails question 1 with `io-error`.

  | # | Question | Reason when it fails | State |
  |---|---|---|---|
  | 1 | Do `statx` on the mount path, without following a symlink, and the mount table `/proc/self/mountinfo` both answer? | `not-mounted` when the path does not exist (`ENOENT`, `ENOTDIR`), otherwise `io-error` | `missing` |
  | 2 | Does the table's entry for the mount id of the path (`STATX_MNT_ID`) name exactly `mount`? An empty mount point gives its parent's entry, and a symlink or a path through a symlinked folder gives another name, so they all fail here. With mounts stacked on one path, the id is that of the top one. A line of the table that cannot be read says nothing about the others: it counts only when no entry has that id | `not-mounted`, or `io-error` when no entry has that id and a line cannot be read | `missing` |
  | 3 | Is the entry's root `/`? A bind mount of a folder, or a btrfs subvolume, does not show the top of the disk | `partial-mount` | `missing` |
  | 4 | Is the entry's type one that `fs` allows? `ext4`: `ext4`. `xfs`: `xfs`. `btrfs`: `btrfs`. `ntfs`: `ntfs3`, `fuseblk` or `ntfs` | `wrong-filesystem` | `missing` |
  | 5 | Does the entry's source lead to a block device with the entry's major and minor numbers, btrfs excepted, as its entry holds an anonymous number? A USB disk that is unplugged keeps its mount in the table and `statx` keeps answering, so this is what notices it (measured) | `device-gone` | `missing` |
  | 6 | Can the head of the device be read, up to byte 65607? A shorter device reads as far as it goes | `device-unreadable` | `missing` |
  | 7 | Does the head hold the magic of `fs`? | `wrong-filesystem` | `missing` |
  | 8 | Is its identifier the disk's `uuid`? | `wrong-identifier` | `missing` |
  | 9 | Are the superblock options free of `shutdown`? | `io-error` | `missing` |
  | 10 | Is `<root>` a folder with the same mount id as the path, and not a symlink? So a missing `media/` is never read as an empty disk | `no-root`, or `io-error` when `statx` fails with anything but `ENOENT` or `ENOTDIR` | `missing` |

  A disk that passes them all is `online`, unless one of these applies, the first in this order. It is then `readonly`: Atoll may read it and writes nothing to it.

  | Reason | When |
  |---|---|
  | `unsupported-driver` | the type is `ntfs`: a driver whose name rules were never measured (§22) |
  | `ads` | type `ntfs3`, with `ads` among the superblock options |
  | `nocase` | type `ntfs3`, with `nocase` among the superblock options |
  | `iocharset` | type `ntfs3`, without `iocharset=utf8` among the superblock options |
  | `casefold` | ext4, with that feature in the superblock |
  | `ascii-ci` | XFS, with that feature in the superblock |
  | `ignore-case` | `<root>` also answers under its name with each ASCII letter in the other case, with the same inode. Nothing else shows lowntfs-3g `ignore_case`: mount information does not, and the FUSE daemon's command line depends on how it was started. A real second folder has another inode. A `<root>` with no ASCII letter cannot be asked |
  | `read-only-mount` | `ro` among the mount options or the superblock options, or `emergency_ro` among the latter |

- Options are compared whole: `errors=remount-ro` is not `ro`, and `ads=0` is not `ads`.
- A check wakes no disk once the head of its device is in the page cache: asked again, these questions caused no device read on ext4, ntfs3, ntfs-3g or lowntfs-3g (measured). The price is that it cannot see a failing disk whose answers are still cached, which stays `online` until an operation gets an I/O error (invariant 4).
- ntfs3 `ads` is on by default from Linux 7.3 and shows in `/proc/mounts`. Mount with `ads=0`; Linux 6.12 does not know that option and refuses the mount, so add it when moving to a kernel that has `ads`. If a mount check finds `ads` on a disk already in the pool, the disk becomes `readonly`, so Atoll writes nothing to it, and an alert is raised. mergerfs keeps its branch, so the risk stays until the disk is remounted with `ads=0`.
- I/O errors (EIO, ENOTCONN, ENODEV) mean "disk unknown", never "file absent". Missing files count as deletions only after the disk's identity has been confirmed in the same scan; an empty mount point after a failed mount is a missing disk, never a mass deletion.
- Recommended Linux setup:
  - fstab by UUID with `nofail` and a short device timeout, on a stable mount point (`/mnt/<id>`, or `/srv/dev-disk-by-uuid-…` under openmediavault);
  - make each empty mount point immutable while unmounted (`chattr +i /mnt/t1`), so nothing can write into it if the disk is absent. `disk add` warns when this is missing.
- USB resets: the old mount keeps returning I/O errors while the disk reappears under a new device name. Atoll marks the disk `missing` and removes its mergerfs branch. With `health.remount_after_reset = true`, it then unmounts the stale mount and remounts by UUID, except for an NTFS volume marked dirty (§22); otherwise it alerts. The disk rejoins only after reconciliation (§18).
- Optional marker `Atoll/disk.toml` (visible): `pool_id`, `disk_id`, `fs_uuid`, `format_version`. Disabled with `marker = false`. `disk add` writes it on a disk `online`, as four lines in this order: `format_version = 1`, then `pool_id`, `disk_id` and `fs_uuid`, each a basic string; through a confirmed handle, as `disk.toml.new` first, synced, then renamed without replacing anything. A marker already there that names another pool, another disk of this pool or another filesystem makes `disk add` refuse the disk; one that says the same is left as it is.
- No automatic adoption: an unknown disk is reported, never added.
- The configuration file is the source of truth for the disk list; `atoll disk add` and `disk remove` edit it and the catalog together. An edit keeps every other byte of the file, comments included. `disk add` appends its `[[disk]]` at the end of the file, after a blank line, which TOML allows after `[[rule]]`; its keys come in the order `id`, `uuid`, `mount`, `fs`, `role`, `domain`, `tags`, `root`, `workdir`, `state`, each as a basic string or a list of them. `disk remove` takes a disk's `[[disk]]` out, from the start of its header's line to the end of its last key's line, so a comment above the header or below the last key stays. A disk's `state` replaces the value of the one there, or goes on a line of its own after the disk's last key; taking it out takes its line out. The line ends follow the file's: CRLF when it has one. A file whose disks are written another way, as an array of inline tables or with a table below a disk, is refused: edit it by hand. Atoll re-reads the result before replacing the file, and refuses the edit if anything else changed (`conformance/vectors/config_edit.json`).

## 11. Rules

### 11.1 Conditions, combined with AND

| Key | Example | Effect |
|---|---|---|
| `path` | `"Films/**"` or `'re:^Films/'` | glob or regex (§29.5) on the logical path |
| `filename` | `"*sample*"` | glob or regex (§29.5) on the file name: the last `/`-separated component of the logical path, which never ends with `/`; a `filename` glob that contains `/` is rejected when the configuration loads (use `path`) |
| `ext` | `["mkv", "mp4"]` | extensions, without the dot; holds when one of them does (§11.3) |
| `size_min`, `size_max` | `"100MiB"`, `"40GiB"` | inclusive bounds; size grammar in §29.5 |
| `ignore_case` | `true` (default) | case ignored for `path`, `filename` and `ext` |

### 11.2 Applied properties

`action` (`manage` or `ignore`), `replicas` (0 to 2), `mode`, `settle`, `priority`, `tags`, `tags_mode`, `domains`, `spread`, `spread_mode`, `colocate`, `overflow`, `cache` (phase 2b), `cache_min_age` (phase 2b).

### 11.3 Semantics

- Ordered list: **the first matching rule wins**. A property the rule leaves out takes its pool-wide value (§24.2). A file that no rule matches takes the pool-wide values, with `action = "manage"`.
- Rules apply to files; folders have no rule of their own.
- `action = "ignore"`: the file stays in `media/` and in the view, but gets no hash and no replica. It has no other property.
- Matching, with the grammars of §29.5:
  - the path and every pattern (glob, regex text, extension) are put in NFC form, so a pattern typed in NFC matches a name stored in NFD. A glob is normalized once, before its braces are expanded. A name that is not valid UTF-8 is normalized run by run: each maximal valid UTF-8 run is put in NFC form, and the other bytes are kept as they are. NFC also applies to a regex's text, before its grammar is checked (§29.5), which changes its meaning when the text holds combining marks: `e` followed by U+0301 and `?` becomes `é?`, where `?` applies to the whole character. A mark typed right after an escape letter or a range endpoint composes with it too: `\n` followed by U+0303 becomes `\ñ`, which is rejected, and `[a-z` followed by U+0301 and `]` becomes `[a-ź]`, which holds `é`. Write such marks as `\x{…}`, which NFC leaves alone. An error quotes the text as typed and its NFC form;
  - with `ignore_case`, two characters are equal when Unicode simple case folding (CaseFolding.txt, statuses C and S) puts them in the same class, exactly as regex `(?i)` compares them: `ß` matches `ẞ` but never `ss`; `ς`, `σ` and `Σ` match each other; `İ` and `ı` match only themselves. A class `[…]` matches a character when one of its case variants is in the class, and `[!…]` matches the others. Folding never rewrites the text: a regex only gets `(?i)` added, so `(?-i:…)` keeps its meaning;
  - nothing is normalized again after folding, in globs as in regexes: `J` followed by U+030C, which has no precomposed form, does not match `ǰ` (U+01F0, which decomposes to `j` followed by U+030C);
  - in a glob, each byte that is not part of a valid UTF-8 sequence is one character, matched only by `?`, `*` and negated classes; in a regex, it is one U+FFFD character, as in Go `regexp`;
  - `ext` holds when the file name ends with `.` followed by one of its entries, characters compared as in a glob. An entry is a literal: `*` or `?` in it match only themselves. So for an entry `x` without glob syntax, `ext = ["x"]` and `filename = "*.x"` agree: `tar.gz` is a valid entry, `.nfo` has the extension `nfo`, and `film.` has none. An entry is non-empty, without `/` and without a leading `.`; an empty list is rejected;
  - a pattern (glob, regex text or `ext` entry) with a run of more than 30 non-starters (§29.5) is rejected when the configuration loads, so normalization never inserts U+034F into a pattern. The run is counted on the pattern as written in the configuration, before NFC and before braces are expanded, and on each `ext` entry on its own;
  - the Go and Rust versions match the same paths, except on the characters §29.5 leaves to each version's Unicode data, and on names with a run of more than 30 non-starters, where Go's normalization inserts U+034F (§29.5).
- Changing rules never deletes anything. Replicas beyond the new level become `surplus` and are removed only by `atoll prune`, to the trash, after confirmation.
- Size is evaluated once the file is stable (`settle`): a download in progress is not classified on a partial size. Decisions taken when a file is created (SSD cache, phase 2b) can only use path, file name and extension. While the size is not known, a rule with a size condition does not match, and the next one may. So the wait for a stable file takes its `settle` from the first rule that matches without a size condition, then from `[defaults]`, and `settle` and `cache` never go with `size_min` or `size_max` (§24.3): they are read before the size is known.
- A size condition is inclusive, and every condition of the rule must hold. A rule with no condition matches every file.
- `atoll rules explain <path> [--size <size>]` shows the matching rule and the effective policy, and where each value comes from: the first of the rule, the file (a section of the configuration), the preset and a default that sets it, even when that value equals what would come next. `<path>` is a logical path, relative to `<root>`. The command reads the configuration only and wakes no disk, and without `--size` it answers for a file whose size is not known yet. It shows what the configuration asks for, before the fallbacks of §12.2.
- `atoll rules check` loads the configuration (§24.3) and shows its problems and warnings. Once it can read the catalog, it also lists rules that no current file reaches and rules entirely hidden by an earlier one in simple cases.

## 12. Redundancy strategy

### 12.1 Four settings

- **How many**: `replicas` (0 to 2), per rule.
- **Where**: `[placement]` (strategy, thresholds, domains, tags) and rule properties.
- **When**: replication `mode` and rebalancing schedule (`[balance]`).
- **How to read**: `[read]`, used by Atoll's own view (phase 2a).

### 12.2 Presets (`strategy`)

A preset sets only the keys listed here; anything set explicitly wins. `replicas` and `mode` are keys of `[defaults]`; `spread_mode`, `new_files` and `colocate` of `[placement]`.

| Preset | Keys set |
|---|---|
| `safety` | replicas = 1, spread_mode = strict, mode = immediate, balance.schedule = immediate |
| `energy` | replicas = 1, new_files = ordered, colocate = season, mode = scheduled, cache.enabled = true when a disk has `role = "cache"` |
| `capacity` | replicas = 0, new_files = most-free |

Until a strategy is implemented (see §28), it falls back to `most-free`, with a warning when the configuration loads (§24.4).

### 12.3 Replication modes

| Mode | Trigger | Typical use |
|---|---|---|
| `immediate` | as soon as the file is closed and stable | photos, documents |
| `deferred` | after `settle` without changes (10 min by default) | files renamed or replaced right after arrival (Sonarr, Radarr) |
| `scheduled` | inside the `[replication].window` time window | movies, series: disks woken once per night |
| `manual` | only through `atoll replicate` | archives; files counted as `unprotected`, alert after `manual_alert_after` (7 days by default) |

- `atoll replicate --now` runs the pending jobs of a path immediately, ignoring the time window. `--force` does the same, also re-verifies the hashes of existing replicas and retries failed jobs.
- Refreshing stale copies ignores the mode and runs as soon as possible (§15.4).
- Before Atoll's own view (phase 2a), `immediate` leaves a window of a few seconds to a few minutes with a single copy.

### 12.4 Placement

- **Hard constraints**, never violated:
  - a disk that holds no other copy of the file (R-06);
  - `min_free` respected;
  - disk `online` or `standby`, and not `draining`, `failed` or `readonly`;
  - name compatible with the target filesystem (§15.8);
  - on an NTFS target, no name collision with the names on that disk (§15.7);
  - the path within the limit of that disk's mergerfs branch (§15.8);
  - the target path is free.
- **Strict settings**, which `overflow` never relaxes:
  - `spread_mode = "strict"`: at least one copy in each failure domain, as long as the number of copies allows it; with fewer copies than domains, each copy in a different domain. A disk with no `domain` forms its own domain;
  - `tags_mode = "strict"`: only disks carrying one of the rule's `tags` when it has any, and in one of its `domains` when it has any (the equivalent of DrivePool's hard file-placement limit). A rule with neither has no limit, even when `[placement]` says `strict`; a rule that itself says `strict` with neither is refused when the configuration loads (§24.3).
  When they cannot be met, the file stays `degraded` and the status says so.
- **Preferences**, in order: the rule's `tags` and `domains` (with `tags_mode = "prefer"`); spreading across domains (`spread`); `colocate`; the `new_files` strategy; a disk that is already spinning.
- `overflow = "allow"` (default): when the preferred disks are full, another disk is used and a warning is raised. With `deny`, the file stays `degraded`.
- `colocate`: `none`; `folder` (copies of files from the same folder go to the same disks); `season` (same as `folder`, the usual choice for series); `show` (same grandparent folder).
- `spread`: `domain`, or `none` to turn this preference off. `spread = "none"` cannot go with `spread_mode = "strict"`.
- **Originals**: the solver optimizes space by moving Atoll copies only. When it cannot reach the wanted level otherwise, it says so and suggests `atoll balance --move-originals`. No setting makes this automatic (§6.1, item 9).

### 12.5 Mapping to DrivePool

| DrivePool | Atoll |
|---|---|
| Per-folder duplication with inheritance | `replicas` per rule |
| File placement rules (folders, `*` and `?` patterns, order, overflow) | ordered rules: globs, regex, extension, size, `overflow` |
| Drive Usage Limiter | `tags` and `domains` per rule, `tags_mode` |
| Prevent Drive Overfill | `min_free` (percentage or size) |
| Duplication Space Optimizer | built into the solver, always on |
| Ordered File Placement | `new_files = "ordered"` (phase 2b) |
| SSD Optimizer | `cache` disk role and transfer (phase 2b) |
| Scanner integration (evacuation, temperature) | `[health]` (phase 4) |
| Real-time duplication | `mode` per rule; parallel writes with Atoll's own view (phase 2a) |
| Read striping | none; read classes, SATA before USB |
| Disk Space Equalizer | `equalize = "off"` by default; `percent` at placement time only |
| Stacked balancers, All in One | a single solver, hence no conflict between modules |

## 13. Placement and capacity

### 13.1 Formula

For n disks of sizes s1 ≥ s2 ≥ … ≥ sn, total S, and k copies per file (k = replicas + 1):

```
U(k) = min over j = 0 … k−1 of (S − s1 − … − sj) / (k − j)
```

- The result is computed exactly, then rounded down to whole bytes.
- With more copies than disks (k > n) the capacity is 0: no file can have k copies on k different disks. The formula gives the same result when the missing disks count as size 0.
- k is at least 1, no size is negative, and S fits in a signed 64-bit integer; any other input is an error. No real pool comes near that total: the limit keeps the two versions from overflowing differently.
- A size s is what the solver may fill on that disk: its capacity minus `min_free`, or 0 if `min_free` is larger. `min_free` is the smallest of its values, each a size or a percentage of the disk's capacity, rounded down. Only the disks the solver can write to count: role `data`, state `online` or `standby` (§12.4). So the result does not move as disks fill, and it does not account for data outside `media/` or for the trash; the reachable capacity does (§13.3).
- With `spread_mode = "strict"` (§12.4) and m failure domains, a domain's size is the sum of its disks' sizes. With fewer copies than domains (k < m), each copy goes to a different domain, so the formula applies to the domains' sizes. Otherwise (k ≥ m), every domain holds a copy of every file, so the capacity is the smaller of U(k) over the disks and the smallest domain's size. A domain with no disk is an error.

### 13.2 Example: 4 + 6 + 8 + 12 TB

- Before `min_free`: x1: 30 TB; x2: 15 TB; x3: 9 TB.
- With `min_free = ["5%", "100GiB"]`, each of these disks gives up 100 GiB, the smaller value: x1: 29.57 TB; x2: 14.79 TB; x3: 8.84 TB.
- With `spread_mode = "strict"` and two domains A = 4 + 6 and B = 8 + 12, before `min_free`: x2: 10 TB, the smallest domain; x3: 9 TB, as U(3) is below it.
- Since user copies do not move, the real capacity can be lower than U(k). `atoll capacity` shows both the theoretical capacity and the capacity reachable without moving originals (§13.3).

### 13.3 Solver

- Inputs: disks (capacity, free space, state, domain, tags, filesystem) and files (size, effective policy, current copies with `created_by`).
- Output: target placement and a minimal job plan. It prefers creating over moving, moving an Atoll copy over moving a user copy, and smaller volumes first.
- Pure, deterministic function, covered by property tests; every list it outputs is sorted by a documented key.
- `atoll balance [--dry-run] [--yes] [--json]` moves, by hand, the Atoll copies that break a strict setting of their file, as a change of rule, or of a disk's tags or domain, leaves them.
  - **Misplaced.** A copy is misplaced when only listed disks are candidates and its disk is not listed (§13.4). With `spread_mode = "strict"`, it is also misplaced when the copies of a file at its level cover fewer failure domains than it must and it doubles up a domain: the youngest such Atoll copy first, as many as domains are missing. A user copy never is.
  - **A move.** The plan of the pool, made with each misplaced copy counting for nothing, places a replacement where the setting wants it. The copy is made through the copy pipeline (§14.2). The plan, made again, then marks the old copy `surplus`, as the new one stands in for it. Only then does the old copy go to the trash, as `rebalance`.
  - Only the files with a misplaced copy get a job. With no disk the setting allows that has room, the copy stays: `no-target`.
  - Without `--yes` it says what it would move and exits with 3; with a copy that stays, it exits with 4. Output: `api/schemas/balance.schema.json`.
  - **`--move-originals`** moves the user copies too, the only way one moves (§6.1, item 9): a user copy is misplaced then as an Atoll copy is, and a file's misplaced user copies come first, by disk position, then its Atoll ones. The first copies made each stand in for one of them, in that order: its job holds the disk of the copy it replaces (§9), and the transaction that records the new copy makes it the user copy and the one it replaces an Atoll copy, so that a file never has fewer user copies, a crash included. The plan, made again, marks the old copy surplus, and it goes to the trash as `rebalance`, as an Atoll copy does. A misplaced user copy for which the plan makes no copy, as the file's copies that break nothing, `ok` or `surplus` on a disk that can be read, already reach its level, hands its part to the first of those, user copies first, then by `first_seen` and by disk position, in one transaction, then goes the same way: its move's `to` is that copy. With none, it stays: `no-target`. The daemon's balancing never moves a user copy (§12.4).
- Hysteresis: rebalancing starts only when the share of bytes correctly placed falls below `[balance].trigger.ratio_below`, or when the bytes to move exceed `or_more_than`.
  - **The share of bytes correctly placed** is the bytes of the files with no misplaced copy over those of every file, compared exactly (`conformance/vectors/percent.json`). **The bytes to move** are those the misplaced copies hold, a file's size once for each. With nothing misplaced, the trigger never holds.
  - **When the daemon checks it** (§15.13): with `balance.schedule` a time window, `maintenance.window` when left out, once a stretch of it; with `immediate`, at most once an hour, as a check plans the whole pool; with `never`, never; in shadow mode, never.
  - **While it holds**, the daemon moves the misplaced copies as `atoll balance --yes` does, under the catalog's lock 5 minutes at a time, so that passes and commands come in between. It stops when none is left, or when `balance.budget` is spent since the check.
  - **A run stopped** there, or by a crash once a copy was made, is taken on by the next: its plan has no copy to make for that file, and the old copy, which the plan marked surplus, goes to the trash then.
- Status indicator: share of files at the wanted level, and share of bytes correctly placed, both in `atoll status` (§15.11).
- Reachable capacity, for each k: the solver plans without running anything, moving no user copy, with every managed file wanted at k copies. The result is the bytes of the files that reach k copies in that plan, plus the formula of §13.1 applied to the free space that plan leaves, minus `min_free`, on each disk. So it accounts for data outside `media/` and for the trash, and it is what Atoll would really do; there is no second algorithm. It reads the whole catalog and wakes no disk.
- `atoll capacity [--json]` gives both for k = 1, 2 and 3, from the catalog alone: the disks' capacity, free space and state the last scan recorded, and their files. A file reaches k copies in that plan unless it is left `degraded`, `unavailable`, `name-incompatible` or `name-collision`; none waits for `atoll replicate` there. Bytes are of files: a file of 1 MB at 3 copies counts 1 MB. With `[placement] spread_mode = "strict"`, both use the formula of failure domains. Output: `api/schemas/capacity.schema.json`.

### 13.4 The placement plan

This is the part of the solver that phase 1a needs: which copies to create, how to mark the others, and the state of each file. Rebalancing, the hysteresis and the reachable capacity come with `balance` and `capacity` (phase 1b). The plan moves and removes nothing, needs no clock and wakes no disk: the caller says which disks are past `health.missing_grace`, which files wait for `atoll replicate`, and which are leaving.

- **A copy counts** toward the file's level (`replicas` + 1) when it is:
  - `ok` or `surplus`, on a disk `online`, `standby` or `readonly`;
  - `ok`, `surplus` or `unavailable`, on a disk `missing` not yet past `missing_grace`. So nothing is re-created before then (§18);
  - `pending`, on a disk the plan can write to, of a file not leaving, and kept below. Its size comes off that disk's free space, as the disk has not seen it yet.

  No other copy counts: a `stale` or `corrupt` copy, or any copy on a disk `draining`, `failed` or `retired`. A file holding a copy that does not count has lost a copy.
- **A writable disk** has the role `data` and the state `online` or `standby`.
- **A source** is a copy `ok` or `surplus` on a disk `online`, `standby`, `readonly` or `draining` (§7), whose clock does not run (`gone_ns`, §15.12): a copy its last look found `moved`, `gone`, `uncertain` or `own` has nothing to read at its name until its file is decided, which a tripped guard can put off for days (§15.6), and a job would find it missing at each try. It still counts, as above. The plan reads from one on an `online` disk, then by position. A file with no source gets no job.
- **A file leaving** has a trash job queued for a deletion (§15.12): its deletion is decided, and waits for a disk to take a copy to its trash, as one full, `readonly`, or whose guard holds it. It gets no job, and its pending copies are canceled: a copy made now would be a copy with no queued job, which ends the deletion (§15.12), and the file would be back on the disk the user deleted it from.
- **Order of the files**: the highest `priority`; then those that have lost a copy; then the smallest; then the path in byte order. Where room is short, this order protects the most files for the same bytes: at 60 % full, 98 % of files, against 68 % in the library's order and 6.5 % largest first (`docs/research/placement-order.md`).
- **Refresh, before everything else** (§15.4): in that order of files, while a file not leaving has a source and is below its level, each `stale` copy on a writable disk gets a `refresh` job on its own disk, by position. It needs the whole size free after `min_free`, as the old copy goes to the trash of that same disk. It applies in `manual` mode too. A stale copy of a file already at its level gets no job. A refresh writes a copy, so its name must pass the same checks as in creating: such a copy on a writable disk that refuses the name, or holds a colliding one, gets no job (§15.7, §15.8, §17), and a file with a source is reported as `name-incompatible` or `name-collision`, whatever its level. A pass queues none of these jobs: it sends a stale copy to the trash first, and the copy is then a pending one, with its `replicate` job (§15.4).
- **Creating**, in that order of files, for each file below its level with a source, not leaving and not waiting for `atoll replicate`, one `replicate` job at a time. A disk is a candidate when it meets the hard constraints of §12.4: it is writable, it holds no copy of the file in any state, the file fits in its free space after `min_free`, its name is compatible (§15.8), collides with nothing there (§15.7) and is within the limit of its branch (§15.8), and the path is free. The path is free when no folder, symlink or ignored file has that exact path, and no parent folder of the path is a file, symlink or ignored file there. The parent folders of every copy and entry on a disk are folders there.
  - What the plan has placed so far counts everywhere: free space, names and paths. So the first name in the plan's order gets the disk (§15.7).
  - **Listed disks**: a disk is listed when it carries one of the rule's `tags`, if it has any, and is in one of its `domains`, if it has any. With neither, every disk is listed. With `tags_mode = "strict"` or `overflow = "deny"`, only a listed disk is a candidate. Otherwise any disk is, and placing a copy on one that is not listed is an overflow, reported with a warning. `overflow` concerns these lists only, so `spread = "domain"` in `prefer` mode always falls back to another domain.
  - **`spread_mode = "strict"`**: the file must cover `min(level, m)` failure domains, where m counts the domains among the disks of role `data` that are not `failed`, `retired` or `draining`, and that are listed when only listed disks are candidates. So a disk `missing` or `readonly` still counts, and a file waits for its domain rather than doubling up on another. Copies that count and are kept, and copies placed or refreshed, cover their domains; a domain outside the m does not help. When at least one domain is left to cover, and they are as many as the jobs left to create, or more, a candidate must be in a domain with no copy.
  - **The best candidate**, in this order: listed; in a domain with no copy, with `spread = "domain"`; the most free space after `min_free`; `online` before `standby`; the lowest position. Phase 2b adds `colocate` and the other `new_files` strategies at their place in the preferences of §12.4; until then, free space decides (§12.2).
  - What fits is placed, even when the level or the domains cannot be reached.
- **Copies beyond the level**: when more copies count than the level, the level's number of them is kept, and only their marks change; nothing is removed (§11.3). The order is: a copy not on a `missing` disk, so that a copy that can be read is never marked for a copy that cannot; `user` before `atoll`; where only listed disks are candidates, a copy on a listed disk before one that is not, so that a copy made where the lists now want it outranks one they no longer allow; the oldest `first_seen`; the lowest position; with `pending` copies last. With `spread = "domain"`, a first pass keeps only copies of domains not yet covered, and a second pass fills what is left. So a replica re-created while a disk was away gives way to the copy that comes back (§18), and a copy that spreads the file can outrank a user copy. A copy `ok` that is not kept becomes `surplus`, a copy `surplus` that is kept goes back to `ok`, and a copy `pending` that is not kept, or that does not count, is canceled. An `unavailable` copy keeps its state.
- **The states the plan gives a file**, for `ShownState` (§4): `corrupt` and `conflict` come from elsewhere.
  - `unavailable`: it has no source;
  - `unprotected`: `replicas` is 0, or it waits for `atoll replicate` and is below its level;
  - `pending`: a job is planned, or a `pending` copy is kept;
  - `degraded`: not waiting, and either the copies that count, leaving out those on a `missing` disk, plus the jobs planned, are below its level, or, with `spread_mode = "strict"`, it covers fewer domains than it must. No job fixes the second case until `balance` exists;
  - `name-incompatible`, `name-collision`: it stays below its level and a search for a disk met a disk that refused its name, past the limit of its branch included (§15.8), or that holds a colliding one; or a refresh met one (above). A disk counts here when it is writable, holds no copy of the file and is not left out by the lists, however little room it has.
- **Input**: a plan refuses what cannot be: two disks with one `id` or `position`, a negative `position`, size or free space, two files with one path, a path that is not well-formed, two copies of a file on one disk, a copy or entry on an unknown disk, `replicas` outside 0 to 2, or a value outside its closed list.
- **Output**: jobs in plan order, the refreshes first; a result for each file with a state, a mark or an overflow, by path in byte order, with its states in the order of §4 and its copies by position. A file with no result has the state `ok`, as most files do on a pool in service.
- **The plan of `atoll balance`** (§13.3): a misplaced Atoll copy counts for nothing, so that one is made where the setting wants it. It stays a source, and its disk still holds a copy. Its file's result lists its disk as `misplaced`. Only the jobs of files with a misplaced copy are run, and the results are not recorded.

## 14. Replication

Copy pipeline:

1. End of write detected through close events (`IN_CLOSE_WRITE`, `FAN_CLOSE_WRITE`) or, failing that, through a stable size and modification time during `settle`.
2. The per-path lock is taken (shared by the watcher and every job) and the operation is journaled in `ops`.
3. The source is copied to `<target>/Atoll/tmp/<op-id>.part`; the hash of each 4 MiB chunk is computed during the read and recorded in the job.
4. Modification time and attributes restored on the temporary file (never the creation time), from the source's `stat`; the time is read back; fsync.
5. The temporary file is re-read without cache: each chunk is checked against the recorded hashes, and the whole-file hash is computed during this pass.
6. The destination is resolved at commit time, since the file may have been renamed meanwhile. Missing parent folders are created with the source's attributes, each synced, then its parent. The temporary file is renamed without replacing anything (`renameat2` with `RENAME_NOREPLACE`, or `link` then `unlink` where that flag is unsupported; on Windows, `MoveFileExW` without `MOVEFILE_REPLACE_EXISTING`, with `MOVEFILE_WRITE_THROUGH`). The file is fsynced at its new name, then its folder, as §17.1 syncs every name it makes, and the parent folders' modification times are restored. On Windows, the creation time is then set explicitly (§7).
7. Catalog commit, recording the new copy's change time. A replica exists only after these steps.

Rules:

- If the destination path appeared meanwhile, the rename fails and the case becomes a `conflict`; nothing is overwritten.
- If the source changes during the copy, the copy is abandoned and retried after `settle`.
- A filesystem that cannot hold a modification time clamps it, without an error. Measured: ext4 and XFS stop at 1901-12-13 below, and above at 2446 (ext4), 2486 (XFS) or 2038 (ext4 with 128-byte inodes, XFS with `bigtime=0`); NTFS and btrfs kept every date tried, from 1601 to 9999. The copy is still made, `copies.mtime_ns` records what the disk holds, and the copy stays in sync. `atoll status` shows both dates, with no alert.
- A replica takes the modification time of its source, the copy the job reads, which may not be the original. So where in-sync copies already hold different dates (§18, or a clamp above), a new replica holds its source's. This is deliberate: the content is the same and no new date appears. Reading the original's date would need its disk, which may be asleep or missing, and §9 rules out the catalog's value. It would change nothing for the copies that differ, which keep their dates.
- One copy at a time per target disk (`max_parallel_per_disk`), at low I/O priority, except with `--force`: the lowest level of the best-effort class, which yields the disk to everything else and is never starved, as the idle class would be under a load that never stops.
- Resume after a power cut or USB disconnection: the chunks already in the `.part` file are re-verified against their recorded hashes, then the copy resumes at the first missing or wrong chunk. The source is never re-read from the start, since the whole-file hash comes from the verification pass.
- At startup, `tmp/` keeps the `.part` files that a pending job owns and deletes the others.
- Processing order: the rule's `priority`, the highest first, then files that lost a replica before new ones, read from their copies (§4).
- Attributes copied: modification time; POSIX permission bits and ownership where the target supports them; NTFS read-only and archive attributes. No ACLs or extended attributes in v1.
- Creating, refreshing or moving replicas never triggers a media server notification: the logical tree does not change.
- A replica is always an independent full copy: hard links are never mirrored.

### 14.1 Jobs and pending copies

- Each copy the plan wants made (§13.4) is queued in one transaction: a `jobs` row, `kind = replicate`, with `file_id`, `src_disk` the source the plan chose, `dst_disk` the target, `priority` the rule's and `state` `queued`; and a `copies` row on the target, `state = pending`, `created_by = atoll`, `first_seen_ns` the time it was queued, with no `file_key`, size or times, as the disk holds nothing yet. That row is what later plans count (§13.4): it holds the room and the name, so that two files never get one name on a disk, and a file is not queued twice.
- A pending copy that a plan no longer keeps, or that no longer counts, is canceled: its row goes, with its job. What it left in `tmp/` goes at the next start.
- A job runs as one operation `kind = create` (§14.2) at a time on its target disk. One that ends with `replicated` goes, its pending copy having become `ok`. After `io-error`, `changed` or `no-room` it stays `queued`, `attempts` one more, and after `changed` `not_before_ns` is `settle` from now (§14). After `conflict`, `path-taken` or `name-refused` it goes with its pending copy, and the next plan decides again: the next walk of that disk finds what took the path, and that disk then holds a copy of the file.
- A file in `conflict` (§15.7) is never queued, as nothing says which version to copy.
- A stale copy that reaches the trash becomes a pending copy, and its `replicate` job reads the copy that changed, whatever the mode and `settle` (§15.4). A modification drops every queued job of its file, as its source may be stale; a job that starts from a copy no longer a source (§13.4) goes with its pending copy, as `changed`, and the next plan decides again.
- The daemon queues, in each pass and after the work of propagation, the copies the plan wants of every file not in `manual` mode once `settle` has passed since the file was first seen (`files.first_seen_ns`). A file in `immediate` mode waits for nothing once the daemon that runs for good saw it closed after a write by someone other than Atoll: a group of its watcher at that path, holding `written`, ripened, as nothing happened there for `deletes.debounce` (§15.1). That is `closed and stable` (§12.3). A file no such group told of, as one a walk finds, or any file of `atoll daemon --until-idle`, which watches nothing, waits for `settle`, as in `deferred`. With no watcher, that is all that tells a file still being written; a source that still changes is found by the copy, which ends `changed` and waits `settle` again. It then runs, one at a time, every queued job whose `not_before_ns` has passed, highest `priority` first, then in the order the plan queued them. The copy of a file in `scheduled` mode runs only inside `[replication].window`, in the time zone of `timezone`: outside it, the job waits in the queue, counts in `waiting`, and `next_due` says when the window opens, which wakes the daemon then (§15.13). `atoll replicate --now` runs it whatever the time.
- A job whose operation the reprise could not conclude stays `running`, and does not run: what is at its target may be its own copy, and nothing is decided of it until that operation is (§17.2). Its line says `io-error`.
- A `replicate` job that ends with no copy of its file left takes the file with it: nothing is left to copy from.
- The outcomes of a job: `replicated`; `queued`, to run when it is due, by a run that only queues; `to-replicate`, in a dry run; `changed`, the source no longer what it was when the copy started, or not what the file's hash says; `conflict`, something took the target path since the look, which the next walk reports (§14); `path-taken`, a parent of the target that is not a folder; `name-refused`, a name the driver refuses (§15.8); `no-room`, `ENOSPC` while copying; `disk-unavailable`, its source or target not confirmed in this run; `io-error`, anything else.

### 14.2 The operation `create`

- `kind = create`, `disk_id` the target, `path` the file's logical path when the copy starts, `path2` the name of the copy in progress in `<workdir>/tmp/`, `<op id>.part`, and `file_key` its inode, written once that file is made and before anything is copied into it. It is journaled with a `replicate` event, which names the source too.
- First a look at the target, the file's path now: something at the path itself ends the job as `conflict`, and a parent that is not a folder as `path-taken` (§14.1), with nothing journaled or copied. What comes after that look is found at the rename, below. Then, for a copy that starts from nothing, a look at the source: a source gone, or no longer what its row says (its size, modification time, change time or inode), ends the run as `changed` with nothing journaled or made, and the job waits `settle` (§14.1), as a source a guard holds may stay so for days and must not cost an operation at each try. A source that cannot be read ends it as `io-error`, as below. A copy that resumes finds its source changed as it reads it.
- In order, with the crash points of the conformance build (§29.4): the operation and its event (`journaled`); `<workdir>/tmp/` made where it is missing, each folder synced, then its parent; `<op id>.part` made with mode 0600, never replacing anything, and synced, then its folder; the copy, with the chunk hashes recorded in `job_chunks` as it goes, a transaction every 64 MiB and at the end (`copied`); the source looked at again, and the copy dropped as `changed` if its size, modification time, change time or inode moved; times and attributes restored, the modification time read back, `fsync`; the re-read without cache, each chunk against its hash, which gives the whole-file hash, recorded in the job's `payload` before anything is renamed (`verified`). When the file has a hash and that is not it, the copy is dropped as `changed`. Then the target is resolved: the file's path now, each missing parent made as an operation `kind = mkdir` of its own with the mode and owner of the same folder on the source disk (§15.12, step 3); the rename without replacing, `fsync` of the file at its new name, then of the target folder (`moved`, `target-synced`), the time of the target folder restored, `fsync` of the tmp folder (`source-synced`).
- In one transaction: the operation `done`; the pending copy becomes `ok`, with what a look at its new name finds, its mode included, `created_by = atoll`, `first_seen_ns` and `verified_ns` now; the file takes the whole-file hash and the chunk hashes when it had none; the source copy gets `verified_ns`, as its bytes were just read against them; and the job goes, with its chunks.
- A copy dropped as `changed` loses its `.part`, as what it holds is no longer the source's, and the operation is `undone`; so does one dropped as `no-room`, which would otherwise hold the room it lacks. After another failure the operation is `undone` too, and the `.part` stays for the job to resume, or goes when the job goes (`conflict`, `path-taken`, `name-refused`). On a disk that cannot be relied on, the operation stays `planned`, for the reprise (§17.1). So nothing stays in `tmp/` but what a job will resume.
- Resume: a job whose `.part` is still there re-reads it, chunk by chunk, against its recorded hashes, and copies from the first chunk missing or wrong; the source is read from there only. The `.part` keeps its name, so the new operation's `path2` names the old one's.
- Reprise (§17.2), with the target the file's path now and the source the `.part`, both compared by inode with `file_key`:

| Target | `.part` | Then |
|---|---|---|
| `same` | `absent` | it finishes as above, with the hashes the job recorded |
| `same` | `same` | `link` was interrupted: the name in `tmp/` goes, and it finishes |
| `absent`, `other` | `same` | it is `undone`, and the `.part` stays for its job to resume; with no job left, it goes |
| `absent`, `other` | `absent` | it is `undone`, and the job starts again from nothing |

- When a command that writes opens a disk, after its reprise, that disk's `<workdir>/tmp/` keeps the `.part` files that a job owns and loses the others named by an operation `done` or `undone`, and nothing else in it is touched. So no disk is woken for this alone.

### 14.3 `atoll replicate`

`atoll replicate <path> [--now | --force] [--dry-run]`, `<path>` a file or a folder, compared by whole components as in §15.11.

- It plans as `atoll scan` does (§13.4), with the files below the path no longer waiting for it (`manual`, §12.3), and queues the jobs of the files below the path (§14.1). With `replication.enabled = false` it queues and copies nothing, and says so on standard error.
- Without `--now` or `--force`, it only queues: the daemon runs the jobs when they are due (§12.3). With `--now`, it then runs every queued job of those files at once, ignoring `[replication].window` and `not_before_ns`. `--force` does the same, and also re-reads first the copies Atoll made of those files, as `atoll scan --full` reads a copy (§15.10): one that reads as its file's hash is verified again, one that does not is no longer, and one that no longer matches its row is a change, for the next scan. With `--dry-run` it writes nothing, and lists what it would queue as `to-replicate`.
- It holds the catalog's `flock` (§25), resolves the planned operations of each disk it writes to first (§17.2), and lists one line per job it queued or ran, by path in byte order, then by target position. Output: `api/schemas/replicate.schema.json`.
- It exits with 0 when every job it ran was `replicated`, when it only queued, or with `--dry-run`; with 4 when a job ended otherwise, or a disk could not be relied on; with 1 when the configuration or the catalog cannot be used; and with 2 on a usage error, `--now` with `--force` included. Creating moves and deletes nothing, so it takes no `--yes` (§25).

## 15. Changes: detection and propagation

### 15.1 Detection

- Sources: watcher events, rescans and the `ops` journal. Events are hints; rescans are the truth.
- Linux watcher: fanotify, on every driver Atoll writes to, otherwise recursive inotify (measured on Linux 6.12 and 7.3, `docs/research/watcher.md`):
  - on ext4, XFS, btrfs and ntfs3, one filesystem-wide mark per disk (`FAN_MARK_FILESYSTEM`, which needs `CAP_SYS_ADMIN`), in a group that reports each entry's folder and name and the file itself (`FAN_REPORT_DFID_NAME`, `FAN_REPORT_FID`, `FAN_REPORT_TARGET_FID`): a rename comes as one `FAN_RENAME` event with both names;
  - on ntfs-3g and lowntfs-3g, whose filesystem has no fsid, a filesystem mark fails with `ENODEV`, so the group puts an inode mark on each folder below `<root>`, for the events of its entries (`FAN_EVENT_ON_CHILD`), with `FAN_UNLIMITED_MARKS`, as the default limit scales with memory (about 50,000 marks for 6 GiB). A folder made or moved in is marked as it appears, then listed, as it may have been filled before its mark;
  - inotify only where fanotify cannot be had at all, with the same watch per folder, and no process.
  - fanotify reports the process behind each event, on all six drivers. Operations made by the mergerfs process are view operations: they already reached the copies mergerfs meant to change, so Atoll reconciles them without propagating. inotify gives no process, so grouping (below) handles that case.
  - The group asks only for the events Atoll needs, so that its queue lasts: `FAN_RENAME` rather than `FAN_MOVED_FROM`, `FAN_MOVED_TO` and `FAN_MOVE_SELF` where the group has it, and `FAN_CLOSE_WRITE` rather than `FAN_MODIFY`. The queue keeps its default bound, 16,384 events, and is read without waiting. An unlimited one would hold a few hundred bytes of kernel memory per event waiting, without bound in a mass change (measured).
- Windows watcher (phase 4): ReadDirectoryChangesW.
- An overflow or watcher error triggers a rescan of the affected disk: `IN_Q_OVERFLOW`, `FAN_Q_OVERFLOW`, or a zero-byte result or `ERROR_NOTIFY_ENUM_DIR` from ReadDirectoryChangesW.
- Rescans: at startup, after an overflow, and every `maintenance.rescan_every` (7 days by default) inside `maintenance.window`. Watching does not wake disks (a mark caused no read of the device, measured); rescans do.
- Events are grouped per logical path over `deletes.debounce` (30 s by default, longer than a USB disk's spin-up) before Atoll decides what happened.
  - An event is kept only below a disk's `<root>/`, as a logical path, `""` for `<root>` itself: what happens in `<workdir>/` or elsewhere on the disk is no part of the library. A folder whose path cannot be told, as its handle no longer opens, gives `""` too, so that the disk is looked at all the same.
  - What it says is one of `created`, `written` (a file closed after a write), `attrib`, `deleted`, `moved-from` and `moved-to`, and whether it names a folder. A rename gives `moved-from` at the old path and `moved-to` at the new one, whether the watcher reports it as one event or as two. An overflow is `overflow` at `""`.
  - Who made it is `view` for the mergerfs process that serves `view.mount` (its name `mergerfs`, and that mount point among its arguments), `atoll` for a process of Atoll's own executable, `user` for any other, and `unknown` where the watcher gives no process (inotify).
  - A group gathers the events of one disk at one path: what they said and who made them, each set in byte order, with the times of the first and the last. It is ripe once `deletes.debounce` has gone by since its last event, `now - last >= debounce`, and leaves the grouping then. Ripe groups come out by disk identifier, then by path, in byte order. An `overflow` group asks for a rescan of its disk. A group still waiting says when it will be ripe: the earliest of those times is when to look again.
  - The watcher only says where to look: what happened is what the next look at that disk finds (§15.12), so an event that is lost or wrong costs a look, never a decision.
- File identity (`same`): on one disk, the same inode or NTFS file ID and the same creation time (§7); where either has none, the same inode, size and modification time. A row with no inode recorded matches no inode, so it is the `same` as nothing. The change time is no part of it, as a rename moves it on every driver, and the inode alone is not enough: a new file right after a removal takes the same inode 40 times of 40 on ext4 and on ntfs3, with another creation time each time (measured on Linux 6.12 and 7.3). If this were wrong, the copies at one name would differ, which a read reports (§15.10), and nothing would have left the library on the strength of it (§15.12).
- Hard links: each name under the root that a look has recorded is a separate logical file, so a name that already has its row is never the target of a rename. This covers Sonarr/Radarr links between `Téléchargements/` and a library: removing one name is a deletion of that name. A name with no row is the target of a rename when it is the only one the file has, however many links it has outside the root: a torrent client's own link must not turn a rename into a deletion.
- Operations recorded in `ops` are Atoll's own and are never interpreted as user actions.

### 15.2 Through the merged view (mergerfs)

- Renames and deletions reach every copy at once (`category.action=epall`). Metadata changes (`utimens`, `chmod`, path-based `truncate`, `setxattr`) also reach every copy; `open` and writes reach only the copy on the first branch (`category.search=ff`).
- A rename does not reach a copy whose filesystem refuses the new name (§15.8). If at least one branch accepts it, the application gets success and mergerfs removes the source from each branch that refused, with no error and no trash; its documentation names no option that turns this off. So a file loses its copy on that branch. A folder that is not empty cannot be removed: it stays at the old name with everything below it, and the merged view shows both folders. If every branch refuses, the application gets the error and nothing changes. Measured with mergerfs 2.40.2, on ntfs-3g and on ntfs3 (Linux 6.12, and 7.3.1 with `ads=0`); the mergerfs documentation states the same rule. The link guard keeps a removed copy's data (§17). Atoll handles the result as in §15.3.
- ntfs3 with `ads` does not refuse a name with `:`, even under `windows_names`: it reads `m:n` as the stream `n` of the file `m`. Through the merged view, a rename onto `m:n` succeeds and replaces the content of the file `m` on that branch, even when that was the only copy of an unrelated file. With no file `m`, the copy becomes a file `m` there, and the merged view shows it too. The page cache hides the damage until the cache is dropped or the disk remounted. Measured on Linux 7.3.1. Atoll cannot stop mergerfs from doing this, so it refuses `ads` (§10).
- An atomic save (`rename(tmp, file)`) renames only on the branch holding `tmp`; mergerfs then removes `file` from the other branches. Atoll sees "replaced on one disk, deleted on the others" and treats it as a modification (§15.4), never as a deletion.
- Phase 1 limit: a deletion through mergerfs removes every copy immediately. Mitigations: the optional link guard (§17), Samba's `recycle` module with a visible repository at the pool root outside the library folders (`recycle:repository = Recycle`), and the Sonarr/Radarr recycling bins.

### 15.3 Renames directly on one disk

- The other copies are renamed the same way (instant, same filesystem), with no re-copy. A folder rename takes one rename per disk (§15.12).
- A disk that refuses the new name does not get the rename: Atoll never writes a name that is incompatible with that disk (§15.8) or that collides on it (§15.7), or a path past the limit of its branch (§15.8). The copy left at the old name cannot stay in `media/`, or the merged view would show both names. Once the renamed copy has been read and verified, and is on a disk walked in this pass (§15.12):
  - a copy unchanged since it was last in sync, verified when it was last read, of a file not in `conflict` or `corrupt`, goes to the trash at the next pass (`reason = renamed`), and the file then lacks a replica. A copy never read waits for a `--full`, as nothing says that it holds what the renamed one holds, and one in a `conflict` may be the only one of its version. Another compatible disk gets a new copy. Failing that, the file keeps fewer replicas than wanted, is reported as `name-incompatible` or `name-collision`, and an alert is raised. Never an automatic rename;
  - a copy changed meanwhile is a modification and stays where it is: `conflict` (§15.7), every version kept;
  - a copy whose target differs from where it sits, first at a folder on the way, only as that NTFS disk reads as one name, as a case-only rename does, is refused because that disk holds that folder: only a folder's operation can do it (§15.12). It stays, is reported as `name-refused`, and never goes to the trash;
  - the renamed copy counts as a user copy from then on, so the trashed copy is never the last user copy of the file;
  - the mass-deletion guard counts these copies (§15.6). A large folder renamed to a refused name waits for confirmation, and until then the merged view shows both folders. If the user meanwhile renames it to a name the disk accepts, the rename reaches those copies with no re-copy;
  - an old folder left empty is removed only if Atoll created it (§6.1, item 8).
- Correcting the name later renames the remaining copies. A copy already in the trash is not brought back, so the new replica is a full copy.
- A copy that mergerfs removed (§15.2) has the same outcome: the file lacks a replica. A folder mergerfs left behind, and a disk that was away during the rename (§18), follow the rules above.
- A copy already at the new name on such a disk, put there by the user, an application or mergerfs, stays a normal copy and is only reported: Atoll does not touch a name it did not write (§15.7).

### 15.4 Modifications

Each copy is compared with the recorded current version:

- **One copy changed** (content differs from the recorded hash and its change time moved): it becomes the current version, whatever its modification time, and a user copy. The other copies become `stale`.
- **Several copies changed, with different content**: `conflict` (§15.7); nothing is overwritten.
- **Only metadata changed** (modification time, permissions or owner; same content after re-hashing): the recorded metadata is updated and applied to the other copies. A `touch`, or Radarr/Sonarr's "change file date", costs one re-hash and no copy. A copy's modification time is compared with its own record (`copies.mtime_ns`), so a difference that was already there is no change, and is never applied to the others: a time its filesystem clamped (§14), or one found when a disk was added (§18). Only a change is applied, and the times are then equal again. A copy's mode is compared the same way, with `copies.mode` (§9): a `chmod` is applied to the other copies, and a difference that was already there is not, as a mode that NTFS cannot hold. A copy whose row has no mode yet gives none. A copy's owner, its user and group ids, is compared the same way, with `copies.uid` and `copies.gid` (§9): a `chown` is applied to the other copies where their disk takes it, and a difference that was already there is not, as the owner an NTFS driver's mount options give every file. A copy whose row has no owner yet gives none.
- **Replacement** (a copy replaced on one disk and removed from the others within the grouping window, typically through mergerfs): a modification of the surviving copy. A pass decides the copies removed first (§15.12), then the one left.
- **Stale copies** are moved to the trash at once (`reason = overwritten`), so mergerfs never presents them, then refreshed first, whatever the mode.

**In a pass** (§15.12, step 4), once the other decisions are made, a file is looked at when a copy of it is `edited`, or `replaced` by a regular file: these are its changed copies. It waits, with no line, while:

- it is in `conflict` or `corrupt`: no version is the current one, nothing changes until that is resolved, and its changed copies count in nothing;
- a copy of it is `moved`, `gone`, `uncertain` or `own` in this look, or its row says `moved` and this look could not get to it, or it is `replaced` by anything but a regular file: what becomes of those is decided first;
- a job of it is `running` (§14.1), as what is at its target may be a copy of the version that came before, or an operation is left `planned` at or above the name of a copy of it, as only a reprise can tell what is there (§17.2);
- the mass-modification guard of the disk of its first changed copy by position stands, and that copy changed after the guard's last answer (§15.6): it is held, and nothing is read;
- a changed copy is not due: its change time, as its walk found it, is less than `settle` old (§12.3, the policy of the file at its size), so that a file still being written is not read. It counts in `waiting`, and `next_due` says when the first is due. A change time in the future, as a disk written by another machine can hold, waits for it. With `--dry-run` everything counts as due.

Then, the first that applies:

| The changed copies | Outcome |
|---|---|
| one is `stale` | `conflict`: a version that came before was changed |
| of two sizes or more | `conflict` |
| one, with another size than the file's | `modified`, with nothing read |
| several, with another size than the file's | each is read: one content is `modified`, with that hash, and several are a `conflict` |
| of the file's size | each is read, and so is the reference: the file's hash, or where it has none, the first copy by position that is `there`, `ok` or `surplus`, on a disk this pass can read. With neither, the file waits, and nothing is read, unless every copy of it changed. Several contents among the changed copies: `conflict`; the reference's: `metadata`; another, or one with no reference: `modified`, with that hash |

- **A read** goes once over the whole file, which feeds both hashes (§16), at the lowest level of the best-effort I/O class, as a copy does (§14). Before it, a changed copy must still be the file its walk found, with the same change time, and the reference copy what its row says, as `--full` compares it (§15.10); neither may move during the read. Otherwise nothing is decided, and the file waits for the next pass, as it does when a copy to read is on a disk this pass can no longer read. A disk that fails a read is `missing`, as in a scan.
- **`modified`**: first the mass-modification guard of the disk of its first changed copy (§15.6), which may hold it instead, or answer it: under a reject it is a `conflict`, and under a confirm its event has `answered`. Then, in one transaction: a `modified` event, when a copy becomes stale; each changed copy takes what the look before the read found (inode, creation time, links, size and times, and `first_seen_ns` now when it was `replaced`), becomes `created_by = user`, and gets `verified_ns` now when it was read, and NULL otherwise; the file takes their hash and chunks, or none when nothing was read, and their size with its original in step 7; every other copy becomes `stale` and loses `verified_ns`; every queued job of the file goes, with its pending copies, as a deletion no longer stands and a source may be stale; and each stale copy that holds a file gets a trash job, `reason = overwritten`, `src_disk` the first changed copy by position, `priority` the rule's.
- **`metadata`**: what moved on a changed copy, against its own row, is applied to the other copies: its modification time, its mode where its row has one, and its owner where its row has one. For each, of the changed copies on which it moved, the one whose change time is the latest, the first by position on a tie, is the last change, and gives it; the time, the mode and the owner may come from different copies. Each other copy that is not `stale` gets what it holds otherwise, as its row says or for a changed copy as its look found it: a time compared at the coarser precision of its disk and of the disk of the copy that gives it (§9), a mode exactly, and an owner by both its ids, an unknown one being another. On a disk this pass can write to, where its look reached it, it gets them through an operation `kind = chown`, then one `kind = chmod`, then one `kind = utimens` (below), as a `chown` may clear setuid and setgid; otherwise it becomes `stale` and gets a trash job, as for `modified` but with `src_disk` the copy that gives the time, or the mode when no time moved, or the owner when neither did, and its refresh brings them once its disk is back. Then, in one transaction, each changed copy takes what the look found, as above, its mode and owner included, or what its own operations left, keeps `created_by`, and gets `verified_ns` now; a file that had no hash takes it, with its chunks, and the reference copy gets `verified_ns` too; and each copy made stale gets its trash job. An operation that ends otherwise than `applied`, or `kept` for a `chown`, leaves the file as it was, for the next pass to decide again, reading it again; what an operation applied stays recorded in its row, so that it is not applied twice. With nothing moved, as after a change to extended attributes alone, nothing is written to a disk, and only the rows of the changed copies change.
- **An operation `chmod`**: `path` the copy's place, `file_key` its inode, journaled with a `chmod` event that holds the mode (`mode`). A look right before must find the copy as its row says, or for a changed copy as its look found it; otherwise the outcome is `changed`, and nothing is written. The mode is set on the open file, then the file is synced and looked at, and in one transaction the operation is `done` and the row takes the mode and change time the look found, which are what the disk holds: NTFS keeps only what its driver makes of a mode, as for a copy (§14), and that difference is then one that was already there. The rest is as for `utimens`. The outcomes are the same.
- **An operation `chown`**: `path` the copy's place, `file_key` its inode, journaled with a `chown` event that holds the owner (`uid`, `gid`). A look right before must find the copy as its row says, or for a changed copy as its look found it; otherwise the outcome is `changed`, and nothing is written. The owner is set on the open file (`fchown`), then the file is synced and looked at, and in one transaction the operation is `done` and the row takes the owner, mode and change time the look found. Measured on Linux 6.12 and 7.3: ext4, XFS, btrfs and ntfs3 keep the owner given; ntfs-3g and lowntfs-3g, mounted without `permissions`, answer the call and keep root's, which the row records, so that the next pass applies nothing. A driver that refuses it, with `EPERM`, `EINVAL` or `EOPNOTSUPP`, as does a run without the right to give a file away (`CAP_CHOWN`), leaves the copy with its own owner: the operation is `undone`, the outcome `kept`, with a line of the log, and the difference is then one that was already there, as for a copy (§14). The rest is as for `utimens`. The outcomes are `applied`, `kept`, `changed` and `io-error`, and `to-apply` in a dry run.
- **An operation `utimens`**: `path` the copy's place, `file_key` its inode, journaled with a `utimens` event that holds the time (`mtime_ns`). A look right before must find the copy as its row says, or for a changed copy as its look found it; otherwise the outcome is `changed`, and nothing is written. The time is set on the open file, the access time left as it is, then the file is synced and looked at, and in one transaction the operation is `done` and the row takes the modification and change times the look found, which are what the disk holds (§14). An operation that fails on a disk that cannot be relied on stays `planned`, for the reprise (§17.2). The outcomes are `applied`, `changed` and `io-error`, and `to-apply` in a dry run.
- **`conflict`**: the file becomes `conflict` (§15.7), and every version stays until that is resolved. Nothing is written to a disk. The changed copies lose `verified_ns`, as nothing says any more which version a hash describes, and no other row changes. A stale copy that changed loses its trash job.

**A stale copy** is a copy like any other until it reaches the trash: it follows a rename of its file, and goes when its file is deleted. It is never a source, never presented by Atoll's own view, and never read as the current version. Its trash job runs in step 5 of the pass, and once the copy is in the trash its row stays, as a pending copy (§14.1) with a `replicate` job from `src_disk` to its own disk and the trash job's `priority`. So the room and the name it held stay held, and the copy is made whatever the mode or `settle`, as its file has gone through both. A stale copy on a disk that cannot be written to waits for it, and mergerfs may present it until then.

**Limits**: a swap of two names on one disk stays as §15.12 says, and a copy `replaced` by anything but a regular file is only reported. An overwriting move is a deletion and a rename (§15.12).

### 15.5 Deletions directly on one disk

- `deletes.direct = "propagate"` (default): the other copies go to the trash, but only copies unchanged since they were last in sync (same inode, size, modification and change times), and known to have held what went (§15.12). A copy changed meanwhile is a modification and survives.
- `deletes.direct = "restore"`: the deleted copy is re-created from another copy, which comes with the copy pipeline (§14). Until then nothing propagates.
- Before treating a missing copy as deleted, Atoll looks for it (`same`, §15.1) in its own `trash/` and `tmp/` folders: if it is there, it was an interrupted Atoll operation, not a user deletion. The link guard's `links/` is not one of those: its link holds the data of every copy, so a copy found only there was removed by someone else. It is a deletion like any other, and what its link holds goes to the trash (§17).

### 15.6 Safety guards

- Mass-deletion guard, for each disk: if more than `deletes.mass_guard.files` copies, or more than `percent` of a disk's copies, disappear within `window`, propagation pauses and an alert asks for confirmation (`atoll guard confirm` or `reject`). Copies that a refused rename would send to the trash count too (§15.3).
  - The count on a disk is the sum of its copies with `gone_kind = gone` and no `old_lpath`, of a file with no copy `moved` on a disk not `missing` or `retired`, later than the last confirm or reject, however old: a pass after a month's gap sees them all; its `copy-gone` events within `window`, whose `gone_ns` is later than that; and its copies that a refused rename would send to the trash (§15.12), with no job yet. A `moved` left from before a disk went away says nothing now, as the decision will be a deletion.
  - A copy of a file with nothing left to send to the trash counts toward `files` and not toward `percent`: nothing can be stopped, so a small pool does not ask at each deletion through mergerfs, and a mass disappearance, a damaged filesystem or a wrong `mv`, still holds. A copy on a `missing` or `retired` disk is something left, whatever its row last said.
  - It trips when the count is more than `files`, or when `left * 100 > percent * rows`, compared exactly (§29.5), `left` being the count without the copies of a file with nothing left to send, `rows` being the copies on that disk when the pass began, before its walk adds any, with those events, as they were its copies too. While a `guard` event stands, `rows` are its copies first seen before that event instead, with those events: what lands on the disk since must not dilute a hold that nobody has answered. So below 20 copies at 5%, a direct deletion asks. It is worked out once, at the start of the decisions.
  - Tripped: nothing `gone` on that disk later than the last confirm or reject is decided, so its rows stay, and if the copies come back nothing was lost, history included. Trash jobs with that `src_disk` wait. Renames go on, but for a folder renamed on another disk with such a copy below it, which waits for the answer (§15.12). A `guard` event at level `warning` when there is none since the last confirm, reject or clear on that disk. As this is worked out from rows and events, no crash can lose a hold.
  - Found no longer tripped with a `guard` event standing, as copies came back or what it counted left the window: a `guard-clear` event, as the hold ended by itself and the next trip is another one, with a time of its own for a confirm to go by.
  - `atoll guard list` reads the catalog only: for each disk by position, whether its guard is tripped, as a `guard` event that stands says or else as rows and events say now; the count; and when tripped the paths held, in byte order, and none otherwise.
  - `atoll guard confirm --disk <id>`: an event with `up_to`, the time of that last `guard` event, in integer nanoseconds, so that what appeared since then is still asked about. The next pass decides those rows. A copy that a refused rename would send has no time to compare with that, so it gets its queued job in the same transaction. A `gone` row the guard left out, beside a copy `moved`, was not asked about: its clock starts again, so that the guard asks about it should that copy stop reading so. `atoll guard reject --disk <id>`: in one transaction, those `gone` rows go, with `old_lpath` or not, with any left out beside a copy `moved`, as no deletion must come of one nobody asked about, a file left with no copy goes, what is left is as after a decision that dropped them (§15.12), for its hash, chunks and conflict, its queued jobs for a deletion go, a copy that a refused rename would send becomes a `canceled` job, with or without a job already, with the same `up_to`. With no `guard` event since the last confirm, reject or clear, either does nothing. Both write nothing to a disk, so they take no `--yes`, and they take the `flock`.
- Mass-modification guard, for each disk: if more than `modifies.mass_guard.files` files, or more than `percent` of a disk's copies, are `modified` from a copy on that disk within `window`, the modifications from that disk stop there, and an alert asks for confirmation (`atoll guard confirm` or `reject`, with `--kind modifications`). A tool that rewrites a library in place, as one that encrypts it, would otherwise make every other copy stale and send it to the trash (§15.4).
  - It counts the `modified` decisions whose first changed copy by position, the event's `disk`, is on that disk, and that made at least one other copy stale: a file with no other copy loses nothing. Not `metadata`, which a `touch` or a `chmod` through mergerfs makes of every file and which destroys nothing, nor `conflict`. The count is that disk's `modified` events within `window`, later than the last confirm or reject of this guard and without `answered`, with the decisions of the pass so far.
  - It trips as the mass-deletion guard does, when the count is more than `files` or `count * 100 > percent * rows`, compared exactly (§29.5), `rows` being the copies on that disk when the pass began. But it is a circuit breaker: it is worked out before each such decision, counting it, and the decision that would trip it is not applied. What was decided before stays, at most as many files as it allows, whose other copies are in the trash; and no file has to be read before the first is decided.
  - Tripped: a `modify-guard` event at level `warning`. While one stands since the last confirm or reject of this guard, a file whose first changed copy by position is on that disk, and changed after that answer by its change time, waits, with no line and nothing read, and the pass is held. It stands until someone answers, with no `clear`: what it holds stays changed, and a hold that ended as the window passed would let through what it stopped. As it is worked out from events, no crash can lose it.
  - `atoll guard confirm --disk <id> --kind modifications`: a `modify-guard-confirm` event with `up_to`, the time of the `modify-guard` event that stands. The next pass decides the files whose changed copy on that disk changed by then, by its change time, as it would have, and their `modified` events, with `answered`, count for nothing; a copy that changed since is still asked about. `atoll guard reject --disk <id> --kind modifications`: a `modify-guard-reject` event, likewise, and the next pass decides those files, a `modified` decision being a `conflict` instead (§15.7): every version stays, and nothing is overwritten. A `metadata` decision stays one. With no `modify-guard` event since the last answer, either does nothing. Without `--kind`, or with `--kind deletions`, they answer the mass-deletion guard.
  - `atoll guard list` gives, after the mass-deletion guard of each disk, its mass-modification guard: whether it is tripped, as a `modify-guard` event that stands says; the count; and when tripped the paths it counted, in byte order, as which files wait only a look can tell.
- An absence is interpreted only after the disk's identity has been confirmed in the same scan (§10).
- Every propagated deletion goes to the trash; nothing is deleted immediately.
- In phase 1, the guard can only stop propagation: deletions made through mergerfs have already reached every copy.

### 15.7 Conflicts and name collisions

- Same logical path with different content on two disks, at adoption, after concurrent changes or after `catalog rebuild` (§9): `conflict` state, every version kept.
- Atoll's own view keeps presenting the copy it presented before; for a conflict found when adding a disk, the copy already in the pool. With mergerfs, the first branch's copy is shown.
- Resolution: `atoll conflicts resolve <path> --keep <disk>`, with confirmation; the other versions go to the trash.
  - `atoll conflicts list [--json]` gives each file in conflict, by path in byte order, with its copies by disk position: the size and modification time each disk holds, as the catalog last recorded them. It reads the catalog only. Output: `api/schemas/conflicts-list.schema.json`.
  - `atoll conflicts resolve <path> --keep <disk> [--dry-run] [--yes] [--json]` keeps the copy on `<disk>` and sends every other copy to the trash, as `overwritten`. Without `--yes`, it says what it would do and exits with 3.
  - A walk leaves the copies of a file in conflict as it found them, so the command looks at each copy again. The copy kept is what its disk holds now. Each other copy goes as its disk holds it, its row first brought to that. A copy no longer on its disk loses its row.
  - A `conflict-resolve` event comes before anything moves. Once no other copy is left, the file takes the kept copy's size and modification time. Its hash, its chunk hashes and the kept copy's verification go, to be read again, and so do its queued jobs. The file is `pending`, and the next pass makes its replicas from the kept copy.
  - A kept copy that is gone, or on a disk that cannot be reached, moves nothing. Another copy on a disk that cannot be reached stays, and so does the conflict, with exit 4, until a run that reaches it.
  - It exits with 1, with nothing on standard output, when the catalog has no file at the path, the file is not in conflict or has no copy on `<disk>`, or the configuration has no such disk. Output: `api/schemas/conflicts-resolve.schema.json`.
- Paths with the same `lpath_key` (§15.8), which differ only by NFC/NFD form, by case as NTFS and Samba compare it, or by both, are reported as `name-collision`: the merged view shows both, and Windows or macOS clients may treat them as one name. Folders count too: `Films/a.mkv` and `films/b.mkv` are reported, because `Films` and `films` have the same key. Nothing is changed automatically.
- Atoll never writes a colliding name to an NTFS disk. This is a hard constraint of the solver (§12.4), checked on NTFS targets only: ext4, XFS and btrfs hold both names, and the variants that would not are refused (§10). Atoll checks it itself, as a pure function of the names, and never relies on the driver:
  - a path collides with a name on the target when, at some component, their prefixes up to that component have the same `lpath_key` and different bytes. So `film.mkv` collides with `Film.mkv`, `films/b.mkv` with the folder `Films`, and `films/a.mkv` with a file named `Films`;
  - the names on the target are its files, folders and symlinks (`copies`, `entries`) and those planned earlier in the same plan, so the first name in the plan's order gets the disk;
  - a path never collides with itself, and a case-only rename does not collide with its own old name;
  - a name without a key, which is not valid UTF-8, never collides; it cannot go to NTFS anyway (§15.8);
  - `lpath_key` is wider than NTFS: the check also refuses an NFC/NFD pair that NTFS could hold, and `ς` against `σ`. Such pairs are rare and are reported anyway.
- A colliding copy goes to another disk. Failing that, the file keeps fewer replicas than wanted, is reported as `name-collision`, and an alert is raised. So a folder `films/` against `Films/` blocks everything below it on that disk until the user renames one of them. Never an automatic rename. A rename to a colliding name follows §15.3.
- Names already side by side on an NTFS disk, created outside Atoll, are only reported: Atoll does not touch them. mergerfs can create them too: on ntfs-3g, a rename to `OTHER.MKV` beside `Other.mkv` succeeds (measured).
- A stated limit: the check knows only the names a walk has recorded. A colliding name made on an NTFS disk since its last walk, which has no row yet, is not seen; the next walk reports it.

### 15.8 Extended names

- Logical key: the exact relative path, byte for byte on Linux and as WTF-8 on Windows (which keeps unpaired UTF-16 surrogates); never normalized. Every copy of a file has this exact path, as mergerfs merges entries by exact name.
- `lpath_key` is computed only to detect collisions (§15.7); it is NULL for names that are not valid UTF-8. It is the NFD form of the path, then the NTFS upcase table applied to each UTF-16 code unit, then the NFC form. NFD comes first so that a precomposed lower-case letter collides with its decomposed upper-case form (`ǰ` and `J` followed by U+030C).
  - The table is the one Windows 10 and 11 and `mkntfs` write, checked byte for byte on volumes they formatted (ntfs-3g says Windows 7 and 8 write it too): Unicode 5.1 simple uppercase on the BMP, kept only where simple lowercase maps it back (973 entries). Windows XP, Vista and Samba also merge `ς` with `σ` and `Σ`, so the table adds `ς` → `Σ`. It is generated from Unicode data, never copied.
  - Characters above U+FFFF are left unchanged, as ntfs-3g, ntfs3 and Samba compare them exactly (and, as the on-disk format implies, Windows).
  - The key depends on the normalization library and its Unicode version: on a name with a run of more than 30 non-starters, Go's inserts U+034F and Rust's does not (§29.5). Two releases of one library may also normalize differently, with the same Unicode version. The catalog records what computed the keys (`meta.lpath_key_version`, §9: the language, the library, its release and its Unicode version); when it is missing or differs in any way from the running version, Atoll recomputes every key before it looks for collisions. So the first run after an upgrade of the library, and the first Rust run on a catalog written by Go, recompute them, and keys from two implementations or two releases are never compared.
  - So `ς` and `σ` share a key, while `ß`, `ẞ` and `ss`, or `ſ` and `s`, do not. The key serves collisions only; `ignore_case` matching follows Unicode simple case folding (§11.3).
- A replica must keep the exact name, so compatibility is checked before every copy to a target filesystem. The check is a pure function of the logical path and the target's `fs`, the same table in both versions; it never asks the operating system, whose answer depends on its version and on mount options:
  - every component of the logical path is checked, folders included, whether the folder already exists on the target or not;
  - lengths are counted on the exact bytes, or on their exact UTF-16 conversion, never on a normalized form: 100 × `é` is 200 bytes in NFC and 300 in NFD;
  - ext4, XFS, btrfs: at most 255 bytes per component; every other byte is allowed;
  - NTFS: every NTFS disk follows the Windows rules, so that Windows and both Linux drivers can read everything Atoll writes there. A component must:
    - be valid UTF-8, so it holds no encoded surrogate (ntfs3 refuses one, and ntfs-3g stores it, or silently rewrites a CESU-8 pair);
    - hold at most 255 UTF-16 code units, a character above U+FFFF counting 2, and at most 255 bytes, since `lowntfs-3g` refuses longer names and cannot be told apart from `ntfs-3g`. A valid UTF-8 component never has more UTF-16 code units than bytes, so the byte limit always binds first; no UTF-16 length is computed for a name that is not valid UTF-8;
    - hold none of U+0001–U+001F and none of `" * : < > ? \ |` (U+007F is allowed). On Linux 7.3, ntfs3 even reads `a:b` as a stream of the file `a`;
    - not end with U+0020 or U+002E, which also rules out `...` and ` `; other spaces and dots, such as U+00A0, are allowed;
    - not be a reserved name, as Windows 10 reads it (which also cuts at a `:`, forbidden anyway): take the component up to its first `.`, remove every trailing U+0020 (`CON  .txt` reads as `CON`), and compare it, ignoring the case of ASCII letters only, with `CON`, `PRN`, `AUX`, `NUL`, `CONIN$`, `CONOUT$`, `COM1`–`COM9`, `LPT1`–`LPT9`, and `COM` or `LPT` followed by `¹`, `²` or `³` (U+00B9, U+00B2, U+00B3). So `CON.txt`, `NUL.tar.gz` and `CON .txt` are reserved, as on Windows 10. Windows 11 no longer reserves a device name followed by an extension, according to Microsoft's .NET documentation; its exact rule is not established. `COM0`, `LPT0`, `NULL`, `CLOCK$`, `CONERR$`, ` CON` (leading space) and `ＣＯＮ` (fullwidth) are not;
  - a component that fails gets one or more reasons, from a closed list (§29.3), reported in this order: `too-long`, `invalid-utf8`, `control-character`, `forbidden-character`, `trailing-dot-or-space`, `reserved-name`. On ext4, XFS and btrfs, only `too-long` applies. Every rule reads bytes, except the UTF-16 length, so a name that is not valid UTF-8 still gets its other reasons;
  - NTFS is case-insensitive: `Film.mkv` and `film.mkv` in the same folder collide, and so do the folders `Films` and `films`. Atoll never writes a colliding name there (§15.7);
  - variants that change these rules are refused when a disk is added (§10): ext4 with the `casefold` feature, XFS with `ascii-ci`, NTFS mounted with ntfs3 `nocase`, ntfs3 `ads` or lowntfs-3g `ignore_case`. A case-insensitive folder found later under `<root>/` (`FS_CASEFOLD_FL`) makes that disk name-incompatible for every path below it.
- Incompatible name: the solver picks a compatible disk. Failing that, `name-incompatible` state and an alert. Never an automatic rename. A rename to an incompatible name follows §15.3.
- Recommended: mount NTFS disks on Linux with `windows_names` (Linux 6.2 or later for ntfs3), so applications cannot create most names Windows rejects. It enforces only part of the rules above: both drivers accept `CON .txt`, `COM¹`, `LPT³.nfo` and `CONIN$` with it. It has one cost, measured on ntfs-3g and ntfs3: a rename through mergerfs to a name it refuses removes the copy on that disk (§15.2), where without `windows_names` the copy stays under a name Windows cannot open. A name that is not valid UTF-8 is refused either way. The recommendation stands: applications creating such names are the common case, and such a rename is rare. From Linux 7.3, ntfs3 also needs `ads=0` (§10).
- Recommended: let Sonarr and Radarr replace `\ / : * ? " < > |` when they import a file (`Replace Illegal Characters`, `Colon Replacement`). Atoll never changes a file name on its own initiative (R-11): it only propagates renames the user made (§6.1, item 3). `atoll names check` lists the names to fix.
- Total path length. Atoll itself works through relative operations (`openat`) on Linux and the `\\?\` prefix on Windows, so neither limit below binds it, and its trash needs no check: mergerfs never goes there. The limits bind what reads the disks. Both checks are pure functions, counted on the exact path, never a normalized form:
  - mergerfs opens `<mount>/<root>/<logical path>` as one string, of at most 4095 bytes (`PATH_MAX`, 4096 with its NUL). Past that, the copy is still listed, but `stat` gives `ENOENT`, and a deletion or a rename through the view leaves it behind (measured with mergerfs 2.40.2). Such a copy protects nothing: if the other disk dies, the file stays listed and unreadable. So Atoll never writes a copy past this limit on a disk: this is a hard constraint of the solver (§12.4), and a path can fit on one disk and not on another, as `mount` differs. Another disk gets the copy. Failing that, the file keeps fewer replicas than wanted, is reported as `name-incompatible`, and an alert is raised. A copy already past the limit, which Atoll did not write, or past it after a mount path changed, is only reported;
  - Windows programs without long-path support stop at 259 UTF-16 code units (`MAX_PATH`, 260 with its NUL), whatever `LongPathsEnabled` says. Measured on Windows 11 with that setting on: `attrib`, `findstr`, `certutil`, `fc`, `xcopy` and `Scripting.FileSystemObject` work at 259 and fail at 260, while `cmd /c type` and PowerShell work at 300. As the limit depends on the program, it is only reported: `atoll names check` lists the paths where `D:\<root>\<logical path>` is past 259 units, with no state, no alert and no effect on placement. A path that is not valid UTF-8 has no UTF-16 length, so it gets no verdict.
- The name corpus of §29.4 records, for each name, its reasons on each of `ext4`, `xfs`, `btrfs` and `ntfs`; the vectors pin the check on whole paths.
- **Short names on NTFS.** The short name Windows gives a long one, as `FILMNA~1.MKV` for `Film Name.mkv`, is no name of its own: no driver lists it; a look at it finds the entry it names, in capitals on every driver and in any case on ntfs3; ntfs3 refuses to create it, and ntfs-3g makes a name in small letters beside it, which Windows would read as the same (measured, `docs/research/ntfs-driver.md`). So existence there is never decided by a look alone. On a disk whose configuration says `ntfs`, a name of that form (`conformance/vectors/short_form.json`) that its folder does not list, for which a look at it, or at it in capitals, finds an entry, is refused as the target of a copy or of a rename, or as a folder on their way, before anything is written: `name-refused`. A copy refused so is queued again at the next pass and refused again, with nothing written; a rename keeps its place, as any refused one (§15.3); a folder renamed there cannot go as one operation, and its copies are refused one by one. A restore through such a name is a look that fails, `io-error`, with nothing written. Only that form is looked for, as reading a folder costs: a short name given otherwise, which only a tool can set, is not.

### 15.9 `atoll names check`

A read-only report of the names to fix. It reads the catalog and the configuration only, so it wakes no disk, and it reports what the last scan saw.

- **Names**: every logical path in `files` and `entries`, and all their parent folders. Each distinct path is one name, and `checked` counts them.
- **Findings**: a finding is about one name, the shortest path that has the problem. A folder `a:b/` holding 10,000 files gives one finding, on the folder. The kinds form a closed list:

| `kind` | Other fields | Meaning |
|---|---|---|
| `invalid-utf8` | | the last component of the name is not valid UTF-8 |
| `not-nfc` | | the last component is not in NFC form, computed run by run (§11.3) |
| `incompatible` | `fs`, `reasons` | `fs` refuses the last component, for these `reasons` (§15.8) |
| `collision` | `with` | the names in `with`, as full paths, have the same `lpath_key` as this one, different bytes and the same parent folder, byte for byte |
| `branch-path-too-long` | `disk` | the name is past the limit of that disk's mergerfs branch, and its parent folder is not (§15.8) |
| `windows-path-too-long` | | the name is past 259 UTF-16 code units, and its parent folder is not (§15.8) |

- `incompatible` always checks `ext4` and `ntfs`, whatever filesystems the pool holds. XFS and btrfs follow the rule of ext4, so they get no finding of their own.
- `collision`: names whose parent folders differ are not listed, as the finding on those folders covers them: `Films/a.mkv` and `films/a.mkv` give a finding on `Films` and one on `films`. A name without a key never collides (§15.7). The check agrees with the solver's: a path that the solver would find colliding has a finding on that name, with the same names.
- `branch-path-too-long`: one finding per disk whose limit the name is past. `windows-path-too-long`: computed with the `root` of the pool that has the most UTF-16 code units, and not at all with no disks. A name at the top of the root has no parent folder to compare, so it only has to be past the limit.
- `--path <path>` keeps the names equal to that path, its parents and everything below it, compared by whole components; `checked` counts those names. Collisions are still looked for in the whole pool.
- **Order**: by `path` in byte order, then by `kind` in the order of the table, then by `fs` or `disk` in byte order. `reasons` keep their order of §15.8, and `with` is in byte order.
- **Output**: `api/schemas/names-check.schema.json`. A name that is not valid UTF-8 goes in `path_b64` (§29.3); a name in `with` always has a key, so it is valid UTF-8.
- **Exit code**: 0 whenever the check ran, with or without findings, as for `atoll dupes`. A script counts `.findings`. 1 when it cannot read the catalog or the configuration, 2 on a usage error (§29.3).

### 15.10 `atoll scan`

A scan records what the disks hold. It writes nothing to a disk, moves nothing and starts no job.

- It runs a mount check (§10) on every disk of the configuration, and records the result in `disks`. A disk that passes and whose `statfs` then fails is `missing` with the reason `io-error`, and keeps what the last check measured. Then it walks the disks asked for, in the order of the configuration. A disk that is `missing` is skipped, never read as empty (invariant 4), and `readonly` is walked like `online`.
- **A walk** starts at `<root>` and goes on only through handles opened below it (`openat`), never following a symlink.
  - A folder on another mount is recorded as a folder, not entered, and reported in `other_mounts`. So is a folder on another device under the same mount: a nested btrfs subvolume, which has inode numbers of its own, so that `file_key` would not tell its files apart, and from which nothing can move to the trash, as a rename across it gives `EXDEV`.
  - A listed name that `lstat`, `openat` or `readlink` answers with `ENOENT`, `ELOOP` or `ENOTDIR` is reported in `unreadable`. It gets no new row, and the entries it and what is below it had stay, for that walk: a name the driver cannot read looks the same as one removed or replaced during the walk, which says nothing about the disk, and a folder renamed since it was listed only says so by its row.
  - Any other error stops the walk. The disk becomes `missing` with the reason `io-error`, nothing that walk found is written and nothing is concluded from it.
  - Once the walk is over, and before anything it found is written, the disk is confirmed (§10), whatever the walk found: one that fails is `missing`, with the reason `io-error`, and nothing that walk found is written. A pass confirms only a disk it would conclude something from (§15.12); a scan records all it finds, a copy it seems to gain as one it seems to lose, and a mount check alone, answered from caches, would let it record the folder below a failed mount as the disk.
- **What it finds**:
  - a regular file that its effective policy (§11.3), given its size, says to manage: a copy, with `created_by = user`, its inode, links, size and times (§7, §9);
  - a folder: an entry `dir`. A symlink: an entry `symlink` with its target;
  - a regular file that a rule ignores, and anything else (FIFO, socket, device): an entry `ignored`.

  An entry records the mode the walk saw, setuid, setgid and sticky included, without the file type.
- **Written in one transaction per disk**, once its walk is over. Files new to the catalog take their ids in the byte order of their paths, disk after disk in the order of the configuration, so both versions give the same ids. `first_seen` is the time of the scan. The entries of that disk then match the walk, but for what stays of a name in `unreadable`, and of a folder renamed, below; a folder Atoll created stays `created_by = atoll`.
- **A copy not yet in the catalog**, of a file that is:
  - with the file's size: `ok`. It stays unverified (`verified_ns` NULL) until `--full`, so a file shown as `ok` may turn out to be a `conflict`, and a scan that reads no content cannot say otherwise;
  - with another size: the file becomes `conflict` (§15.7), as its content must differ. Every copy stays `ok` and nothing is written to any of them.
- **A copy already in the catalog** is looked for where it sits, and gets a verdict (§15.12):
  - `there`: `known`: the `same` file (§15.1), creation time included where both have one, with the same size, modification time and change time;
  - `edited`, `replaced`: reported in `changed`;
  - `moved`: reported in `moved`, by its old path in byte order, with its new name. The new name counts in `files` and `bytes`, and gets no row, whether the rename reached one disk or all of them, so a rename seen by several looks leaves nothing to undo, and `--full` reads nothing there;
  - `gone`, `uncertain`: reported in `absent`;
  - its row does not change, but for `gone_ns` and `gone_kind`, which follow the verdict, and for the mode or the owner of a copy `there` whose row has none, which takes what the walk saw (§9). Propagation (§15.12) is what acts on them;
  - if a rule now ignores it, its row stays what it was, its rule, mode and `replicas_target` included, and it counts in `files`. The plan sees those, with the pool's priority and spread;
  - below a name in `unreadable` or `other_mounts`, it is not `absent`: the walk did not get there, so nothing can be said about it, also when that name came there with a folder renamed.
- **A file at the path of a file whose copy on this disk still sits elsewhere** (`old_lpath`) gets no row, as a disk holds one copy of a file, and is reported in `path_taken`.
- **A folder renamed** (§15.12): the row of a folder no longer found stays, when a folder with no row is the `same` one elsewhere, at a name the walk entered, the first in byte order if there are several, also if something else now holds that name, and neither that one nor anything new below it gets a row until the folder is decided: what says that a copy moved with the folder is that one sits below its new name. A folder at that name with another inode or creation time is something else, as an application that recreates its folder makes, unless a file below it is what the row at its name says: then no folder went with all it held, and the row takes the new identity. Otherwise nothing below that one gets a row either until the folder is decided, as it would read as a copy left behind. The row of a folder found at neither name, as one moved during the walk from what it had not listed to what it had read, stays while a copy of that disk still sits below it, so that the look that finds the new name reads the rename. A folder that is the `same` at a name below its own old one is no rename another disk could make: it reads as files moved out, and the row takes the new identity. A folder with no row that is the `same` as one found at its own row's name was renamed while the walk went: neither it nor anything below it gets a row, and the next look reads the rename. A folder entry records its inode and creation time.
- **A file renamed during the walk**, found at a name with no row, with one link or with as many links as the name a row holds and another change time, as a link outside the root leaves, when the walk also found the `same` file at a name a row holds; or with one link or another change time than the row says, when it could not reach a row that is the `same`; and not when that name is recorded as an `ignored` entry: two names with one link are one file, so it gets no row, and the next look reads the rename. Given a row, the row it left could only read as `gone`.
- **Then, on the whole catalog**, for each file: its original (§7), whose size and modification time become the file's; its policy, into `rule`, `mode` and `replicas_target`, unless a rule ignores it; and the plan of §13.4, into `state`, with `conflict` and `corrupt` kept (§4), and into the marks `surplus` and `ok` of its copies. The plan's jobs are not kept, as nothing runs them yet. A disk is past `health.missing_grace` when it is `missing` and more than that time has gone since `last_seen_ns`; one never seen is not. The settings §24.4 says have no effect yet are not given to the plan: it sees no `tags` or `domains`, and `prefer`, `prefer` and `allow` for `tags_mode`, `spread_mode` and `overflow`.
- **`--full`** then hashes the copies on the disks that were walked: file after file in the byte order of their paths, on each file by position, one read feeding both hashes (§16), with one transaction per file, so an interruption loses little. A copy in `changed`, `moved` or `absent`, or that the walk did not reach, is not read, a copy with `old_lpath` is read where it sits, a name that can no longer be read is added to `unreadable`, and an I/O error stops that disk as above, with what it already hashed kept. Once the reading is over, the step above runs again on the whole catalog, so that a disk it stopped counts as `missing` in each file's original, state and marks.
  - A copy whose size or times move during the read is added to `changed`, and its hash is not used.
  - The reference is `files.blake3`. Where it is NULL, it becomes the hash of the original's copy if that was read, otherwise of the first copy read, with its chunk hashes (`file_chunks`).
  - A copy with the reference's hash gets `verified_ns`, verified before or not. An unverified copy with another hash makes the file a `conflict`, and stays unverified.
  - A verified copy with another hash is added to `changed`, and loses its `verified_ns`, so that nothing relies on it: `atoll prune` would otherwise count it as a good copy, and send the good one to the trash. Deciding between a modification and corruption is for the scrub (§16). Read again by a later `--full`, a copy with the reference's hash is verified again, and one with another makes the file a `conflict`, as any unverified copy does.

### 15.11 `atoll status`

A read-only report of what the catalog holds. It reads the catalog and the configuration only, so it touches no disk, and it says what the last scan saw: `atoll disk list` is the command that looks now. A disk unplugged since the last scan still shows its state, with its `last_seen`.

- **Disks**: every row of `disks`, retired ones included, by position, with its state, what the last check measured, and the copies, in any state, and the bytes the catalog holds on it, whatever `--path` says. A disk no check has confirmed has no capacity, free space or date: `null`.
- **Files at their level**: a file is at its level when its copies `ok` or `surplus` on a disk `online`, `standby` or `readonly` reach `replicas` + 1. A copy on a `missing` disk does not count, even before `health.missing_grace`, unlike in the plan (§13.4): this counts what can be read now. A file in `conflict` counts by its copies like any other, and `by_state` says how many there are. `bytes_placed` is the bytes of the files with no misplaced copy, as `balance.trigger` counts them (§13.3), from a plan of the whole pool that reads the catalog only; with neither a `tags_mode` or `spread_mode` of `"strict"` nor an `overflow` of `"deny"`, in the pool or a rule, no copy can be misplaced, and no plan is made. Over `bytes_total`, it is the share of bytes correctly placed.
- **Counts** are integers, with no share in the JSON, so that no rounding can differ between two versions: `files_total` and `files_at_level`, `bytes_total`, `bytes_at_level` and `bytes_placed` by the size of each file, and `by_state`, the state the last scan gave each file, with every state of §4, at 0 when no file has it.
- **`--path <path>`** keeps the file at that path and everything below it, compared by whole components, in the counts and in `files`, by path in byte order. A path the catalog does not hold gives none, and exit code 0. Without `--path`, `files` is empty. Each file comes with its copies by position, each with `old_path`, or `old_path_b64`, when it has not followed a rename yet, and `original` is the disk of its original, the last one known when no copy can be read now (§7).
- **Output**: `api/schemas/status.schema.json`.
- **Exit code**: 0 whatever it reports; 1 when it cannot read the configuration or the catalog; 2 on a usage error, which takes in a path that is not well-formed (§4). Files are written as they are read, as a large folder would not fit in memory (§26). So a catalog that fails once they are on their way leaves an unfinished output, and a script goes by the exit code. Otherwise a failure writes nothing to standard output.

### 15.12 The propagation pass

`atoll daemon --until-idle [--dry-run]` does all the work that is due, once, and exits; the daemon that runs for good runs the same pass whenever something calls for one (§15.13). It writes under the same rules as every command, and takes no `--yes`: starting a daemon is the consent, and the guard (§15.6) is the safety. A copy `edited` or `replaced` is a modification (§15.4); of a metadata-only change, the modification time, the mode and the owner are applied to the other copies.

**The steps:**

1. An exclusive `flock` on the catalog, as in §25.
2. A mount check of every disk, recorded. The reprise of §17.2 on each writable disk, which confirms it.
3. The walk of each disk not `missing`, and a verdict for each copy on it. A disk with a copy `moved`, `gone` or `uncertain` is confirmed before anything is concluded from it, `readonly` included (§10, §15.6): one that fails is `missing`, and its copies `unknown`. What is new is written in one transaction per disk, as in §15.10, and `gone_ns` and `gone_kind` follow the verdicts.
4. The guard is worked out, once, for each disk. Decisions, for the files that are due, by path as they stood when this step began, in byte order, at most 1000 files to a transaction, and a page takes in any file that holds a target of one of its files, as it may be leaving it. Then the modifications (§15.4), by path in byte order, one file to a transaction.
5. What is pending on each writable disk, disks by position: folder renames by their old path, one after another if its new name lands at or below that one's old name or below that one's new name, or if that one's old name is below its own and that one's new name is neither at or below its own old name nor below its own new name; the first of those in a cycle goes, its copies following one by one; then renames by old path, again and again until a round moves nothing, as one may free another's target; then trash jobs by id. A disk's folder renames are decided in the order of their jobs. Nothing says in what order two disks acted, so across disks a copy decided below a folder's old name is below it like any other, and the next look finds it `moved`.
6. With the link guard, or with links left of one, the links of each disk walked that can be written to (§17).
7. The original, policy and state of each file, as after a scan (§15.10).

**A verdict** says what a look found of one copy, at the place where it sits: `COALESCE(old_lpath, files.lpath)`. The first that applies:

| Verdict | When |
|---|---|
| `unknown` | its disk was not walked; or the place is below a name in `unreadable` or `other_mounts`, or below a folder found renamed, at what is now such a name; or, for a pass, at or below a name of an operation left planned on that disk, which only a reprise can tell of. What that name holds gets no row |
| `there` | the name holds the file, as §15.10 compares a known copy |
| `edited` | the name holds the `same` file (§15.1) but not as §15.10 compares it, or the same inode where either has no creation time |
| `moved` | the name does not hold it, exactly one candidate is `same`, none is doubtful, as below, and no other row has that candidate as its target: that candidate's name is the target |
| `uncertain` | the name does not hold it, and either several candidates are `same`, or a candidate is doubtful: the same inode and another size or modification time, where either has no creation time, or two rows have one candidate as their target, as hard links do. It stays so while the name is absent: those candidates get rows of their own, and say no more than at the first look |
| `replaced` | the name holds another file, but for the target of another row's copy `moved` there (`gone`, below), or anything that is no regular file: a symbolic link, a folder, a FIFO; or a folder on the way is now a symbolic link, which the walk does not enter |
| `own` | as `gone`, and the `same` file is in that disk's `<workdir>/trash/` or `<workdir>/tmp/` (§15.5, invariant 11). A pass looks; `atoll scan` reads this as `gone` |
| `gone` | the name is absent; or it holds the target of another row's copy `moved` there, and this copy's own file is nowhere: an overwriting move, `mv a b` with `b` there, loses the file of `b`, which a deletion then decides, and the file that moved takes the name once it is free (Limits, below) |

A candidate is a regular file found by this walk at a name with no row on this disk, or at the name of a row that it is not `same` as, which a chain of renames leaves, and that has the copy's inode. A name recorded as that file is a logical file of its own (§15.1), and one recorded as an `ignored` entry is no place a copy went to. `gone_ns` starts with `moved`, `uncertain`, `gone` or `own`, and is kept while the verdict stays among them, but for a `moved` that turns `gone`, which starts again: until then its name was known, so it only disappeared now, and it waits a debounce of its own and answers to the guard. `there`, `edited` and `replaced` clear it. `gone_kind` says the last verdict. `unknown` touches neither. `replaced` starts no clock, as nothing is decided on it, and a later absence must wait its own debounce.

**Due**: a file is decided once every copy of it that is `moved`, `gone`, `uncertain` or `own` has `gone_ns + deletes.debounce <= now`, with the guard's exception: a file with no copy `moved` and a `gone` copy that a tripped guard holds (§15.6), wherever it sat, is not decided at all, on any of its disks, until that guard is answered. A rename goes on, and keeps that row (below). In `--dry-run` everything counts as due. A copy whose row says `moved`, on a disk walked, that this look could not get to, is decided nothing of: its file waits for a look that can, in `--dry-run` too, as the guard does not count what is gone beside a copy `moved`, and a deletion would go around it. That wait has no end of its own, and no line: nothing says it but `unreadable` or `other_mounts` at the next scan.

A copy may be **peer** of the copies that are `gone`: it is `created_by = atoll`; or it has `verified_ns`, each `gone` copy had one too, and its `first_seen_ns` is no later than the earliest among them. Any other surviving copy may be where the file went: moved by hand from another disk, or an atomic save through mergerfs landing on a branch that held none. A file that stays can be deleted again, and one in the trash is purged after `retention` with nothing in the library to say so.

**The decision about a file:**

| The copies | What happens |
|---|---|
| at least one `moved` | Rename, below |
| some `gone`, none `uncertain` or `own`, no `gone` with `old_lpath`; at least one copy remains, and every copy that remains is `there` or `unknown`, and a peer; the file not in `conflict` or `corrupt`; `deletes.direct = "propagate"` | Deletion: a `copy-gone` event for each `gone`, whose rows go, and a trash job for each that remains, unless it has one queued for a deletion, which stands (one queued for a refused rename stands beside it, and one queued for an overwrite goes, as §15.4 has nothing to refresh then), `reason = deleted`, `src_disk` the disk of the first `gone` by position |
| some `gone` or `uncertain` or `own`, otherwise, with at least one that remains | Kept (§15.3, §15.4, invariant 10): their rows go, and nothing goes to the trash. A `copy-gone` event for each `gone` without `old_lpath`, and a `copy-kept` event at level `warning` |
| all `gone` or `uncertain` or `own` | Gone: a `copy-gone` event for each `gone` without `old_lpath`, and no `copy-kept`, as no copy is left that a deletion did not reach. The rows go, and the file goes with its chunks. Nothing can be propagated: an x1 file, or a deletion through mergerfs |
| none `moved`, `gone`, `uncertain` or `own` | Nothing |

- A copy `gone` with `old_lpath` is a name the file has already left: removing it says nothing about the file at its new name, and the guard does not count it. Under a tripped guard it is held all the same, as what comes back must find its row.
- A copy `own` writes no event: it was an interrupted operation of Atoll's, not a user's action.
- When rows go and every copy left was first seen after the earliest of those rows, or when rows go or a copy leaves for a file row of its own and none of the copies left has `verified_ns`, the file's hash and chunks go too, as what they describe went with those, and the copies left lose `verified_ns`, as no later hash says anything of them: a copy verified against another could otherwise make one of them its peer. Otherwise an atomic save of the same size reads as a `conflict` at the next `--full`. A Deletion keeps them, and the copies left keep `verified_ns`: what is left is on its way to the trash, and a job canceled leaves the file as it was. In a `conflict`, once rows go or a copy leaves, what is left with one copy, or with one size, is no longer in conflict: the next `--full` finds any difference of content again.

**Rename.** The target is the target of the first `moved` copy by position, when it is free; otherwise of the next. A target is free when no file row holds it, or when the row that holds it is itself decided in this step, leaving it. A file's own path is free to it, as a copy still at an old name has only caught up, and a file whose first target is its own path does not leave it, whatever else wants it. So decide a greatest fixed point: start with every due file that has a `moved` copy, drop any whose targets are all held by rows outside the set, and repeat. Two files wanting one target: the first by path gets it, and one left with no free target leaves the set and holds its path after all, which may stop another, and the fixed point goes on. A file with no free target changes nothing, writes no event and is reported as `path-taken`, and is looked at again next pass. For a file that takes its target, in one transaction:

- `files.lpath`, with its key, becomes the target. Rows that hold each other's target pass through a value holding a NUL byte within the transaction, which no path can.
- A copy `moved` to the target: `old_lpath`, `gone_ns` and `gone_kind` become NULL, and nlink comes from the walk. Its change time comes from the walk when its size and modification time are what its row says, and otherwise stays, so that an edit still reads as `edited`. When its change time moved, it loses `verified_ns`: the move hides any edit made before it, so that a later mismatch is a `conflict`, never `corrupt`, and the copy is no peer until it is read again (§15.10, §16). Below a renamed folder the change time does not move, and nothing is lost. It becomes `created_by = user` when another copy of the file did not move there (§15.3).
- A copy `there`, `edited`, `replaced` or `unknown`, first seen before the earliest `gone_ns` of the moved copies that sat at the file's path, or with none of those: `old_lpath` and `old_lpath_key` become where it sits, or NULL when that is the target (a rename back: nothing to do on that disk). One that sits at the target gets NULL whenever it was first seen, as the file does not leave that path. A copy that was already behind says nothing of when the file left its path. A copy that went `moved`, `gone`, `moved` counts from when it turned `gone`, as no row says when it first left, so a new copy that landed before that follows it.
- Such a copy at the path the file leaves, first seen at or after that time, did not exist when the file left: it leaves the file for a file row of its own at the old path, with no hash. Copies of several sizes: the first by position gives that file row its size, and a copy of another size has its row go, and the next walk finds it, as a `conflict`. A new release imported at the old name of a file moved to a recycling bin must not follow the old one there.
- A copy `moved` to another target: a file row of its own there, with the hash and chunks copied, but for a file in `conflict` and a copy with no `verified_ns`, as the hash may be another version's, if that is free; no propagation between them, and a line of its own in `decisions`. Not free: `old_lpath` says where its row said it was, or NULL when that is the target, as it is at neither name, and `decisions` has a `path-taken` line for that target.
- A copy `gone`, `uncertain` or `own`: the row goes, nothing goes to the trash, and no event counts for the guard. If the guard of its disk is tripped, a `gone` keeps its row with `old_lpath` where it was, or NULL when that is the target: should it come back, the rename reaches it. When a row goes and every copy left was first seen after the earliest that went, the hash and chunks go too, as in a decision above.
- When a copy of it moved, every queued trash job of the file goes: a rename seen after a deletion means that the deletion no longer stands. A file that only follows a folder keeps its jobs, as nothing was done to it: a copy removed before its folder was renamed goes to the trash from the new path.

**On each writable disk**, for a copy with `old_lpath`, unless another copy of its file is `moved` in this pass and not due, or its row says `moved` and this look could not get to it, as its target may be about to change, and unless its target lands at or below the old name of a folder whose job stays, as that operation would carry it, the target being `files.lpath` now:

1. The name is checked, as a pure function, as for a new copy (§15.7, §15.8): the name on that filesystem, the limit of its branch, and on NTFS the collision check with the names that disk holds, leaving out the copy's own old name. Refused: Refused names, below.
2. A look at both names:

| Where it sits | The target | Outcome |
|---|---|---|
| the `same` file | nothing | it is renamed, edited or not: nothing is lost, and the next look reports an edit |
| absent | the `same` file | `already-there`: only the row follows |
| absent | anything else | nothing here: the next look gives the verdict |
| another file | anything | `changed`: it stays, and is looked at again |
| the `same` file | something else | `path-taken`: it stays, and is looked at again |

3. Each missing parent of the target is an operation `kind = mkdir`, with the mode and owner of the same folder on the first disk by position whose copy is at the target, or on any disk when none of those holds it, and 0755 less the umask when there is none to read (§17.4). A look at that folder that fails, a parent that cannot be made, or no such folder found with a disk that may hold it not looked at in this pass, is `io-error`: it stays, and is looked at again, as a folder made with no model would stay open for good. A parent that the driver refuses is `name-refused`.
4. `kind = rename`, `path` where it sits, `path2` the target, `file_key` the inode, journaled with a `rename` event, and moved as in §17.1: a look right before it, `RENAME_NOREPLACE` or `link`, `fsync` of the file then of the target folder, a look, `unlink`; then `fsync` of the file at its new name, then of the target folder, the times of both folders restored, `fsync` of the source's folder. In one transaction: the operation `done`, `old_lpath` and `old_lpath_key` NULL, nlink from the look, and the change time from the look when the look before the move found what the row says (§17.1, step 6). `verified_ns` stays: Atoll knows what it did.

**A cycle.** Once a round renames nothing, copies left `path-taken` whose targets lead round a cycle back to one of them, each target holding the next of them, as a swap of two names leaves them, wait for one another. On ext4, XFS and btrfs, the first of them by place and the copy at its target exchange their names in one step, `renameat2` with `RENAME_EXCHANGE`: the first reaches its target, and the other takes its place, which is its own target or the next step round the cycle, from where the next round goes on. Both are journaled as renames before anything moves, the second with the place of the first as `path2`; a look right before finds each name holding its copy as its row says, then the exchange, `fsync` of each file at its new name and of the folders, whose times are restored, and each finishes as a rename does. A reprise finishes or undoes each by the table of §17.2. The NTFS drivers refuse the exchange, `EINVAL` on ntfs3, ntfs-3g and lowntfs-3g on Linux 6.12 and 7.3 (measured), so a disk the configuration says is `ntfs` is not asked, and there the copies of a cycle wait, a limit, with nothing journaled. A dry run lists the exchanges a real pass would make as `to-rename`.

A disk away keeps `old_lpath`. Back, its walk finds the copy where it sits, so it can never be read as a user renaming it back. A rename back meanwhile clears `old_lpath` with no disk operation. A new file at the old name is a file of its own, and a file at the target on that disk is reported in `path_taken`, with no row (§15.10).

**Folders.** A folder row whose folder a walk no longer finds at D, where a folder with no row is `same` at E with a creation time on both sides, is a folder renamed to E: its row stays, and E gets none, until the files below it are decided (§15.10 says of another folder made at D, and of E below D). That is what tells a folder rename from files moved out one by one, where the folder keeps its inode. As the rows stand before any decision of the pass, the rename is real if a copy of that disk sits below E with no clock running, or sat below D and was found moved to below E. Otherwise it says nothing of other disks, as an empty folder renamed does not, and a copy moved in from elsewhere changes nothing of that: its entries just follow. A copy moved in from elsewhere and decided in an earlier pass, while the folder waited for another, does read as one that came with it, as no row says where it came from, and one decided while the folder waited and moved on before the folder is decided reads as one from elsewhere, so only what moved follows. All this is acted on once no copy of that disk still sits below D, the copies that moved being decided. The folders a disk found renamed are taken in the order of step 5, those in a cycle last: one that lands in another's old name came after that one left, and must not read as having been in it. One with a folder renamed inside it that waits waits too, as its entries would carry that one's row away. Before the files of a pass are decided, each folder found renamed that waits for what no decision of the pass ends, a clock of a copy below it on another disk, the guard, or a look that cannot get there, is worked out, and a copy of that disk that moved to a name at or below its old name waits for it, its file decided nothing of; so does a folder renamed to there, as its copies wait. Otherwise what lands there would take that name before the one that waits leaves it, and the next look would read that one as a folder made where it now is, leaving what only other disks hold below its old name in the folder that took the name. Only a wait of that kind holds anything back, so that nothing waits for itself. Two folders that swap their names are read as no folder renamed, as each name still holds a row: their files move one by one, and what only other disks hold below them stays at its path, a limit, with nothing lost. Nothing says in what order two disks acted: a copy a decision of this pass put below D, on another disk, is below D like any other, so its disk gets the job, and the next look finds that copy `moved`. A copy no pass can take from below D holds nothing back: one `replaced`, as nothing decides that, one `moved` to a name another file holds, or one this pass decided in from elsewhere, which never sat in the folder renamed. One decided in by an earlier pass, or that Atoll's own rename brought to a D it made, reads at the next look as a file at home there, so that no folder went with all it held, and only what moved follows. A real rename waits while a file below D with a copy on other disks only has a copy `moved`, `gone`, `uncertain` or `own` not yet due, a debounce at most, as what becomes of that copy is decided at the path it has, or a copy the guard holds, on a disk looked at or not, until it is answered, as following would take that copy out of the guard's count and end the hold with nobody answering, or a copy a look cannot get to, as under Due, for as long as that lasts. Until then no such file follows, as what goes alone its one operation can no longer carry. A copy decided already, for what it did, does not wait: it goes alone, as any copy behind does, and the job then finds E there and goes, so the rest follow one by one and the old folder stays. In this order, so that a crash lets the next pass go on from where it was:

- every file below D with a copy on other disks only, and none `moved`, as that one was decided on what its copy did, follows it, `old_lpath` as above, if its new path is free;
- in one transaction, the entries of that disk below D become E, and
- each other disk holding a copy or an entry below D gets a job `kind = rename`, `file_id` NULL, `dst_disk` that disk, `payload` `{"from_b64", "to_b64"}`, the two paths in base64, unless it has that job queued, or a decision of this pass cleared the `old_lpath` of a copy of it below D. A job whose target was E, or below it, moves with a further rename, and one whose paths become equal goes.

The job runs when D is a folder on that disk, E is not below D, as a further rename can leave it, E passes the name check of step 1 with D and all below it left out of the names, before anything asks the disk about E, E is absent and no parent of it is anything but a folder, each copy below it would fit its branch, and no copy there at its own file's path would land past its branch or on a name a file row holds, where its row would say nothing true: one operation `kind = rename`, `path` D, `path2` E, `file_key` the folder's inode. Everything below moves with it, whatever it is, and the files keep every time, as measured. On ntfs-3g and lowntfs-3g, where the flag gives `EINVAL` and a folder has no `link`, it is a look at E and a plain `rename`, whose only risk, measured, is replacing an empty folder that appeared since the look. The folder is synced at E before its parent is, as a file is (§17.1). In one transaction: the operation `done`, each copy behind with its place below D and no clock running gets `old_lpath` NULL when its new path is the file's, and that new path otherwise, the entries below D become E, and the job goes. A row whose clock runs was not where it says at the last look, and stays as it says, so that a copy removed at its old name is Kept, and never reads as a deletion at the file's new name. A row at its own file's path, as of a file new on that disk, was told nothing: it stays too, and the next look finds it `moved`, as a user's rename would be. A job that cannot run goes, and the copies follow one by one. So does a job above a copy whose `old_lpath` a decision clears, as it was moved back and the job would carry it away again, and one that a job before it left with nothing at D, as no order gives both their one operation when a folder inside D went to a name below E, or to a name below D: an empty folder can then stay at what was its place. A look that fails, a parent that cannot be made, or a move that fails with `io-error`, keeps the job, and what it will carry waits with it: no copy below it goes on its own. The old folder stays then, as only Atoll's own are removed (§6.1, item 8), and is not removed yet. Where a folder has no creation time, or files were moved out one by one, only what moved follows.

**Trash jobs** (`kind = trash`, `file_id`, `src_disk`, `dst_disk`, `reason`, `state`). One operation for each, as §17.1, compared with its row right before the move: `changed` leaves the copy, and the job goes. Before that:

- the guard of `src_disk` is tripped: it waits, `held`;
- `dst_disk` is not writable: it waits, `disk-unavailable`;
- for `reason = deleted`: `deletes.direct` is no longer `propagate`, the file is in `conflict` or `corrupt`, it has a copy with no queued job, as one re-imported or restored on any disk since, or another copy of it is `moved` in this pass, due or not, or has a row that says so and that this look could not get to: the deletion no longer stands, every job of the file goes, and it is reported as `canceled`;
- for `reason = renamed`: the copy is no longer behind, the target is no longer refused, or it now differs from where the copy sits first at a folder, only as NTFS reads one name (§15.3), the file is in `conflict` or `corrupt`, the copy has lost `verified_ns`, or no copy at the target can be relied on now: the job goes, it is reported as `canceled`, and the rename takes over;
- for `reason = overwritten`: the copy is no longer `stale`, the file is in `conflict` or `corrupt`, or it has no copy `ok` or `surplus` left to refresh the copy from, which is then what is left of it: the job goes, and it is reported as `canceled`.

Finishing, immediately or in a reprise, is one transaction: the operation `done`, the row of the copy found by disk, place and inode gone, the job of that file on that disk gone, and the file gone if that was its last copy, as `prune` never does. For `reason = overwritten`, the row stays instead, as a pending copy, with its `replicate` job (§15.4); without that trash job, as one a later decision dropped, it goes as for any other reason, and the next plan places the copy. A disk away keeps its job and its row: the file stays, `unavailable`, until it is back.

**Refused names** (§15.3). A copy whose rename is refused keeps `old_lpath`, and each pass checks the target again: a name corrected later just renames it, with no re-copy. A copy a queued folder job would carry to its target is not asked about, whatever the name table says of a name that disk holds: should the job go, it is first refused then. A name that only a row whose clock runs holds, or that is at or below a folder found renamed and not yet decided, or at or below the old name of a folder whose job is queued, or at or below a folder a look found at neither name, can make a rename wait as `name-refused`, and sends nothing to the trash: it is on its way out. A name its look found at a row's name, with no row of its own yet, as a file made at the old name of a file renamed, is not in the names that disk holds until the next look, so a rename decided in that same pass can land beside it on NTFS, and the next walk reports that collision. As its row says, it wants the trash, `reason = renamed`, `src_disk` its own disk, when it has `verified_ns`, the file is not in `conflict` or `corrupt`, another copy at the target is `there`, verified, on a disk walked in this pass and not lost since, it has no job yet, and the guard of its disk lets it; otherwise it waits, reported as `name-refused`. The guard counts these when it is worked out, in step 4, so a copy refused for the first time in this pass waits for the next. Edited, it stays, and the file is in `conflict`, every version kept. Once it is in the trash the file lacks a replica, and shows `name-incompatible` or `name-collision` through the plan. A job the guard rejected stays `canceled`: that copy only ever renames, once the name is accepted. It goes in the first pass that finds the copy no longer behind, however it caught up, so a later refused name is asked about again.

**An overwriting move** (`mv a b` with `b` there, on one disk) loses the file of `b` there, whose copy is `gone`: `b` is decided as any deletion, its other copies going to the trash after the debounce, as `deleted`, held by the guard as any other. Until the row of `b` has gone, the file `a` is `path-taken`; then it takes the name, and its other copies are renamed there, with nothing copied again: a pass later at the earliest. Where the other copies of `b` are no peers, `b` is `kept`, and `a` waits for its name, as nothing says that those copies are the file the move overwrote; `b`, then below its level, is queued again for the disk of the move, where each copy ends `conflict` at the look before it, until the user settles it. Only copies that no scan has read are no peers there: a pool whose copies were all read, or made by Atoll, has none.

**Limits:** a swap of two names, or a cycle of more, directly on one disk is followed by the rows; on an NTFS disk, whose drivers cannot exchange two names, its copies wait as `path-taken` until one of the names is freed (an exception to R-10). With `deletes.direct = "restore"`, nothing propagates, and the file lacks a replica.

**Output**: `api/schemas/daemon.schema.json`. `decisions` lists each file decided, in the order decided: by old path in byte order within a page of at most 1000 files due, a file that holds a target of one of those being taken into that page and listed in that order, and what follows a folder after those, then the files a modification was decided for, by path in byte order, `modified`, `metadata` or `conflict` (§15.4). A copy that went its own way has a line of its own, `renamed` to its other target or `path-taken` for it, and a file `deleted` that then follows its folder has a `renamed` line too. `operations` lists what ran: first the operations `chown`, `chmod` and `utimens` of the modifications, in the order they ran, as they run with their decisions; then, disk by disk by position: folder renames, renames in the rounds they ran, then trash jobs, with their `reason`; then the copies, in the order they ran (§14.1), each with its target as `disk` and its `source`, and the outcomes of §14.1. With `--dry-run` it lists what would run, in the same order: it looks at each disk as a real pass does, and says `to-rename`, `to-trash`, `to-apply` and `to-replicate` where those say `renamed`, `trashed`, `applied` and `replicated`, a copy queued already or one the plan would queue, and the same otherwise, naming a copy where it sits now. It reads what a real pass would read to decide a modification, and lists the trash jobs that would follow. It decides on the rows as they stand: a copy its own walk found new has no row until a pass or a scan records it, so until then that copy neither stops a deletion nor takes a name. It is a preview: it applies nothing, so what one decision or operation of the pass does to the next is worked out only as far as a folder's operation carrying what it carries, one rename freeing another's name on that disk, and a decision or a job dropping a job. So it does not work out: a collision on NTFS that a rename of the same pass frees or makes; a folder renamed inside another renamed in the same pass, which it lists as two operations; a folder's operation for a disk whose only copies below it a decision takes away; two files its decisions give one target across its pages, or a file that follows a folder onto a name a file's decision took, which it says `renamed` of where a real pass says `path-taken`; a name a decision frees, or a copy a decision leaves behind, for a file that follows a folder or for a folder's operation, which it says `path-taken` of, or lists no folder for; a copy behind that is deleted and then follows its folder, for which it also lists a rename no real pass runs; the clocks its look would write, for the guard of a disk away, which reads the rows of that file's other copies as they stood before the walks, for whether a copy still sits below a folder renamed, for whether a name a refused copy wants is on its way out, and for whether a copy it found gone is still a source (§13.4), so that it lists a copy a real pass would not queue; a trash job that depends on an earlier disk's work, as a good copy arriving by a rename, a copy found `changed`, or a conflict a refused rename sets; the model of a missing parent, which a real pass only looks for when the operation runs; the place in the list of a file with a queued job that is decided again; what a reprise finishes with no line of its own, for which it has one; a modification of a file a decision renames, which it lists under the path the file had, or keeps, which it does not decide, as its rows still hold what went; the mode and the owner a look records of a copy whose row has none (§9), which it reads as unknown, and so as another; and the copy a stale copy is given once it is in the trash, which only the next pass lists. It writes nothing to a disk, to the journal or to the catalog but for what the mount check of step 2 records. `waiting` counts the copies not yet due, the changed copies not yet due (§15.4), and the files whose copy waits for `settle` (§14.1), and `next_due` says when the first of either is, or `null`, so that a timer knows when to run the next pass. `guards` lists the guards tripped, by disk position, each with its `kind`: `deletions`, then `modifications` (§15.6). These are written as they come, as a rename of a whole pool would not fit in memory (§26): every decision before the first operation, and `guards`, `waiting` and `next_due` at the end. So a failure once they are on their way leaves an unfinished output, and a script goes by the exit code.

**Exit code**: 4 when a disk was skipped or failed, an operation stayed planned or ended in `io-error`, or something due could not run for want of a writable disk the configuration lists; what waits for a disk it no longer lists has no line either, until `atoll disk remove` says what becomes of it; otherwise 3 while a guard holds anything; otherwise 0, with work possibly waiting for its debounce. With `--dry-run`, 0 once it has answered. 1 when the configuration or the catalog cannot be used, and 2 on a usage error. `atoll daemon` without `--until-idle` is a usage error until the loop exists.

**`--dry-run`** writes nothing to a disk or to the catalog except what a mount check records, as `prune` does, resolves nothing and confirms nothing. It answers as if every difference it sees were due.

### 15.13 The daemon that runs for good

`atoll daemon [--foreground] [--dry-run]` runs until it is stopped. Atoll never detaches itself: `--foreground` changes nothing, and a service manager runs it (`deploy/atoll.service`). It does the work of §15.12, pass after pass, when something calls for one.

- **Starting.** It loads the configuration and opens the catalog, refusing to start, with exit code 1, when either cannot be used. It tells the service manager it is ready (`sd_notify`, `READY=1`, when `NOTIFY_SOCKET` is set), checks every disk's mount, starts the watcher of §15.1 on each disk not `missing`, and runs a first pass, which walks every disk: the rescan at startup.
- **A pass** is that of §15.12, on every disk: each pass confirms every writable disk anyway (§10), and sparing the disks' sleep is phase 2b. For each pass, the daemon takes the catalog's lock, waiting while a command holds it (§25), opens the catalog, runs the pass, puts the runtime branches right (below), closes the catalog, and lets go of the lock, in that order, as closing a descriptor of the catalog's file would drop SQLite's own locks. Between passes it holds neither.
- **When it acts.** At a time `now`, it checks every disk's mount when the check is due, 30 s after the check before ended: one that took longer than that, as when a remount waited for its disk, would otherwise be due again at once, and it comes before every pass. It runs a pass when one of these holds, each a reason:
  - `start`: no pass has run since it started;
  - `reload`: the configuration was read again since the last pass (below);
  - `gone`: a disk went `missing` since the last pass;
  - `back`: a disk that was `missing` is found again;
  - `events`: a group of the watcher ripened (§15.1) that someone other than Atoll made, as Atoll's own operations are known from its journal. A ripe group leaves the grouping, so what it asks for is kept until a pass takes it: a check due at the same time runs first, and the pass follows;
  - `due`: the earliest time the last pass said something becomes due (`next_due`) has come;
  - `rescan`: `maintenance.rescan_every` has gone by since the last pass ended, and `now` is inside `maintenance.window`.

  Right after a pass, another step due below (the media servers to tell, SMART, balancing, hashes, the backup, the scrub) runs before a pass whose every reason is `events` or `due`, which are kept for the turn after it: passes that each take longer than the changes take to come, as on a large pool under constant changes, would otherwise follow one another for as long as the changes do, and nothing else would ever run. A pass for any other reason does not wait. Each turn begins with what the watcher and `SIGHUP` said since the last, taken without waiting: the sleep is not the only time they are heard, as a step due turn after turn, hashes read slice after slice, would keep every change waiting until it is over.

  Otherwise it sleeps until the earliest of: the next check, the last pass's `next_due`, the time a waiting group ripens, the next rescan, the first time inside `maintenance.window` at or after the last pass's end plus `rescan_every`, and, while files wait for their hash, the window's next start. It works this out as a pure function (`conformance/vectors/daemon_wake.json`).
- **New files' hashes.** A pass records what it finds, and reads no file whole. So after each pass, inside `maintenance.window`, the daemon reads the copies of the files that have no hash yet, on the disks that pass their mount check, by slices of 30 s under the catalog's lock, so that a command never waits long, and checks and events come in between. A copy that is no longer what its row says is left for the next pass to look at (§15.10), and a disk that fails is skipped. Once no such file is left, it waits for the next pass. `--dry-run` reads none.
- **The catalog backup** (§9). Once for each stretch of `maintenance.window`, with no hash to read, the daemon backs the catalog up under its lock, as `atoll catalog backup` does, before any scrub. One that fails is not tried again before the next stretch: a line of its log says so. It starts after the first pass. `--dry-run` backs nothing up.
- **Balancing** (§13.3). After the passes, the notifications and SMART, and before any hash is read, the daemon checks `balance.trigger` as `balance.schedule` says, and while it holds moves the misplaced copies by slices of 5 minutes under the catalog's lock, within `balance.budget`.
- **The scrub** (§16). With no hash to read and no backup due, inside `integrity.window`, the daemon scrubs by slices of 30 s under the catalog's lock, within `scrub_budget` for each stretch of that window: a stretch is told from the next by when it began, the start of the window that day, or the day before for a window wrapping past midnight. It repairs a corrupt copy, or moves it to the trash, when another copy is healthy, as `atoll scrub --yes` does, as starting it is the consent. A corrupt copy brings a pass, which sends its alert and makes the copy moved again. A stretch in which the scrub found nothing left to read waits for the next one, when copies come due again. It starts after the first pass. `--dry-run` scrubs nothing.
- **A disk that goes `missing`.** Its watch stops at once, and the pass that follows records it. The pass also takes its branch out of the running mergerfs.
- **After a USB reset.** When it went `missing` for `device-gone`, `device-unreadable` or `io-error`, which a USB reset gives, `health.remount_after_reset` is on, and `/etc/fstab` has an entry for its mount point, each check while it stays `missing` runs `umount -l <mount point>`, then `mount <mount point>`, so that the remount takes the administrator's options. Each of the two has 90 s, what systemd gives a mount: past them it is killed, with the helpers it started, and the next check tries again, as a disk that died halfway can hold a mount for good while the daemon does nothing else. With no such entry, nothing is tried. A dirty NTFS volume is never forced: the mount fails, and the disk stays `missing`.
- **A disk found again.** A pass reconciles it first. Only once a pass has walked it does its watch start again and its branch come back to mergerfs (§18).
- **Runtime branches** (§21.1). After each pass, when `<view.mount>/.mergerfs` exists, the daemon reads `user.mergerfs.branches`. When the branches should differ, it writes the whole list there: the branches of `atoll mergerfs-config`, without the disks that are `missing` or not yet walked since they came back, in their order. A disk `failed` needs no walk, as it is never walked: present is enough. A list written whole puts a branch back at its place (measured). It writes nothing when nothing would change, or with no mergerfs at `view.mount`.
- **The trash** (§17). After each pass, under the catalog's lock, the daemon purges the trash as `atoll trash purge --yes` does, as starting it is the consent: what has reached its time, then what is purged for room. It reads the catalog first, and checks no disk when nothing is due. The alerts that follow see what it left. `--dry-run` purges nothing.
- **Alerts** (§23). After each pass, under the catalog's lock, the daemon works out the conditions that hold and the alerts due, then lets go of the lock to send them, and takes it again to record those that went.
- **Stopping.** `SIGTERM` or `SIGINT` ends the wait, or the pass under way at its next step, as a long loop can be cancelled. What an operation left is the reprise's at the next start (§17.2). It then exits with 0. It tells the service manager `STOPPING=1` first.
- **Reloading.** It reads the configuration again on `SIGHUP`, and only then (`systemctl reload atoll`); `disk add` and `disk remove` send it themselves. A configuration that does not load is left aside with a line of its log, and the one it had stays. One that loads takes effect at once: the disks it no longer lists are no longer watched, a mount check follows, then a pass for `reload`.
- **`--dry-run`**, shadow mode (§29.2), runs the same loop with passes in `--dry-run`: no runtime branch is changed, and no remount is tried.
- **What it says.** Each pass, each state a disk changes to, each remount, each change of branches and each alert sent or not is a line of its log on standard error, which the service manager keeps.

## 16. Integrity

- Hashes:
  - each 4 MiB chunk: a standalone BLAKE3-256 hash (the last chunk may be shorter). A file has as many chunks as it takes to hold its bytes: none when it is empty, and no empty chunk after a size that is an exact multiple;
  - whole file: standard BLAKE3-256, b3sum-compatible.
  When indexing an existing file, one read feeds both. When copying, chunk hashes come from the source read and the whole-file hash from the verification pass (§14).
- Scrub: every `scrub_every` (30 days by default), inside `[integrity].window`, within a nightly budget, one disk at a time to limit wake-ups, resumable at chunk level.
  - **What is due**: on each disk that can be read and is not `failed`, in configuration order, the copies `ok` or `surplus` of a hashed file, never verified or verified `scrub_every` ago or more: those never verified first, then by the time they were, then by file id.
  - **A copy** is opened through the disk's confirmed handle, after a look that its name still holds what its row says; one that does not is a change, for the next pass. Each chunk is read and its hash compared with the one recorded. A copy whose every chunk matches, and whose name, size and times are still those its row says once read, is verified (`verified_ns`).
  - **A chunk that does not match**: the copy's name, size and times are looked at first, and a copy changed is a modification, left for the next pass. Otherwise the chunk is read again past the page cache. A match then was a bad read: a line of the log, and the read goes on. A mismatch makes the copy `corrupt`, and its file `corrupt`, with a `corrupt` event first.
  - **The budget**, `scrub_budget` or `--budget`, counts from the start of the scrub. When it runs out in a copy, `scrub_cursor` records the copy, the next chunk, and the size and times its row had. The next scrub of that disk reads that copy first, from that chunk when its row has not changed, and from its start otherwise.
  - **Repair or quarantine**: when another copy of the file, `ok` on a disk that can be read, is read whole first and holds what its hashes say, its name, size and times unchanged, the corrupt copy is repaired from it with `auto_repair` (§16.1). Without `auto_repair`, or when the repair fails, it goes to the trash, with the reason `quarantined` and no hash: the file is then `degraded`, and the next pass makes the copy again from the healthy one. A file is `corrupt` while a copy of it is, and once none is, it takes the state its copies give it. With no healthy copy, the corrupt copy stays where it is, and so does every other: nothing is lost to a scrub.
- Decision for a copy whose hash differs from the recorded version:
  - size, modification time and change time unchanged (compared at the coarser precision of the filesystems involved), and the mismatch confirmed by a second read without cache: `corrupt`;
  - otherwise: modification (§15.4). An edit by a tool that keeps the modification time still moves the change time, so it is never "repaired" away. A rename moves it too, which is why a copy renamed by anyone but Atoll loses `verified_ns` (§15.12): read again, a mismatch there is a `conflict`.
- Repair (`auto_repair`, `repair_mode`): §16.1.
- Quarantine: when automatic repair is off or fails, a corrupt copy is moved to the trash whenever a healthy copy exists, so the view never serves it; the file is then `degraded` until a replica is re-created.
- x1 file: detection and alert only.
- **Duplicates**: `atoll dupes` groups the files that hold the same content under different logical paths, from the catalog alone: a read-only report, which touches no disk. A group is two or more files hashed, of one size and one BLAKE3; an empty file is never in one, nor a file not hashed yet, which the report counts instead (`unhashed`). Each logical path is a file, so two names of one file on a disk, hard links, still make a group. A group is listed when one of its files is at or below `--path`, compared by whole components, with all its files, and when its size is at least `--min-size`. Groups go by the bytes they hold more than once, `size` times one less than their files, the most first, then by hash in byte order; the paths of a group in byte order.
- Limit: two versions of the same movie (1080p and 4K) have different hashes. Grouping by metadata (title, year, duration) is planned for phase 5.
- Optional per-disk manifest (`Atoll/manifest.b3sum`), checkable without Atoll by running `b3sum -c` from the disk root.

### 16.1 Repair

A corrupt copy is repaired from a healthy one, which is only ever read: by the scrub, with `integrity.auto_repair` (`true` by default), and by `atoll repair` (§25). `integrity.repair_mode` says how, `chunk` by default, and `--mode` for one run of `atoll repair`. A copy with more than one link is always repaired by chunk, so that every name of it stays the same file.

- **The healthy copy** is found as for quarantine (§16): another copy `ok` on a disk that can be read, read whole, holding what its hashes say, its name, size and times unchanged once read; the first by disk position. With none, the corrupt copy stays as it is, `no-healthy-copy`.
- **`file`**: the corrupt copy goes to the trash as §17.1 sends it, with the reason `repaired` and no hash, then is made again from the healthy copy once the run is over, as `atoll replicate --now` makes a replica (§14), with a new creation time (§7). A copy that cannot be made then is made by the next pass, as after a quarantine.
- **`chunk`**: one operation `kind = repair`. The copy keeps its inode and its creation time.
  1. The copy is read whole through its disk's confirmed handle, and the chunks whose hash is not the file's are its bad chunks. A copy whose name, size or times are no longer its row's is a change, for the next pass, and a copy with no bad chunk reads right after all, and is verified: nothing is written for either.
  2. In one transaction: the `ops` row, `planned`, `file_key` the copy's inode; the `trash` row, with the reason `repaired`, the trash path `<YYYY-MM-DD>/<op-id>.chunks/<logical path>`, as its size the bad chunks' together, and no hash; and the event `repair`, which holds the indexes of the bad chunks and the copy's modification time.
  3. Every bad chunk is saved before any is written: its bytes, read from the copy, are written to `<workdir>/tmp/<op-id>.<index>.part`, synced, then moved without replacing anything to its place in the trash (§17), whose folders are made as §17.1 makes them, each synced. The folder that holds the chunks is synced once they are all there.
  4. Right before the first write, the copy is compared with its row as §15.10 compares a known copy: an edit in place is never overwritten. One found gives `changed`, and the repair stops, below.
  5. Each bad chunk in turn, by index: read from the healthy copy and checked against its hash; written in place, at its offset; the file synced; then read again past the page cache and checked. A healthy chunk that no longer holds what its hash says stops the repair with `changed`, and what was written is resolved as a reprise resolves it.
  6. The modification time is set back to the row's, the access time left as it is, and the file synced.
  7. In one transaction: the operation `done`; the copy `ok`, verified now, with the change time the writes gave it; and the event `repaired`. The file is `corrupt` only while a copy is (§16), so the end of the run works out its state again.
- Measured on ext4, XFS, ntfs3, ntfs-3g and lowntfs-3g, on Linux 6.12 and 7.3, with a byte rotted on the device below each, in the second chunk of a file whose name takes 255 bytes: the copy keeps its inode and its modification time, and the chunk it held reaches the trash under that name. btrfs checks its own data, and answers a read of a block that rotted with `EIO`.
- **A repair that stops** before step 7 leaves the copy `corrupt`, and its operation ends at once: `done` when a chunk reached the trash, the entry's size being then the chunks saved together, and `undone`, its `trash` row gone, when none did. The event `repair-stopped` says why, with the outcome. The next scrub or `atoll repair` begins again, with an operation of its own; the entry stays in the trash until it is purged.
- **Reprise** (§17.2): a `repair` left `planned` reads the copy whole. The repair explains what it finds when the name holds the file of `file_key`, at its row's size, every chunk it did not find bad holds what its hash says, and its bad chunks, by index, hold what their hashes say, then at most one chunk that holds neither that nor what was saved of it, then what was saved of them, every one of them saved; or, while saving was not over, what it found, with the copy's change time still its row's. Then:
  - every chunk holds what its hash says: the writes were all done, and steps 6 and 7 end the operation;
  - a write began: the modification time is set back to the row's, the row takes the change time the copy has then, and the repair stops;
  - no write began: nothing is written, and the repair stops.

  Anything else is someone else's change: nothing is written, the repair stops, and the next pass finds the change (`conformance/vectors/repair_reprise.json`).
- **Outcomes**: `repaired`, which for `file` says that the corrupt copy reached the trash, its copy being made again then; `to-repair`, in a run that may not write; `verified`, a copy found corrupt that reads right after all, read whole now, recorded `ok` and verified, with nothing written; `no-healthy-copy`; `changed`; `name-refused`, for a copy `file` cannot send to the trash (§17); `no-room`, `ENOSPC`, with the operation resolved as a reprise resolves it; `disk-unavailable`, the copy's disk, or every healthy copy's, not confirmed in this run; `io-error`, anything else, with the same exit as §17.1 for a disk that fails. A disk that answers `ENOSPC` takes no other repair in that session, as §17.1 says of its trash.
- The copies a repair leaves `corrupt` are quarantined by the scrub that found them, when it may write (§16).

## 17. Trash

- Location: `Atoll/trash/<YYYY-MM-DD>/<op-id>/<logical path>` on each disk (UTC date; operation id from `ops`). Sending a copy to the trash is a rename within the same filesystem. Paths never collide and names are never changed.
- Chunks a repair overwrites: `Atoll/trash/<YYYY-MM-DD>/<op-id>.chunks/<logical path>/<index>`. The logical path ends in a folder with the file's own name, holding one file per chunk. `<index>` is the chunk's number in decimal, from 0, with at least 8 digits (`00000012`). The suffix sits on the operation's folder, so a file name of up to 255 bytes still fits. Such an entry is listed like the others, with no hash. It is never restored, `not-a-copy`: it holds the bytes a corrupt copy had, to look at, never to put back. Its purge is one operation `kind = unlink`, which removes each chunk, then its folder, Atoll's own, and whose reprise does the same; what is not a chunk stays there, with a warning.
- What goes in:
  - copies removed by a propagated deletion (`deleted`);
  - stale copies before their refresh (`overwritten`);
  - corrupt copies set aside (`quarantined`), and the chunks a repair overwrites (`repaired`);
  - surplus replicas removed by `prune` (`surplus`) and replicas removed by rebalancing (`rebalance`);
  - copies left at the old name by a rename their disk refuses (`renamed`, §15.3);
  - every copy of a file deleted through Atoll's own view (phase 2a).
- Retention `[trash].retention` (30 days by default): an entry is purged once it reaches `expires_ns`. `max_size` is a share of each disk's capacity, as the catalog last recorded it (§10). When a disk's trash holds more than that, counted as the sizes of its entries, purging goes in this order until it holds no more: surplus and rebalance, then overwritten, repaired and quarantined, each by `deleted_ns`, the oldest first, then by operation. Deleted and renamed files are never purged before their retention ends, since a renamed copy may be the file's only other copy: Atoll raises an alert instead (§23). An entry of unknown size is never purged for room, as nothing says what it would give back, and nothing is on a disk with no capacity recorded. A trashed path hard-linked elsewhere counts with its size, though it frees no space until its last link goes. `atoll trash purge` purges (§17.4), and so does the daemon after each pass (§15.13). What goes is a pure function (`conformance/vectors/trash_purge.json`).
- Commands: `atoll trash list`, `restore <path> [--to <disk>]`, `purge [--older-than <duration>]`.
- Link guard (optional, `trash.link_guard = true`), against a deletion or a replacement made around Atoll, as through mergerfs (§15.2). Off by default.
  - **The links.** Each pass that writes gives each copy of a disk a hard link at `<workdir>/links/<file id>`, an operation `kind = link`, journaled a thousand at a time and made durable with one sync of the folder. The folder is made with mode 0700, as a link reaches its file whatever the folders above it in the root forbid. A file that Sonarr or Radarr already linked elsewhere gets one too. A link moves its file's change time, for every name: their rows take it, as after a move (§17.1, step 6).
  - **What a pass does with them**, once everything else of it ran, on each disk it walked and can write to, decided by a pure function (`conformance/vectors/link_guard.json`):
    - a link that leads to its file's copy stays;
    - one whose data is another copy found there, or an entry of the trash, goes, `kind = unlink`: Atoll moved that data itself;
    - any other's data lost every name Atoll knows in the root, as a deletion or a replacement made around Atoll leaves it. It goes to the trash, an operation `kind = trash` from the link, as a copy goes (§17.1): at its file's path, or, once the file's row went with its last copy, at the path the last `copy-gone` event of that disk gave it; `overwritten` when its file still has a copy on that disk, of another version or waiting for one, `deleted` otherwise. Data whose name in the root is still there, as a file a rule now ignores keeps it, only loses its link;
    - a link nothing names, with neither a row nor a `copy-gone` event, stays, with a warning, as does anything of the folder Atoll did not make, a name that is no file id as Atoll writes it or what is no regular file: none of it is ever removed;
    - then each copy found there without its link gets one.
  - **Through mergerfs**, a deletion removes every copy at once, and each disk's link keeps that disk's data: each goes to its own trash, `deleted`, and `atoll trash restore` brings it back. An atomic save replaces one copy and removes the others: once the pass has made the new version current and copied it back, each old version goes to its disk's trash, `overwritten`, and each link follows the new file.
  - **Atoll's own link is never counted** among a file's names: a walk takes the links of `links/` out of `nlink`, and so does every look of an operation, so that a row records what it would without the guard.
  - **A purge** removes first the links that lead to an entry it purges, `kind = unlink`: Atoll sent that data to the trash itself, and a link left behind would keep it once purged, then send it back to the trash at the next pass.
  - **Turned off**, at the next pass, each link goes the same way: one that leads to its copy is removed, one whose data lost every other name goes to the trash first, and the folder goes once empty. A copy whose deletion a guard holds (§15.6) keeps its link until that is decided, as the link may hold its data alone. A disk the guard never wrote to, by its journal, is not opened for it: an index on the operations with a link answers that. Turn it off and let a pass run before uninstalling Atoll: links left behind keep the data of the files deleted since.
  - **Cost**: every managed file shows one more link to tools that count them, as some torrent and *arr helpers do; each pass reads the folder, a look at each link; the first pass makes a link for each copy. A run that may not write leaves the links as they are.
- A copy whose name its driver refuses cannot go to the trash: `a:b.mkv`, written before the disk was mounted with `windows_names`, gives `EINVAL` on ntfs-3g and ntfs3 (measured). The copy stays where it is, the command says `name-refused`, and an alert is raised. Atoll never removes it another way, and never refreshes or repairs it, as what those overwrite must reach the trash first. Until the user renames it, mergerfs may present it even when it is stale.
- Limits: without the link guard, deletions through mergerfs bypass the trash in phase 1. An x1 file deleted directly on its disk has no other copy to keep. A trashed path that is also hard-linked elsewhere frees no space until its last link goes.

### 17.1 Sending a copy to the trash

One operation for each copy, `kind = trash`:

1. In one transaction: the `ops` row, `planned`; the `trash` row, with `expires_ns` at the time plus `[trash].retention`, or the largest time §9 holds if that is past it; and the event. A `trash` row whose operation is not `done` is not in the trash yet.
2. The name must still hold the copy: a regular file whose inode is `file_key`. Otherwise nothing moves, and the outcome is `changed`.
3. The folders of the target are created below `<workdir>`, `<workdir>` itself if needed, on a disk confirmed in this session (§10), each synced, then its parent, before anything goes through it. A folder that stands for one of the logical path, or the operation's folder, which stands for the root, is made with the mode of that folder, then takes its mode and owner where the driver allows it, so that a private folder is never open, even for a moment. Any other is made with mode 0755, less the umask: it exposes dates and ids.
4. Right before the move, as step 3 can be long, the copy is compared with its row as §15.10 compares a known copy: an edit in place keeps the inode, and must not leave the library for the trash unseen. Otherwise nothing moves, the outcome is `changed`, and the folders of step 3 go. The modification time of the source's folder is read, and the copy is moved without replacing anything: `renameat2` with `RENAME_NOREPLACE`; on `EINVAL` or `ENOSYS`, `link`, `fsync` of the file at the target, then of the target folder, a look at the source's name, then `unlink`. That look compares the inode, the size and the modification time, as `link` moved the change time. If it finds another file or an edit, the link goes, and the outcome is `changed`. If it finds nothing, the link is the file's name now: it stays, and the move goes on as done. Afterwards the name at the target must hold the copy: a file that took the source's name since the look goes back, as §17.2 resolves it, and the outcome is `changed`.
5. `fsync` of the file at its new name, then of the target folder; the time of the source's folder restored (§6.1, item 8); `fsync` of the source's folder.
6. In one transaction: the operation `done`, and the `copies` row removed. When other names lead to that inode, their rows take its link count, and its change time if they recorded the one it had just before the move: the move changed them for all, and a row that recorded another has a change nobody has looked at, for the next scan to report.

- Measured on ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g, on Linux 6.12 and 7.3: the move keeps the inode, the modification time and the creation time, and moves the change time, which Linux 6.12 can leave the same within a clock tick. ntfs-3g and lowntfs-3g give `EINVAL` for `RENAME_NOREPLACE` whatever the name. So `EINVAL` from the first call says nothing, and only `link` tells: `EINVAL` from `link` too, or from `mkdir` on the way, means a name the driver refuses (measured on the three NTFS drivers, with `windows_names`), and nothing moved. The operation is then `undone`, and the outcome `name-refused`.
- An operation is never `done` on the strength of the move alone. On a device that fails, XFS answers `rename` with success, only the first `fsync` after it gives `EIO`, whichever folder it is for, a second gives success, and after a remount the file is back at its source (measured). So any error from an `fsync`, or `EIO`, `ENOTCONN`, `ENODEV`, `ENXIO`, `ESTALE` or `EROFS` from another step, leaves the operation `planned`, makes the disk `missing` and ends the session on that disk, with exit code 4. A folder time that cannot be restored for another reason is only logged: a folder's date protects nothing, and the next scan records what is there. ntfs3 keeps no journal of its own: after such a failure, a name and its record can disagree, and it answers `ESTALE` or `EINVAL` (measured). A look that gets either concludes nothing, and only a repair of the volume can say more.
- Every name a move makes, and every folder made, is synced through what it leads to before its folder is: on NTFS a file or a folder keeps its names in its own record too, which ntfs3 does not write when only the folder is synced. Measured on ntfs3, Linux 6.12 and 7.3, with a device that fails right after the target folder's `fsync`: without it, a file and a folder renamed were gone under both names, the folder with all it held, an entry of the trash and a file restored answered `ESTALE` with their operations `done`, and a folder of the trash just made answered `EINVAL`. With it, each is at its new name, or at its old one with the operation `planned`, as on ext4, XFS and ntfs-3g, a file renamed into another folder included. One case stays, seen on a move to and from the trash: the device failing between the syncs of the two folders leaves both names leading to the file, which §17.2 resolves by concluding nothing. A name that no longer leads to what moved has nothing to sync, and what Atoll may not open keeps the sync of its folder alone, with a warning. It costs about 1 ms a move, measured in qemu on ext4 and ntfs3, and about 1.5 ms while another file is being written, against 35 to 120 ms for `syncfs`, which also made the move durable.
- Before each operation the device id is checked again: the mount path must still have the mount id and device of the handle (§10). A folder on another mount is not entered, as in §15.10. Nothing has moved and no look can go through it, so the operation is `undone` at once, with no event. The outcome is `changed` when that folder is on the way to a copy going to the trash, and `io-error` otherwise, for a restore and a purge too: a file hidden by a mount is not gone.
- Whatever stops a move before it is done, step 2 and step 4 included, is resolved at once by looking at both names, by the table of §17.2, its `op-undone` event included: a replacement has one, and an edit in place has none. After another error, such as `ENOSPC` or a file already at the target, the operation is `undone` and the outcome is `io-error`; or the look finds that the move went through, the operation finishes as in §17.2, and the outcome is the one it would have had. The session goes on.
- A disk that answers `ENOSPC` to a move to its trash, at any step, takes no more moves to its trash in that session, as each takes a folder there: they wait in the queue, since each would be journaled and undone for nothing until room comes back. The next session tries again.
- Folders of the trash left empty go once an operation is over, whether it was `undone`, restored or purged, down to and including the date. `<workdir>/trash/` stays.

### 17.2 Reprise after a crash

Every command that writes resolves the `planned` operations of each writable disk, first, in the order of their ids, and confirms that disk to do so. A run that may not write, with `--dry-run` or without `--yes`, resolves nothing, and its exit code says nothing of them. A disk that is not confirmed keeps its operations for later. A disk lost here, or an operation that stays `planned`, gives exit code 4, whatever else the command lists. `atoll scan` resolves nothing, as it writes nothing to a disk: until then it can report as `absent` a copy whose operation is `planned`.

A move looks at both names, and compares each with `ops.file_key`: `absent`, `same` or `other`. The target is the trash for `trash`, and the logical path for `restore` and `rename`. For `rename`, `same` is the identity of §15.1 where a row says it, so a reused inode is `other`, and the file or folder either way.

| Target | Source | Then |
|---|---|---|
| `same` | `same` | `link` was interrupted: the source's name is removed, and the operation finishes. For `trash`, when the file's size or modification time is no longer what its row says, it was edited in place since: the name in the trash is removed instead, and the operation is `undone`. A name that `unlink` answers with `ENOENT` while a look right after still finds the file there is one its record no longer holds: ntfs3 leaves both names leading to the file, with one link, when a device fails between the syncs of a move's two folders, and then removes nothing (measured). Nothing is concluded: the operation stays `planned`, and only a repair of the volume can say which name is the file's |
| `same` | `absent`, `other` | the operation finishes; an `other` at the source is a new file, for the next scan |
| `absent` | `same` | it is `undone`: nothing happened |
| `absent` | `absent`, `other` | it is `undone`, with an `op-undone` event |
| `other` | `absent` | it is `undone`, with an `op-undone` event. For `trash`, the file at the target goes back to the logical path first, synced there as §17.1 syncs any name it makes: it took that name between the look and the move, and nothing else writes in the trash. If the folder it goes back to is gone, the operation stays `planned`, and nothing is made. For `restore` and `rename`, a logical path is anyone's, so what is there stays |
| `other` | `same`, `other` | it is `undone`, with an `op-undone` event, and the file at the target stays where it is |

- An operation `trash` that is `undone` loses its `trash` row. A `restore` or a purge that is `undone` leaves the entry as it was.
- First, before it looks at either name, what the target's name leads to is synced, if anything, then every folder from the target's to the top of the disk, as the operation does not say which were made, nor whether the move reached the sync of what it moved, and the next move goes through them without syncing them. That can be long, and an edit during it must be seen.
- An `undone` move then syncs the file at the source, then the folders of both names, as a name may have gone or gone back.
- To finish, the source's name goes, after a look at its inode right before: a name that no longer holds the file stays, as a new file for the next scan. The file is synced at the target, as its own record lost that name, then the folder it leaves gets its time back, and is synced. Then the target is looked at again: if it no longer holds the file, the operation is `undone`, with an `op-undone` event, and nothing is recorded of it.
- The rows of the other names take the link count and the change time of what is there now, whatever they recorded: what the file had just before the move is gone with the crash. So an edit in place that kept the size and date, and came at that moment too, only shows at `--full`.
- A `trash` that finishes here starts its retention again: `deleted_ns` and `expires_ns` are computed from now, as the entry could not be seen until then, so the command that finishes it cannot also purge it, with `--older-than` either.
- A `restore` that finishes here counts for its path: this run leaves any older entry of that path alone, as a retry of the same command would otherwise restore that too. An entry trashed since that restore was journaled, with a higher id, is another matter, and goes back as usual.
- A purge (`kind = unlink`) looks at the file in the trash: `absent`, it finishes; `same`, it is `undone`; `other`, it is `undone`, with an event.
- An operation of the link guard on a link (§17): `kind = link` finishes when the link is there with its inode, its file's names taking the change time they find now, as what it had before is gone with the crash, and is `undone` otherwise; `kind = unlink` with a link finishes when nothing is there, likewise, and is `undone` otherwise. A `kind = trash` from a link follows the table above with the link as its source, and nothing is ever found edited in place: a link has no row to be compared with.
- An operation `utimens` (§15.4) looks at the copy at its place, after the file there is synced. The same inode and size with the time its event holds, at the precision of that disk (§9), as NTFS keeps only multiples of 100 ns of it: it finishes, and the row takes the modification and change times there; with the time its row says, it is `undone`, as nothing happened; anything else, it is `undone`, with an `op-undone` event, as the copy changed since, and the next look says how.
- An operation `chmod` (§15.4) does the same with the mode its event holds, compared exactly: that mode, it finishes, and the row takes the mode and change time there; the mode its row says, it is `undone`, as nothing happened, or as NTFS kept none of it, and the next pass applies it again and records what the disk holds; anything else, `undone` with an `op-undone` event.
- An operation `chown` (§15.4) does the same with the owner its event holds, both ids compared: that owner, it finishes, and the row takes the owner, mode and change time there; the owner its row says, it is `undone`, as nothing happened, or as the driver kept none of it; anything else, `undone` with an `op-undone` event.
- A folder made below the root (`kind = mkdir`, §17.4): a folder there, the operation finishes; nothing there, it is `undone`; anything else, it is `undone`, with an event.
- A folder removed below the root (`kind = rmdir`, §17.5): nothing there, the operation finishes, and the folder's row goes; the folder its row says, by inode, it is `undone`, as nothing happened; anything else, it is `undone`, with an event.
- A `rename` that finishes here finds the copy by disk, inode and where its row says it sits, a row with no `old_lpath` being at its file's path, as a rename back clears it. The identity its names are compared with is that row's: while an exchange is half recorded, two rows say they sit at one place, and only one has the operation's inode. It gives the copy `old_lpath` NULL when `path2` is the file's path now, and `path2` otherwise, so that a rename the user made meanwhile still reaches it: the next run renames it from there. No row is ever said to be at a name it is not at. It loses `verified_ns`: what it finds records the change time, which hides any edit made before the move, so read again a mismatch is a `conflict`. A folder gives each copy behind below it `old_lpath` as above, and likewise a copy at its own file's path below it with another copy there, which a rename back left so, also when its clock says `moved`, as a scan between the crash and the reprise reads that very move, and its job goes. Those copies keep `verified_ns`: the change time of a file below a renamed folder does not move, and none is recorded, so an edit made before the move still reads as `edited`.
- An operation `repair` (§16.1) reads the copy whole, and the chunks of its entry in the trash, as §16.1 says: the writes all done, it finishes; otherwise the repair stops, `done` when a chunk reached the trash and `undone` when none did.
- A folder time the crash left moved stays moved, and the next scan records it: nothing depends on it.
- A move, `trash`, `restore` or `rename`, that a reprise finishes counts as moved: the original, policy and state of each file are worked out again when the command ends. A purge or a folder does not, as when they finish at once.
- A crash after an operation is `done`, and before the command ends, leaves the original, policy and state to the next scan, or to the next command that moves something. No file depends on them.

### 17.3 `atoll prune`

`atoll prune [--dry-run] [--yes]` moves `surplus` copies to the trash (`reason = surplus`). It reads the catalog, runs a mount check on every disk, looks at each copy of each file that has a `surplus` one (`lstat`, compared with its row as §15.10 compares a known copy), and confirms what it needs when it is going to write (§10). So it wakes the disks that hold those copies, and no other.

For each `surplus` copy, by position, the first that applies:

| Outcome | When |
|---|---|
| `conflict`, `corrupt` | the file is in that state |
| `disk-unavailable` | its disk is neither `online` nor `standby`, the disks §13.4 calls writable |
| `changed` | it is not what its row says, or it has not followed a rename yet |
| `unverified` | it has no `verified_ns`, or the file has no hash: run `atoll scan --full` |
| `below-level` | the copies that stay, `ok`, unchanged and on a disk `online`, `standby` or `readonly`, are fewer than the level |
| `unverified` | of those, the ones with `verified_ns` are fewer than the level |
| `last-user-copy` | it is a user copy, and none of those is (§6.1) |
| `to-trash` | otherwise: `trashed` once done, or `changed`, `name-refused` or `io-error` (§17.1) |

- The level counts `replicas` + 1, with `replicas` the larger of the file's row and what the configuration says now: a copy now wanted does not go on the strength of the last scan's marks.
- A copy is compared with its row as that row stands when the copy is looked at: a move earlier in this run changed the rows of the other names of its file (§17.1, step 6), which must not read as a change of them.
- Output: `api/schemas/prune.schema.json`, each `surplus` copy by path in byte order, then by position. `bytes` adds up the sizes of those `to-trash` or `trashed`. `applied` says whether it was allowed to write.
- A catalog that fails once output has started leaves it unfinished, as for `atoll status` (§15.11).
- Once anything has moved, the original, policy and state of each file are worked out again, as after a scan (§15.10).

### 17.4 `atoll trash`

- `atoll trash list` reads the catalog and touches no disk: each entry whose operation is `done`, by path in byte order, then by operation. Output: `api/schemas/trash-list.schema.json`.
- `atoll trash restore <path> [--dry-run] [--yes]` takes a file or a folder, compared by whole components as in §15.11. For each path below it, the entry with the highest operation id goes back to its own disk, which must be `online` or `standby`. Output: `api/schemas/trash-restore.schema.json`.
  - It looks first, and journals nothing for what it finds: something at the path, a parent of it that is not a folder, or a file at that path whose copy on this disk still sits elsewhere (§15.12) gives `path-taken`; on an NTFS disk, a name in the catalog with the same `lpath_key` as the path or a folder on the way to it, and other bytes, gives `name-collision` (§15.7); a folder on another mount on the way gives `io-error`. It does not look at the file in the trash, so a run without `--yes` can say `to-restore` of what then gives `changed`.
  - Then the operation `kind = restore`, journaled as in §17.1. Each missing parent folder is an operation of its own, `kind = mkdir`, `path` being the folder: made with the mode and owner of the same folder in the trash, where the driver allows it, and with mode 0755, less the umask, when there is none there; a folder of the trash that cannot be looked at stops the restore with `io-error`, as nothing can be said of the mode it should have. It is synced, the time of the folder it lands in is restored and that folder synced, and it is recorded with `created_by = atoll`. A folder someone else made meanwhile is theirs: the operation is `undone`, and no row is written for it. A folder made for a restore that then fails stays, as Atoll's own. The root is never made (invariant 3).
  - The move follows §17.1, with the time of the folder it lands in restored. A name the driver refuses gives `name-refused`, and a path taken since the look, or a file on the way to it, `path-taken`, with the operation `undone`. A folder gone since the look gives `io-error`, likewise. A file in the trash that is no longer the entry's, gone or replaced, gives `changed`, with the operation `undone` and an `op-undone` event, and the entry stays, for a purge to end. If that other file already reached the path, it stays there, for the next scan.
  - `--to <disk>` puts each entry on that disk rather than on its own. An entry already there goes back as above. Any other is copied through the copy pipeline (§14.2) from the trash it is in, which keeps it, to expire as any other; its disk must be one that can be read, `online`, `standby`, `readonly` or `draining`, and `<disk>` one that can be written to, `online` or `standby`, neither `draining` nor `failed`: otherwise `disk-unavailable`. It looks first as above, on `<disk>`, where a name its filesystem refuses gives `name-refused` (§15.8). Then one operation `kind = create` on `<disk>`, journaled with a `restore` event whose `source` is the entry's disk: the part `<op id>.part` in `<workdir>/tmp/`; the file of the trash, whose inode must be the entry's, read once, each chunk hashed as it is written; the file looked at again, as a copy's source is; the times, mode and owner given to the part, which is synced, read again without its cache, each chunk against its hash, and against the entry's whole-file hash when it has one; then the missing parents, each an operation `kind = mkdir` with the mode and owner of the same folder in the trash it comes from; and the part takes the path without replacing anything, as a copy does (§14.2). The new copy is recorded as a restored one (below). A file of the trash that is no longer the entry's gives `changed`, a part that does not read back as it was written `io-error`, each with the operation `undone` and the part gone. A reprise finds the part at the path and finishes, recording it as a restored copy, or removes the part otherwise (§14.2, §17.2). Output: each entry with `to`, the disk it went to.
- A restored copy is a user copy, `ok` and unverified, with its `first_seen` at that time, and otherwise follows §15.10 for a copy not yet in the catalog: another size makes the file a `conflict`. A reprise that finds that path held by a file whose copy on that disk still sits elsewhere writes no row, as a disk holds one copy of a file, and the next walk reports it in `path_taken`. A row left at that path, of a file now at another, goes, with its file if that was its last copy. The original, policy and state are then worked out again. When other names lead to that inode, their rows take its link count, and its change time if they recorded the one it had just before the move, as in §17.1, step 6.
- `atoll trash purge [--older-than <duration>] [--dry-run] [--yes]` deletes what has reached `expires_ns` (`expired`), then what §17 purges for room on each disk whose trash still holds more than `trash.max_size` of its capacity (`max-size`). With `--older-than`, it deletes what was trashed at least that long ago, whatever its reason (`older-than`), and nothing for room: the user chose what goes. An entry that reached the trash in this run's reprise counts in its disk's trash, and is not purged, for room either (§17.2). One operation for each entry, `kind = unlink`, with `file_key` that of the entry's `trash` operation. A file there that is not that one stays, with its entry: `changed`, with an `op-undone` event. An entry on a disk neither `online` nor `standby` stays: `disk-unavailable`. Without `--yes` it does not look at the file, so it can say `to-purge` of what then gives `changed`. When other names lead to that inode, their rows take its link count, and its change time if they recorded the one it had just before the `unlink`, as in §17.1, step 6. With the link guard, the links that lead to an entry go first (§17). The first name by path in byte order that still leads to it says what those are. Output: `api/schemas/trash-purge.schema.json`, each entry with why it goes, and `full`: the disks whose trash still holds more than `trash.max_size` of its capacity once the entries purged are gone, or for a run that may not write those it would purge, by id in byte order.

### 17.5 Atoll's own empty folders

A folder below the root that Atoll made, recorded with `created_by = atoll` (§14, §17.4), goes once Atoll left it empty (§6.1, item 8).

- At the end of each command and pass that writes, for each folder a copy left in that run, sent to the trash or renamed away by Atoll, then for each folder above it that this left empty, the root never included: when its row says it is Atoll's, and the catalog holds nothing of that disk below it, neither a copy, at its file's path or behind, nor an entry, ignored files included.
- One operation for each, `kind = rmdir`, `path` the folder, `file_key` the inode its row records, journaled with an `rmdir` event. The name must still hold that folder, compared by inode. The time of its parent is read, the folder removed (`unlinkat` with `AT_REMOVEDIR`, which only an empty folder passes), the parent's time restored and the parent synced; then, in one transaction, the operation is `done` and the folder's row goes.
- A folder that is not empty, `ENOTEMPTY` or `EEXIST`, gone, or no longer that folder, stays, with the folders above it: the operation is `undone`. An error that makes a disk `missing` follows §17.1; another leaves the folder, with a line of the log.
- A folder the user emptied, or one a run left empty before it was stopped, stays: Atoll removes only what its own operations of that run emptied, as an empty folder the user kept may be one they want. A command's output does not list these operations: the journal and the events hold them.

## 18. Disk lifecycle

| Operation | Command | Effect |
|---|---|---|
| Add a disk, empty or full | `atoll disk add /mnt/u2 --domain d4-320 --tags usb [--dry-run]` | checks identity, topology and mount point; adds the disk to the configuration; indexes `media/` in place and hashes inside `maintenance.window`, moving nothing; files with the same content at the same path become existing copies, each keeping its own modification time, and nothing is written to them; others follow §15.7; appends the branch at the end of the running mergerfs |
| Remove | `atoll disk remove u2 --evacuate [--dry-run]` | `draining`: user and Atoll copies re-created elsewhere; once nothing depends on it, `atoll disk remove u2 --yes` takes it out of the configuration, and the catalog keeps it `retired` |
| Missing disk | automatic | detected within 30 s (`missing`); its mergerfs branch is removed at runtime; its files stay visible through their replicas; x1 files become `unavailable`; alert. After `health.missing_grace` (24 h), the missing replicas are re-created elsewhere when space allows |
| Declare failed | `atoll disk fail u2 --yes` | `failed`, even if it comes back: re-creates the missing replicas of degraded files at once; `atoll disk remove u2 --yes` takes it out |
| Replace | `atoll disk replace u2 --with /mnt/u3` | the new disk comes in as `disk add` brings one, with an id of its own and the domain and tags of `u2`, which becomes `draining`, or stays `failed`: the copies `u2` held are made again, and `u2` is taken out once nothing depends on it |
| Disk comes back | automatic | identity check and reconciliation first (copies deleted, renamed or modified meanwhile are updated), then its branch is added back; replicas re-created during its absence become `surplus`; in-sync copies may become originals again (§7) |

- **A disk's `state`**, in its `[[disk]]` of the configuration, says that it is leaving: `draining` or `failed`. The disk commands set it; left out, the disk is in service. It lives in the configuration rather than in the catalog, as the configuration is the truth about the disk list (§10) and the catalog can be rebuilt (§9): a disk declared failed must stay so. A change made by hand needs `systemctl reload atoll` (§15.13).
- **`draining`.** Its copies count for no file's level, so the plan re-creates elsewhere what they hold, reading them as sources (§13.4); it is a target for nothing. It is read, and kept in step with the others until it leaves: renames, deletions and modifications reach its copies as they reach any other. Its mergerfs branch is `NC`. Mounted read-only, it is `readonly`: its copies count again, and its evacuation waits until it is mounted read-write. It is drained when every file with a copy on it is `ok` without that copy.
- **`failed`.** Never read or written again: no walk, no pass, no hash read, no watch, no remount, no alert of its own. Its copies count for nothing and are never sources, so the plan re-creates the replicas of its files at once, from their other copies, without waiting for `health.missing_grace`. A file whose only copies are there has none to read: it stays in the view through the disk's `RO` branch, which comes last, as long as the disk can still be read. It stays `failed` even if it comes back.

## 19. SSD cache (phase 2b, designed in phase 1)

- The cache needs Atoll's own view (phase 2a): mergerfs cannot apply per-path rules when a file is created. Cache disks are left out of the mergerfs branches until then.
- A disk with `role = "cache"` has its own `media/`, which is part of the merged view. New files land there, either written through the view (the cache disk is preferred for creation) or written to it directly.
- Transfer to HDD:
  - copy (pipeline §14) to the HDD chosen by the solver, at the same logical path;
  - creation of the replicas;
  - removal of the SSD copy only after every HDD copy is verified. This removal is a journaled Atoll operation, never propagated as a user deletion.
- The logical path never changes: media servers see no difference.
- The SSD copy is the older one, so the HDD copy becomes the original once the SSD copy is gone.
- Residency: `cache.min_age` (7 days by default), overridable per rule (`cache_min_age`). A better release that replaces the file during its residency costs only SSD writes.
- `cache.max_fill` (share of the cache's capacity): above it, the oldest files leave early; when the cache is full, writes go straight to an HDD. A write that runs out of space on the cache moves the partial file to an HDD and continues (§21.2).
- During residency a file has a single copy, unless `cache.mirror` (two SSDs) or `replicate_early` (an immediate HDD replica, which wakes a disk) is set.
- At creation, only path, file name and extension are known: `cache = false` on a rule using only those conditions sends matching files straight to HDD. Size conditions apply once the file is stable: a large file can leave the cache first with `cache_min_age = "0s"`.
- What the SSD protects, and what it does not: §20.

## 20. Power loss

- A file that an application is writing when power fails is incomplete, whether it lands on SSD or HDD. The application must write it again; no cache prevents that.
- Atoll guarantees that its own operations can be replayed: atomic writes (§14), the `ops` journal committed with `synchronous=FULL`, the job queue in SQLite, the cleanup of `Atoll/tmp/` and resume at startup.
- What the SSD cache adds: HDDs only receive complete files, copied and verified from the SSD. A power cut during a transfer is recovered without loss.
- Hardware recommendations:
  - a UPS, essential with a USB DAS;
  - an SSD with power-loss protection (PLP) when it serves as a buffer, since consumer SSDs without it can lose writes that were not yet flushed.
- NUT integration (phase 2b):
  - on battery: no new copy starts, copies in progress finish;
  - low battery: caches flushed and the daemon stopped cleanly.

## 21. Merged view and media servers

### 21.1 Phase 1: mergerfs

- `atoll mergerfs-config` prints the configuration as one fstab line, `<branches> <view.mount> mergerfs <options> 0 0`, and nothing else, so that it can be appended to `/etc/fstab` as it is. The type `mergerfs` is the mount helper mergerfs ships (`mount.mergerfs`). With `--json`, it gives the branches, the options and that line (`api/schemas/mergerfs-config.schema.json`). The settings of the openmediavault mergerfs plugin come later. Atoll never edits `/etc/fstab` itself.
  - branches: the `<root>` folders of the data disks (`<mount>/<root>`), in configuration order, each written `<path>=<mode>,<minfreespace>`, or `<path>=<mode>` when it has no `minfreespace` of its own; new disks are appended at the end, so the copy `ff` presents does not change; cache disks are left out until phase 2b. The mode is `RW`; `NC` for a disk `draining`, whose files stay in the view and can be changed, but which takes no new file; and `RO` for a disk `failed`, which comes after all the others, so that `ff` reads its copy of a file only when no other disk has one, and what only it holds stays in the view, never written (§18);
  - `category.search=ff`;
  - `category.action=epall`, so renames and deletions reach every copy, except where a branch refuses a rename (§15.2);
  - `category.create` from `placement.new_files`: `mfs` for `most-free`, `ff` with branches in fill order for `ordered`, `pfrd` for `percent`; Atoll then replicates. Until phase 2b every value works as `most-free` (§24.4), so the option is `mfs`;
  - a per-branch `minfreespace`: what `placement.min_free` keeps free on that disk (§13.1), from the capacity the catalog holds for it, which a scan records each time it finds the disk. A disk no scan has found yet gets the smallest size in `min_free`; with no size there, its branch has no `minfreespace` of its own, mergerfs uses its global one (4 GiB by default), and the command exits with 4. A size is written in the largest of `T`, `G`, `M` and `K` that holds it exactly, which mergerfs reads as powers of 1024, and otherwise in bytes. It must be larger than the largest file expected: when one is not larger than the largest file the catalog knows, the command warns on standard error and still exits with 0;
  - `moveonenospc=false`, and `rename-exdev` and `link-exdev` pinned to `passthrough`, so mergerfs never moves data or creates links on its own;
  - short attribute and entry caches only: `cache.attr=1`, `cache.entry=1` and `cache.negative_entry=0`, in seconds, as Atoll changes branches out of band and mergerfs advises against caching in that case.
  - The options come in the order of this list. In the line, a space, a tab, a line feed and a backslash of a path are written `\040`, `\011`, `\012` and `\134`, as fstab(5) reads them. mergerfs reads `:` as the end of a branch and `,` as the end of an option, so a branch path holding either is refused: the command names the disk and exits with 1, as it does with no data disk, which would leave mergerfs no branch. `=` is no trouble, as mergerfs takes the last one of a branch, and the mode always follows (measured on mergerfs 2.40.2).
- Atoll removes a missing disk's branch at runtime through the `.mergerfs` control file (`user.mergerfs.branches`), and adds it back after reconciliation. Runtime changes are not persisted; the printed configuration always lists every data disk.
- Media servers and the SMB share use this mount. The same configuration is the break-glass fallback.
- Prefer SMB for network access: NFS exports of a FUSE mount handle out-of-band changes poorly (stale file handles).

### 21.2 Phase 2a: Atoll's own view on Linux (FUSE)

- Mounted at the same mount point as mergerfs, through cgofuse.
- Presents the original (§7), never a stale, corrupt or unavailable copy.
- Every entry (files, ignored files, folders, symlinks) and `statfs` is served from the catalog: browsing wakes no disk, and the listing matches what mergerfs would show.
- Writes: new files placed by the solver or on the SSD cache. On ENOSPC during a write, the view moves the partial file to a disk with enough space and retries.
- Deletions: every copy goes to the trash.
- Optional parallel replica writes in `immediate` mode.

### 21.3 Phase 4: Windows

- Until then, Windows PCs map the SMB share of the merged view as a network drive.
- Phase 4, in Rust: Windows service and pool drive through WinFsp. Member disks keep their own drive letters or mount folders. Files are opened with `FILE_SHARE_DELETE`, so users can still rename or delete a file during a long copy or scrub.

### 21.4 Plex and other media servers

- Plex already reads a mergerfs mount: nothing changes; Atoll's view later replaces mergerfs at the same mount point.
- Plex reads each disk's folders:
  - before the first replica is created, move the libraries to the merged view once;
  - Plex documents this move ("Move Media Content to a New Location"): back up the Plex database, disable automatic trash emptying, add the merged-view folder to each library while keeping the old folders, scan, check that everything plays, remove the old folders, then empty the trash. Plex associates the files at the new location with the existing items;
  - the scan examines every file, which wakes every disk and can take hours: try it on a small library first;
  - until then, keep `replication.enabled = false`; otherwise every replicated file appears in Plex as a second version of the same item.
- Keep Plex's "Empty trash automatically after every scan" **off permanently**: when a disk is missing, its x1 files vanish from the view, and a scan would otherwise delete their entries, metadata and watch history.
- While a disk is missing, Atoll sends no scan notification for the affected folders (§21.5) and raises an alert. How Jellyfin and Emby treat missing files during their own scans is to be checked (§30).

### 21.5 Notifications

- Partial scan of the changed folder:
  - Plex: `GET /library/sections/<id>/refresh?path=<folder>`, for each library whose folders, the `Location` entries `GET /library/sections` gives, hold the folder; a folder above a library's folder asks for that folder. The token `token_file` holds goes in `X-Plex-Token`; without `token_file`, Plex must take this host's requests without one.
  - Emby and Jellyfin: `POST /Library/Media/Updated` with `{"Updates": [{"Path": <folder>, "UpdateType": "Modified"}]}`, a hundred folders a request. The key `api_key_file` holds goes in `Authorization: MediaBrowser Token="<key>"` for Jellyfin, which no longer takes `X-Emby-Token` from its version 12.0, and in `X-Emby-Token` for Emby; with no `api_key_file`, nothing is sent, and the log says why. A folder whose path is not UTF-8 cannot be named in JSON, and is left out.
  - A request has 10 s. A server that does not answer 2xx is tried again 5 minutes later, with all it was to be told. A redirect is not followed, and is such an answer: the token or the key would go to whatever host it names. The `url` is the server's own address.
- `path_map` translates host paths into the paths the server sees (Docker container). The longest key that is a prefix of the path, by whole components, applies. A folder's host path is `view.mount`, then the folder (`conformance/vectors/path_map.json`).
- Batching: one notification per folder per 30-second window. A folder is told at once, then not again for 30 s; a change there in between is told when they end.
- A media server's automatic scan sees changes made through the mount on the same host; Atoll notifies the others. The daemon that runs for good, after each pass, tells each server set of:
  - the folder that holds each path a ripe group of the watcher names (§15.1), when someone other than the view and Atoll made it, or the watcher cannot tell who, as with inotify, and it changed more than attributes;
  - the folders of the pass's operations that change a name: both paths of a rename of a file or a folder, and the path of a copy moved to the trash.

  A copy Atoll makes or repairs changes no name, and calls for nothing. A command tells no server; a daemon in shadow mode logs what it would tell, and tells nothing.
- While a disk is missing, a folder below which a file has a copy on that disk, in any state, is held. It is told after a pass finds the disk back, or once the disk is `failed` (§21.4).
- `atoll notify test` asks each server set something that changes nothing (§23).
- The media server's periodic scan can then be disabled. A new file written straight to a disk while the daemon is stopped is found by its next walk, which renames and deletes nothing, so nothing tells the server of it: keep a periodic scan where that happens.

### 21.6 One disk awake

- Reading a file wakes one disk.
- Browsing:
  - phase 1: mergerfs lists every branch; its caches reduce wake-ups but do not prevent them;
  - phase 2a: Atoll's view serves listings from the catalog and wakes no disk.
- Background work wakes disks, so it runs inside the time windows of the configuration, which all default to `maintenance.window`: rescans, scrubs, catalog backups to the disks, scheduled replication. SMART polling uses `smartctl -n standby`, which skips sleeping disks.
- What still wakes disks: Windows Explorer thumbnails, antivirus, indexing, Plex analysis tasks (media analysis, chapter thumbnails).
- Measures:
  - disable thumbnails and indexing on the pool;
  - idea to validate: turn off thumbnails on network folders through Group Policy;
  - schedule heavy Plex tasks inside the night window.
- Idea to evaluate (phase 5): keep the first and last MiB of each video on the SSD to absorb header reads.

## 22. Filesystems

| Filesystem | Linux | Windows | Notes |
|---|---|---|---|
| ext4 | native | no | recommended for disks Windows never reads; 256-byte inodes needed for nanosecond times and creation time |
| NTFS | ntfs3 (kernel) or ntfs-3g | native | the only format shared by both systems; ntfs3 less proven than ext4; mount with `windows_names` and, for ntfs3 from Linux 7.3, `ads=0` |
| XFS | native | no | optional (phase 4) |
| btrfs | native | no | optional (phase 4); snapshots possible later |

- A pool can mix filesystems; the solver checks name compatibility (§15.8) and compares times at the coarser precision (§9).
- A pool used directly under Windows can contain NTFS disks only.
- Linux 7.3 also has an in-kernel driver of the type `ntfs`, which Linux 6.12 does not. `mount -t ntfs` reaches it unless a `mount.ntfs` helper exists, as Debian's `ntfs-3g` package installs. It mounted a dirty volume that ntfs3 refused. Its name rules were never measured, so a disk mounted with it is `readonly` (§10).
- NTFS after an unclean USB drop: the volume is marked dirty, ntfs3 refuses to mount it without `force`, and Linux has no full equivalent of chkdsk. Atoll never forces such a mount; it alerts, and the volume is repaired with chkdsk on Windows. This is a reason to prefer ext4 for USB disks that Windows never reads.
  - **How Atoll knows.** The daemon that runs for good reads it once each time an NTFS disk is missing, as soon as the device its serial names under `/dev/disk/by-uuid/` is there. It reads a few sectors and writes nothing, so an unplugged disk is never woken for it.
  - **What it reads:** the flag `VOLUME_IS_DIRTY` (0x0001) of `$VOLUME_INFORMATION` in `$Volume`, the fourth record of the MFT, once the record's update sequence is applied (`conformance/vectors/ntfs_dirty.json`). Measured on Linux 6.12 and 7.3: ntfs3 refuses to mount a volume so marked, and does not mark one it mounts for writing; `ntfsfix -d` clears the flag.
  - **What follows.** A volume found dirty is never remounted after a USB reset (§10), and raises the alert `ntfs-dirty:<id>` (§23). A disk back, or `failed`, forgets it.
- §7 reads the creation time where a driver exposes it, and uses `first_seen` elsewhere. Measured per driver in `docs/research/file-times.md`.

## 23. Monitoring and alerts

- SMART through `smartctl -j -n standby` (with `-d sat` behind USB bridges): temperatures, reallocated and pending sectors, power state; sleeping disks are skipped.
  - **Who reads it.** The daemon that runs for good, once an hour, the first time after its first pass, for each disk its last mount check did not find `missing`, a `failed` disk excepted. It reads each whole disk below the mount, as `disk add` finds them (§10), again with `-d sat` when the answer asks for a device type, and keeps the worst of them: failing if one is, each count and the temperature the highest. `conformance/vectors/smart.json` says how an answer reads.
  - **What it keeps.** The reading of a disk awake goes in `disks.smart_json`, a JSON object: `read_ns`; `failing`, the disk's own health check failed or an NVMe disk raised a critical warning; `reallocated`, `pending` and `uncorrectable`, ATA attributes 5, 197 and 198, or an NVMe disk's media errors; `temperature`, in degrees Celsius, 0 when none is given. A disk asleep keeps the reading it had, and a daemon in shadow mode records none.
  - A reading that tells of more trouble than the one before, a health check failed that had passed or more bad sectors, brings a pass, whose alerts tell of it.
  - Without `smartctl`, the daemon says so in its log and reads no SMART until its configuration is read again. `smartctl` takes `CAP_SYS_RAWIO` for ATA pass-through, which `deploy/atoll.service` gives.
- Sensors (lm-sensors): CPU, fans.
- Alerts through ntfy, Gotify or SMTP on:
  - missing disk, degrading SMART, NTFS volume marked dirty;
  - `corrupt`, `conflict`, `unavailable`, `name-incompatible` or `name-collision` file;
  - mass-deletion guard triggered;
  - trash full before deleted or renamed files reach their retention;
  - files waiting in `manual` mode for too long;
  - pool more than 90 % full.
- **Who sends them.** The daemon that runs for good, after each pass (§15.13); a command says what it found on standard error and through its exit code, and sends nothing.
- **Conditions.** After each pass, the daemon works out which of these hold, each with a key and a count:
  - `disk-missing:<id>` and `disk-readonly:<id>`: a disk the pass found so, count 1;
  - `files-<state>`, for each of `corrupt`, `conflict`, `unavailable`, `name-incompatible` and `name-collision`: the files in that state, counted;
  - `guard-deletions:<id>` and `guard-modifications:<id>`: a guard tripped on that disk (§15.6), count 1;
  - `manual-waiting`: the files in `manual` mode left `unprotected` longer than `defaults.manual_alert_after` since first seen, counted;
  - `pool-full`: the data disks that hold their files now, `online`, in `standby`, `readonly` or `draining`, together more than 90 % full, count 1;
  - `name-refused`: the operations of the pass that a driver refused (§15.8, §17), counted;
  - `trash-full:<id>`: a disk that holds its files now, `online`, in `standby`, `readonly` or `draining`, whose trash holds more than `trash.max_size` of its capacity, counted as in §17, count 1. It is worked out after the daemon's purge, so what is left there waits for its retention, or for a purge that could not reach it;
  - `smart-failing:<id>`: the last reading of SMART of a disk neither `failed` nor retired says that its health check failed, count 1;
  - `smart-sectors:<id>`: the last reading of such a disk counts sectors reallocated, pending or uncorrectable, counted together, so that the alert comes again when the count grows;
  - `ntfs-dirty:<id>`: a missing NTFS disk whose volume the daemon found marked dirty (§22), count 1.
- **When.** A condition that begins is sent at once, as one alert: a pass's files in one state are one alert, which names the first ten by path in byte order, as many of them as fit in 4,000 bytes of text, and says how many more there are. One that holds is sent again when its count grows, and every 24 h while it lasts, saying since when. One that ends is forgotten, with no message. The table `alerts` keeps what was sent, so a restart sends again only what is due. An alert that cannot be sent is kept unsent, and the daemon passes again within 5 minutes to try it again. A channel that cannot be reached is not tried again for the other alerts of that pass, each of which would wait as long; one whose server answered with a refusal is. Sending happens outside the catalog's lock. With no channel set, the daemon works out no alert and leaves the table as it is, so that a channel set later hears of what holds then; a daemon in shadow mode (`--dry-run`) logs the alerts due, and sends and records none.
- **ntfy.** An HTTP `POST` of the alert's text to `alerts.ntfy`, with its title in `Title`, its priority in `Priority` (4 for a disk `missing` or failing, a volume marked dirty, a guard tripped, `corrupt` or `unavailable` files, sectors pending or uncorrectable; 3 for the others) and the tag `warning`. With `alerts.ntfy_token_file`, the token that file holds goes in `Authorization: Bearer`, so that the token stays out of the configuration. A request has 10 s. A redirect is not followed, and the alert is then not sent: the token, and the alert with it, would go to whatever host the answer names, and a `POST` redirected with 301 or 302 becomes a `GET`, which sends nothing and is answered 200. Where it leads stays out of the error, as the topic of a public server is its secret.
- **Gotify.** An HTTP `POST` to `<alerts.gotify>/message` of a JSON object, `title`, `message` and `priority`: 8 where ntfy's priority is 4, which Gotify's clients sound, and 5 otherwise. The application token that `alerts.gotify_token_file` holds goes in `X-Gotify-Key`, read at each send; Gotify takes no message without one, so a channel with no token file never goes, and says why. A request has 10 s, and a redirect is not followed, as for ntfy.
- **SMTP.** `alerts.smtp` is `smtp://[user@]host[:port]?from=<address>&to=<address>`, with one `from` and one `to` or more, bare addresses each; or `smtps://` for TLS from the first byte. The port is 587 for `smtp` and 465 for `smtps` when left out. `smtp` takes `STARTTLS` when the server offers it, and must when a user is given, as a password never goes in clear: that password is the one `alerts.smtp_password_file` holds, read at each send, sent by `AUTH PLAIN`, and never written in the address. The mail goes from `from` to each `to`; its subject is the alert's title, encoded as RFC 2047 says when it is not ASCII; its text is the alert's, in UTF-8 and quoted-printable; and `X-Priority: 1` marks what ntfy sends at 4. A conversation has 10 s, and a mail went when the server took it after `DATA`. A refusal by the server is its answer.
- `atoll notify test` sends a test alert through each channel that is set, then asks each media server set (§21.5) something that changes nothing, which its token or key must open: Plex's `GET /library/sections`, Jellyfin's and Emby's `GET /System/Info`. It says for each whether it went: when its server answered 2xx. It reads the configuration only, and needs no catalog. It exits with 0 when each did, with 4 when one did not, with 1, and nothing on standard output, when no channel and no media server is set or the configuration cannot be used, and with 2 on a usage error. Output: `api/schemas/notify-test.schema.json`.
- Prometheus metrics on `/metrics` (phase 2b).

## 24. Configuration

One TOML 1.1 file (§29.3). `schema/config.schema.json` is the reference for every key: its type, its allowed values and its default. This section gives what a schema cannot say.

### 24.1 Example

Also in `deploy/atoll.toml`.

```toml
# /etc/atoll/atoll.toml — Windows: %ProgramData%\Atoll\atoll.toml
pool_id  = "home-media"
strategy = "energy"                     # safety | energy | capacity
catalog  = "/var/lib/atoll/catalog.db"
timezone = "local"                      # time zone of every time window (IANA name or "local")

[layout]
root    = "media"                       # managed folder inside each disk
workdir = "Atoll"                       # visible folder: trash/, tmp/
marker  = true                          # Atoll/disk.toml (optional)

[[disk]]
id     = "t1"
uuid   = "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b"
mount  = "/mnt/t1"
fs     = "ext4"
domain = "thinknas"
tags   = ["sata"]

[[disk]]
id     = "t2"
uuid   = "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d"
mount  = "/mnt/t2"
fs     = "ext4"
domain = "thinknas"
tags   = ["sata"]

[[disk]]
id     = "u1"
uuid   = "01D9F3A2B4C5E6F7"             # 64-bit NTFS serial number
mount  = "/mnt/u1"
fs     = "ntfs"
domain = "d4-320"
tags   = ["usb"]

[[disk]]                                # phase 2b
id     = "c1"
uuid   = "9b2e4d6f-1a3c-4e5f-8a7b-6c5d4e3f2a1b"
mount  = "/mnt/c1"
fs     = "ext4"
role   = "cache"
domain = "thinknas"
tags   = ["ssd"]

[defaults]
replicas           = 1                  # 0 to 2 replicas (x1 to x3)
mode               = "deferred"         # immediate | deferred | scheduled | manual
settle             = "10m"
manual_alert_after = "7d"

[placement]
new_files      = "most-free"            # most-free | ordered | percent
fill_to        = "90%"                  # of each disk's capacity, for ordered
min_free       = ["5%", "100GiB"]       # whichever is smaller; also mergerfs minfreespace
spread         = "domain"
spread_mode    = "prefer"               # prefer | strict
tags_mode      = "prefer"               # prefer | strict
overflow       = "allow"                # allow | deny (never relaxes strict settings)

[view]
engine = "mergerfs"                     # mergerfs (phase 1) | atoll (phase 2a)
mount  = "/srv/pool"

[read]                                  # Atoll's own view (phase 2a)
policy   = "oldest"                     # oldest | prefer-spinning
classes  = ["sata", "usb"]

[maintenance]
window           = "02:00-06:00"        # rescans, catalog backups; default of the windows below
rescan_every     = "7d"
catalog_on_disks = false                # a catalog backup in each disk's Atoll/catalog/

[replication]
enabled               = true
window                = "02:00-06:00"   # may wrap past midnight, e.g. "23:00-05:00"
max_parallel_per_disk = 1

[deletes]
direct     = "propagate"                # propagate | restore
debounce   = "30s"                      # longer than a USB disk's spin-up
mass_guard = { files = 100, percent = "5%", window = "10m" }

[modifies]
mass_guard = { files = 100, percent = "5%", window = "10m" }   # files modified, not touched

[balance]
schedule = "02:00-06:00"                # never | immediate | time window
trigger  = { ratio_below = "90%", or_more_than = "200GiB" }   # share of bytes correctly placed
budget   = "3h"
equalize = "off"

[integrity]
scrub_every  = "30d"
window       = "02:00-06:00"
scrub_budget = "4h"
auto_repair  = true
repair_mode  = "chunk"                  # chunk | file
manifest     = false                    # Atoll/manifest.b3sum

[trash]
retention        = "30d"
max_size         = "5%"                 # of each disk's capacity
link_guard       = false                # phase-1 protection against deletions through mergerfs

[cache]                                 # phase 2b
enabled         = false
min_age         = "7d"                  # minimum residency on the SSD
max_fill        = "80%"                 # of the cache's capacity
mirror          = false
replicate_early = false

[health]
missing_grace       = "24h"             # before missing replicas are re-created elsewhere
remount_after_reset = true              # never for an NTFS volume marked dirty
evacuate_on         = ["smart_failing", "pending_sectors"]   # phase 4
avoid_hotter_than   = "50C"                                  # phase 4

[ups]                                   # phase 2b, through NUT
enabled = false
nut     = "ups@localhost"

[notify.plex]
url        = "http://127.0.0.1:32400"
token_file = "/etc/atoll/plex.token"
path_map   = { "/srv/pool" = "/data" }  # paths as seen by the container

[notify.jellyfin]
url          = ""
api_key_file = ""

[notify.emby]
url          = ""
api_key_file = ""

[alerts]
ntfy            = "https://ntfy.sh/my-secret-topic"
ntfy_token_file = ""                    # a Bearer token, for a ntfy server that asks one
gotify          = ""                    # "https://gotify.example", the server
gotify_token_file = ""                  # the application token Gotify asks for
smtp            = ""                    # "smtps://user@mail.example?from=atoll@example&to=me@example"
smtp_password_file = ""                 # the password of that user

[api]                                   # the local API of the daemon (§25.1)
socket     = "/run/atoll/atoll.sock"
listen     = ""                         # "127.0.0.1:7341", or an address of the Tailscale network
token_file = ""                         # the Bearer token TCP asks for

# Rules: the first matching rule wins; conditions are combined with AND.
[[rule]]
label    = "temporary and system files"
filename = 're:\.(part|!qb|tmp)$|^(thumbs\.db|\.ds_store|desktop\.ini)$'
action   = "ignore"

[[rule]]
label    = "samples"
filename = "*sample*"
size_max = "300MiB"
action   = "ignore"

[[rule]]
label    = "photos"
path     = "Photos/**"
replicas = 2
mode     = "immediate"
priority = 100

[[rule]]
label    = "subtitles and metadata: x3, they are small"
ext      = ["srt", "ass", "nfo"]
replicas = 2
mode     = "immediate"

[[rule]]
label    = "artwork"
filename = "{poster,fanart,folder,banner}.{jpg,png}"
replicas = 2
mode     = "immediate"

[[rule]]
label         = "large movies: leave the SSD cache first"
path          = "Films/**"
size_min      = "40GiB"
spread_mode   = "strict"                # one copy on the ThinkNAS, one on the D4-320
cache_min_age = "0s"                    # size is known only once the file is written

[[rule]]
label         = "movies"
path          = "Films/**"
spread_mode   = "strict"
cache_min_age = "14d"                   # leaves time for release upgrades

[[rule]]
label    = "series"
path     = "Séries/**"
mode     = "scheduled"
colocate = "season"

[[rule]]
label = "archives: replicate on demand"
path  = 're:^Archives/'
mode  = "manual"

[[rule]]
label    = "downloads: no replica"
path     = "Téléchargements/**"
replicas = 0
```

### 24.2 Where a value comes from

- **Required keys**: `pool_id` and `view.mount`; on each `[[disk]]`, `id`, `uuid`, `mount` and `fs`; on each `[[rule]]`, `label`, unique in the file (it names the rule in messages and in `files.rule`). `atoll init` writes the first two. A pool may have no disk and no rule.
- **Any other key left out** takes the value of the `strategy` preset when the preset sets that key (§12.2), then its `default` in the schema. Without `strategy`, no preset applies. So a key set in the file wins over the preset, and the preset wins over the default.
- **Keys that follow another key**, when neither the file nor the preset sets them:
  - `replication.window`, `integrity.window` and `balance.schedule` take `maintenance.window`;
  - a disk's `root` and `workdir` take `layout.root` and `layout.workdir`.
- **Keys with no value when left out**: `strategy`, `health.avoid_hotter_than`, a disk's `domain`, and a disk's `state`, which leaves the disk in service.
- `""` means "not set" for `ups.nut`, a `url`, a `token_file`, an `api_key_file` and the keys of `[alerts]`.
- **A rule property the rule leaves out**:

| Rule property | Pool-wide value |
|---|---|
| `replicas`, `mode`, `settle`, `priority` | `[defaults]` |
| `spread`, `spread_mode`, `tags_mode`, `colocate`, `overflow` | `[placement]` |
| `cache_min_age` | `cache.min_age` |
| `action`, `ignore_case`, `cache` | none: `manage`, `true` and `true` |
| `tags`, `domains` | none: no tag and no domain |

A loaded rule holds what the file sets, plus `action` and `ignore_case`. The rest of this table applies when the rule gives a file its policy (§11.3).

### 24.3 Loading

Loading takes three steps. A `timezone` is then looked up among the time zones of the machine, the only part that depends on it, and a problem is reported at `timezone`. `local` means the machine's own and `UTC` is always known, so neither is looked up. Any other name, `Local` included, must be a time zone in the machine's database: a folder or another file of that database is none, and a machine with no database knows none.

1. **Syntax.** One leading UTF-8 byte order mark is ignored, as Windows editors may write one and go-toml alone refuses it (measured); a second one is a syntax error. The rest must be valid UTF-8 and valid TOML 1.1. A syntax error stops here.
2. **Keys and values.** The tree the parser gives is checked against the schema, with keys compared byte for byte. A `format` starting with `atoll-` applies a grammar of §29.5. TOML types count: `1.0` is a float, so it is no integer, where JSON Schema would take it for one. A value gets one problem at most, and a list with a wrong item is not also checked as a whole: `min_free = ["5"]` gives `placement.min_free[0]`, not `placement.min_free`. A third-party validator ignores `format`, so it can accept a value Atoll refuses, such as `100.5%`, but never the reverse.

   Nothing converts the tree before this check. Measured with go-toml v2.4.3 and BurntSushi v1.6.0, decoding into a typed structure takes `Pool_ID` for `pool_id` with no error, and keeps one of the two when a file holds both; go-toml also reads `[disk]` as `[[disk]]`. Rust's `toml` 1.2.0 with `deny_unknown_fields` refuses all of them.
3. **Relations.** Only when step 2 found no problem, and after the values of §24.2 are applied:
   - two disks never have the same `id`, `uuid` or `mount`, and the later one is reported;
   - a `uuid` is 8-4-4-4-12 lowercase hexadecimal digits on ext4, XFS and btrfs, and 16 uppercase hexadecimal digits on NTFS;
   - a disk's `mount` is never inside another disk's. `view.mount` is never equal to a disk's `mount`, inside it, or around it. `catalog`, its default included, is never equal to a disk's `mount` or `view.mount`, or inside either. "Inside" compares whole components: `/mnt/ab` is not inside `/mnt/a`. The inner `mount`, `view.mount` or `catalog` is reported;
   - a disk's `root` and `workdir`, as §24.2 gives them to that disk, are names its `fs` accepts (§15.8), and differ from each other: by their exact bytes, and on NTFS by `lpath_key` too. So a name in `[layout]` is not checked for a disk that sets its own. `layout.root` and `layout.workdir` differ even with no disk. A name is reported at the disk's key when the disk sets it, otherwise at the key in `[layout]`; two names that do not differ are reported at `workdir`, or at `root` when the disk sets only that;
   - `spread = "none"` never goes with `spread_mode = "strict"`, in `[placement]` (reported at `placement.spread`) and for each rule that sets either (reported at the rule's `spread`, or its `spread_mode` when it sets only that);
   - `ups.enabled = true` needs `ups.nut`, reported at `ups.nut`;
   - no two rules have the same `label`, and the later one is reported;
   - `path`, `filename` and `ext` compile (§11.1, §29.5), reported at that key, `ext` as a whole;
   - `size_min` is never above `size_max`, compared in bytes after rounding (§29.5), reported at `size_min`;
   - `action = "ignore"` takes no other property of §11.2, reported at each such property. Conditions and `label` stay allowed;
   - `settle` and `cache` never go with `size_min` or `size_max`, as they are read before the size is known (§11.3), reported at `settle` and `cache`;
   - a rule that sets `tags_mode = "strict"` has at least one item in `tags` or `domains`, reported at `tags_mode`. A `strict` that comes from `[placement]` asks for nothing (§12.4).

Each step reports every problem it finds, at most one per key, sorted by key as written, in byte order. A key reads `placement.spread`, `disk[0].uuid` or `rule[2].ext[1]`, with indexes from 0. A part holding anything other than ASCII letters, digits, `_` and `-` is written in double quotes, such as `notify.plex.path_map."/srv/pool"`: `"` and `\` after a backslash, U+0000–U+001F and U+007F as `\uXXXX` in uppercase hexadecimal, and any other character as it is. A syntax error has no key. The wording of messages is not part of the contract.

### 24.4 Warnings

- A setting whose feature arrives in a later phase of §28 is accepted, has no effect yet, and gets a warning when the configuration loads, with the fallback of §12.2 when there is one. Only a value that asks for the feature gets one (`mode = "scheduled"`, `cache.enabled = true`), and only when the file or the preset sets it, never a default. So the schema holds every key from the start, and a configuration written today keeps loading.
- `view.engine = "atoll"` is the exception: an error until phase 2a, as there would be no merged view at all. It is reported at `view.engine`, with the relations of §24.3.
- A tag or domain that a rule or `read.classes` names and no disk carries gets a warning too, as `disk add` may bring it later. A disk with no `domain` carries none, not even its `id`. The key is that of the item, such as `rule[0].tags[1]`, and warnings are sorted as problems are.

Phase 1a warns about these settings, and each phase removes its own lines. The key is the one written, or the one a preset sets (`rule[0].tags`, `disk[1].role`), and "any" takes in an empty list:

| Setting | Value that gets a warning | Phase | Until then |
|---|---|---|---|
| `read.policy` | anything but `oldest` | 2a | no effect |
| `read.classes` | any | 2a | no effect |
| `placement.new_files` | anything but `most-free` | 2b | works as `most-free` |
| `placement.colocate`, a rule's `colocate` | anything but `none` | 2b | no effect |
| `balance.equalize` | anything but `off` | 2b | no effect |
| `cache.enabled` | `true` | 2b | no effect |
| a disk's `role` | `cache` | 2b | no effect |
| a rule's `cache`, `cache_min_age` | any | 2b | no effect |
| `ups.enabled` | `true` | 2b | no effect |
| `health.evacuate_on`, `health.avoid_hotter_than` | any | 4 | no effect |

## 25. Command-line interface

```
atoll init [--pool-id <id> --view-mount <path> [--catalog <path>]]   create the configuration and the catalog
atoll disk add | list | show | remove | fail | replace
atoll scan [--disk <id>] [--full]        quick: size and times; --full: re-hash everything
atoll status [--path <path>]             copies, original and states
atoll capacity [--json]                  what the pool holds at 1, 2 and 3 copies, in theory and without moving a copy (§13.3)
atoll replicate <path> [--now | --force] [--dry-run]
atoll rules check | explain <path> [--size <size>]
atoll scrub [--disk <id>] [--path <path>] [--budget <duration>] [--dry-run] [--yes]
atoll repair <path> [--disk <id>] [--mode chunk|file] [--dry-run] [--yes]
atoll dupes [--path <path>] [--min-size <size>]   read-only: the files that hold the same content (§16)
atoll trash list | restore <path> [--to <disk>] [--dry-run] [--yes] | purge [--older-than <duration>] [--dry-run] [--yes]
atoll conflicts list | resolve <path> --keep <disk> [--dry-run] [--yes]   (§15.7)
atoll names check [--path <path>]        read-only: names not in NFC, not valid UTF-8, incompatible with a filesystem, or colliding, and paths past a length limit (§15.9)
atoll guard list | confirm --disk <id> [--kind deletions|modifications] | reject --disk <id> [--kind deletions|modifications]
atoll prune [--dry-run] [--yes]          surplus copies to the trash (§17.3)
atoll balance [--dry-run] [--yes]        Atoll's copies a strict setting no longer allows, moved where it wants them (§13.3)
atoll notify test [--json]               a test alert through each channel set, and a request to each media server (§21.5, §23)
atoll catalog backup | verify | rebuild  verify: SQLite integrity check, then catalog vs disks (read-only)
atoll mergerfs-config                    (phase 1 and break glass)
atoll mount <point>                      (phase 2a on Linux, phase 4 on Windows)
atoll daemon [--foreground] [--until-idle] [--dry-run]
```

- `--config <path>` or `--config=<path>` comes before the command, and nowhere else, and names the configuration file. By default it is `/etc/atoll/atoll.toml`, and on Windows `%ProgramData%\Atoll\atoll.toml`.
- Every command that moves or deletes accepts `--dry-run` and writes nothing without `--yes`. Without `--yes` it asks no question: it shows what it would do and exits with 3, or as below if there is nothing to do. So both versions behave alike, and a scenario can judge it. `--dry-run` with `--yes` is a usage error. The daemon takes no `--yes`, as starting it is the consent (§15.12). `daemon --dry-run` plans jobs without running them (shadow mode, §29.2).
- Every command accepts `--json`; outputs follow the schemas in `api/schemas/`, and exit codes follow §29.3.
- `atoll --help`, `atoll -h` and `atoll help` list the commands, each with what it does; `atoll help <command>`, and `--help` or `-h` anywhere among a command's arguments before a `--`, show that command's synopsis and what it does, on standard output, and exit with 0 whatever else the arguments hold. An unknown command or subcommand there is a usage error, exit 2.
- **Language.** What the CLI writes for people, its human output, its usage lines and the messages it gives on standard error, is in French when the first of `LC_ALL`, `LC_MESSAGES` and `LANG` that is set and not empty starts with `fr`, and in English otherwise. The `--json` outputs, the words they share with the human output (outcomes, reasons, states, decisions), command and flag names, paths, the configuration's keys and values, the daemon's log, and the details of an error that comes from below the CLI stay in English: they are what a script or a search reads. Durations are written as the configuration writes them (§29.5), as `10m`, never in nanoseconds.
- A command's flags come after its whole name, so `atoll disk --json list` is a usage error, and so is a flag given twice. They may come before or after a path, and `--` ends them, for a path that starts with `-`. A command line is checked before the configuration is read, so a usage error comes first. It writes nothing to standard output, with `--json` or without.
- `atoll init` creates what is missing, and may run again. The configuration is written with `--pool-id`, `--view-mount` and `--catalog`, and its folder made, only when the file does not exist, and needs the first two; with a file already there they are refused, with exit code 1. Then the catalog is created, with its folder, if it does not exist; a catalog of another `pool_id` is refused. It reads no disk, but for a catalog it creates: its operations start past the highest id the disks' trash and `tmp/` name, as those of `catalog rebuild` do (§9), so that a new operation's folder of the trash or part never meets an old one, as when a catalog went missing; a disk whose folders cannot be read is a warning on standard error. It exits with 0 once both exist; with 1 when the configuration cannot be written, read or loaded, or the catalog cannot be used; and with 2 on a usage error, which takes in a value the configuration would refuse, one of `--pool-id` and `--view-mount` without the other, and `--catalog` without them.
- `atoll scan` follows §15.10, and `--disk <id>` limits the walk to one disk, without changing what is checked. A disk `failed` is never walked, and has no line. It exits with 0 when every disk asked for was walked, and hashed with `--full`; with 4 when one was skipped or failed; with 1, and nothing on standard output, when the configuration or the catalog cannot be used, or `<id>` is no disk of the configuration; and with 2 on a usage error.
- `atoll status` follows §15.11. It opens the catalog and looks at no disk.
- `atoll mergerfs-config` follows §21.1. It opens the catalog and looks at no disk. It exits with 0 once the line is printed; with 4 when a branch has no `minfreespace` of its own, the line printed all the same; with 1, and nothing on standard output, when the configuration or the catalog cannot be used, there is no data disk, or a branch path is one mergerfs cannot take; and with 2 on a usage error.
- `atoll scan`, `atoll scrub`, `atoll repair`, `atoll prune`, `atoll replicate`, `atoll trash restore`, `atoll trash purge`, `atoll daemon --until-idle`, `atoll guard confirm`, `atoll guard reject`, and without `--dry-run` `atoll disk add`, `remove`, `fail` and `replace` hold an exclusive `flock` on the catalog file for their whole run, `--dry-run` included, so that a reprise never takes another command's operation for a crash, and a scan never records a move half done. A second one, or one that meets a pass of the daemon that runs for good, which takes the lock for each pass and lets go of it between them, waits for it: it says so once on standard error, tries again every 200 ms, and then runs as it would have, its output and exit code unchanged. The lock creates no file, and the kernel lets go of it when the process ends, however it ends. `atoll status`, `atoll dupes`, `atoll trash list`, `atoll guard list`, `atoll disk list`, `atoll disk show`, `atoll names check`, `atoll mergerfs-config` and `atoll notify test` take none.
- `atoll prune`, `atoll trash restore` and `atoll trash purge` follow §17.3 and §17.4. They exit with 0 when everything asked for was done, or with `--dry-run` once they have answered; with 3 when they lacked `--yes` and there was something to do; with 4 when something stayed or a disk could not be relied on; with 1 when the configuration or the catalog cannot be used; and with 2 on a usage error. `atoll trash list` exits with 0, 1 or 2 likewise.
- `atoll replicate` follows §14.3.
- `atoll daemon --until-idle` follows §15.12, `atoll daemon` without it §15.13, and `atoll guard` §15.6. `atoll daemon` exits with 0 once stopped by `SIGTERM` or `SIGINT`, and with 1 when it cannot start, as when the configuration or the catalog cannot be used; `--foreground` changes nothing, and `--json` goes only with `--until-idle`. `atoll guard` exits with 0 once it has answered or done its work, also when there was nothing to confirm or reject; with 1 when the configuration or the catalog cannot be used, or `<id>` is no disk of the configuration; and with 2 on a usage error. Output: `api/schemas/guard-list.schema.json` and `guard-answer.schema.json`.
- `atoll disk add <mount> [--id <id>] [--domain <domain>] [--tags <tag>,…] [--dry-run]` follows §10 and §18. The disk's id is the last component of its mount point, or `--id`; the id, the domain and each tag are identifiers (§29.5). It refuses, with exit 1 and nothing written:
  - an id the configuration has, or that the catalog knows as another disk holding anything, and a mount point the configuration has;
  - a path where nothing is mounted, a filesystem other than ext4, XFS, btrfs and NTFS, or a disk its mount check finds `missing`, such as one with no `<root>` folder, which Atoll never creates;
  - an identifier another disk of the configuration has, or that the catalog knows under another id;
  - a variant that changes the rules of names: the reasons `ads`, `nocase`, `casefold`, `ascii-ci` and `ignore-case` of §10;
  - a disk on a physical disk another disk of the configuration lives on (R-06): a partition's disk, and the disks below a device-mapper or md device, followed down, or below a btrfs filesystem; a loop device is its own;
  - a marker of another pool, another disk or another filesystem.

  A disk mounted read-only is taken, `readonly`, with a warning. Then it appends the disk to the configuration (§10), records every disk's state in the catalog, writes the disk's marker, and sends `SIGHUP` to the daemon that runs for good, which walks the disk at its next pass; it walks nothing itself, as `atoll scan` would. It holds the catalog's lock, except with `--dry-run`, which writes nothing and answers as it would have. A warning says what it could not check or do: `topology-unchecked`, when a disk of the pool is not mounted; `not-immutable` and `immutable-unchecked`, for the folder under the mount point, read through a detached clone of its parent's mount that leaves out the mounts below it; and `catalog-not-updated`, `marker-not-written` and `daemon-not-told`. It exits with 0 once the configuration holds the disk; with 4 when one of these last three was left undone; with 1 when it refused, or when the configuration or the catalog cannot be used; and with 2 on a usage error. Output: `api/schemas/disk-add.schema.json`.
- `atoll catalog backup` makes a backup of §9 and copies it to the disks with `catalog_on_disks`. It holds the catalog's lock. It exits with 0; with 4 when a disk did not take its copy; with 1 when the configuration or the catalog cannot be used; and with 2 on a usage error. Output: `api/schemas/catalog-backup.schema.json`. `atoll catalog verify` runs SQLite's integrity and foreign key checks on the catalog, then, when they find nothing, walks each disk not `failed` and compares it with the catalog as a scan would (§15.10), each confirmed past the cache first, writing nothing to the catalog or a disk. It takes no lock. It exits with 4 when the checks find anything, or a disk was not walked, or holds a file the catalog does not know, a copy changed, moved or absent, a path another file took, or a name that cannot be read: `atoll scan` records what it found. It exits with 0 otherwise, 1 when the configuration or the catalog cannot be used, and 2 on a usage error. Output: `api/schemas/catalog-verify.schema.json`, each disk as `atoll scan` gives it.
- `atoll catalog rebuild [--dry-run] [--yes]` follows §9. Without `--yes` it says what it would do and exits with 3, and with `--dry-run` with 0. With `--yes` it holds the old catalog's lock, and the new one's from its making. It exits with 0 once the new catalog is in place; with 4 when a disk was skipped or failed; with 1 when it could not make the new catalog or put it in place, the old one then where it was; and with 2 on a usage error. Output: `api/schemas/catalog-rebuild.schema.json`.
- `atoll scrub [--disk <id>] [--path <path>] [--budget <duration>] [--dry-run] [--yes]` follows §16. `--disk` keeps one disk, and `--path` the files at that path and below it, by whole components. It records what it finds; with `--yes` it also repairs each corrupt copy another copy is healthy for, with `auto_repair` (§16.1), or moves it to the trash, and without it says which it would repair or move. `--dry-run` reads, and records nothing. It holds the catalog's lock, except with `--dry-run`. It exits with 4 when a disk could not be read or failed, or a corrupt copy has no healthy copy elsewhere or stays where it is; otherwise with 3 when, without `--yes`, a corrupt copy is to be repaired or moved; otherwise with 0, the budget run out or not. 1 when the configuration or the catalog cannot be used, or `--disk` names no disk of the configuration, and 2 on a usage error. Output: `api/schemas/scrub.schema.json`.
- `atoll dupes [--path <path>] [--min-size <size>]` follows §16, with `--min-size` a size of §29.5, 1 byte by default. It takes no lock and reads only the catalog. It exits with 0 whenever the report ran, with groups or none; 1 when the configuration or the catalog cannot be used; and 2 on a usage error. Output: `api/schemas/dupes.schema.json`.
- `atoll repair <path> [--disk <id>] [--mode chunk|file] [--dry-run] [--yes]` follows §16.1, for the corrupt copies of the files at that path and below it, by whole components, on `--disk` alone when given, by path in byte order, then by disk position. Without `--yes` it reads what a repair reads, and says `to-repair`, with the bad chunks it found; `--dry-run` does so too, and records nothing, as a copy that reads right after all is verified otherwise. It holds the catalog's lock. It exits with 4 when a disk could not be read or failed, or a copy stays corrupt; otherwise with 3 when, without `--yes`, a copy is to be repaired; otherwise with 0, also when no copy at that path was corrupt. 1 when the configuration or the catalog cannot be used, or `--disk` names no disk of the configuration, and 2 on a usage error, `--mode` other than `chunk` or `file` included. Output: `api/schemas/repair.schema.json`.
- `atoll disk remove <id> [--evacuate] [--dry-run] [--yes]` and `atoll disk fail <id> [--dry-run] [--yes]` follow §18. A file depends on a disk when it has a copy there, `ok` or `unavailable`, and fewer copies `ok` on the disks `online`, in `standby` or `readonly` than its level, or when it is in `conflict`, as that disk may hold one of its versions.
  - `disk remove --evacuate` sets the disk's `state` to `draining`. It needs no `--yes`, as it adds copies and removes nothing, and refuses a disk `failed`, which is never read.
  - `disk remove` takes the disk's `[[disk]]` out of the configuration. It refuses a disk with operations left `planned` on it, and a disk not `failed` that a file depends on: `--evacuate` first. What the disk holds stays on it, and the catalog keeps it `retired`.
  - `disk fail` sets the disk's `state` to `failed`, whatever depends on it.

  Without `--yes`, `remove` and `fail` say what they would do and exit with 3. A disk already in the state asked for is nothing to do. Each then records the disks' states in the catalog and tells the daemon, as `disk add` does. They exit with 0; with 4 when one of these was left undone; with 1 when they refused, the disk is not in the configuration, or the configuration or the catalog cannot be used; and with 2 on a usage error. Output: `api/schemas/disk-change.schema.json`.
- `atoll disk replace <id> --with <mount> [--id <new id>] [--dry-run]` brings the new disk in as `disk add` does, with the domain and tags of `<id>`, and in the same edit sets `<id>` `draining`, unless it is `failed`. It needs no `--yes`, and exits as `disk add` does. Output: `api/schemas/disk-replace.schema.json`.
- `atoll disk show <id>` runs the mount check of that disk, and reads the catalog and the disk's marker. Its `state` is `failed` for a disk configured so, `draining` for a disk configured so that its check finds `online`, and what its check finds otherwise. It exits with 0; with 1 when the disk is not in the configuration, or the configuration or the catalog cannot be used; and with 2 on a usage error. Output: `api/schemas/disk-show.schema.json`.
- `atoll disk list` runs a mount check (§10) on each disk of the configuration, in its order, and shows the state and the reason. It reads the configuration, the mount table and the disks, opens no catalog and writes nothing. So the states it shows for now are `online`, `readonly` and `missing`: `standby` comes with SMART, and `draining`, `failed` and `retired` with the catalog. It exits with 0 whatever the states; with 1 when the configuration does not load or cannot be read, with nothing on standard output; and with 2 on a usage error.
- `atoll rules check` exits with 0 when the configuration loads, warnings or not, and with 1 when it does not load or cannot be read, with the same output. So a script can run it before a restart.
- `atoll rules explain` exits with 0 once it has answered, whatever the answer; with 1 when the configuration does not load or cannot be read, with nothing on standard output; and with 2 on a size or a path that is not well-formed. `<size>` is a size of §29.5, so it needs its unit.

### 25.1 The local API

The daemon that runs for good (§15.13) answers questions about the pool over HTTP/1.1 and JSON: on a Unix socket, and over TCP when `api.listen` says so. In the alpha it only answers: a command that moves or deletes goes through the CLI, which takes the catalog's lock (§25). `api/openapi.yaml` describes it.

- **The socket**, `api.socket` (`/run/atoll/atoll.sock` by default, `""` for none), is made when the daemon starts, with mode 0660: whoever may open it may ask, as its permissions say (§26). Its folder is the service manager's (`RuntimeDirectory=atoll` in `deploy/atoll.service`), and the daemon never makes it. A socket an earlier run left at that path is replaced; anything else there stays, and so does a missing folder, the socket being then off, with a line of the log.
- **TCP**, `api.listen` (`""` by default, for none): an address and a port, the address a loopback one or one of the Tailscale network, `100.64.0.0/10` or `fd7a:115c:a1e0::/48` (§29.5); anything else is refused when the configuration loads. Every request carries `Authorization: Bearer <token>`, the token being what `api.token_file` holds, its last line break left out, compared in constant time. Without it, or with another, the answer is 401 with nothing more. `api.listen` with no `api.token_file` is refused when the configuration loads. The token is read at each request, so that a new one takes effect at once; a token file that cannot be read, or is empty, refuses every request.
- **What it answers**, to `GET` alone, any other method being 405:
  - `/v1/health`: `{"version": …}`, Atoll's version;
  - `/v1/status`: what `atoll status --json` gives;
  - `/v1/disks`: what `atoll disk list --json` gives;
  - `/v1/disks/{id}`: what `atoll disk show <id> --json` gives, and 404 for a disk the configuration does not list;
  - `/v1/trash`: what `atoll trash list --json` gives;
  - `/v1/guards`: what `atoll guard list --json` gives.

  Each answer is the output of that command, with its schema, made whole before it is sent, with 200. One that cannot be made, as when the catalog cannot be used, gives 503 with `{"error": …}`; another path gives 404.
- **Limits**: 10 s for a request's headers, 60 s for its answer, 16 KiB of headers, and no body; a connection that asks nothing more is closed after 60 s, as one left open holds a descriptor the daemon needs for its disks. An answer whose 60 s are over, or whose client is gone, is given up. The answers that read the catalog, `/v1/status`, `/v1/trash` and `/v1/guards`, are made one at a time, as each is made in the daemon's own process, beside its passes, and `/v1/status` may plan the whole pool (§13.3): another waits for it within its own 60 s, and gives 503 past them. After a reload (§15.13) the answers follow the new configuration; a changed `api.socket` or `api.listen` takes effect at the next start. A daemon in `--dry-run` answers too: answering writes nothing.

## 26. Non-functional requirements

- Resources (ZimaBoard 2 or M920q): under 200 MB of memory for 200,000 files; under 5 % CPU outside scrubs. The daemon that runs for good, under a change every second for four hours on 4,000 files, kept its heap in use between 2.8 and 6.8 MiB, with no growth, and its resident memory under 52 MiB (`TestDaemonSoak`, §27). The binary sets a soft memory limit of 170 MiB unless `GOMEMLIMIT` says otherwise. A first scan of 200,000 files with two copies each peaks at about 180 MiB, in 12 s on an idle machine, and a later one at about 155 MiB, in 5 s (measured on empty files in a container, so it says nothing about the time real disks take). `atoll status` peaks at about 21 MiB on the same pool, with `--path` over all of it too, as it writes files as it reads them. `atoll prune --yes` takes about 8 ms per copy and `atoll trash purge --yes` about 4 ms, nearly all of it in commits and `fsync` (measured on 20,000 files in a container), and they peak at about 40 MiB there. On the same 200,000 files with two copies each, a pass with nothing to do peaks at about 156 MiB, in 5 s; a folder holding them all renamed on one disk, at about 187 MiB for the scan that first sees it, the most of all, 178 MiB for a pass that first sees it, 178 MiB and 28 s for the pass that decides it with one folder operation, and 181 MiB for a dry run. That same rename through the view, on both disks, and every copy of a disk gone at once, are the most a pass holds: about 181 MiB and 179 MiB. The passes after every copy of a disk went, while the guard holds them and once they go, take about 8, 7 and 15 s: what reads below a folder must read by the folder's paths, not by its disk, as it is asked once for each folder that disk lost. 200 MB is 190.7 MiB. What keeps the worst under it, measured by the peak the kernel records rather than by samples: no list of every file grown entry by entry while the rest is held, as for a moment it is both its old size and its new one; no job list for a plan whose jobs nothing runs; one string where the catalog gives the same path twice; a compact row for each copy not where its row says; and a scan letting go of what its looks found of each copy before its plan. A pass writes what it does as it does it, so its output adds nothing to this. `atoll replicate` on a folder that holds all 200,000 files, none of them on the second disk, peaks at about 175 MiB for a dry run and 178 MiB for queuing every copy, in 8 s; with every copy pending, a scan peaks at about 179 MiB and a pass at 180 MiB; 500 empty files are copied with `--now` in 9 s, nearly all of it in commits and `fsync` (measured in a container). On 200,000 files a disk on two disks, in qemu without KVM, where every instruction is emulated and a time says how a step grows rather than what a machine takes (`TestScale`): the first pass, which finds every copy of a catalog that knows none, takes 5 min 50 s; a scan after it 1 min 8 s; reading the hash of every copy 31 min; a pass with nothing to do 1 min 14 s; `atoll status` 35 s, and 1 min 13 s with a strict rule, whose plan of the whole pool it includes. The passes and the scan peak at 190.0 to 190.6 MiB there, the soft limit and what the process holds outside its heap, and `atoll status` with a strict rule at 192.5 MiB. With the link guard (§17), the first pass makes the 400,000 links in 19 min 52 s, a pass after it takes 1 min 36 s, 22 s of it for reading them, and the pass that removes them, the guard off, 17 min 5 s; they peak at 196.5, 205.3 and 197.2 MiB. A strict `atoll status` and the link guard are so past the 200 MB at that size, which is still to bring down (TODO.md). The catalog is 125 MiB after the first pass, 140 MiB with the hashes, and 197 MiB after the 800,000 operations of the guard, which the journal keeps.
- Throughput: copies and scrubs at 90 % or more of the disk's raw throughput; hashing is never the bottleneck.
- Robustness: a hard kill (kill -9) or a power cut at any moment loses no file and leaves a consistent catalog after restart.
- Security: local API over a socket with restricted permissions; HTTP limited to localhost or the Tailscale network, with a token (§25.1).
- Distribution: a single binary. Go version: Linux amd64 and arm64. Rust version: the same, then Windows amd64 (phase 4).

## 27. Test plan

- Unit and property tests:
  - solver: never two copies on a disk; `min_free` respected; capacity matching §13; user copies never removed implicitly;
  - rules: order, combined conditions, sizes, globs and regexes, NFC matching and case folding;
  - original selection: creation time on each filesystem and driver, ties, stale copies never presented;
  - names: the corpus of §29.4.
- Integration without privileges: disks simulated with directories.
- Loop devices (Linux, root): ext4 (128- and 256-byte inodes), NTFS (ntfs3 and ntfs-3g), XFS, btrfs; disk really full.
- USB drop simulation with dm-error, dm-flakey or `scsi_debug`: the mount stays live and returns EIO, as with a real USB drop; I/O errors are never read as deletions.
- Mount guards: a failed mount leaving an empty mount point yields `missing`, never deletions, and nothing is written there.
- Mirror layout: replica path and name identical; modification time preserved and compared at the coarser precision (ext4 → NTFS); a time the target cannot hold is clamped, recorded and never applied to the other copies; creation time newer than the source.
- Copy pipeline: kill -9 and simulated power cuts at every step; `RENAME_NOREPLACE` race (a file appearing at the destination); rename of the source during a copy; resume without re-reading the source.
- Changes:
  - rename, deletion and modification made directly on one disk;
  - operations through mergerfs, attributed by process (no double propagation);
  - atomic save through mergerfs (`rename(tmp, file)`): a modification, never a deletion;
  - metadata-only change (`touch`): no copy;
  - in-place edit that keeps size and modification time: a modification, never a repair;
  - bursts of renames (Sonarr-like) and folder renames during a replication;
  - rename to a name another copy's filesystem refuses or holds as a collision: directly on one disk, and through mergerfs for a file and for a folder; a copy changed meanwhile survives; the guard pauses a large folder;
  - hard-linked paths never mistaken for renames;
  - adding a disk or rebuilding the catalog: copies with the same content and different modification times are in sync and nothing is written to them; different content is a `conflict`, whatever the times.
- Guards: mass-deletion guard; propagation limited to copies unchanged since the last sync.
- Names: NFC/NFD and case collisions reported as `name-collision`, folders included; no colliding name ever written to an NTFS target; NTFS upcase folding (`ς` and `σ` share a key, `ẞ` and `ß` do not); ntfs3 `ads` refused at `disk add`, and `readonly` when found later; a copy whose name the driver refuses stays in place and raises an alert; no copy written past the limit of its mergerfs branch.
- Trash: a chunk repair of a file with a 255-byte name, on every filesystem. Tests of what a filesystem did re-read through a cold cache.
- mergerfs with a dying branch: files stay readable through their replicas once the branch is removed; behavior before removal recorded.
- Disk sleep: SMART polling does not wake a sleeping disk; no background work outside the windows.
- **Break-glass test**, in CI, on a pool with no stale copy and no conflict: daemon stopped then uninstalled, plain mergerfs mounted from `mergerfs-config` at the same mount point: identical listing and content, no hidden file, test library unchanged.
- Plex through the merged view: no duplicate; rename, deletion and loss of a disk handled; metadata kept.
- **Conformance suite** (§29.4): vectors and scenarios shared with the Rust version, run in CI on every change.
- Windows (phase 4, CI): long paths, case, reserved names, NTFS tunneling, `FILE_SHARE_DELETE`.

## 28. Roadmap

| Phase | Milestone | Language | Included | Excluded |
|---|---|---|---|---|
| 1a — Safe core | alpha | Go | disk identity and mounts, `disk add`/`remove`, scan, file and chunk hashes, rules, `deferred` and `manual` modes with `--now`/`--force`, propagation of renames, deletions and modifications with grouping and guards, trash, `prune`, detection-only scrub, `mergerfs-config` and runtime branches, status, ntfy alerts, contracts and conformance suite (§29) | repair, notifications, other modes |
| 1b — Complete Linux MVP | beta | Go | `immediate` and `scheduled` modes, repair and quarantine, duplicates, domains and tags, SMART, Gotify and SMTP, media-server notifications, `conflicts resolve`, `balance`, `capacity`, optional link guard | own view |
| 2a — Own view | v0.1 | Go | Atoll's FUSE view on Linux: original presented, every entry from the catalog, trash on delete; contracts frozen at format version 1 | SSD cache |
| 2b — Disk sleep | — | Go | SSD cache, NUT, `ordered` and `colocate` strategies, Prometheus | Windows |
| 3 — Rust rewrite | — | Rust | feature parity with phase 2 on Linux, same contracts, conformance suite passed, switch without data migration (§29.2) | new features |
| 4 — Windows | — | Rust | Windows engine (service) and WinFsp pool drive, health evacuation, XFS and btrfs | — |
| 5 | — | Rust | web interface, openmediavault plugin, duplicates by metadata, header cache, parity study | — |

A milestone is reached when its phase and every phase before it are done: the alpha is the safe core, the beta the complete Linux MVP, and v0.1, the first stable version, adds Atoll's own view, with the contracts frozen at format version 1. The phases after 2a have no milestone yet. Where the work stands, and what remains before each milestone, is in `ROADMAP.md`.

## 29. Implementation and Rust rewrite

### 29.1 Go first

- Go, latest stable release, for phases 1 and 2 (Linux only):
  - a mature FUSE layer (cgofuse, used by rclone);
  - reference code in Go (Syncthing, rclone);
  - fast builds and easy review for a single maintainer.
- License: GPL-3.0-or-later.
- No external plugins in v1. An internal `Strategy` interface can be exposed later (WebAssembly or a separate process).
- Language: repository files (docs, code, comments, commit messages, CLI messages) in English; the CLI ships with a French translation.

### 29.2 Planned Rust rewrite

- Goal: a Rust version that replaces the Go version as is: same configuration, catalog, on-disk layout, CLI and API. Switching binaries requires no data migration.
- Timing: after phase 2, once the contracts are frozen at format version 1 and the Go version has run on the real library for some time. Windows (phase 4) is then written once, in Rust.
- During the rewrite, the Go version receives bug fixes only.
- Acceptance before the switch:
  - the full conformance suite and the break-glass test pass;
  - in shadow mode (`daemon --dry-run`) on a copy of the real catalog, the Rust version plans the same jobs as the Go version over several days.
- Fallback: the Go binary stays usable on the same data until the Rust version is accepted.
- Names: the command stays `atoll` in both versions. The name `atoll` is taken on crates.io, so the Rust package is published as `atoll-pool` if it is published there.

### 29.3 Language-neutral contracts

| Contract | Where | Rule |
|---|---|---|
| On-disk layout | §6, `disk.toml` | versioned by `format_version` |
| Catalog schema | `schema/migrations/NNNN_name.sql` | plain SQL, applied in order by both versions, needing SQLite 3.38 or later; `PRAGMA user_version` holds the schema version; pragmas of §9 |
| Configuration | §24, `schema/config.schema.json` | TOML 1.1; unknown keys are errors |
| Value grammars | §29.5 | durations, sizes, percentages, time windows, globs, regexes |
| Local API | `api/openapi.yaml` | HTTP/1.1 and JSON over a Unix socket (named pipe on Windows) |
| CLI | §25, `api/schemas/` | same commands, flags, `--json` outputs and exit codes |
| Hashes | §16 | BLAKE3-256 of the file; BLAKE3-256 of each 4 MiB chunk, concatenated |
| States, job kinds, operation kinds, event kinds, name-compatibility reasons, mount-check reasons | §4, §9, §10, §15.8 | closed lists of lowercase strings |

- Nothing persisted or exchanged uses a language-specific encoding (no Go `gob`, no Rust `bincode`).
- Times: integer Unix nanoseconds UTC in the catalog, out-of-range values as §7 and §9 state; RFC 3339 in JSON, in UTC with nine digits of fraction (`2026-10-26T01:00:00.000000000Z`), so that both versions write the same bytes.
- Sizes: integer bytes everywhere except in the configuration file.
- Paths: SQLite BLOBs (never TEXT, which a Rust `String` cannot read when the bytes are not UTF-8). In JSON, a `path` string when the path is valid UTF-8; otherwise `path_b64` with the raw bytes in base64.
- Determinism: every list the solver or the CLI outputs is sorted by a documented key; no dependence on map iteration order or random seeds.
- Exit codes: 0 success; 1 error; 2 usage error; 3 confirmation required or guard triggered; 4 partial success.

### 29.4 Shared conformance suite

- `conformance/` at the repository root, independent of the implementation language:
  - `vectors/`: input and expected output for the pure functions: rule matching, globs and the regex subset, NFC run by run, durations, sizes, percentages and time windows, name compatibility of whole paths, NTFS collisions, path lengths, the name report, the state a file shows, original selection, creation, modification and change times, capacity formula with `min_free` and strict domains, solver plans, BLAKE3 file and chunk hashes, the configuration, the effective policy of a file, the catalog schema, the mount table, the superblock and the mount check;
  - `scenarios/`: black-box cases: initial disk trees, a sequence of actions (CLI commands, direct file operations, operations through mergerfs, disconnections, crashes), expected trees, catalog content and `--json` outputs. Their format is in `conformance/scenarios/README.md`;
  - `corpus/names/`: the name corpus of §15.8: single names, with their reasons on each filesystem. The unit tests of both versions load it, and the loop-device tests create its compatible names on real filesystems.
- Unit tests of both versions load the same `vectors/` files.
- A runner drives any `atoll` binary through its CLI and the filesystem only; it imports nothing from the implementation.
- The binary it judges is a **conformance build** (`go build -tags conformance`; a feature of the same name in Rust), because a scenario needs what a real pool must never allow. The environment variable `ATOLL_CONFORMANCE` names a control file. That build takes the folders listed there as mounted disks with those identifiers, and reads the time from it. `daemon --until-idle` does all the work that is due and exits, so nothing waits or polls, and Go and Rust reach the same result, ids included; the delivered binary has it too (§15.12). The delivered binary has none of the rest of this code, so it can never skip a mount check (invariant 3). The control file can also name a point where the binary stops at once with exit code 137, as `kill -9` would stop it, and say of a disk that it lacks `RENAME_NOREPLACE` or refuses a name.
- So the scenarios do not cover real mounts, which `test-loopdev` does, or the watcher: changes reach them through rescans, and fanotify and inotify are tested in `test/integration`.
- Every behavior change first adds or updates a vector or scenario.

### 29.5 Value grammars

```
duration = digits ( "s" | "m" | "h" | "d" | "w" )                 ; "90s", "10m", "7d", "0s"
size     = number ( "B" | "KB" | "MB" | "GB" | "TB"                 ; powers of 1000
                  | "KiB" | "MiB" | "GiB" | "TiB" )                 ; powers of 1024
percent  = number "%"                                               ; 0 to 100
window   = hh ":" mm "-" hh ":" mm                                  ; "02:00-06:00", "23:00-05:00"
number   = digits [ "." digits ]
temperature = 1 to 3 digits "C"                                     ; "50C"
identifier  = ( "a"…"z" | "0"…"9" ) { "a"…"z" | "0"…"9" | "_" | "-" }  ; at most 64 characters: "t1", "d4-320"
listen   = ( ipv4 | "[" ipv6 "]" ) ":" port                        ; "127.0.0.1:7341", "[::1]:7341"
port     = nonzero digit, at most 5 digits                          ; 1 to 65535
```

- Each percentage is a share of what its key states: `min_free`, `fill_to` and `trash.max_size` of each disk's capacity; `cache.max_fill` of the cache's capacity; `ratio_below` of the bytes correctly placed; `mass_guard.percent` of a disk's copies.
- A duration must fit in a signed 64-bit count of nanoseconds. A size is computed exactly, then rounded down to whole bytes, and must fit in a signed 64-bit integer. In a window, hours run from 00 to 23 and minutes from 00 to 59 (midnight is `00:00`), and the start differs from the end, which could mean an empty window or the whole day.
- `pool_id`, a disk's `id`, `domain` and `tags`, a rule's `tags` and `domains`, and `read.classes` are identifiers. They pass through the CLI, JSON and `disk.toml`, and are compared byte for byte, so each has one spelling. NFC and NFD forms of `été` would otherwise be two domains, and `spread_mode = "strict"` would count them as two.
- An absolute path (`catalog`, a `mount`, a `token_file`, an `api_key_file`, both sides of `path_map`) starts with `/`, is not `/` itself, and has no empty, `.` or `..` component and no NUL. The Windows form comes with phase 4.
- `min_free` is a list of one or more sizes and percentages; the smallest applies to each disk (§13.1).
- `api.listen` is an address and a port (§25.1): an IPv4 address in four decimal parts from 0 to 255, each with no leading zero, or an IPv6 address in brackets, with no zone and not an IPv4 address written as IPv6; the address must be a loopback one (`127.0.0.0/8`, `::1`) or one of the Tailscale network (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`). A name is refused, as it would be resolved, and so is the unspecified address, which would listen everywhere.
- Time windows use wall-clock time in `timezone` and may wrap past midnight. A window crossing a daylight-saving change is shortened or lengthened accordingly; on a spring-forward night the missing hour is simply skipped. The first time inside a window at or after an instant is that instant when it is inside; otherwise the window's start that day, or the next day if that start has gone by, and a start that a spring-forward night skips is the first instant after it.
- Glob, implemented in-house in both versions (third-party matchers disagree: doublestar's `*` stops at `/`, globset's crosses it), matched against the whole logical path without a leading `/`:
  - `*` matches any run of characters except `/`; `?` one character except `/`;
  - `**` as a whole path component matches zero or more components;
  - `[abc]`, `[a-z]`, `[!a]` character classes. A class ends at its first `]`, so `[]a]` is rejected as an empty class. A `-` between two characters marks a range, so `[--a]` is the range from `-` to `a`; any other `-` must come first or last. `[^` is reserved: negation is written `[!`. `/` is never a character or a range endpoint in a class; a range may span it, but never matches `/`;
  - `{a,b}` alternatives, not nested; the product of the numbers of alternatives is at most 1024. Classes are read first, so a `,` or `}` in a class is literal (`{[,],x}` matches `,`); braces are then expanded, every other rule (`**` and empty components included) applies to each expansion, and the pattern matches when one expansion does (`{*,x}*` expands to `**`, which matches `p/q`);
  - names starting with a dot match like any other name;
  - rejected: an empty pattern; an unterminated or empty class; `[^`; `/` as a class character or range endpoint; a reversed range; a `-` in a class that is neither first, last nor between two characters; a backslash, reserved for a future escape syntax; `**` inside a component; braces that are nested, unbalanced, or with an empty or single alternative; an empty component (a leading or trailing `/`, or `//`); more than 1024 expansions;
  - NFC and case folding as in §11.3.
- Regex: `re:` prefix. A regex must follow the grammar below, a common subset of Go `regexp` and Rust `regex` (RE2 semantics: no backreferences, no lookaround) where both engines give the same meaning. Atoll checks it itself, in both versions, on the text in NFC form (§11.3), before the engine compiles it, and rejects anything else when the configuration loads. Unanchored unless the pattern uses `^`, `$`, `\A` or `\z`; `ignore_case` adds `(?i)`.

  ```
  regex    = branch { "|" branch }
  branch   = piece { piece }                      ; no empty branch: "", "a|" and "()" are rejected
  piece    = atom [ repeat ] | anchor | "(?" flags ")"
  anchor   = "^" | "$" | "\A" | "\z"
  repeat   = ( "*" | "+" | "?" | "{" n "}" | "{" n ",}" | "{" n "," n "}" ) [ "?" ]
  n        = "0" | nonzero { digit }              ; at most 1000; in {n,m}, n <= m
  digit    = "0" … "9"
  nonzero  = "1" … "9"
  hex      = digit | "a" … "f" | "A" … "F"
  atom     = char | "." | class | "(" regex ")" | "(?:" regex ")" | "(?" flags ":" regex ")"
  flags    = letters [ "-" letters ] | "-" letters ; letters among i, m, s, U; each letter
                                                  ; at most once in the whole group, on
                                                  ; either side of "-": "(?i-i)" is rejected
  char     = any character except  \ . + * ? ( ) | [ ] { } ^ $
           | escape | property
  escape   = "\" one of  \ . + * ? ( ) | [ ] { } ^ $ -
           | "\a" | "\f" | "\t" | "\n" | "\r" | "\v"
           | "\x" hex hex | "\x{" 1 to 6 hex "}"   ; a Unicode scalar value
  property = ( "\p" | "\P" ) ( major | "{" name "}" )
  class    = "[" [ "^" ] item { item } "]"       ; a "^" right after "[" always negates
  item     = cchar [ "-" cchar ] | property | "-" ; a "-" item only first or last
  cchar    = any character except  \ [ ] -  | escape
  ```

  - The text, in NFC form, is at most 1000 bytes long.
  - A repeat applies to an atom: `a**`, `a{2}{3}`, `^*` and `(?i)*` are rejected. Nested counted repeats multiply to at most 1000, as Go requires: along any chain of counted repeats nested in one another, the product of their counts is at most 1000. The count of `{n}` is n, of `{n,m}` m, of `{n,}` n (1 when n is 0); a repeat with an upper bound of 0 ends the chain.
  - In a class: an empty class is rejected, and a `]` is never a class character, even first (write `\]`); a `-` marks a range between two `cchar`s in order, and is otherwise allowed only first or last; two unescaped `-`, `&` or `~` in a row are rejected wherever they stand: Rust reads such a pair as a set operation (`[a--b]`, `[a&&b]`), and right after a range end, where Rust reads a literal (`[!-&&]`), the pair is rejected all the same, so the rule needs no parsing. A class never needs one of these characters twice: `[!-&&]` is `[!-&]`, and `[!---]` is `[!-\-]`.
  - `major` is one of `C L M N P S Z`. `name` is a general category (`C Cc Cf Co L Ll Lm Lo Lt Lu M Mc Me Mn N Nd Nl No P Pc Pd Pe Pf Pi Po Ps S Sc Sk Sm So Z Zl Zp Zs`) or a script of Unicode 16.0 spelled as in its `Scripts.txt` (`Latin`, `Greek`, `Old_Italic`, `Common`, `Inherited`…).
  - So, among others: `\d \D \w \W \s \S \b \B`, which mean different things in Go and Rust (write `[0-9]` or `\p{L}`), `\<` and `\>`, octal and back-references (`\0`, `\101`), `\Q…\E`, `{` or `}` outside a repeat (write `\{` and `\}`, as in `\{tmdb-[0-9]+\}`), POSIX classes, nested classes, other flags, and loose or prefixed property names (`\p{greek}`, `\p{^Lu}`, `\p{sc=Greek}`) are rejected.
  - A regex weighs at most 1000, so that what both engines build stays small. Every element that adds to the automaton weighs at least 1, for each copy that the counted repeats around it make:
    - a character or escape, a `.`, an anchor, a flag group `(?flags)` and each item of a class (a character or a range) weigh 1; a `\p` or `\P`, in a class or not, 100;
    - a capturing group weighs 1 more than what it holds; `(?:…)` and `(?flags:…)` weigh what they hold;
    - a repeat weighs its count times what it repeats (the count as above: `{n}` n, `{n,m}` m, `{n,}` n, or 1 when n is 0, and `*`, `+`, `?` 1; an upper bound of 0 gives 0), plus 1 for `*`, `+`, `?`, `{n,}` and `{n,m}` with n < m, which add branches (Rust adds one for each optional copy of `{n,m}`, but each copy already weighs at least 1); `{n}`, `{n,n}` and a lazy `?` after a repeat add nothing;
    - an alternation weighs the sum of its branches, and a sequence the sum of its pieces.

    The length and the weight are those of the text as written, in NFC form, without the `(?i)` that `ignore_case` adds.

    So `\pL{10}`, `[a-z]{1000}`, `a{0,999}`, `(a){500}` and `(?:\pL{11}){0}a` pass, while `^\pL{10}$` (1002), `\pL{11}`, `[a-z0-9]{1000}`, `a{500}b{501}`, `(?:a|\pL){10}` and `(?:^^^a){1000}` (4000) are rejected. Without the weight, the length limit and the product rule let a 1000-byte regex need gigabytes in Rust (111 copies of `\pL{1000}`), or three times Rust's size limit with anchors alone (`(?:` and 989 `^`, then `a){1000}`).
  - The Rust version sets `size_limit` to 10 MiB, the `regex` default, and `nest_limit` to 1000. Every regex within the bounds above fits them. Measured with `regex` 1.13.1, against the memory used while the automaton is built (which `size_limit` bounds):
    - the costliest regex found, `(?i)[^\x{1141}-\x{34CF}]{0,998}`, needs 4.26 MB, 0.41 of the limit;
    - a property costs at most 52 KB per 100 of weight;
    - anchors, groups and repeats cost at most about 120 bytes per unit of weight.

    Nesting never goes beyond 750 levels, as a group, a sequence and a repeat make 3 levels in 4 bytes. `regex` compiles recursively, one call per level: an optimized build needs about 0.5 MiB of stack for the deepest regexes, and an unoptimized one up to about 5 MiB, more than the 2 MiB of a default Rust thread (`cargo test` included). So the Rust version compiles regexes on a thread with at least 8 MiB of stack. With the length limit above, Go's own limits are never reached.
  - The weight bounds the size of the automaton, not the time to compile it. With `ignore_case`, both engines fold every character of each class range, so a 1000-byte regex of wide ranges, such as `[B-` followed by U+10FFFF and `]`, 125 times, can take up to about 0.7 s to compile in Rust and 0.6 s in Go when the configuration loads, against 1 ms without `ignore_case`.
- Unicode data: each version takes case folding and character properties from its regex engine (Go `unicode`, as `regexp` does; Rust `regex-syntax`), for globs and extensions too, so on valid UTF-8 a glob and the equivalent regex always agree; on other bytes, §11.3 gives them different rules. NFC comes from the normalization library, applied run by run (§11.3). The Unicode versions of these libraries differ (Go 1.27: 17.0; `regex-syntax` 0.8.5 and later: 16.0; `unicode-normalization` 0.1.25: 17.0, 0.1.26: 18.0). So the vectors (§29.4) only use characters assigned in Unicode 16.0, with expected results that no later version of these libraries' data changes.
- Go's normalization follows the UAX #15 stream-safe format: after 30 non-starters it inserts U+034F, and Rust's does not. Go counts non-starters in the compatibility decomposition (NFKD) of each character: characters with a nonzero canonical combining class, and characters of class 0 that NFC may compose with the character before them (NFC_Quick_Check = Maybe: the conjoining jamo V and T, and others such as U+09BE). So `각` (U+AC01) ends with two non-starters, and the compatibility jamo U+3133 is one. A pattern (glob, regex text or `ext` entry) with a run of more than 30 non-starters is rejected when the configuration loads, in both versions, so U+034F is never inserted into a pattern. A name cannot be rejected: on a name with such a run, the two versions may match differently (§11.3) and compute different `lpath_key` (§15.8). The vectors keep every accepted pattern and every name below the bound.

### 29.6 Code structure for the port

- Pure core packages without I/O (`config`, `rules`, `glob`, `names`, `solver` with original selection, hashing) map one-to-one to future Rust crates. Dependencies point from the shell (daemon, watch, jobs, view) to the core, never back.
- `internal/fsx` and the view interface stay thin, so the Rust version can implement them with `std::fs`, `rustix`, fuser and the winfsp crate.

### 29.7 Planned stacks

| Need | Go (phases 1–2) | Rust (phase 3 onward, to confirm) |
|---|---|---|
| SQLite | `modernc.org/sqlite`, v1.61.1 or later | `rusqlite` (bundled SQLite) |
| BLAKE3 | `github.com/zeebo/blake3`, pinned to a commit of its main branch for its arm64 backends (`docs/research/blake3.md`) | `blake3` (official) |
| TOML | `github.com/pelletier/go-toml/v2`, v2.4.3 or later | `toml`, 1.2.0 or later (TOML 1.1) |
| Regex | `regexp`, after Atoll's grammar check (§29.5) | `regex`, with `regex-syntax` 0.8.5 or later, after the same check; `size_limit` 10 MiB, `nest_limit` 1000 (§29.5) |
| Unicode normalization (NFC, NFD) | `golang.org/x/text/unicode/norm`, v0.43.0 or later | `unicode-normalization`, at the Unicode version of the Go toolchain it replaces (0.1.25 for Unicode 17.0) |
| Case folding | `unicode.SimpleFold`, as `regexp` | `regex-syntax` (`case_fold_simple`), the version `regex` uses |
| Glob | in-house | in-house, same vectors |
| CLI | standard library or cobra | `clap` |
| System calls (`statx`, `renameat2`, `openat`, fanotify, inotify) | `golang.org/x/sys/unix` | `rustix` or `nix` |
| Filesystem view | cgofuse (FUSE) | `fuser` (Linux), `winfsp` crate (Windows, GPL-3.0) |
| Local API | `net/http` | `axum` or `hyper` |
| Windows service and watcher | — | `windows-service`, `windows` crate |

## 30. Risks and open questions

- How does Plex read the media today: a mergerfs mount, or each disk's folders (§21.4)? Answered on 2026-11-12: each disk's folders, so its libraries move to the merged view before the first replica.
- mergerfs behavior with a failing branch before Atoll removes it (I/O errors or fallback to the next branch): to verify with a USB drop simulation.
- Phase 1 limits: deletions through mergerfs bypass the trash unless the link guard is on; mergerfs presents the first branch's copy, not the oldest one.
- Jellyfin and Emby: whether their scans delete items whose files are missing while a disk is absent.
- Link guard: which tools in the user's stack count hard links (torrent clients, *arr helpers). The maintainer's library has none on 2026-11-12, but the guard must handle them for users of Sonarr and Radarr.
- Plex behavior when libraries move to the merged view (metadata and watch history kept): test on a small library first.
- NTFS driver on Linux (ntfs3 or ntfs-3g): choose after power-loss and dirty-volume tests.
- Cost: two or three times the raw capacity for protected files.
- USB DAS: resets, sleep behavior, bridges; a UPS is essential.
- Single-copy windows: `immediate` mode before Atoll's own view, and SSD residency.
- Rust rewrite: cost of a second implementation and of keeping both in sync until the switch; maturity of the `winfsp` crate to check before phase 4.
- Windows arrives after the rewrite; until then, Windows PCs use the SMB share.
- Names (research in `docs/research/name-compatibility.md`), to decide before the solver, the scanner and `atoll names check`:
  - Atoll's own view (phase 2a): should a rename that a copy's filesystem refuses fail with an error, which mergerfs cannot do, instead of following §15.3?
  - scanning: a listed entry whose `lstat` fails means a name the driver cannot read, never a missing file (measured: under `ads`, a real name `a:b.mkv` is listed and `stat` gives `ENOENT`).
