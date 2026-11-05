# Creation times per driver

This report backs the choice of the original in `specs.md` §7. It is research, not a contract: the spec and the vectors (`conformance/vectors/original.json`, `creation_time.json`) are the contract.

## Measurements

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

## Dates that are not real

| Value on disk | Reported as | Fits in `btime_ns`? |
|---|---|---|
| ext4 crtime 0, in a 256-byte inode, set with `debugfs` | 1970-01-01 00:00:00, with the birth time marked as known | yes, as 0 |
| FILETIME 0, read by ntfs3 | 1601-01-01 00:00:00, or -11644473600 s | no |
| FILETIME of 1675-12-14, read by ntfs3 | -9279273600 s | no |
| FILETIME of 1700-01-01, read by ntfs3 | -8520336000 s | yes |
| FILETIME of 1959-12-31, read by ntfs3 | -315673600 s | yes |
| FILETIME of 2300-01-01, read by ntfs3 | 10413835200 s | no |

A signed 64-bit count of nanoseconds covers 1677-09-21 00:12:43.145224192 to 2262-04-11 23:47:16.854775807 UTC. As the oldest copy wins, a date of 0 or of 1601 would always win, so §7 treats these as absent. Go's `time.Time.UnixNano` is undefined outside that range and does not report it, so the conversion checks the range itself (`solver.CreationTimeNS`).

Not measured: Windows, and whether real files on the maintainer's disks hold such dates.

## Decisions

The maintainer chose (§7):

| Point | Decision |
|---|---|
| Comparison | Exact nanoseconds, not the coarser precision of §9: creation times are never copied, and the choice is among identical copies. |
| Dates that are not real | Exactly 0, or outside the range: absent, so `first_seen` stands in. |
| Eligible copies | Copy state `ok` or `surplus`, on a disk `online`, `standby`, `readonly` or `draining`. |
| Conflicts | The previous original stays as long as its copy is eligible. Otherwise the normal rule applies. |

Comparing at the coarser precision was rejected for what it would cost, as it would have changed nothing in practice: a difference under 100 ns between copies written at different times does not arise. And pairwise it is not transitive: with `a` = 1000001 ns on ext4, `b` = 1000000 ns on NTFS and `c` = 1000099 ns on ext4, `a` ties with `b` and `b` with `c`, while `a` is older than `c`.

## Open

Modification and change times have the same range problem, and a replica must keep the modification time, so they need a decision of their own (`specs.md` §30).
