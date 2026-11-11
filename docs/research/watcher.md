# The Linux watcher: fanotify and inotify on each driver

This report measures what the watcher of `specs.md` §15.1 can rely on, on each filesystem Atoll meets, for the daemon that runs for good. It closes the question of §30, "fanotify on ntfs-3g and ntfs3", and records the two choices the maintainer made from it. It is research, not a contract.

## Why it matters

- §15.1 wants fanotify where it works, for the process behind each event: an operation the mergerfs process made is a view operation, which Atoll reconciles without propagating. Otherwise recursive inotify, which gives no process.
- Create, delete and rename events need the group to report file handles (`FAN_REPORT_FID` and its kin), and a filesystem-wide mark needs a filesystem fanotify can identify. Whether ntfs-3g and ntfs3 allow both was not known.
- Without a watcher, each pass finds changes by walking every disk, which wakes them. So the daemon that runs for good starts from these measurements.

## Method

- A Debian 13 container with no KVM boots two kernels in qemu, as `test/qemu/run.sh` does:
  - Debian's Linux 6.12.116 (`6.12.116+deb13-amd64`);
  - Ubuntu's mainline Linux 7.3.1 (`7.3.1-070301-generic`).
- A throwaway Go test in `test/loopdev`, not committed, ran as root on a loop image of each driver: ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g. ntfs-3g and lowntfs-3g are 2022.10.3, and mergerfs is 2.40.2.
- For each driver, it tries these fanotify group flags in order and keeps the first whose filesystem mark (`FAN_MARK_FILESYSTEM`) the driver accepts:
  1. `FAN_REPORT_DFID_NAME | FAN_REPORT_FID | FAN_REPORT_TARGET_FID`;
  2. without the target;
  3. `FAN_REPORT_DFID_NAME` alone;
  4. `FAN_REPORT_FID` alone.

  Where no filesystem mark is accepted, it tries an inode mark on the root folder, a mount mark (which reports no handle), and then a group with one inode mark per folder (`FAN_EVENT_ON_CHILD`). That last group marks each folder as an event shows it made or moved in, and finds folders again from their handles with `name_to_handle_at`.
- **Operations.** The same thirteen operations ran below the disk's `media/`:
  - create, append, chmod, touch, mkdir;
  - a rename into a folder, and one inside a folder;
  - an atomic save (write `c.part`, rename it onto `c`);
  - link, unlink;
  - a folder renamed, a file deleted, a folder removed.

  They ran three times:
  - directly, through the fanotify group;
  - through mergerfs mounted over that `media/` alone (`minfreespace=1M`), through the same group;
  - directly, through inotify with one watch per folder, a watch added for each folder made or moved in.
- **Waking.** Reads completed by the loop device (`/sys/block/loopN/stat`), over 3 s idle with the mark in place, after `sync` and `drop_caches`.
- **Overflow.** On ext4, 9,000 files made then removed (18,000 events) before reading anything, once with the default queue and once with `FAN_UNLIMITED_QUEUE`.
- **Kernel memory.** Unreclaimable slab (`SUnreclaim` in `/proc/meminfo`), on XFS, before and after each of these:
  - 20,000 files made, their events waiting in an unlimited queue;
  - 20,000 folders each given a fanotify inode mark, in a group with `FAN_UNLIMITED_MARKS`;
  - the same 20,000 folders each given an inotify watch.
- The guest has 5.9 GiB of memory. Measured on 2026-11-12.

## Summary

- **ext4, XFS, btrfs and ntfs3** take a filesystem mark in a group with the full set of flags (1). Every operation is reported, with its process. A rename comes as one `FAN_RENAME` event with both names.
- **ntfs-3g and lowntfs-3g** (`fuseblk`, fsid 0) refuse every filesystem mark with `ENODEV`.
  - A mount mark reports no create, delete or rename.
  - An inode mark per folder works. It reports every operation of the entries of a marked folder, with the process. A folder made or moved in must be marked as it appears, as inotify's watches must.
- **Through mergerfs**, every operation on every driver is reported with the mergerfs process's pid. fanotify tells a view operation from a user's, as §15.1 wants.
- **inotify** reports every operation on all six drivers, with no process.
- **Watching wakes no disk**: no read of the device in 3 s idle with the mark.
- **Overflow.** The default queue keeps 16,384 events and then one `FAN_Q_OVERFLOW`. An unlimited queue lost none of 18,000 events.
- **Kernel memory per waiting event, per mark and per watch**: a few hundred bytes (the table below).
- **Both kernels** gave the same results, but for the memory figures and the limits that scale with memory.

## Results

### The group and the mark each driver takes

| Driver | statfs type | fsid | Filesystem mark, flags (1) | Without a filesystem mark |
|---|---|---|---|---|
| ext4 | `0xef53` | set | accepted | not needed |
| XFS | `0x58465342` | set | accepted | not needed |
| btrfs | `0x9123683e` | set | accepted | not needed |
| ntfs3 | `0x7366746e` | set | accepted | not needed |
| ntfs-3g | `0x65735546` (FUSE) | 0 | `ENODEV`, with every set of flags | an inode mark is accepted with every set of flags; a mount mark is accepted, and reports no handle |
| lowntfs-3g | `0x65735546` (FUSE) | 0 | `ENODEV`, with every set of flags | as ntfs-3g |

On ntfs-3g and lowntfs-3g, `name_to_handle_at` gives a folder's handle without `AT_HANDLE_FID`, and the handles of files resolve with `open_by_handle_at` too.

### What a filesystem mark reports (ext4, XFS, btrfs, ntfs3)

The same on the four drivers and on both kernels. The kernel merges the events of one object that wait together, so one event may carry several bits. "dfid" is the folder's handle with the entry's name, and "fid" is the file's own handle.

| Operation | Events |
|---|---|
| create | `CREATE`, `MODIFY` and `CLOSE_WRITE` merged, dfid and fid |
| append | `MODIFY` and `CLOSE_WRITE` merged |
| chmod, touch | `ATTRIB` |
| mkdir | `CREATE` with `ONDIR` |
| rename, into a folder or inside one | `RENAME` with the old and the new dfid and the fid; then `MOVED_FROM`, `MOVED_TO` and `MOVE_SELF` |
| atomic save | the temporary file's `CREATE`, `MODIFY`, `CLOSE_WRITE` and `MOVED_FROM`; `RENAME` from it onto the target; `MOVED_TO`; the replaced file's `ATTRIB` and `DELETE_SELF`; `MOVE_SELF` |
| link | the file's `ATTRIB`; `CREATE` at the new name |
| unlink of one name | the file's `ATTRIB`; `DELETE` at the name |
| folder renamed | `RENAME` with `ONDIR`, then `MOVED_FROM`, `MOVED_TO` and `MOVE_SELF` |
| delete | `ATTRIB` and `DELETE_SELF`; `DELETE` at the name |
| rmdir | `DELETE_SELF` with `ONDIR`; `DELETE` at the name |

A file that no longer exists, deleted or replaced, comes with a handle that no longer resolves (`ESTALE`). Its name is in the dfid of the `DELETE`.

### What an inode mark per folder reports (ntfs-3g, lowntfs-3g)

These groups were made with `FAN_REPORT_DFID_NAME | FAN_REPORT_FID` and without `FAN_RENAME`, which needs the target flag. A group with that flag and per-folder marks was not tried.

| Operation | Events |
|---|---|
| create | `CREATE` with dfid only; then `MODIFY` and `CLOSE_WRITE` with dfid and fid |
| append, chmod, touch | as with a filesystem mark |
| mkdir | `CREATE` with `ONDIR`. The new folder is not marked until the watcher marks it |
| rename | `MOVED_FROM` at the old name, `MOVED_TO` at the new one |
| atomic save | `CREATE` and `MOVED_FROM` of the temporary name; `MODIFY` and `CLOSE_WRITE`; `MOVED_TO` at the target |
| link, unlink | `CREATE`, `DELETE` at the name |
| folder renamed | `MOVED_FROM` and `MOVED_TO` with `ONDIR`, and `MOVE_SELF` of the folder, whose mark follows it |
| delete | `DELETE` at the name |
| rmdir | `DELETE_SELF` with `ONDIR`; `DELETE` at the name |

### Through mergerfs

On all six drivers, every event of the thirteen operations made through mergerfs carried the pid of the mergerfs process, with filesystem marks and with marks per folder alike. The events were otherwise the same as for an operation made directly.

### inotify

The same on the six drivers and on both kernels:
- `CREATE`, `MODIFY` and `CLOSE_WRITE` for a create;
- `ATTRIB` for chmod and touch;
- `MOVED_FROM` and `MOVED_TO` paired by a cookie for each rename;
- `CREATE` and `DELETE` for link and unlink;
- for a folder renamed, `MOVE_SELF` from the folder's own watch;
- for rmdir, `DELETE_SELF` and `IGNORED`.

A folder made or moved in is watched only once the watcher adds it.

### Waking, overflow and limits

| | Linux 6.12.116 | Linux 7.3.1 |
|---|---|---|
| Device reads in 3 s idle with a mark, every driver | 0 | 0 |
| Default queue, 18,000 events waiting | 16,384 events, then 1 `FAN_Q_OVERFLOW` | the same |
| `FAN_UNLIMITED_QUEUE`, 18,000 events waiting | 18,000, no overflow | the same |
| `fs.fanotify.max_queued_events` | 16,384 | 16,384 |
| `fs.fanotify.max_user_groups` | 128 | 128 |
| `fs.fanotify.max_user_marks` | 48,797 | 54,231 |
| `fs.inotify.max_queued_events` | 16,384 | 16,384 |
| `fs.inotify.max_user_watches` | 45,857 | 50,616 |

`max_user_marks` and `max_user_watches` scale with the machine's memory: 5.9 GiB here. A library with more folders than that needs `FAN_UNLIMITED_MARKS`, which takes `CAP_SYS_ADMIN` like a filesystem mark.

### Kernel memory

| Unreclaimable slab, 20,000 of each | Linux 6.12.116 | Linux 7.3.1 |
|---|---|---|
| an event waiting in an unlimited queue (a create, names of 44 bytes) | 328 bytes | 413 bytes |
| a fanotify inode mark on a folder | 171 bytes | 194 bytes |
| an inotify watch on a folder | 155 bytes | 178 bytes |

A mark or a watch also keeps its folder's inode in memory, which is reclaimable slab and is not counted here. A rename gives four events with a filesystem mark. So an unlimited queue that waits through a rename of 200,000 files holds about 800,000 events: 260 MB to 330 MB of kernel memory.

## The maintainer's choices (2026-11-12)

- **ntfs-3g and lowntfs-3g: fanotify with an inode mark per folder**, not inotify. The process stays known on every driver, so a view operation is told apart the same way everywhere. The group takes `FAN_UNLIMITED_MARKS`. Each folder that appears is marked as it appears, then listed, as it may have been filled before its mark.
- **The queue stays bounded**, read without waiting, and asks only for the events Atoll needs:
  - `RENAME` rather than `MOVED_FROM`, `MOVED_TO` and `MOVE_SELF`, where the group can have it;
  - `CLOSE_WRITE` rather than `MODIFY`.

  An overflow is a rescan of that disk, which is the truth anyway (§15.1).

## Not measured, or still open

- A spinning disk: the device's read count stands in for its spin-up. A mark takes no read, so a disk that the mark finds asleep stays so as far as fanotify goes; what reads a disk is a look at it.
- `FAN_RENAME` with per-folder marks on ntfs-3g and lowntfs-3g, which needs `FAN_REPORT_TARGET_FID` in that group.
- The `ntfs` driver of Linux 7.1 and later (`ntfs-driver.md`), which Atoll keeps `readonly` for now.
- Events of Atoll's own operations, which the `ops` journal names (§15.1), and grouping per path over `deletes.debounce`: they come with the watcher's code, not with this measurement.
