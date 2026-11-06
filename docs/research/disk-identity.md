# Disk identity and mount checks

This report backs `specs.md` §10 and §22. It is research, not a contract: the spec and the vectors (`conformance/vectors/superblock.json`, `mountinfo.json`, `disk_check.json`) are the contract.

## Method

A Debian 13 container with no KVM boots each kernel in qemu, from an initramfs built from the container's own files: Debian's Linux 6.12.115 and Ubuntu's mainline Linux 7.3.1. Loop devices are image files in the guest's RAM. USB disks are `usb-storage` devices of qemu, backed by image files of the container, and unplugged with `device_del` from its monitor. A small static program calls `statx`, `statfs`, `FS_IOC_GETFSUUID`, `getdents` and reads the device, buffered and with `O_DIRECT`. Both kernels gave the same results, except where a row says otherwise. No real disk was involved.

Not measured: real USB bridges, real sleeping disks, `/dev/disk/by-uuid` (no udev in the guest), and any filesystem on a partition rather than a whole device.

## Reading the identifier

| Filesystem and driver | Type in the mount table | Major:minor in the table | Superblock read, as `specs.md` §10 says | `blkid -p` | `FS_IOC_GETFSUUID` | `f_fsid` of `statfs` |
|---|---|---|---|---|---|---|
| ext4 | `ext4` | the device's | same as `blkid -p` | yes | yes, with no privilege and no device read | a hash |
| XFS | `xfs` | the device's | same | yes | yes, the same | the device number |
| btrfs | `btrfs` | anonymous (`0:29`), and `statx` gives yet another (`0:30`) | same | yes | `ENOTTY` | a hash |
| ntfs3 | `ntfs3` | the device's | same | yes | `ENOTTY` | the serial |
| ntfs-3g, lowntfs-3g | `fuseblk` | the device's | same | yes | `ENOTTY` | 0 |

- **`blkid` without `-p` answers from a cache.** Right after a new filesystem was made on a loop device, it gave the UUID of the filesystem that was there before. On a real machine, this names the wrong disk.
- `FS_IOC_GETFSUUID` would need no root, but only ext4 and XFS have it, so NTFS would need something else anyway. ntfs3's `f_fsid` is the serial, but ntfs-3g's is 0.
- Without root or the `disk` group, opening the device gives `EACCES`.
- **Two NTFS volumes made by `mkntfs` in the same second get the same serial.** Measured: two images made one after the other both read `74FBEEC83AFD3A63`, and a third, 2 s later, `501F97232413B474`. So a clone is not the only way to meet two disks with one identifier (`specs.md` §10).
- btrfs on two devices: both hold the same fsid, and the table names the first device, even when mounted through the second.

## What a mount path looks like

From `statx` with `AT_SYMLINK_NOFOLLOW`, then the table's line for `stx_mnt_id`. `STATX_ATTR_MOUNT_ROOT` was known on every filesystem.

| Situation | Mount root | The table's entry for that id |
|---|---|---|
| Empty mount point | no | the parent's |
| Mounted | yes | root `/`, at the path |
| Bind of the whole filesystem | yes | root `/`, at the path |
| Bind of a folder | yes | root `/sub`, at the path |
| btrfs subvolume | yes | root `/sv`, at the path |
| Another filesystem mounted over it | yes | the top one: the table holds both lines, and only the id tells them apart |
| A symlink to the mount | no | the parent's, and the mode says symlink |
| A path through a symlinked folder | yes | the real path, which differs from the one asked |
| `media/` mounted over | yes, on `media/` | another id than the disk's |
| `ro` mount | | `ro` in both lists of options |
| `ro` bind of a `rw` filesystem | | `ro` in the mount options, `rw` in the superblock options |

The entry alone tells these apart, so a check needs no more than the mount id and the entry's mount point and root. Names are escaped in the table: a space is `\040`, a tab `\011`. The kernel escapes what it writes itself, and each driver writes its own options: under Docker Desktop, 9p shows `aname=drvfs;path=C:\;uid=0`, with a backslash that escapes nothing, beside a source written `C:\134`.

## Mount variants that change name rules

| Variant | Mount table | Superblock | `stat MEDIA` beside a folder `media` |
|---|---|---|---|
| ntfs3 `nocase` | `nocase` | | `ENOENT`, while `Media` is found |
| lowntfs-3g `ignore_case` | nothing | | the same inode |
| lowntfs-3g, ntfs-3g | nothing | | `ENOENT` |
| ext4 `casefold` | nothing | the feature bit | `ENOENT` on a plain folder, found in a `+F` folder |
| XFS `ascii-ci` | nothing | the version bit | the same inode |

- lowntfs-3g `ignore_case` shows only in the daemon's command line, or by asking. Asking is what §10 does: a command line depends on how the mount was started, and on which process to look at.
- After a `nocase` mount, ntfs3 refuses the next mount of that volume as dirty, on both kernels.
- ntfs3 shows `iocharset=utf8` by default, on both kernels. Linux 7.3.1 also shows `ads` by default, and shows no `ads` with `ads=0`. Linux 6.12 refuses `ads=0` as an unknown parameter.

## A third NTFS driver

On Linux 7.3.1, `mount -i -t ntfs` mounts with an in-kernel driver of the type `ntfs`, with the options `rw,umask=00,iocharset=utf8,case_sensitive,errors=continue,mft_zone_multiplier=1,native_symlink=raw,symlink=wsl`. It mounted the dirty volume that ntfs3 refused just before. Linux 6.12 has no such type. Without `-i`, Debian's `mount` runs the `mount.ntfs` helper of its `ntfs-3g` package, which gives `fuseblk`. Its name rules, its handling of `:` and its times were not measured.

## When a disk goes away

### A USB disk unplugged while mounted

| Reading, right after | ext4 | ntfs3 | ntfs-3g |
|---|---|---|---|
| Line in the mount table | stays, and from Linux 7.3 gains `shutdown` | stays | stays |
| `statx` on the mount path | answers | answers | answers |
| `getdents` on the mount path | answers | `open` gives `EIO` | `EIO` |
| `statx` on `media/` | answers | `EIO` | answers |
| `/sys/dev/block/<major>:<minor>` | gone within 2 s | gone | gone |
| The device node | gone | gone | gone |

- So `statx` alone reads an unplugged ext4 disk as healthy, and a look at the source in the table is what notices it.
- Plugged back, it comes up under a new name (`sdf`), the old mount stays as it was, and its old name stays free. Mounted again on the same path, the new mount lies over the old one, and `stx_mnt_id` gives the new one.

### A device that returns errors (dm-error)

| Reading | ext4 | XFS | ntfs3 | ntfs-3g |
|---|---|---|---|---|
| Mount table, later | `rw`; from Linux 7.3 gains `emergency_ro` | `rw` | `rw` | `rw` |
| Before dropping caches: `statx`, `getdents` | answer | answer | answer | answer |
| After dropping caches: `statx` on the mount path | answers | `EIO` | answers | answers |
| After dropping caches: `statx` on `media/` | `EIO` | `EIO` | `EIO` | answers |
| A read of the device through its node | `EIO` | `EIO` | `EIO` | `EIO` |

- ext4 logs "Remounting filesystem read-only", while Linux 6.12 still shows `rw` in the table, so `ro` is not a reliable sign of a failing disk.
- The device read failed here as that part of the head was not cached. Once it is cached, a buffered read answers with no device read.

### The FUSE daemon killed

The line stays in the table, and `statx`, `statfs` and `open` all give `ENOTCONN`.

## What a check costs

Device reads counted from `/sys/class/block/<dev>/stat`, on ext4, ntfs3, ntfs-3g and lowntfs-3g, after one cold pass.

| Asked again | Device reads |
|---|---|
| `statx`, `statfs`, `getdents` on the mount path | 0 |
| `stat` of `media`, and of `MEDIA` | 0, also 2 s later on FUSE |
| Buffered read of the head | 0 |
| `O_DIRECT` read of the head | 1 per read |

So a check can be repeated without waking a disk, as long as the head stays in the page cache, and an `O_DIRECT` read always reaches it. Whether a sleeping disk really stays asleep was not measured.

## The code on real filesystems

`test/loopdev/disks_test.go` runs the real observer in the same guests, on both kernels, with `blkid -p` as the outside check on the identifier: each driver, each shape of mount above, each variant, dm-error, a killed FUSE daemon, and `scsi_debug` for a device removed under its mount. All pass. On Linux 6.12, `ads` and the `ntfs` driver are skipped, as it has neither. This found one mistake the vectors could not: on XFS `ascii-ci` the root answers in the other case too, so `ignore-case` came first and advised a remount that cannot help. The features of the superblock now come first.

## Decisions

The maintainer chose:

| Point | Decision |
|---|---|
| Reading the identifier | From the superblock, with no external tool. It costs four small decoders and root or the `disk` group. |
| A mount that changes name rules, an unknown driver, or a `ro` mount | `readonly`, with an alert. §10 asked this only for `ads`. |
| No `media/`, a bind of a folder, a btrfs subvolume | `missing`. |
| What shows it | `atoll disk list`. The clock of the conformance build and `daemon --until-idle` wait for `scan`, their first use. |
| A check that stays cached | `disk list` wakes no disk, so a failing disk with cached answers stays `online` until an operation gets an I/O error. |
