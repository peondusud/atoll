# How far apart two changes may be and carry one change time

This report measures, on each filesystem and driver Atoll supports, how far apart two changes of one file may be and still carry one change time, as a look between them sees it, for the maintainer's answer of 2026-11-26 on the observations of flaws 31 and 32's fix (DONE.md, `conformance/models/README.md`). It is research, not a contract.

## Why it matters

Atoll tells an edit from rot by the times a look finds (specs.md §16, invariant 9): a copy whose content no longer matches its hashes while its size and times are still its row's is called corrupt, and repaired. A filesystem's `time_precision_ns` (§10) says how finely it holds a time: an ext4 made with 128-byte inodes holds whole seconds, which flaw 31's remedy, `coarse-conflict`, takes as a disk whose times cannot tell an edit. But the kernel dates a change too, by its own clock. A kernel without the multigrain timestamps of Linux 6.13 gives a change the time of its clock's coarse tick, a jiffy, whatever the filesystem could hold. Two changes within one tick, the change Atoll recorded and a user's edit in place that keeps the size and the modification time, then carry one change time on any disk, and the edit is called corrupt, as in flaw 31.

## Method

`TestTheChangeTimeGranularityOfEachDriver` (`test/loopdev/ctimegrain_test.go`, kept, run with `make test-qemu`): on a loop device of each filesystem and driver, a file written with one byte, then 4,000 times in a row a byte written in place with `pwrite` and the file looked at with `stat`; then again with a pause of 1 ms before each write. For each pair of changes in a row, whether the second carried the change time of the first, and how far apart on the wall clock the two looks were; and the smallest step between two change times that differ. It logs what it finds, and fails only when it cannot measure. Run on 2026-11-26 in qemu without KVM, through `heavy.sh`, on Linux 6.12.118 (Debian 13) and 7.3.5 (Ubuntu's mainline), one after the other; the kernel's `ntfs` exists only on 7.3. ext4 is made as `mkfs.ext4` makes it, with 256-byte inodes.

## Measured

| Kernel | Filesystem, driver | Changes in a row that carried the change time before (no pause; 1 ms pause) | Farthest apart with one change time | Smallest step between two change times |
|---|---|---|---|---|
| 6.12.118 | ext4 | 3,979 of 3,999; 2,661 | 5.55 ms | 4 ms |
| 6.12.118 | XFS | 3,979; 2,677 | 10.86 ms | 4 ms |
| 6.12.118 | btrfs | 3,964; 2,648 | 4.19 ms | 4 ms |
| 6.12.118 | ntfs3 | 3,974; 2,677 | 5.06 ms | 4 ms |
| 6.12.118 | ntfs-3g | 0; 0 | none | 0.43 ms |
| 6.12.118 | lowntfs-3g | 0; 0 | none | 0.51 ms |
| 7.3.5 | ext4 | 0; 0 | none | 18 µs |
| 7.3.5 | XFS | 0; 0 | none | 18 µs |
| 7.3.5 | btrfs | 0; 0 | none | 18 µs |
| 7.3.5 | ntfs3 | 3,885; 14 | 1.57 ms | 1 ms |
| 7.3.5 | ntfs | 3,932; 6 | 1.86 ms | 1 ms |
| 7.3.5 | ntfs-3g | 0; 0 | none | 0.45 ms |
| 7.3.5 | lowntfs-3g | 0; 0 | none | 0.53 ms |

- On 6.12, every filesystem the kernel drives dates a change to its clock's tick, 4 ms there (`CONFIG_HZ=250`), and a change after a look carries the time of the one before for as long as the tick lasts. The farthest apart, past the tick on XFS, is two looks on a guest whose ticks come late, without KVM.
- On 7.3, ext4, XFS and btrfs take the multigrain timestamps of 6.13: a change after a look always carries a time of its own. ntfs3 and the kernel's `ntfs` do not take them, and date a change to the tick, 1 ms there (`CONFIG_HZ=1000`).
- ntfs-3g and lowntfs-3g date a change by their own clock, in userspace, and never gave two changes after a look one change time.

## What it means for Atoll

A window exists: on a kernel without multigrain timestamps, and on 7.3 for the kernel's NTFS drivers, an edit in place that keeps the size and the modification time, made within a tick of the change Atoll's look recorded, a few milliseconds, carries that change time, and the scrub calls it corrupt, on a disk whose `time_precision_ns` is the nanosecond or 100 ns. Flaw 31's remedy does not close it, as such a disk's times vouch for it; `racy-conflict`, noted for format version 2, which takes a row recorded within the tick of the copy's last change as one that cannot tell an edit, would. Within a repair, the read again of a chunk before its write, flaw 32's remedy, covers the same tick. The maintainer chose to record the window as a limit of flaw 31's remedy, with no change to the code now (§16, TODO.md).
