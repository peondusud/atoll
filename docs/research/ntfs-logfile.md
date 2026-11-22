# NTFS's journal, `$LogFile`, and a volume Windows left in use

This report backs the maintainer's decision of 2026-11-21 ("Lire $LogFile aussi", `TODO.md`): once a disk's mount is confirmed, Atoll reads `$LogFile` as it reads `hiberfil.sys`, and a journal left unclean makes the disk `readonly`, `hibernated` (`specs.md` §10, §22). It is research, not a contract: the rule Atoll applies is pinned by `conformance/vectors/logfile.json`.

## Why it matters

- Fast Startup, on by default since Windows 8, hibernates the kernel at each shutdown. Windows writes `hiberfil.sys` at the top of the volume it starts from alone; a data volume it had mounted holds none, and is not dismounted either. When Windows resumes, it takes back what it held in memory of that volume, over whatever was written to it meanwhile.
- The sign such a data volume carries is its journal: a restart area that says a client still has it in use, and a version of 2.0, which Windows 8 and later leave on a volume they did not dismount. ntfs-3g reads both: "The disk contains an unclean file system (0, 0)", then "Metadata kept in Windows cache, refused to mount", and it falls back to a read-only mount.

## Sources

Read on 2026-11-21, at the heads of their main branches:

- ntfs-3g (`github.com/tuxera/ntfs-3g`, branch `edge`): `include/ntfs-3g/logfile.h` (the layout), `libntfs-3g/logfile.c` (`ntfs_check_restart_page_header`, `ntfs_check_restart_area`, `ntfs_check_log_client_array`, `ntfs_check_and_load_restart_page`, `ntfs_check_logfile`, `ntfs_is_logfile_clean`), `libntfs-3g/volume.c` (`ntfs_volume_check_logfile`, `ntfs_device_mount`), `libntfs-3g/inode.c` (`ntfs_inode_update_times`), `src/ntfs-3g.c` and `src/lowntfs-3g.c` (the mount flags, the lookup and the open of a system file).
- Linux (`torvalds/linux`, `master`): `fs/ntfs3/fslog.c` (`log_read_rst`, `log_replay`), `fs/ntfs3/fsntfs.c` (`ntfs_loadlog_and_replay`), `fs/ntfs3/super.c` (`ntfs_fill_super`); `fs/ntfs/logfile.c` and `fs/ntfs/super.c` of the type `ntfs` of Linux 7.1 and later, whose `logfile.c` keeps the functions of the old read-only driver `fs/ntfs` that Linux 6.9 removed, by the same author as ntfs-3g's.

## The format

Little-endian throughout (`logfile.h`). `$LogFile` begins with two restart pages, each a system page long (4 KiB as Windows writes it): the first at 0, the second at its system page size. Each is protected by an update sequence, as an MFT record is: the last two bytes of each 512 hold the update number, and an array after the header keeps what they held.

| Restart page header | Offset | |
|---|---|---|
| magic | 0 | `RSTR`, or `CHKD` once chkdsk changed it |
| update sequence array: offset, count | 4, 6 | the count is 1 + the system page size / 512 |
| chkdsk LSN | 8 | 0 on an `RSTR` page |
| system page size, log page size | 16, 20 | powers of two of 512 or more |
| restart area offset | 24 | aligned to 8, after the array |
| minor, major version | 26, 28 | 1.1, or 2.0 |

| Restart area | Offset | |
|---|---|---|
| current LSN | 0 | of two pages, the greater counts |
| log clients | 8 | |
| first free client, first client in use | 10, 12 | `0xFFFF` (`LOGFILE_NO_CLIENT`) for none |
| flags | 14 | `0x0002`, `RESTART_VOLUME_IS_CLEAN` |
| sequence number bits | 16 | 67 less the bit length of the file size |
| restart area length, client array offset | 20, 22 | |
| file size | 24 | |
| log record header length, log page data offset | 36, 38 | multiples of 8 |

A client record is 160 bytes, its previous and next clients at 16 and 18. A block of a journal never written, or emptied, begins with `FF FF FF FF`.

## What each reader decides

- **ntfs-3g** (`ntfs_check_logfile`): it looks for restart pages at 0, then at each power of two from 512, until a log record page (`RCRD`), an empty block after one that is not, or a second restart page. A page that fails a check is skipped, and the search goes on. All empty, the journal is empty, and clean. No page that holds, and not empty: not clean. Of two, the greater LSN counts (the chkdsk LSN of a `CHKD` page). An update sequence that fails is fatal to a page only when its restart area runs past the first sector; otherwise the page is kept, its magic set to `BAAD`, and `ntfs_is_logfile_clean` then refuses it. Clean: no client in use, or `RESTART_VOLUME_IS_CLEAN` set (`ntfs_is_logfile_clean`). Then `ntfs_volume_check_logfile` refuses any restart page of version 2.0, clean or not ("Windows may have kept a cached copy of metadata for fast restarting"), with `EPERM`.
- **ntfs-3g at a mount** (`ntfs_device_mount`): it reads the journal only for a read-write mount. A journal of version 2.0 makes it fall back to read-only (`NTFS_MNT_MAY_RDONLY`, which `ntfs-3g` and `lowntfs-3g` set unless `remove_hiberfile` is given). Any other one not clean, with `recover` (the default), is emptied ("The file system wasn't safely closed on Windows. Fixing.", `ntfs_logfile_reset`), and the mount is read-write; with `norecover`, read-only.
- **ntfs3** (`log_replay`): clean means no client in use; `RESTART_VOLUME_IS_CLEAN` is not read. Otherwise it replays the journal on a read-write mount, writes restart pages with none in use, and fills the journal with 0xFF (`ntfs_bio_fill_1`); on a read-only mount it replays nothing and writes nothing. A replay that fails makes it refuse a read-write mount ("failed to replay log file. Can't mount rw!").
- **the type `ntfs`** (`load_and_check_logfile`): it checks the journal as ntfs-3g does, but knows version 1.1 alone ("This driver supports version 1.1 only"), and asks nothing of whether it is clean. A journal that fails makes the volume read-only, under `errors=continue` too. One that holds is emptied at a read-write mount (`ntfs_empty_logfile`), whatever `errors=` says.

## Atoll's rule

Atoll reads what ntfs-3g reads, and decides as ntfs-3g decides: of the four, it is the one that refuses most, and it is the one whose refusal Windows users meet. `conformance/vectors/logfile.json` pins it, and its README says each check. Where Atoll reads less than ntfs-3g, it says less:

- It reads the first 128 KiB, enough for two restart pages of a system page of 64 KiB. A restart page that runs past what it read makes the journal `invalid`; a journal empty in its first 128 KiB is empty.
- A torn restart page that counts, and a page whose update sequence array is at an odd offset, are `invalid` (ntfs-3g keeps the latter as it is).
- Lengths are counted without wrapping, where ntfs-3g counts the restart area's in 16 bits.

So `empty` and `clean` leave the disk `online`; `unclean`, `version-2` and `invalid` make it `readonly`, `hibernated`, as do a `$LogFile` absent, not a regular file, or unreadable; `EIO` and the other errors of a disk lost make it `missing` (§10).

## Measured

`test/loopdev`, `TestAnUncleanLogFileOnEachDriver`, in qemu, 2026-11-21: on a volume `mkntfs` made, its `$LogFile` (2 MiB) written on the device before each mount, with two restart pages of 4 KiB and no log record after them, as ntfs-3g reads such a journal. Windows was not run: what Windows writes was not measured, and a journal whose restart area points at real log records, which ntfs3 replays, could not be made. Each driver mounted it read-write as Atoll recommends (§22), and read-only when it refused.

| Journal | ntfs3 | `ntfs`, `errors=remount-ro` | `ntfs`, `errors=continue` | ntfs-3g, lowntfs-3g |
|---|---|---|---|---|
| empty (0xFF, as `mkntfs` makes it) | read-write, left | read-write, left | read-write, left | read-write, left |
| 1.1, clean, its client in use | refused read-write, read-only when asked, left | read-write, **emptied** | read-write, **emptied** | read-write, left |
| 1.1, clean, none in use | read-write, **emptied** | read-write, **emptied** | read-write, **emptied** | read-write, left |
| 1.1, a client in use, not clean | refused read-write ("failed to replay log file. Can't mount rw!"), read-only when asked, left | read-write, **emptied** | read-write, **emptied** | read-write, **emptied** ("unclean file system (0, 0)"); with `norecover`, **read-only**, left |
| 2.0, clean | refused read-write, read-only when asked, left | superblock `ro`, left ("version 2.0 is not supported") | superblock `ro`, left | **read-only**, left ("Metadata kept in Windows cache") |
| 2.0, a client in use, not clean | refused read-write, read-only when asked, left | superblock `ro`, left | superblock `ro`, left | **read-only**, left |
| an update sequence of the wrong count | refused read-write, read-only when asked, left | superblock `ro`, left | superblock `ro`, left | read-write, **emptied**; with `norecover`, **read-only**, left |

The same on Linux 6.12 and 7.3.5, but for the type `ntfs`, which 6.12 does not have. The check gave `hibernated` wherever the journal was left in use, of version 2.0 or invalid, `read-only-mount` for ntfs3's read-only mount of the clean one with a client in use, `errors-continue` first under `errors=continue`, and `online` elsewhere.

- **Every driver answers `$LogFile` by its name** at the top of the mount: a regular file of its size, whose bytes are the device's (ntfs3 `-rwxr-xr-x`, the others `-rwxrwxrwx`). So Atoll reads it through the mount, as `hiberfil.sys`, and a top that does not answer that name tells nothing.
- **No driver kept a read-write mount over a journal left in use, or one that proves nothing, without emptying it.** Each either refused, mounted read-only, or emptied the journal as it mounted. So the check reads a journal left in use only on a read-only mount, where it gave `read-only-mount` and now gives `hibernated`, which says what to do; and on a driver that would mount read-write without emptying it, none of the four. A driver that empties the journal writes to a volume Windows may resume, and that has happened before Atoll looks: §22 says it.
- ntfs3 refused every restart area with a client in use, `RESTART_VOLUME_IS_CLEAN` set or not, as its replay found no log record after it; with a real one it would have replayed it (source). It mounted read-only when asked, and wrote nothing. One with no client in use it mounted read-write, and emptied.
- The check moved no access time of `$LogFile` and wrote nothing to the journal, on any driver (`O_NOATIME`, and ntfs-3g updates no time of a system file, `ntfs_inode_update_times`).
