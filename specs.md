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
- **Disk states**: `online`, `standby` (spun down), `missing`, `readonly`, `draining`, `failed`, `retired`.

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

- **Pragmas**, set on every connection, never in a migration: `journal_mode = WAL`, `synchronous = FULL` (a committed `ops` row survives a power cut), `foreign_keys = ON`. Atoll checks that they took effect when it opens the catalog: a filesystem that cannot do WAL fails there, not later.
- **Migrations**: `schema/migrations/NNNN_name.sql`, numbered from 1 with no gap. `PRAGMA user_version` holds the version. Each file runs in its own transaction with its `user_version`, so a failure leaves the catalog at the version before it. A catalog of a higher version than the binary is refused: going back to an older Atoll takes `catalog rebuild`. A file holds no pragma and no transaction statement.
- **`disks`** is rewritten from the configuration at each start, and the configuration wins (§10). A retired disk keeps its row, so its identifier and its history stay: a disk the configuration no longer lists becomes `retired`, placed after the others by `id`, and comes back if it is listed again.
  - A disk whose `id` now has another `uuid` is refused while it holds copies, entries, entries of the trash or `planned` operations, and so is a `uuid` that another `id` has: what the catalog knows about them would be read against another disk. `disk replace` is the way to do that. Another `root` or `workdir` is refused likewise, as the catalog's paths are relative to them.
- **`meta`**: `atoll init` writes `pool_id` and `lpath_key_version`. A catalog that does not exist, or whose `pool_id` is not the configuration's, is refused, so a mistyped path or an unmounted folder cannot start again from an empty catalog in silence.
- **Chunk hashes have their own table**, as SQLite rewrites a whole row on each `UPDATE`. Measured on 200,000 files and 20 TB with `modernc.org/sqlite`, WAL and `synchronous = FULL`: changing the state of a 100 GB file writes 780 KB to the WAL and takes 4 to 5 ms with its hashes in `files`, against 5 KB and 1.3 ms here. The catalog is about 280 MB. The chunk size is fixed (§16), so the catalog does not store it.
- **Tables are `STRICT`, and closed lists have a `CHECK`** (§29.3). Through the Go driver, a Go string written to a `BLOB` column of a table that is not strict is stored as `TEXT` with no error. A new state therefore takes a migration that rebuilds the table: about 0.7 s for `files` or `copies` at this size. `events.kind` has no `CHECK`, as each module adds its own; its list is in this specification.
- **Deleting**: a file with copies cannot be deleted, and neither can a disk with copies or entries, so a bug cannot lose that knowledge in passing. Deleting a file deletes its chunk hashes and jobs. `ops` and `trash` have no foreign key to `disks`: the journal outlives a disk.
- **`ops`**: `path` is the logical path, `path2` the location inside `<workdir>/trash/` (§17), and `file_key` the identity of the file moved, its inode in decimal. For `kind = rename`, `path` is where it sat and `path2` the target, both logical paths on that disk, and `file_key` the inode of the file or folder (§15.12). For `kind = create`, `path2` is the name of the copy in progress inside `<workdir>/tmp/`, and `file_key` its inode (§14.2). An operation is `planned` from before it starts until it is known to be finished or not to have happened (§17.2): `done` or `undone`.
- **`events.kind`**: `trash`, `restore`, `purge`, `rename`, `replicate` and `utimens`, each written with its `planned` operation, and `op-undone`, written when an operation finds that a name no longer holds what it expected (§17.1, §17.2, §15.12). `copy-gone`, for a copy that disappeared, `copy-kept`, for a file a deletion did not reach, and `modified`, for a file whose copies a modification made stale (§15.4). `guard`, `guard-confirm`, `guard-reject` and `guard-clear` (§15.6). `op-undone`, `copy-kept` and `guard` have the level `warning`, and the others `info`. `data` is a JSON object with `disk`, `path` or `path_b64` for what names a file, `op` for what comes with an operation, `reason` for `trash`, `source` for `replicate`, `mtime_ns` for `utimens`, `gone_ns` and `nothing_left` for `copy-gone`, and `up_to` for `guard-confirm` and `guard-reject`, in integer nanoseconds.
- **`copies.gone_ns`** says since when a look has found that the name no longer holds this copy, and **`gone_kind`** what the last look found (§15.12). **`copies.old_lpath`**, NULL for nearly all, says that this copy has not followed its file's rename yet, and still sits at that path on its disk; **`old_lpath_key`** is its key. So a rename waiting for a disk is one column, and its target is always `files.lpath`, resolved when it runs. Every reader of a copy's place reads `COALESCE(old_lpath, files.lpath)`.
- **`entries.file_key`** and **`btime_ns`** identify a folder, as a rename keeps both: they tell a folder renamed from files moved out one by one (§15.12).
- **`jobs`** remembers what cannot run now: `kind = trash`, with `reason` the `trash.reason` it will give and `state` `queued`, or `canceled` for a refused rename the user rejected, which keeps it from being proposed again; and `kind = rename` for a folder, `file_id` NULL, `payload` `{"from_b64", "to_b64"}`. `kind = replicate` is a copy to make, with its pending copy (§14.1); its `payload` is `{"part", "blake3"}`, the `.part` it copies into and, once verified, the whole-file hash in hex, and `job_chunks` holds the hash of each chunk copied so far. A job that ran is deleted.
- **Ids that name something on a disk never come back**: `files.id` names `Atoll/links/<file id>`, and `ops.id` names a folder in the trash (§17).
- All INTEGER times are Unix nanoseconds, UTC. A time that must match across disks is compared at the coarser precision of the filesystems involved (`time_precision_ns`), so an ext4 → NTFS replica still matches: two times are the same at a precision p when each, divided by p and rounded down, toward the past, gives the same number, which is what a filesystem that holds only multiples of p keeps of a time (`conformance/vectors/same_time.json`). Creation times are never copied, so §7 compares them exactly; and a copy's times are compared exactly with its own row, which recorded what that filesystem held.
- **`disks.time_precision_ns`** is how finely the filesystem holds a time: 1 for XFS and btrfs, 100 for NTFS, and for ext4 1, or 10^9 when its inodes are 128 bytes, which leave no room for nanoseconds (§10). Each mount check that reads the head of the device records it, as it records the disk's state; a check that does not keeps what an earlier one recorded. A disk never seen has none, and its times compare exactly, which at worst applies a time again.
- A modification or change time outside the range of a signed 64-bit count of nanoseconds, from -2^63 to 2^63-1 (before 1677-09-21 or after 2262-04-11), is stored at the nearest limit, the same way in both versions. Reading it again gives the same value, so it is never taken for a change. A creation time is absent instead (§7). Atoll sets a time from a copy's `stat`, never from the catalog, so a stored limit never reaches a disk.
- `files.lpath_key` and `entries.lpath_key` are indexed: the collision checks of §15.7 look names up by key.
- Chunk hash volume: 20 TB ÷ 4 MiB × 32 bytes ≈ 150 MB, one BLOB per file.
- Catalog location: `catalog` in the configuration, by default `/var/lib/atoll/catalog.db` (Linux), `%ProgramData%\Atoll\catalog.db` (Windows).
- Daily catalog backup on the system disk and, with `maintenance.catalog_on_disks`, in each disk's `Atoll/catalog/`, inside `maintenance.window` (§21.6).
- Rebuild (`atoll catalog rebuild`): from the manifests when present, otherwise through a full rescan with re-hashing. Since every copy sits at the same path, a rescan alone tells which copies belong together. No data is lost, but `first_seen`, recorded conflicts and corruption history are. So copies with different hashes become conflicts, whatever their sizes and modification times, as nothing is left to say which one is current. Copies with the same hash are in sync, each with its own modification time.

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
- A **confirmation** is a mount check that does not find the disk `missing`, then a read of the head past the page cache (`O_DIRECT`), 68 KiB aligned to 4 KiB, whose identifier must be the disk's. A failure makes the disk `missing`, with the reason the mount check gives for the same answer: `device-gone`, `device-unreadable`, `wrong-filesystem` or `wrong-identifier`, or `not-mounted` when the mount table no longer has an entry for the path. A session confirms each disk it reads or writes, once, before it first does so. It opens its handle on the top of the disk first, and after the confirmation that handle must have the mount id and device of the mount path, so that an unmount in between cannot pass an empty mount point as the disk. It wakes that disk, as the writes will; the disks it does not need stay asleep. Measured on a device that fails (dm-error, on ext4, XFS, ntfs3 and ntfs-3g, Linux 6.12 and 7.3): the mount check still answers from the cache, the read gives `EIO` on all four, and without it ntfs-3g accepts `mkdir`, `link` and `unlink`, and only `fsync` of the target folder says `EIO`.
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
- Optional marker `Atoll/disk.toml` (visible): `pool_id`, `disk_id`, `fs_uuid`, `format_version`. Disabled with `marker = false`.
- No automatic adoption: an unknown disk is reported, never added.
- The configuration file is the source of truth for the disk list; `atoll disk add` and `disk remove` edit it and the catalog together. An edit keeps every other byte of the file, comments included. `disk add` appends its `[[disk]]` at the end of the file, which TOML allows after `[[rule]]`. Atoll re-reads the result before replacing the file, and refuses the edit if anything else changed.

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

Until a strategy is implemented (see §28), it falls back to `most-free`, and the mode `immediate` or `scheduled` falls back to `deferred`, with a warning when the configuration loads (§24.4).

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
- Hysteresis: rebalancing starts only when the share of bytes correctly placed falls below `[balance].trigger.ratio_below`, or when the bytes to move exceed `or_more_than`.
- Status indicator: share of files at the wanted level (`atoll status`, §15.11) and, with `balance` (phase 1b), share of bytes correctly placed.
- Reachable capacity, for each k: the solver plans without running anything, moving no user copy, with every managed file wanted at k copies. The result is the bytes of the files that reach k copies in that plan, plus the formula of §13.1 applied to the free space that plan leaves, minus `min_free`, on each disk. So it accounts for data outside `media/` and for the trash, and it is what Atoll would really do; there is no second algorithm. It reads the whole catalog and wakes no disk.

### 13.4 The placement plan

This is the part of the solver that phase 1a needs: which copies to create, how to mark the others, and the state of each file. Rebalancing, the hysteresis and the reachable capacity come with `balance` and `capacity` (phase 1b). The plan moves and removes nothing, needs no clock and wakes no disk: the caller says which disks are past `health.missing_grace`, and which files wait for `atoll replicate`.

- **A copy counts** toward the file's level (`replicas` + 1) when it is:
  - `ok` or `surplus`, on a disk `online`, `standby` or `readonly`;
  - `ok`, `surplus` or `unavailable`, on a disk `missing` not yet past `missing_grace`. So nothing is re-created before then (§18);
  - `pending`, on a disk the plan can write to, and kept below. Its size comes off that disk's free space, as the disk has not seen it yet.

  No other copy counts: a `stale` or `corrupt` copy, or any copy on a disk `draining`, `failed` or `retired`. A file holding a copy that does not count has lost a copy.
- **A writable disk** has the role `data` and the state `online` or `standby`.
- **A source** is a copy `ok` or `surplus` on a disk `online`, `standby`, `readonly` or `draining` (§7). The plan reads from one on an `online` disk, then by position. A file with no source gets no job.
- **Order of the files**: the highest `priority`; then those that have lost a copy; then the smallest; then the path in byte order. Where room is short, this order protects the most files for the same bytes: at 60 % full, 98 % of files, against 68 % in the library's order and 6.5 % largest first (`docs/research/placement-order.md`).
- **Refresh, before everything else** (§15.4): in that order of files, while a file has a source and is below its level, each `stale` copy on a writable disk gets a `refresh` job on its own disk, by position. It needs the whole size free after `min_free`, as the old copy goes to the trash of that same disk. It applies in `manual` mode too. A stale copy of a file already at its level gets no job. A refresh writes a copy, so its name must pass the same checks as in creating: such a copy on a writable disk that refuses the name, or holds a colliding one, gets no job (§15.7, §15.8, §17), and a file with a source is reported as `name-incompatible` or `name-collision`, whatever its level. A pass queues none of these jobs: it sends a stale copy to the trash first, and the copy is then a pending one, with its `replicate` job (§15.4).
- **Creating**, in that order of files, for each file below its level with a source and not waiting for `atoll replicate`, one `replicate` job at a time. A disk is a candidate when it meets the hard constraints of §12.4: it is writable, it holds no copy of the file in any state, the file fits in its free space after `min_free`, its name is compatible (§15.8), collides with nothing there (§15.7) and is within the limit of its branch (§15.8), and the path is free. The path is free when no folder, symlink or ignored file has that exact path, and no parent folder of the path is a file, symlink or ignored file there. The parent folders of every copy and entry on a disk are folders there.
  - What the plan has placed so far counts everywhere: free space, names and paths. So the first name in the plan's order gets the disk (§15.7).
  - **Listed disks**: a disk is listed when it carries one of the rule's `tags`, if it has any, and is in one of its `domains`, if it has any. With neither, every disk is listed. With `tags_mode = "strict"` or `overflow = "deny"`, only a listed disk is a candidate. Otherwise any disk is, and placing a copy on one that is not listed is an overflow, reported with a warning. `overflow` concerns these lists only, so `spread = "domain"` in `prefer` mode always falls back to another domain.
  - **`spread_mode = "strict"`**: the file must cover `min(level, m)` failure domains, where m counts the domains among the disks of role `data` that are not `failed`, `retired` or `draining`, and that are listed when only listed disks are candidates. So a disk `missing` or `readonly` still counts, and a file waits for its domain rather than doubling up on another. Copies that count and are kept, and copies placed or refreshed, cover their domains; a domain outside the m does not help. When at least one domain is left to cover, and they are as many as the jobs left to create, or more, a candidate must be in a domain with no copy.
  - **The best candidate**, in this order: listed; in a domain with no copy, with `spread = "domain"`; the most free space after `min_free`; `online` before `standby`; the lowest position. Phase 2b adds `colocate` and the other `new_files` strategies at their place in the preferences of §12.4; until then, free space decides (§12.2).
  - What fits is placed, even when the level or the domains cannot be reached.
- **Copies beyond the level**: when more copies count than the level, the level's number of them is kept, and only their marks change; nothing is removed (§11.3). The order is: a copy not on a `missing` disk, so that a copy that can be read is never marked for a copy that cannot; `user` before `atoll`; the oldest `first_seen`; the lowest position; with `pending` copies last. With `spread = "domain"`, a first pass keeps only copies of domains not yet covered, and a second pass fills what is left. So a replica re-created while a disk was away gives way to the copy that comes back (§18), and a copy that spreads the file can outrank a user copy. A copy `ok` that is not kept becomes `surplus`, a copy `surplus` that is kept goes back to `ok`, and a copy `pending` that is not kept, or that does not count, is canceled. An `unavailable` copy keeps its state.
- **The states the plan gives a file**, for `ShownState` (§4): `corrupt` and `conflict` come from elsewhere.
  - `unavailable`: it has no source;
  - `unprotected`: `replicas` is 0, or it waits for `atoll replicate` and is below its level;
  - `pending`: a job is planned, or a `pending` copy is kept;
  - `degraded`: not waiting, and either the copies that count, leaving out those on a `missing` disk, plus the jobs planned, are below its level, or, with `spread_mode = "strict"`, it covers fewer domains than it must. No job fixes the second case until `balance` exists;
  - `name-incompatible`, `name-collision`: it stays below its level and a search for a disk met a disk that refused its name, past the limit of its branch included (§15.8), or that holds a colliding one; or a refresh met one (above). A disk counts here when it is writable, holds no copy of the file and is not left out by the lists, however little room it has.
- **Input**: a plan refuses what cannot be: two disks with one `id` or `position`, a negative `position`, size or free space, two files with one path, a path that is not well-formed, two copies of a file on one disk, a copy or entry on an unknown disk, `replicas` outside 0 to 2, or a value outside its closed list.
- **Output**: jobs in plan order, the refreshes first; a result for each file with a state, a mark or an overflow, by path in byte order, with its states in the order of §4 and its copies by position. A file with no result has the state `ok`, as most files do on a pool in service.

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
- A stale copy that reaches the trash becomes a pending copy, and its `replicate` job reads the copy that changed, whatever the mode and `settle` (§15.4). A modification drops every queued job of its file, as its source may be stale; a job that starts from a source no longer `ok` or `surplus` goes with its pending copy, as `changed`, and the next plan decides again.
- The daemon queues, in each pass and after the work of propagation, the copies the plan wants of every file not in `manual` mode once `settle` has passed since the file was first seen (`files.first_seen_ns`): `immediate` and `scheduled` fall back to `deferred` until they exist (§12.2). With no watcher, that is all that tells a file still being written; a source that still changes is found by the copy, which ends `changed` and waits `settle` again. It then runs, one at a time, every queued job whose `not_before_ns` has passed, highest `priority` first, then in the order the plan queued them.
- A job whose operation the reprise could not conclude stays `running`, and does not run: what is at its target may be its own copy, and nothing is decided of it until that operation is (§17.2). Its line says `io-error`.
- The outcomes of a job: `replicated`; `queued`, to run when it is due, by a run that only queues; `to-replicate`, in a dry run; `changed`, the source no longer what it was when the copy started, or not what the file's hash says; `conflict`, something took the target path since the look, which the next walk reports (§14); `path-taken`, a parent of the target that is not a folder; `name-refused`, a name the driver refuses (§15.8); `no-room`, `ENOSPC` while copying; `disk-unavailable`, its source or target not confirmed in this run; `io-error`, anything else.

### 14.2 The operation `create`

- `kind = create`, `disk_id` the target, `path` the file's logical path when the copy starts, `path2` the name of the copy in progress in `<workdir>/tmp/`, `<op id>.part`, and `file_key` its inode, written once that file is made and before anything is copied into it. It is journaled with a `replicate` event, which names the source too.
- First a look at the target, the file's path now: something at the path itself ends the job as `conflict`, and a parent that is not a folder as `path-taken` (§14.1), with nothing journaled or copied. What comes after that look is found at the rename, below.
- In order, with the crash points of the conformance build (§29.4): the operation and its event (`journaled`); `<workdir>/tmp/` made where it is missing, each folder synced, then its parent; `<op id>.part` made with mode 0600, never replacing anything, and synced, then its folder; the copy, with the chunk hashes recorded in `job_chunks` as it goes, a transaction every 64 MiB and at the end (`copied`); the source looked at again, and the copy dropped as `changed` if its size, modification time, change time or inode moved; times and attributes restored, the modification time read back, `fsync`; the re-read without cache, each chunk against its hash, which gives the whole-file hash, recorded in the job's `payload` before anything is renamed (`verified`). When the file has a hash and that is not it, the copy is dropped as `changed`. Then the target is resolved: the file's path now, each missing parent made as an operation `kind = mkdir` of its own with the mode and owner of the same folder on the source disk (§15.12, step 3); the rename without replacing, `fsync` of the file at its new name, then of the target folder (`moved`, `target-synced`), the time of the target folder restored, `fsync` of the tmp folder (`source-synced`).
- In one transaction: the operation `done`; the pending copy becomes `ok`, with what a look at its new name finds, `created_by = atoll`, `first_seen_ns` and `verified_ns` now; the file takes the whole-file hash and the chunk hashes when it had none; the source copy gets `verified_ns`, as its bytes were just read against them; and the job goes, with its chunks.
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
- Linux watcher: fanotify when available, otherwise recursive inotify.
  - A filesystem-wide fanotify mark (`FAN_MARK_FILESYSTEM`) needs `CAP_SYS_ADMIN`. Create, delete and rename events need `FAN_REPORT_FID`, which fails with `EOPNOTSUPP` on filesystems without file-handle support; ntfs-3g is to be tested (§30).
  - fanotify reports the process behind each event. Operations made by the mergerfs process are view operations: they already reached the copies mergerfs meant to change, so Atoll reconciles them without propagating. inotify gives no process, so grouping (below) handles that case.
- Windows watcher (phase 4): ReadDirectoryChangesW.
- An overflow or watcher error triggers a rescan of the affected disk: `IN_Q_OVERFLOW`, `FAN_Q_OVERFLOW`, or a zero-byte result or `ERROR_NOTIFY_ENUM_DIR` from ReadDirectoryChangesW.
- Rescans: at startup, after an overflow, and every `maintenance.rescan_every` (7 days by default) inside `maintenance.window`. Watching does not wake disks; rescans do.
- Events are grouped per logical path over `deletes.debounce` (30 s by default, longer than a USB disk's spin-up) before Atoll decides what happened.
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
- **Only metadata changed** (modification time or permissions; same content after re-hashing): the recorded metadata is updated and applied to the other copies. A `touch`, or Radarr/Sonarr's "change file date", costs one re-hash and no copy. A copy's modification time is compared with its own record (`copies.mtime_ns`), so a difference that was already there is no change, and is never applied to the others: a time its filesystem clamped (§14), or one found when a disk was added (§18). Only a change is applied, and the times are then equal again. Permissions are not applied yet (§28): no row records a copy's mode, so a change of them could not be told from a difference that was already there, and only the row of the copy that changed follows it.
- **Replacement** (a copy replaced on one disk and removed from the others within the grouping window, typically through mergerfs): a modification of the surviving copy. A pass decides the copies removed first (§15.12), then the one left.
- **Stale copies** are moved to the trash at once (`reason = overwritten`), so mergerfs never presents them, then refreshed first, whatever the mode.

**In a pass** (§15.12, step 4), once the other decisions are made, a file is looked at when a copy of it is `edited`, or `replaced` by a regular file: these are its changed copies. It waits, with no line, while:

- it is in `conflict` or `corrupt`: no version is the current one, nothing changes until that is resolved, and its changed copies count in nothing;
- a copy of it is `moved`, `gone`, `uncertain` or `own` in this look, or its row says `moved` and this look could not get to it, or it is `replaced` by anything but a regular file, or by the file that another copy of that disk was found `moved` to, an overwriting move (§15.12, Limits): what becomes of those is decided first;
- a job of it is `running` (§14.1), as what is at its target may be a copy of the version that came before, or an operation is left `planned` at or above the name of a copy of it, as only a reprise can tell what is there (§17.2);
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
- **`modified`**, in one transaction: a `modified` event; each changed copy takes what the look before the read found (inode, creation time, links, size and times, and `first_seen_ns` now when it was `replaced`), becomes `created_by = user`, and gets `verified_ns` now when it was read, and NULL otherwise; the file takes their hash and chunks, or none when nothing was read, and their size with its original in step 6; every other copy becomes `stale` and loses `verified_ns`; every queued job of the file goes, with its pending copies, as a deletion no longer stands and a source may be stale; and each stale copy that holds a file gets a trash job, `reason = overwritten`, `src_disk` the first changed copy by position, `priority` the rule's.
- **`metadata`**: when the modification time of a changed copy is no longer what its own row says, that time is applied to the other copies: of the changed copies whose time moved, the one whose change time is the latest, the first by position on a tie, as the last change. Each other copy that is not `stale`, and whose time is another, as its row says or for a changed copy as its look found it, at the coarser precision of its disk and of the disk of the copy that gives the time (§9), gets it: on a disk this pass can write to, where its look reached it, through an operation `kind = utimens` (below); otherwise it becomes `stale` and gets a trash job, as for `modified` but with `src_disk` the copy that gives the time, and its refresh brings the time once its disk is back. Then, in one transaction, each changed copy takes what the look found, as above, or what its own operation left, keeps `created_by`, and gets `verified_ns` now; a file that had no hash takes it, with its chunks, and the reference copy gets `verified_ns` too; and each copy made stale gets its trash job. An operation that ends otherwise than `applied` leaves the file as it was, for the next pass to decide again, reading it again; what an operation applied stays recorded in its row, so that it is not applied twice. With no time moved, as after a `chmod` alone, nothing is written to a disk, and only the rows of the changed copies change.
- **An operation `utimens`**: `path` the copy's place, `file_key` its inode, journaled with a `utimens` event that holds the time (`mtime_ns`). A look right before must find the copy as its row says, or for a changed copy as its look found it; otherwise the outcome is `changed`, and nothing is written. The time is set on the open file, the access time left as it is, then the file is synced and looked at, and in one transaction the operation is `done` and the row takes the modification and change times the look found, which are what the disk holds (§14). An operation that fails on a disk that cannot be relied on stays `planned`, for the reprise (§17.2). The outcomes are `applied`, `changed` and `io-error`, and `to-apply` in a dry run.
- **`conflict`**: the file becomes `conflict` (§15.7), and every version stays until that is resolved. Nothing is written to a disk. The changed copies lose `verified_ns`, as nothing says any more which version a hash describes, and no other row changes. A stale copy that changed loses its trash job.

**A stale copy** is a copy like any other until it reaches the trash: it follows a rename of its file, and goes when its file is deleted. It is never a source, never presented by Atoll's own view, and never read as the current version. Its trash job runs in step 5 of the pass, and once the copy is in the trash its row stays, as a pending copy (§14.1) with a `replicate` job from `src_disk` to its own disk and the trash job's `priority`. So the room and the name it held stay held, and the copy is made whatever the mode or `settle`, as its file has gone through both. A stale copy on a disk that cannot be written to waits for it, and mergerfs may present it until then.

**Limits**: an overwriting move and a swap of two names on one disk stay as §15.12 says, and a copy `replaced` by anything but a regular file is only reported. Permissions are not applied to the other copies yet.

### 15.5 Deletions directly on one disk

- `deletes.direct = "propagate"` (default): the other copies go to the trash, but only copies unchanged since they were last in sync (same inode, size, modification and change times), and known to have held what went (§15.12). A copy changed meanwhile is a modification and survives.
- `deletes.direct = "restore"`: the deleted copy is re-created from another copy, which comes with the copy pipeline (§14). Until then nothing propagates.
- Before treating a missing copy as deleted, Atoll looks for it (`same`, §15.1) in its own `trash/` and `tmp/` folders: if it is there, it was an interrupted Atoll operation, not a user deletion.

### 15.6 Safety guards

- Mass-deletion guard, for each disk: if more than `deletes.mass_guard.files` copies, or more than `percent` of a disk's copies, disappear within `window`, propagation pauses and an alert asks for confirmation (`atoll guard confirm` or `reject`). Copies that a refused rename would send to the trash count too (§15.3).
  - The count on a disk is the sum of its copies with `gone_kind = gone` and no `old_lpath`, of a file with no copy `moved` on a disk not `missing` or `retired`, later than the last confirm or reject, however old: a pass after a month's gap sees them all; its `copy-gone` events within `window`, whose `gone_ns` is later than that; and its copies that a refused rename would send to the trash (§15.12), with no job yet. A `moved` left from before a disk went away says nothing now, as the decision will be a deletion.
  - A copy of a file with nothing left to send to the trash counts toward `files` and not toward `percent`: nothing can be stopped, so a small pool does not ask at each deletion through mergerfs, and a mass disappearance, a damaged filesystem or a wrong `mv`, still holds. A copy on a `missing` or `retired` disk is something left, whatever its row last said.
  - It trips when the count is more than `files`, or when `left * 100 > percent * rows`, compared exactly (§29.5), `left` being the count without the copies of a file with nothing left to send, `rows` being the copies on that disk when the pass began, before its walk adds any, with those events, as they were its copies too. While a `guard` event stands, `rows` are its copies first seen before that event instead, with those events: what lands on the disk since must not dilute a hold that nobody has answered. So below 20 copies at 5%, a direct deletion asks. It is worked out once, at the start of the decisions.
  - Tripped: nothing `gone` on that disk later than the last confirm or reject is decided, so its rows stay, and if the copies come back nothing was lost, history included. Trash jobs with that `src_disk` wait. Renames go on, but for a folder renamed on another disk with such a copy below it, which waits for the answer (§15.12). A `guard` event at level `warning` when there is none since the last confirm, reject or clear on that disk. As this is worked out from rows and events, no crash can lose a hold.
  - Found no longer tripped with a `guard` event standing, as copies came back or what it counted left the window: a `guard-clear` event, as the hold ended by itself and the next trip is another one, with a time of its own for a confirm to go by.
  - `atoll guard list` reads the catalog only: for each disk by position, whether its guard is tripped, as a `guard` event that stands says or else as rows and events say now; the count; and when tripped the paths held, in byte order, and none otherwise.
  - `atoll guard confirm --disk <id>`: an event with `up_to`, the time of that last `guard` event, in integer nanoseconds, so that what appeared since then is still asked about. The next pass decides those rows. A copy that a refused rename would send has no time to compare with that, so it gets its queued job in the same transaction. A `gone` row the guard left out, beside a copy `moved`, was not asked about: its clock starts again, so that the guard asks about it should that copy stop reading so. `atoll guard reject --disk <id>`: in one transaction, those `gone` rows go, with `old_lpath` or not, with any left out beside a copy `moved`, as no deletion must come of one nobody asked about, a file left with no copy goes, what is left is as after a decision that dropped them (§15.12), for its hash, chunks and conflict, its queued jobs for a deletion go, a copy that a refused rename would send becomes a `canceled` job, with or without a job already, with the same `up_to`. With no `guard` event since the last confirm, reject or clear, either does nothing. Both write nothing to a disk, so they take no `--yes`, and they take the `flock`.
- An absence is interpreted only after the disk's identity has been confirmed in the same scan (§10).
- Every propagated deletion goes to the trash; nothing is deleted immediately.
- In phase 1, the guard can only stop propagation: deletions made through mergerfs have already reached every copy.

### 15.7 Conflicts and name collisions

- Same logical path with different content on two disks, at adoption, after concurrent changes or after `catalog rebuild` (§9): `conflict` state, every version kept.
- Atoll's own view keeps presenting the copy it presented before; for a conflict found when adding a disk, the copy already in the pool. With mergerfs, the first branch's copy is shown.
- Resolution: `atoll conflicts resolve <path> --keep <disk>`, with confirmation; the other versions go to the trash.
- Paths with the same `lpath_key` (§15.8), which differ only by NFC/NFD form, by case as NTFS and Samba compare it, or by both, are reported as `name-collision`: the merged view shows both, and Windows or macOS clients may treat them as one name. Folders count too: `Films/a.mkv` and `films/b.mkv` are reported, because `Films` and `films` have the same key. Nothing is changed automatically.
- Atoll never writes a colliding name to an NTFS disk. This is a hard constraint of the solver (§12.4), checked on NTFS targets only: ext4, XFS and btrfs hold both names, and the variants that would not are refused (§10). Atoll checks it itself, as a pure function of the names, and never relies on the driver:
  - a path collides with a name on the target when, at some component, their prefixes up to that component have the same `lpath_key` and different bytes. So `film.mkv` collides with `Film.mkv`, `films/b.mkv` with the folder `Films`, and `films/a.mkv` with a file named `Films`;
  - the names on the target are its files, folders and symlinks (`copies`, `entries`) and those planned earlier in the same plan, so the first name in the plan's order gets the disk;
  - a path never collides with itself, and a case-only rename does not collide with its own old name;
  - a name without a key, which is not valid UTF-8, never collides; it cannot go to NTFS anyway (§15.8);
  - `lpath_key` is wider than NTFS: the check also refuses an NFC/NFD pair that NTFS could hold, and `ς` against `σ`. Such pairs are rare and are reported anyway.
- A colliding copy goes to another disk. Failing that, the file keeps fewer replicas than wanted, is reported as `name-collision`, and an alert is raised. So a folder `films/` against `Films/` blocks everything below it on that disk until the user renames one of them. Never an automatic rename. A rename to a colliding name follows §15.3.
- Names already side by side on an NTFS disk, created outside Atoll, are only reported: Atoll does not touch them. mergerfs can create them too: on ntfs-3g, a rename to `OTHER.MKV` beside `Other.mkv` succeeds (measured).

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
  - A folder on another mount is recorded as a folder, not entered, and reported in `other_mounts`.
  - A listed name that `lstat`, `openat` or `readlink` answers with `ENOENT`, `ELOOP` or `ENOTDIR` is reported in `unreadable`. It gets no new row, and the entries it and what is below it had stay, for that walk: a name the driver cannot read looks the same as one removed or replaced during the walk, which says nothing about the disk, and a folder renamed since it was listed only says so by its row.
  - Any other error stops the walk. The disk becomes `missing` with the reason `io-error`, nothing that walk found is written and nothing is concluded from it.
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
  - its row does not change, but for `gone_ns` and `gone_kind`, which follow the verdict. Propagation (§15.12) is what acts on them;
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
- **Files at their level**: a file is at its level when its copies `ok` or `surplus` on a disk `online`, `standby` or `readonly` reach `replicas` + 1. A copy on a `missing` disk does not count, even before `health.missing_grace`, unlike in the plan (§13.4): this counts what can be read now. A file in `conflict` counts by its copies like any other, and `by_state` says how many there are. The share of bytes correctly placed (§13.3) comes with `balance`.
- **Counts** are integers, with no share in the JSON, so that no rounding can differ between two versions: `files_total` and `files_at_level`, `bytes_total` and `bytes_at_level` by the size of each file, and `by_state`, the state the last scan gave each file, with every state of §4, at 0 when no file has it.
- **`--path <path>`** keeps the file at that path and everything below it, compared by whole components, in the counts and in `files`, by path in byte order. A path the catalog does not hold gives none, and exit code 0. Without `--path`, `files` is empty. Each file comes with its copies by position, each with `old_path`, or `old_path_b64`, when it has not followed a rename yet, and `original` is the disk of its original, the last one known when no copy can be read now (§7).
- **Output**: `api/schemas/status.schema.json`.
- **Exit code**: 0 whatever it reports; 1 when it cannot read the configuration or the catalog; 2 on a usage error, which takes in a path that is not well-formed (§4). Files are written as they are read, as a large folder would not fit in memory (§26). So a catalog that fails once they are on their way leaves an unfinished output, and a script goes by the exit code. Otherwise a failure writes nothing to standard output.

### 15.12 The propagation pass

`atoll daemon --until-idle [--dry-run]` does all the work that is due, once, and exits, so a timer can run it until the watcher exists. It writes under the same rules as every command, and takes no `--yes`: starting a daemon is the consent, and the guard (§15.6) is the safety. A copy `edited` or `replaced` is a modification (§15.4); of a metadata-only change, the modification time is applied to the other copies, and the permissions are not yet.

**The steps:**

1. An exclusive `flock` on the catalog, as in §25.
2. A mount check of every disk, recorded. The reprise of §17.2 on each writable disk, which confirms it.
3. The walk of each disk not `missing`, and a verdict for each copy on it. A disk with a copy `moved`, `gone` or `uncertain` is confirmed before anything is concluded from it, `readonly` included (§10, §15.6): one that fails is `missing`, and its copies `unknown`. What is new is written in one transaction per disk, as in §15.10, and `gone_ns` and `gone_kind` follow the verdicts.
4. The guard is worked out, once, for each disk. Decisions, for the files that are due, by path as they stood when this step began, in byte order, at most 1000 files to a transaction, and a page takes in any file that holds a target of one of its files, as it may be leaving it. Then the modifications (§15.4), by path in byte order, one file to a transaction.
5. What is pending on each writable disk, disks by position: folder renames by their old path, one after another if its new name lands at or below that one's old name or below that one's new name, or if that one's old name is below its own and that one's new name is neither at or below its own old name nor below its own new name; the first of those in a cycle goes, its copies following one by one; then renames by old path, again and again until a round moves nothing, as one may free another's target; then trash jobs by id.
6. The original, policy and state of each file, as after a scan (§15.10).

**A verdict** says what a look found of one copy, at the place where it sits: `COALESCE(old_lpath, files.lpath)`. The first that applies:

| Verdict | When |
|---|---|
| `unknown` | its disk was not walked; or the place is below a name in `unreadable` or `other_mounts`, or below a folder found renamed, at what is now such a name; or, for a pass, at or below a name of an operation left planned on that disk, which only a reprise can tell of. What that name holds gets no row |
| `there` | the name holds the file, as §15.10 compares a known copy |
| `edited` | the name holds the `same` file (§15.1) but not as §15.10 compares it, or the same inode where either has no creation time |
| `moved` | the name does not hold it, exactly one candidate is `same`, none is doubtful, as below, and no other row has that candidate as its target: that candidate's name is the target |
| `uncertain` | the name does not hold it, and either several candidates are `same`, or a candidate is doubtful: the same inode and another size or modification time, where either has no creation time, or two rows have one candidate as their target, as hard links do. It stays so while the name is absent: those candidates get rows of their own, and say no more than at the first look |
| `replaced` | the name holds another file, or anything that is no regular file: a symbolic link, a folder, a FIFO; or a folder on the way is now a symbolic link, which the walk does not enter |
| `own` | the name is absent, and the `same` file is in that disk's `<workdir>/trash/` or `<workdir>/tmp/` (§15.5, invariant 11). A pass looks; `atoll scan` reads this as `gone` |
| `gone` | the name is absent |

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

A disk away keeps `old_lpath`. Back, its walk finds the copy where it sits, so it can never be read as a user renaming it back. A rename back meanwhile clears `old_lpath` with no disk operation. A new file at the old name is a file of its own, and a file at the target on that disk is reported in `path_taken`, with no row (§15.10).

**Folders.** A folder row whose folder a walk no longer finds at D, where a folder with no row is `same` at E with a creation time on both sides, is a folder renamed to E: its row stays, and E gets none, until the files below it are decided (§15.10 says of another folder made at D, and of E below D). That is what tells a folder rename from files moved out one by one, where the folder keeps its inode. As the rows stand before any decision of the pass, the rename is real if a copy of that disk sits below E with no clock running, or sat below D and was found moved to below E. Otherwise it says nothing of other disks, as an empty folder renamed does not, and a copy moved in from elsewhere changes nothing of that: its entries just follow. A copy moved in from elsewhere and decided in an earlier pass, while the folder waited for another, does read as one that came with it, as no row says where it came from, and one decided while the folder waited and moved on before the folder is decided reads as one from elsewhere, so only what moved follows. All this is acted on once no copy of that disk still sits below D, the copies that moved being decided. The folders a disk found renamed are taken in the order of step 5, those in a cycle last: one that lands in another's old name came after that one left, and must not read as having been in it. One with a folder renamed inside it that waits waits too, as its entries would carry that one's row away. One that lands at or below the old name of one that waits does not wait for it, and its entries take that name: the next look reads the one that waited as a folder made where it now is, so what only other disks hold below its old name stays there, in the folder that now has that name. That is a limit, with nothing lost: holding back what lands there would need that wait known before the files of the pass are decided. Nothing says in what order two disks acted: a copy a decision of this pass put below D, on another disk, is below D like any other, so its disk gets the job, and the next look finds that copy `moved`. A copy no pass can take from below D holds nothing back: one `replaced`, as nothing decides that, one `moved` to a name another file holds, or one this pass decided in from elsewhere, which never sat in the folder renamed. One decided in by an earlier pass, or that Atoll's own rename brought to a D it made, reads at the next look as a file at home there, so that no folder went with all it held, and only what moved follows. A real rename waits while a file below D with a copy on other disks only has a copy `moved`, `gone`, `uncertain` or `own` not yet due, a debounce at most, as what becomes of that copy is decided at the path it has, or a copy the guard holds, on a disk looked at or not, until it is answered, as following would take that copy out of the guard's count and end the hold with nobody answering, or a copy a look cannot get to, as under Due, for as long as that lasts. Until then no such file follows, as what goes alone its one operation can no longer carry. A copy decided already, for what it did, does not wait: it goes alone, as any copy behind does, and the job then finds E there and goes, so the rest follow one by one and the old folder stays. In this order, so that a crash lets the next pass go on from where it was:

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

**Limits:** an overwriting move (`mv P Q` with Q there, on one disk) is reported as `path-taken` and changes nothing, and a swap of two names directly on one disk is followed by the rows and reported as `path-taken` on the others, where it will cost a re-copy (an exception to R-10). With `deletes.direct = "restore"`, nothing propagates, and the file lacks a replica.

**Output**: `api/schemas/daemon.schema.json`. `decisions` lists each file decided, in the order decided: by old path in byte order within a page of at most 1000 files due, a file that holds a target of one of those being taken into that page and listed in that order, and what follows a folder after those, then the files a modification was decided for, by path in byte order, `modified`, `metadata` or `conflict` (§15.4). A copy that went its own way has a line of its own, `renamed` to its other target or `path-taken` for it, and a file `deleted` that then follows its folder has a `renamed` line too. `operations` lists what ran: first the operations `utimens` of the modifications, in the order they ran, as they run with their decisions; then, disk by disk by position: folder renames, renames in the rounds they ran, then trash jobs, with their `reason`; then the copies, in the order they ran (§14.1), each with its target as `disk` and its `source`, and the outcomes of §14.1. With `--dry-run` it lists what would run, in the same order: it looks at each disk as a real pass does, and says `to-rename`, `to-trash`, `to-apply` and `to-replicate` where those say `renamed`, `trashed`, `applied` and `replicated`, a copy queued already or one the plan would queue, and the same otherwise, naming a copy where it sits now. It reads what a real pass would read to decide a modification, and lists the trash jobs that would follow. It decides on the rows as they stand: a copy its own walk found new has no row until a pass or a scan records it, so until then that copy neither stops a deletion nor takes a name. It is a preview: it applies nothing, so what one decision or operation of the pass does to the next is worked out only as far as a folder's operation carrying what it carries, one rename freeing another's name on that disk, and a decision or a job dropping a job. So it does not work out: a collision on NTFS that a rename of the same pass frees or makes; a folder renamed inside another renamed in the same pass, which it lists as two operations; a folder's operation for a disk whose only copies below it a decision takes away; two files its decisions give one target across its pages, or a file that follows a folder onto a name a file's decision took, which it says `renamed` of where a real pass says `path-taken`; a name a decision frees, or a copy a decision leaves behind, for a file that follows a folder or for a folder's operation, which it says `path-taken` of, or lists no folder for; a copy behind that is deleted and then follows its folder, for which it also lists a rename no real pass runs; the clocks its look would write, for the guard of a disk away, which reads the rows of that file's other copies as they stood before the walks, for whether a copy still sits below a folder renamed, and for whether a name a refused copy wants is on its way out; a trash job that depends on an earlier disk's work, as a good copy arriving by a rename, a copy found `changed`, or a conflict a refused rename sets; the model of a missing parent, which a real pass only looks for when the operation runs; the place in the list of a file with a queued job that is decided again; what a reprise finishes with no line of its own, for which it has one; a modification of a file a decision renames, which it lists under the path the file had, or keeps, which it does not decide, as its rows still hold what went; and the copy a stale copy is given once it is in the trash, which only the next pass lists. It writes nothing to a disk, to the journal or to the catalog but for what the mount check of step 2 records. `waiting` counts the copies not yet due, the changed copies not yet due (§15.4), and the files whose copy waits for `settle` (§14.1), and `next_due` says when the first of either is, or `null`, so that a timer knows when to run the next pass. `guards` lists the disks whose guard is tripped. These are written as they come, as a rename of a whole pool would not fit in memory (§26): every decision before the first operation, and `guards`, `waiting` and `next_due` at the end. So a failure once they are on their way leaves an unfinished output, and a script goes by the exit code.

**Exit code**: 4 when a disk was skipped or failed, an operation stayed planned or ended in `io-error`, or something due could not run for want of a writable disk the configuration lists; what waits for a disk it no longer lists has no line either, until `atoll disk remove` says what becomes of it; otherwise 3 while a guard holds anything; otherwise 0, with work possibly waiting for its debounce. With `--dry-run`, 0 once it has answered. 1 when the configuration or the catalog cannot be used, and 2 on a usage error. `atoll daemon` without `--until-idle` is a usage error until the loop exists.

**`--dry-run`** writes nothing to a disk or to the catalog except what a mount check records, as `prune` does, resolves nothing and confirms nothing. It answers as if every difference it sees were due.

## 16. Integrity

- Hashes:
  - each 4 MiB chunk: a standalone BLAKE3-256 hash (the last chunk may be shorter). A file has as many chunks as it takes to hold its bytes: none when it is empty, and no empty chunk after a size that is an exact multiple;
  - whole file: standard BLAKE3-256, b3sum-compatible.
  When indexing an existing file, one read feeds both. When copying, chunk hashes come from the source read and the whole-file hash from the verification pass (§14).
- Scrub: every `scrub_every` (30 days by default), inside `[integrity].window`, within a nightly budget, one disk at a time to limit wake-ups, resumable at chunk level.
- Decision for a copy whose hash differs from the recorded version:
  - size, modification time and change time unchanged (compared at the coarser precision of the filesystems involved), and the mismatch confirmed by a second read without cache: `corrupt`;
  - otherwise: modification (§15.4). An edit by a tool that keeps the modification time still moves the change time, so it is never "repaired" away. A rename moves it too, which is why a copy renamed by anyone but Atoll loses `verified_ns` (§15.12): read again, a mismatch there is a `conflict`.
- Repair (`auto_repair`):
  - another copy is re-verified first; the healthy copy is never written to;
  - `repair_mode = "chunk"`: the chunks about to be overwritten are saved to the trash, then rewritten in place; the copy keeps its creation time. Files with several hard links are always repaired this way, so every path stays consistent;
  - `repair_mode = "file"`: the faulty copy goes to the trash and a fresh copy replaces it, with a new creation time (§7);
  - afterwards the modification time is restored and the new change time recorded.
- Quarantine: when automatic repair is off or fails, a corrupt copy is moved to the trash whenever a healthy copy exists, so the view never serves it; the file is then `degraded` until a replica is re-created.
- x1 file: detection and alert only.
- Duplicates: `atoll dupes` groups files with identical content (size, then hash) under different logical paths. Read-only report.
- Limit: two versions of the same movie (1080p and 4K) have different hashes. Grouping by metadata (title, year, duration) is planned for phase 5.
- Optional per-disk manifest (`Atoll/manifest.b3sum`), checkable without Atoll by running `b3sum -c` from the disk root.

## 17. Trash

- Location: `Atoll/trash/<YYYY-MM-DD>/<op-id>/<logical path>` on each disk (UTC date; operation id from `ops`). Sending a copy to the trash is a rename within the same filesystem. Paths never collide and names are never changed.
- Chunks a repair overwrites: `Atoll/trash/<YYYY-MM-DD>/<op-id>.chunks/<logical path>/<index>`. The logical path ends in a folder with the file's own name, holding one file per chunk. `<index>` is the chunk's number in decimal, from 0, with at least 8 digits (`00000012`). The suffix sits on the operation's folder, so a file name of up to 255 bytes still fits.
- What goes in:
  - copies removed by a propagated deletion (`deleted`);
  - stale copies before their refresh (`overwritten`);
  - corrupt copies set aside (`quarantined`), and the chunks a repair overwrites (`repaired`);
  - surplus replicas removed by `prune` (`surplus`) and replicas removed by rebalancing (`rebalance`);
  - copies left at the old name by a rename their disk refuses (`renamed`, §15.3);
  - every copy of a file deleted through Atoll's own view (phase 2a).
- Retention `[trash].retention` (30 days by default). `max_size` is a share of each disk's capacity; when it is reached, purging goes in this order: surplus and rebalance, then overwritten, repaired and quarantined, then deleted and renamed files. Deleted and renamed files are never purged before their retention ends, since a renamed copy may be the file's only other copy: Atoll raises an alert instead.
- Commands: `atoll trash list`, `restore <path> [--to <disk>]`, `purge [--older-than <duration>]`.
- Link guard (optional, `trash.link_guard = true`): Atoll keeps one hard link per managed copy in `Atoll/links/<file id>`. A deletion or replacement through mergerfs then leaves the data in place, and Atoll moves it to the trash; this closes the phase-1 gap of §15.2. Cost: every managed file shows two links, which misleads tools that count links (some torrent and *arr helpers). Off by default.
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
- An operation `utimens` (§15.4) looks at the copy at its place, after the file there is synced. The same inode and size with the time its event holds, at the precision of that disk (§9), as NTFS keeps only multiples of 100 ns of it: it finishes, and the row takes the modification and change times there; with the time its row says, it is `undone`, as nothing happened; anything else, it is `undone`, with an `op-undone` event, as the copy changed since, and the next look says how.
- A folder made below the root (`kind = mkdir`, §17.4): a folder there, the operation finishes; nothing there, it is `undone`; anything else, it is `undone`, with an event.
- A `rename` that finishes here finds the copy by disk, inode and where its row says it sits, a row with no `old_lpath` being at its file's path, as a rename back clears it. It gives the copy `old_lpath` NULL when `path2` is the file's path now, and `path2` otherwise, so that a rename the user made meanwhile still reaches it: the next run renames it from there. No row is ever said to be at a name it is not at. It loses `verified_ns`: what it finds records the change time, which hides any edit made before the move, so read again a mismatch is a `conflict`. A folder gives each copy behind below it `old_lpath` as above, and likewise a copy at its own file's path below it with another copy there, which a rename back left so, also when its clock says `moved`, as a scan between the crash and the reprise reads that very move, and its job goes. Those copies keep `verified_ns`: the change time of a file below a renamed folder does not move, and none is recorded, so an edit made before the move still reads as `edited`.
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
  - `--to` comes with the copy pipeline (§14), as another disk takes a copy.
- A restored copy is a user copy, `ok` and unverified, with its `first_seen` at that time, and otherwise follows §15.10 for a copy not yet in the catalog: another size makes the file a `conflict`. A reprise that finds that path held by a file whose copy on that disk still sits elsewhere writes no row, as a disk holds one copy of a file, and the next walk reports it in `path_taken`. A row left at that path, of a file now at another, goes, with its file if that was its last copy. The original, policy and state are then worked out again. When other names lead to that inode, their rows take its link count, and its change time if they recorded the one it had just before the move, as in §17.1, step 6.
- `atoll trash purge [--older-than <duration>] [--dry-run] [--yes]` deletes what has reached `expires_ns`, or with `--older-than` what was trashed at least that long ago, whatever its reason. One operation for each entry, `kind = unlink`, with `file_key` that of the entry's `trash` operation. A file there that is not that one stays, with its entry: `changed`, with an `op-undone` event. An entry on a disk neither `online` nor `standby` stays: `disk-unavailable`. Without `--yes` it does not look at the file, so it can say `to-purge` of what then gives `changed`. When other names lead to that inode, their rows take its link count, and its change time if they recorded the one it had just before the `unlink`, as in §17.1, step 6. The first name by path in byte order that still leads to it says what those are. Output: `api/schemas/trash-purge.schema.json`.

## 18. Disk lifecycle

| Operation | Command | Effect |
|---|---|---|
| Add a disk, empty or full | `atoll disk add /mnt/u2 --domain d4-320 --tags usb [--dry-run]` | checks identity, topology and mount point; adds the disk to the configuration; indexes `media/` in place and hashes inside `maintenance.window`, moving nothing; files with the same content at the same path become existing copies, each keeping its own modification time, and nothing is written to them; others follow §15.7; appends the branch at the end of the running mergerfs |
| Remove | `atoll disk remove u2 --evacuate [--dry-run]` | `draining`: user and Atoll copies re-created elsewhere, then `retired` |
| Missing disk | automatic | detected within 30 s (`missing`); its mergerfs branch is removed at runtime; its files stay visible through their replicas; x1 files become `unavailable`; alert. After `health.missing_grace` (24 h), the missing replicas are re-created elsewhere when space allows |
| Declare failed | `atoll disk fail u2 --yes` | re-creates the missing replicas of degraded files at once |
| Replace | `atoll disk replace u2 --with /mnt/u3` | rebuilds onto the new disk |
| Disk comes back | automatic | identity check and reconciliation first (copies deleted, renamed or modified meanwhile are updated), then its branch is added back; replicas re-created during its absence become `surplus`; in-sync copies may become originals again (§7) |

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

- `atoll mergerfs-config` prints the configuration: an fstab line, or the matching settings for the openmediavault mergerfs plugin. Atoll never edits `/etc/fstab` itself.
  - branches: the `media/` folders of the data disks, in configuration order; new disks are appended at the end, so the copy `ff` presents does not change; cache disks are left out until phase 2b;
  - `category.search=ff`;
  - `category.action=epall`, so renames and deletions reach every copy, except where a branch refuses a rename (§15.2);
  - `category.create` from `placement.new_files`: `mfs` for `most-free`, `ff` with branches in fill order for `ordered`, `pfrd` for `percent`; Atoll then replicates;
  - a per-branch `minfreespace` taken from `placement.min_free`, larger than the largest expected file;
  - `moveonenospc=false`, and `rename-exdev` and `link-exdev` pinned to `passthrough`, so mergerfs never moves data or creates links on its own;
  - short attribute and entry caches only (a few seconds): Atoll changes branches out of band, and mergerfs advises against caching in that case.
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
- While a disk is missing, Atoll sends no scan notification for the affected folders and raises an alert. How Jellyfin and Emby treat missing files during their own scans is to be checked (§30).

### 21.5 Notifications

- Partial scan of the changed folder:
  - Plex: `GET /library/sections/<id>/refresh?path=<folder>`;
  - Emby and Jellyfin: `POST /Library/Media/Updated`.
- `path_map` translates host paths into the paths the server sees (Docker container). The longest key that is a prefix of the path, by whole components, applies.
- Batching: one notification per folder per 30-second window.
- A media server's automatic scan sees changes made through the mount on the same host; Atoll notifies the others (changes made on member disks, propagated renames and deletions).
- The media server's periodic scan can then be disabled.

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
- §7 reads the creation time where a driver exposes it, and uses `first_seen` elsewhere. Measured per driver in `docs/research/file-times.md`.

## 23. Monitoring and alerts

- SMART through `smartctl -j -n standby` (with `-d sat` behind USB bridges): temperatures, reallocated and pending sectors, power state; sleeping disks are skipped.
- Sensors (lm-sensors): CPU, fans.
- Alerts through ntfy, Gotify or SMTP on:
  - missing disk, degrading SMART, NTFS volume marked dirty;
  - `corrupt`, `conflict`, `unavailable`, `name-incompatible` or `name-collision` file;
  - mass-deletion guard triggered;
  - trash full before deleted or renamed files reach their retention;
  - files waiting in `manual` mode for too long;
  - pool more than 90 % full.
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
ntfy   = "https://ntfy.sh/my-secret-topic"
gotify = ""
smtp   = ""

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
- **Keys with no value when left out**: `strategy`, `health.avoid_hotter_than`, and a disk's `domain`.
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
| `defaults.mode`, a rule's `mode` | `immediate`, `scheduled` | 1b | works as `deferred` |
| `placement.spread_mode`, `placement.tags_mode`, and a rule's | `strict` | 1b | no effect |
| `placement.overflow`, a rule's `overflow` | `deny` | 1b | no effect |
| a rule's `tags`, `domains` | any | 1b | no effect |
| `integrity.auto_repair` | `true` | 1b | scrubs only detect |
| `integrity.repair_mode` | any | 1b | scrubs only detect |
| `trash.link_guard` | `true` | 1b | no effect |
| `balance.schedule` | anything but `never` | 1b | no effect |
| `notify.plex.url`, `notify.jellyfin.url`, `notify.emby.url`, `alerts.gotify`, `alerts.smtp` | anything but `""` | 1b | no effect |
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
atoll capacity
atoll replicate <path> [--now | --force] [--dry-run]
atoll rules check | explain <path> [--size <size>]
atoll scrub [--disk <id>] [--path <path>] [--budget <duration>]
atoll repair <path> [--mode chunk|file]
atoll dupes [--path <path>] [--min-size <size>]
atoll trash list | restore <path> [--dry-run] [--yes] | purge [--older-than <duration>] [--dry-run] [--yes]
atoll conflicts list | resolve <path> --keep <disk>
atoll names check [--path <path>]        read-only: names not in NFC, not valid UTF-8, incompatible with a filesystem, or colliding, and paths past a length limit (§15.9)
atoll guard list | confirm --disk <id> | reject --disk <id>
atoll prune [--dry-run] [--yes]          surplus copies to the trash (§17.3)
atoll balance [--dry-run] [--move-originals]
atoll notify test
atoll catalog backup | verify | rebuild  verify: SQLite integrity check, then catalog vs disks (read-only)
atoll mergerfs-config                    (phase 1 and break glass)
atoll mount <point>                      (phase 2a on Linux, phase 4 on Windows)
atoll daemon [--foreground] [--until-idle] [--dry-run]
```

- `--config <path>` or `--config=<path>` comes before the command, and nowhere else, and names the configuration file. By default it is `/etc/atoll/atoll.toml`, and on Windows `%ProgramData%\Atoll\atoll.toml`.
- Every command that moves or deletes accepts `--dry-run` and writes nothing without `--yes`. Without `--yes` it asks no question: it shows what it would do and exits with 3, or as below if there is nothing to do. So both versions behave alike, and a scenario can judge it. `--dry-run` with `--yes` is a usage error. The daemon takes no `--yes`, as starting it is the consent (§15.12). `daemon --dry-run` plans jobs without running them (shadow mode, §29.2).
- Every command accepts `--json`; outputs follow the schemas in `api/schemas/`, and exit codes follow §29.3.
- A command's flags come after its whole name, so `atoll disk --json list` is a usage error, and so is a flag given twice. They may come before or after a path, and `--` ends them, for a path that starts with `-`. A command line is checked before the configuration is read, so a usage error comes first. It writes nothing to standard output, with `--json` or without.
- `atoll init` creates what is missing, and may run again. The configuration is written with `--pool-id`, `--view-mount` and `--catalog`, and its folder made, only when the file does not exist, and needs the first two; with a file already there they are refused, with exit code 1. Then the catalog is created, with its folder, if it does not exist; a catalog of another `pool_id` is refused. It reads no disk. It exits with 0 once both exist; with 1 when the configuration cannot be written, read or loaded, or the catalog cannot be used; and with 2 on a usage error, which takes in a value the configuration would refuse, one of `--pool-id` and `--view-mount` without the other, and `--catalog` without them.
- `atoll scan` follows §15.10, and `--disk <id>` limits the walk to one disk, without changing what is checked. It exits with 0 when every disk asked for was walked, and hashed with `--full`; with 4 when one was skipped or failed; with 1, and nothing on standard output, when the configuration or the catalog cannot be used, or `<id>` is no disk of the configuration; and with 2 on a usage error.
- `atoll status` follows §15.11. It opens the catalog and looks at no disk.
- `atoll scan`, `atoll prune`, `atoll replicate`, `atoll trash restore`, `atoll trash purge`, `atoll daemon --until-idle`, `atoll guard confirm` and `atoll guard reject` hold an exclusive `flock` on the catalog file for their whole run, `--dry-run` included, so that a reprise never takes another command's operation for a crash, and a scan never records a move half done. A second one exits with 1 at once. The lock creates no file, and the kernel lets go of it when the process ends, however it ends. `atoll status`, `atoll trash list`, `atoll guard list` and `atoll disk list` take none.
- `atoll prune`, `atoll trash restore` and `atoll trash purge` follow §17.3 and §17.4. They exit with 0 when everything asked for was done, or with `--dry-run` once they have answered; with 3 when they lacked `--yes` and there was something to do; with 4 when something stayed or a disk could not be relied on; with 1 when the configuration or the catalog cannot be used; and with 2 on a usage error. `atoll trash list` exits with 0, 1 or 2 likewise.
- `atoll replicate` follows §14.3.
- `atoll daemon --until-idle` follows §15.12, and `atoll guard` §15.6. `atoll guard` exits with 0 once it has answered or done its work, also when there was nothing to confirm or reject; with 1 when the configuration or the catalog cannot be used, or `<id>` is no disk of the configuration; and with 2 on a usage error. Output: `api/schemas/guard-list.schema.json` and `guard-answer.schema.json`.
- `atoll disk list` runs a mount check (§10) on each disk of the configuration, in its order, and shows the state and the reason. It reads the configuration, the mount table and the disks, opens no catalog and writes nothing. So the states it shows for now are `online`, `readonly` and `missing`: `standby` comes with SMART, and `draining`, `failed` and `retired` with the catalog. It exits with 0 whatever the states; with 1 when the configuration does not load or cannot be read, with nothing on standard output; and with 2 on a usage error.
- `atoll rules check` exits with 0 when the configuration loads, warnings or not, and with 1 when it does not load or cannot be read, with the same output. So a script can run it before a restart.
- `atoll rules explain` exits with 0 once it has answered, whatever the answer; with 1 when the configuration does not load or cannot be read, with nothing on standard output; and with 2 on a size or a path that is not well-formed. `<size>` is a size of §29.5, so it needs its unit.

## 26. Non-functional requirements

- Resources (ZimaBoard 2 or M920q): under 200 MB of memory for 200,000 files; under 5 % CPU outside scrubs. The binary sets a soft memory limit of 170 MiB unless `GOMEMLIMIT` says otherwise. A first scan of 200,000 files with two copies each peaks at about 180 MiB, in 12 s on an idle machine, and a later one at about 155 MiB, in 5 s (measured on empty files in a container, so it says nothing about the time real disks take). `atoll status` peaks at about 21 MiB on the same pool, with `--path` over all of it too, as it writes files as it reads them. `atoll prune --yes` takes about 8 ms per copy and `atoll trash purge --yes` about 4 ms, nearly all of it in commits and `fsync` (measured on 20,000 files in a container), and they peak at about 40 MiB there. On the same 200,000 files with two copies each, a pass with nothing to do peaks at about 156 MiB, in 5 s; a folder holding them all renamed on one disk, at about 187 MiB for the scan that first sees it, the most of all, 178 MiB for a pass that first sees it, 178 MiB and 28 s for the pass that decides it with one folder operation, and 181 MiB for a dry run. That same rename through the view, on both disks, and every copy of a disk gone at once, are the most a pass holds: about 181 MiB and 179 MiB. The passes after every copy of a disk went, while the guard holds them and once they go, take about 8, 7 and 15 s: what reads below a folder must read by the folder's paths, not by its disk, as it is asked once for each folder that disk lost. 200 MB is 190.7 MiB. What keeps the worst under it, measured by the peak the kernel records rather than by samples: no list of every file grown entry by entry while the rest is held, as for a moment it is both its old size and its new one; no job list for a plan whose jobs nothing runs; one string where the catalog gives the same path twice; a compact row for each copy not where its row says; and a scan letting go of what its looks found of each copy before its plan. A pass writes what it does as it does it, so its output adds nothing to this. `atoll replicate` on a folder that holds all 200,000 files, none of them on the second disk, peaks at about 175 MiB for a dry run and 178 MiB for queuing every copy, in 8 s; with every copy pending, a scan peaks at about 179 MiB and a pass at 180 MiB; 500 empty files are copied with `--now` in 9 s, nearly all of it in commits and `fsync` (measured in a container).
- Throughput: copies and scrubs at 90 % or more of the disk's raw throughput; hashing is never the bottleneck.
- Robustness: a hard kill (kill -9) or a power cut at any moment loses no file and leaves a consistent catalog after restart.
- Security: local API over a socket with restricted permissions; HTTP limited to localhost or the Tailscale network, with a token.
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

| Phase | Language | Included | Excluded |
|---|---|---|---|
| 1a — Safe core | Go | disk identity and mounts, `disk add`/`remove`, scan, file and chunk hashes, rules, `deferred` and `manual` modes with `--now`/`--force`, propagation of renames, deletions and modifications with grouping and guards, trash, `prune`, detection-only scrub, `mergerfs-config` and runtime branches, status, ntfy alerts, contracts and conformance suite (§29) | repair, notifications, other modes |
| 1b — Complete Linux MVP | Go | `immediate` and `scheduled` modes, repair and quarantine, duplicates, domains and tags, SMART, Gotify and SMTP, media-server notifications, `conflicts resolve`, `balance`, `capacity`, optional link guard | own view |
| 2a — Own view | Go | Atoll's FUSE view on Linux: original presented, every entry from the catalog, trash on delete; contracts frozen at format version 1 | SSD cache |
| 2b — Disk sleep | Go | SSD cache, NUT, `ordered` and `colocate` strategies, Prometheus | Windows |
| 3 — Rust rewrite | Rust | feature parity with phase 2 on Linux, same contracts, conformance suite passed, switch without data migration (§29.2) | new features |
| 4 — Windows | Rust | Windows engine (service) and WinFsp pool drive, health evacuation, XFS and btrfs | — |
| 5 | Rust | web interface, openmediavault plugin, duplicates by metadata, header cache, parity study | — |

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
```

- Each percentage is a share of what its key states: `min_free`, `fill_to` and `trash.max_size` of each disk's capacity; `cache.max_fill` of the cache's capacity; `ratio_below` of the bytes correctly placed; `mass_guard.percent` of a disk's copies.
- A duration must fit in a signed 64-bit count of nanoseconds. A size is computed exactly, then rounded down to whole bytes, and must fit in a signed 64-bit integer. In a window, hours run from 00 to 23 and minutes from 00 to 59 (midnight is `00:00`), and the start differs from the end, which could mean an empty window or the whole day.
- `pool_id`, a disk's `id`, `domain` and `tags`, a rule's `tags` and `domains`, and `read.classes` are identifiers. They pass through the CLI, JSON and `disk.toml`, and are compared byte for byte, so each has one spelling. NFC and NFD forms of `été` would otherwise be two domains, and `spread_mode = "strict"` would count them as two.
- An absolute path (`catalog`, a `mount`, a `token_file`, an `api_key_file`, both sides of `path_map`) starts with `/`, is not `/` itself, and has no empty, `.` or `..` component and no NUL. The Windows form comes with phase 4.
- `min_free` is a list of one or more sizes and percentages; the smallest applies to each disk (§13.1).
- Time windows use wall-clock time in `timezone` and may wrap past midnight. A window crossing a daylight-saving change is shortened or lengthened accordingly; on a spring-forward night the missing hour is simply skipped.
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

- How does Plex read the media today: a mergerfs mount, or each disk's folders (§21.4)?
- fanotify on ntfs-3g and ntfs3: file-handle support for create, delete and rename events; fallback to inotify.
- mergerfs behavior with a failing branch before Atoll removes it (I/O errors or fallback to the next branch): to verify with a USB drop simulation.
- Phase 1 limits: deletions through mergerfs bypass the trash unless the link guard is on; mergerfs presents the first branch's copy, not the oldest one.
- Jellyfin and Emby: whether their scans delete items whose files are missing while a disk is absent.
- Link guard: which tools in the user's stack count hard links (torrent clients, *arr helpers).
- Plex behavior when libraries move to the merged view (metadata and watch history kept): test on a small library first.
- NTFS driver on Linux (ntfs3 or ntfs-3g): choose after power-loss and dirty-volume tests.
- Cost: two or three times the raw capacity for protected files.
- USB DAS: resets, sleep behavior, bridges; a UPS is essential.
- Single-copy windows: `immediate` mode before Atoll's own view, and SSD residency.
- Rust rewrite: cost of a second implementation and of keeping both in sync until the switch; maturity of the `winfsp` crate to check before phase 4.
- Windows arrives after the rewrite; until then, Windows PCs use the SMB share.
- Names (research in `docs/research/name-compatibility.md`), to decide before the solver, the scanner and `atoll names check`:
  - Atoll's own view (phase 2a): should a rename that a copy's filesystem refuses fail with an error, which mergerfs cannot do, instead of following §15.3?
  - on NTFS, 8.3 short names: both Linux drivers refuse to create a name equal to another file's short name, and ntfs-3g's `stat` returns that other file, so existence must never be decided by `stat` alone;
  - scanning: a listed entry whose `lstat` fails means a name the driver cannot read, never a missing file (measured: under `ads`, a real name `a:b.mkv` is listed and `stat` gives `ENOENT`).
