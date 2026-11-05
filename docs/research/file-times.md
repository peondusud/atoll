# File times per filesystem and driver

This report backs the choice of the original in `specs.md` §7, and the handling of modification times in §9 and §14. It is research, not a contract: the spec and the vectors (`conformance/vectors/original.json`, `creation_time.json`, `time_ns.json`) are the contract.

## Creation times

Measured in qemu with no KVM, on Debian 13's Linux 6.12.115 and Ubuntu's mainline Linux 7.3.1, on image files in the guest's RAM, with coreutils `stat -c %w`, which reads `statx`. The two kernels gave the same results.

| Filesystem and driver | Through `statx` | Unchanged by `touch`, a write, a rename and a move | Other source |
|---|---|---|---|
| ext4, 256-byte inodes | yes, nanoseconds | yes | |
| ext4, 128-byte inodes | none | | |
| XFS | yes, nanoseconds | yes | |
| btrfs | yes, nanoseconds | yes | |
| ntfs3 | yes, 100 ns units | yes | |
| ntfs-3g | none | | `system.ntfs_crtime` |
| lowntfs-3g | none | | `system.ntfs_crtime` |
| mergerfs view | none | | read it on the disks |

`system.ntfs_crtime` holds 8 little-endian bytes, a FILETIME: 100 ns units since 1601-01-01 UTC. ntfs3 reads the value that ntfs-3g wrote, to the 100 ns. For example, the FILETIME 132539040001234567 reads as 2020-12-31 16:00:00.1234567.

### Dates that are not real

| Value on disk | Reported as | Fits in `btime_ns`? |
|---|---|---|
| ext4 crtime 0, in a 256-byte inode, set with `debugfs` | 1970-01-01 00:00:00, with the birth time marked as known | yes, as 0 |
| FILETIME 0, read by ntfs3 | 1601-01-01 00:00:00, or -11644473600 s | no |
| FILETIME of 1675-12-14, read by ntfs3 | -9279273600 s | no |
| FILETIME of 1700-01-01, read by ntfs3 | -8520336000 s | yes |
| FILETIME of 1959-12-31, read by ntfs3 | -315673600 s | yes |
| FILETIME of 2300-01-01, read by ntfs3 | 10413835200 s | no |

A signed 64-bit count of nanoseconds covers 1677-09-21 00:12:43.145224192 to 2262-04-11 23:47:16.854775807 UTC. As the oldest copy wins, a date of 0 or of 1601 would always win, so §7 treats these as absent. Go's `time.Time.UnixNano` is undefined outside that range and does not report it. Measured with Go 1.27.2, it wraps: 1601 gives +6802270473709551616, a date in 2185, and 2300 gives a negative number. So the conversions check the range themselves (`solver.TimeNS`, `solver.CreationTimeNS`).

Not measured: Windows, and whether real files on the maintainer's disks hold such dates.

## Modification times

Measured the same way, setting the time with `touch` and reading it after dropping the caches. `touch` always exited 0, even when the filesystem kept another value.

| Filesystem | Earliest kept | Latest kept | Sub-second |
|---|---|---|---|
| ext4, 256-byte inodes | 1901-12-13 (-2147483648 s) | 2446-05-10 (15032385535 s) | nanoseconds |
| ext4, 128-byte inodes | 1901-12-13 | 2038-01-19 (2147483647 s) | none |
| XFS, default (`bigtime`) | 1901-12-13 | 2486-07-02 (16299260424 s) | nanoseconds |
| XFS, `bigtime=0` | 1901-12-13 | 2038-01-19 | nanoseconds |
| btrfs | every date tried, from 1601 to 9999 | | nanoseconds |
| ntfs3, ntfs-3g, lowntfs-3g | every date tried, from 1601 to 9999 | | 100 ns |

A file with a FILETIME of 0 as its modification time reads as -11644473600 s through ntfs3 and ntfs-3g. `cp --preserve=timestamps` to ext4 exits 0, and the copy holds -2147483648 s, so its time differs from the source's by more than 300 years. Under the earlier rules, Atoll would have seen that as a metadata change on one copy, and applied 1901 to the source. Not measured: change times out of range, which tools can only set on NTFS.

## Decisions

The maintainer chose:

| Point | Decision |
|---|---|
| Comparison of creation times (§7) | Exact nanoseconds, not the coarser precision of §9: creation times are never copied, and the choice is among identical copies. |
| Creation dates that are not real (§7) | Exactly 0, or outside the range: absent, so `first_seen` stands in. |
| Eligible copies (§7) | Copy state `ok` or `surplus`, on a disk `online`, `standby`, `readonly` or `draining`. |
| Conflicts (§7) | The previous original stays as long as its copy is eligible. Otherwise the normal rule applies. |
| A target that cannot hold a modification time (§14) | The copy is made with the time the filesystem keeps, which is recorded per copy. It stays in sync, and the difference is never applied to the other copies. |
| Modification and change times out of range (§9) | Stored at the nearest limit. Atoll sets times from a copy's `stat`, never from the catalog. |

Comparing at the coarser precision was rejected for what it would cost, as it would have changed nothing in practice: a difference under 100 ns between copies written at different times does not arise. And pairwise it is not transitive: with `a` = 1000001 ns on ext4, `b` = 1000000 ns on NTFS and `c` = 1000099 ns on ext4, `a` ties with `b` and `b` with `c`, while `a` is older than `c`.

## Open

Copies with the same content and different modification times, when a disk is added or the catalog rebuilt: which time is the current version's, and do they count as in sync (`specs.md` §30)?
