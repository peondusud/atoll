# The in-kernel `ntfs` driver of Linux 7.1 and later

This report measures the read-write NTFS driver of the type `ntfs` against what Atoll relies on from an NTFS driver, for a decision on `specs.md` §10, §15.8 and §22. It is research, not a contract, and it changes nothing: until the maintainer decides, a disk mounted with this driver stays `readonly` with the reason `unsupported-driver` (§10).

## Why it matters

- Linux 7.1 added a second read-write NTFS driver beside ntfs3: module `ntfs.ko`, "NTFS read-write filesystem driver", by Namjae Jeon and Anton Altaparmakov, alias `fs-ntfs`, filesystem type `ntfs`. `fs/ntfs/` is absent from the stable tree at v6.18, v6.19 and v7.0, and present at v7.1, v7.2 and v7.3.1.
- Ubuntu's mainline 7.3.1, which `test/qemu/run.sh` boots, builds both (`CONFIG_NTFS_FS=m`, `CONFIG_NTFS3_FS=m`). Debian sid's 7.3.3+deb14, the future Debian 14 kernel, builds only `ntfs` (`# CONFIG_NTFS3_FS is not set`, read from its package, not booted). So a Debian 14 user has this driver or ntfs-3g, and no ntfs3.
- `mount -t ntfs` reaches it only when no `mount.ntfs` helper exists. Debian's `ntfs-3g` package installs one, and then the same command mounts ntfs-3g (`fuseblk`), as `specs.md` §22 already says (measured again here).

## Method

- A Debian 13 container with no KVM boots Ubuntu's mainline Linux 7.3.1 (`7.3.1-070301-generic`) in qemu, from an initramfs built from the container's own files, as `test/qemu/run.sh` does. The kernel has `CONFIG_NTFS_FS=m`, `CONFIG_NTFS_FS_POSIX_ACL=y` and `CONFIG_NLS_DEFAULT="utf8"`. Volumes are made by `mkntfs` from ntfs-3g 2022.10.3, on image files in the guest's RAM, and the driver is mounted with `mount -i -t ntfs`, which skips the helper. ntfs3 and ntfs-3g 2022.10.3 ran beside it as controls.
- Throwaway Go tests in `test/loopdev`, not committed, made each measurement and printed it. A file "survives a power cut" when a copy of the image taken right after the call, mounted read-only by ntfs-3g and by `ntfs`, still holds it.
- A scratch copy of Atoll, not committed either, removed the `unsupported-driver` case from `judgeWrites`, mounted `ntfs` with `-i`, and added `ntfs` to the driver lists of `test/loopdev`. Its suite then ran on this driver (below, "Atoll's own suite").
- The driver's source at v7.3.1 (`fs/ntfs/` in the stable tree) explains several results. A statement taken from the source alone says so.
- Measured on 2026-11-11. Debian's own 7.3.3 build was not booted: the same source, with another configuration.

## Summary

- **Names.** Every name the corpus accepts on NTFS is accepted, and ntfs3, ntfs-3g and `ntfs` read each other's names byte for byte. Like ntfs3, the driver refuses invalid UTF-8 (`EILSEQ`) and components past 255 UTF-16 units (`ENAMETOOLONG`), never normalizes, and is case-sensitive by default. The Windows table of §15.8 covers it unchanged.
- **No stream interface.** There is no `ads` option, `a:b` is created and listed as an ordinary name, and the source gives `:` no meaning in a name: the risk that made ntfs3 `ads` a refused variant is not there.
- **Mount information says what matters.** `iocharset=…`, `case_sensitive` or `nocase`, `windows_names` and `errors=…` are always among the superblock options.
- **Times and identity are ntfs3's, bit for bit**: 100 ns, rounded down, the same dates, a birth time through `statx`, the same inode and times as ntfs3 reads from the same volume, and the same mode wherever a file has one of its own.
- **Atoll's own suite passes on it**, except where ntfs3 already needs an exemption (a device that fails between two syncs of a move) and one difference of `windows_names` (below).
- **Five things differ from ntfs3** and need a decision:
  1. With the default `errors=continue`, it mounts read-write a volume that Windows left hibernated, or that is marked dirty. With `errors=remount-ro` it mounts both read-only. (ntfs3 also mounts a hibernated volume read-write.)
  2. `windows_names` is not applied to `link`, and is applied to `unlink` and `rmdir`: a refused name can be linked into the trash and never removed.
  3. `link` sets the file's modification time to the current time (ntfs3 does not).
  4. A file with no WSL mode (written by Windows or ntfs-3g) reads `0777` under its default `umask=00`, where ntfs3 reads `0755`.
  5. Names holding `" * < > ?`, which ntfs-3g writes, can be read but not renamed or removed (`ERANGE`); ntfs3 renames and removes them.

## Mount options

Each option alone, on a fresh volume:

| Option | Result | Superblock options in `/proc/self/mountinfo` |
|---|---|---|
| none | mounted | `rw,umask=00,iocharset=utf8,case_sensitive,errors=continue,mft_zone_multiplier=1,native_symlink=raw,symlink=wsl` |
| `iocharset=utf8`, `nls=utf8` | mounted | `iocharset=utf8` |
| `iocharset=iso8859-1`, `ascii`, `koi8-r` | mounted | `iocharset=<that name>` |
| `case_sensitive` | mounted (the default) | `case_sensitive` |
| `nocase` | mounted | `nocase` in place of `case_sensitive`; with both, the last one wins |
| `windows_names` | mounted | `windows_names` |
| `errors=remount-ro` | mounted | `errors=remount-ro` |
| `acl`, `show_sys_files` or `showmeta`, `hide_dot_files`, `nohidden`, `sys_immutable`, `discard`, `disable_sparse`, `native_symlink=rel`, `symlink=native` | mounted | each its own token |
| `uid=`, `gid=`, `umask=`, `fmask=` and `dmask=` | mounted | shown |
| `sparse`, `preallocated_size=` | mounted | not shown |
| `ads`, `ads=0`, `streams_interface=`, `force`, `noacl`, `prealloc`, `user_xattr`, `noacsrules` | refused: "Unknown parameter" | |
| `case_sensitive=1`, `nocase=0`, `nocase=1`, `windows_names=1` | refused: "Unexpected value". They are flags, though the documentation writes `<BOOL>` | |
| `mft_zone_multiplier=2` | refused: any value but 1 on a fresh mount (a bug of its parser, irrelevant here) | |

- `iocharset` is always shown, so Atoll can require `iocharset=utf8` as it does for ntfs3. The default is the kernel's `CONFIG_NLS_DEFAULT` (source): `utf8` in the configurations of Ubuntu's mainline 7.3.1 and Debian 13's 6.12, but nothing makes it so on every kernel.
- `nocase` is the same token as ntfs3's. Atoll's `ignore-case` check also catches it: `<root>` answers in the other case with the same inode.
- The default `umask=00` gives `0777` to what has no mode of its own, where the driver's documentation says files are `rw-------` by default.
- `FS_IOC_GETFLAGS` gives `ENOTTY`, with or without `nocase`: the driver reports no `FS_CASEFOLD_FL`.
- `statfs`: type `0x5346544e` (`NTFS_SB_MAGIC`); block size 4096; `namelen` 255.
- Atoll reads the same identifier as `blkid -p` from the head of the device, and `time_precision_ns` 100: the superblock is NTFS's, whatever the driver.

## Names

The 101 names of `conformance/corpus/names/`, each created as a file and then as a folder in a folder of its own, listed back and found by `lstat` (as `TestNameCorpus` does), on three mounts:

| Corpus verdict on NTFS | Default | `windows_names` | `nocase` |
|---|---|---|---|
| accepted (31 names) | all accepted | all accepted | all accepted |
| `invalid-utf8`, alone or with reasons other than `too-long` (9) | `EILSEQ` | `EILSEQ` | `EILSEQ` |
| `too-long` alone (5) | 4 accepted, with 255 UTF-16 units or fewer; 1 `ENAMETOOLONG`, with 256 | the same | the same |
| `too-long` with another reason (4) | `ENAMETOOLONG` or `EILSEQ` | the same | the same |
| `control-character` alone (4) | accepted | `EINVAL` | accepted |
| `forbidden-character` alone, with one of `" * < > ?` (5) | **`ERANGE`** | `EINVAL` | `ERANGE` |
| `forbidden-character` alone, with one of `: \ \|` (4) | accepted | `EINVAL` | accepted |
| `forbidden-character` with `control-character` or `trailing-dot-or-space` (2) | accepted | `EINVAL` | accepted |
| `trailing-dot-or-space` alone (5) | accepted | `EINVAL` | accepted |
| `reserved-name` alone (29) | accepted | 15 refused; 14 accepted: `CON .txt`, `nul . .x`, `CON  .txt`, `nul   .srt`, `CONIN$`, `CONOUT$`, `conout$.log`, `CONIN$.txt`, `COM¹`, `COM².mkv`, `COM³`, `LPT¹`, `lpt².mkv`, `lpt³.nfo` | accepted |
| `trailing-dot-or-space` and `reserved-name` (3) | accepted | `EINVAL` | accepted |

- **Lengths.** 255 × `é` (510 bytes, 255 units) and 127 × U+1F600 + `a` (509 bytes, 255 units) are accepted; 256 × `a` and 128 × U+1F600 (256 units) give `ENAMETOOLONG`. So the limit is 255 UTF-16 units, as ntfs3's and ntfs-3g's, and the 255-byte limit of §15.8, kept for lowntfs-3g, still binds first.
- **Invalid UTF-8.** An encoded surrogate, a CESU-8 pair, `\xff`, an overlong `/` and a code point past U+10FFFF all give `EILSEQ`. U+FFFF and U+FEFF are accepted. The conversion is the kernel's `utf8s_to_utf16s`, which ntfs3 uses too.
- **`windows_names`** refuses the same set as ntfs3's (`fs/ntfs/namei.c`, `ntfs_check_bad_windows_name`): U+0001–U+001F, `" * / : < > ? \ |`, a trailing space or dot, and `CON PRN AUX NUL COM1-9 LPT1-9` alone or before a `.`. So it misses what §15.8 says ntfs3 misses.
- **`" * < > ?` without `windows_names`**: `ERANGE`, an error no other driver gives for a name. The source's name collation calls exactly these five characters invalid (`ntfs_collate_names`, `fs/ntfs/unistr.c`), and its index code returns `ERANGE` on a collation error. Atoll never writes them to NTFS (§15.8), so this matters only for names already on a disk (below).
- **System files** are hidden from the listing of the top folder but answer by name (`$MFT`, `$UpCase`, `$Volume`, `$Extend`, `$Boot`, `$Bitmap`, `$LogFile`, `$Secure`); writing to `$MFT` gives `EPERM`. Ordinary files named `$MFT` or `hiberfil.sys` below the top are accepted.
- **NFC and NFD.** `été.mkv` in both forms are two files with two inodes, listed with their exact bytes.
- **Across drivers.** The 31 accepted names, written as files by each of `ntfs`, ntfs3 and ntfs-3g, were listed and read back by the two others byte for byte, content included: 0 differences in all six directions.

### Names already on a disk

Names that ntfs-3g wrote, read through `ntfs` and ntfs3:

| Name written by ntfs-3g | `ntfs` | ntfs3 |
|---|---|---|
| `q?.mkv`, `s*.mkv`, `l<g>.mkv`, `d"q.mkv` | listed, `lstat` and read work; `rename` and `unlink` give `ERANGE` | everything works |
| `lone\xed\xa0\x80.mkv` (stored as an unpaired surrogate) | listed as `lone.mkv`, whose `lstat` gives `ENOENT`; the written name gives `EILSEQ` | listed as `lone.mkv`, `ENOENT`; the written name gives `EINVAL` |

So such a file can never reach Atoll's trash on `ntfs`: an operation on it ends with an error that is neither a disk lost nor a name refused. The unpaired surrogate behaves as on ntfs3, which §15.10 already covers (`unreadable`).

### Case and short names

| | Default (`case_sensitive`) | `nocase` |
|---|---|---|
| `Film.mkv`, then `lstat film.mkv`, `FILM.MKV` | `ENOENT` | the same inode |
| create `film.mkv` beside `Film.mkv` | a second file | `EEXIST` |
| `mkdir films` beside `Films` | a second folder | `EEXIST` |
| `renameat2(NOREPLACE)` of another file to `FILM.MKV` | a third name | `EEXIST` |
| plain `rename` of another file to `FILM.mkv` | a fourth name | **replaces `Film.mkv`** |
| `media`, then `lstat MEDIA` | `ENOENT` | the same inode |

- Default: the same as ntfs3 and ntfs-3g, which hold both `Film.mkv` and `film.mkv` (§15.8).
- `nocase` folds every name, also those Linux created in the POSIX namespace, though the source's comment says it does not. It is a variant that changes name rules, like ntfs3 `nocase`.

Short names (8.3), set with ntfs-3g's `system.ntfs_dos_name` on `Film Name.mkv` (`FILMNA~1.MKV`), `Media Library` (`MEDIAL~1`) and `media` (`MEDIA`):

| | `ntfs` | `ntfs` `nocase` | ntfs3 | ntfs-3g |
|---|---|---|---|---|
| listing | long names only | long names only | long names only | long names only |
| `lstat FILMNA~1.MKV` | the file | the file | the file | the file |
| `lstat filmna~1.mkv` | the file | the file | the file | `ENOENT` |
| `lstat FILM NAME.MKV`, `MEDIA LIBRARY` | `ENOENT` | the file | `ENOENT` | `ENOENT` |
| `lstat MEDIA` (the long name is `media`) | `ENOENT` | the folder | `ENOENT` | `ENOENT` |
| create `FILMNA~1.MKV`, `renameat2(NOREPLACE)` or `link` onto `filmna~1.mkv`, `mkdir medial~1` | `EEXIST` each | `EEXIST` each | `EEXIST` each | `EEXIST`, `EINVAL` (the flag), `EEXIST`, created |

- A short name answers whatever its case on `ntfs` and ntfs3, and is never replaced. `MEDIA` does not, though ntfs-3g reads it back as the short name of `media`: ntfs-3g seems to have made `media` one name of both namespaces, which only `nocase` folds. So the `ignore-case` check of §10 reads such a `media` root correctly: Atoll's check gave `online` on ntfs3 and ntfs-3g. A root folder whose short name differs from it only in case was not tried.
- A rename by `ntfs` drops the short name, as its documentation says: after `Film Name.mkv` became `Film Name 2.mkv`, ntfs-3g found no `system.ntfs_dos_name`.
- Measured again on 2026-11-12, by `TestShortNamesOnEachNTFSDriver` on Linux 6.12 and 7.3, with the short names set by ntfs-3g: Atoll's copy of `filmna~1.mkv` beside `Film Name.mkv` (`FILMNA~1.MKV`) was made on ntfs-3g and lowntfs-3g, through `link` after `renameat2` gave `EINVAL`, where the table above says `EEXIST` for `link`. ntfs3 found the other file at the look. So the look before a copy reads a name of the short form in capitals too, and refuses it on every driver before anything is written (`specs.md` §15.8).

## Times

`utimensat` with nanoseconds, read back from the cache, through a cold cache, after a remount, and by ntfs3 and ntfs-3g:

| Set | Every reading by `ntfs` and ntfs3 | ntfs-3g |
|---|---|---|
| 2020, +123456789 ns | +123456700 ns | the same |
| 2020, +50 ns and +99 ns | +0 ns | the same |
| 2020, +100 ns | +100 ns | the same |
| 1969-12-31 23:59:59.999999999 | cached: −1 s +999999900 ns; then `tv_sec` 0 with `tv_nsec` −100 | 0 |
| 1601-01-01, 1600-12-31 23:59:59, 1677, 1901, 2038, 2262, 2300, 9999, 30828-09-14 | the same, to the second | the same |
| year 40000 | `utimensat` succeeds; cached the same; then −644674407370 s | the same, wrapped |

- **Precision**: 100 ns, rounded down, as `time_precision_ns` says (§10): `TestTheTimePrecisionOfEachFilesystem` passes on it. A time set by ntfs3 reads the same through `ntfs`, in every row.
- The second before the epoch reads with a negative `tv_nsec` on both kernel drivers, which `statx` hands out as 4294967196 in its unsigned field. `fsx` turns that into a time 4.29 s late. This is shared with ntfs3, so not a reason to treat `ntfs` differently; it is noted for `fsx`.
- **Birth time**: `statx` reports `STATX_BTIME`, in 100 ns units. A `utimensat`, a write, a `chmod`, a `chown`, a rename, a move to another folder, a `link` and a read left it unchanged, also through a cold cache and after a remount. ntfs3 reads the same value, and ntfs-3g gives it as `system.ntfs_crtime`. `ntfs` has no `system.ntfs_crtime` attribute (`ENODATA`), which Atoll reads only when `statx` has no birth time. So §7 works as on ntfs3.
- **Change time** moves with `utimensat`, a write, `chmod`, `chown`, a rename, a move and a `link`.
- **`link` moves the modification time.** After `link`, the file's modification time was the current time, through a cold cache too; on ntfs3 it stayed 1500000000. The source sets every time of the file in `ntfs_link` (`simple_inode_init_ts`). So a hard link made by the user or an application (Sonarr, Radarr) to a copy on such a disk reads as a metadata change, and §15.4 applies the new time to the other copies.
- Inode reuse: 20 files made right after a removal took 20 new inode numbers (ntfs3 reuses the number every time, §15.1).

## Operations of the copy pipeline and the trash

| Call | Result |
|---|---|
| `renameat2(RENAME_NOREPLACE)` of a file or folder onto an absent name | works |
| the same onto a file, onto an empty folder | `EEXIST` |
| `RENAME_EXCHANGE`, `RENAME_WHITEOUT` | `EINVAL` |
| plain `rename` of a folder onto an empty folder | `EEXIST` (POSIX would replace it; Atoll never needs this here, as the flag works) |
| plain `rename` of a file onto a file | replaces it |
| `link` | works, in the same folder or another; `EEXIST` onto a name; `EPERM` for a folder |
| `fsync` of a file or folder, `fdatasync`, `posix_fadvise(DONTNEED)`, `O_DIRECT` reads, `fallocate`, a `user.` attribute | work |
| `name_to_handle_at`, `fanotify_init` with `FAN_REPORT_FID` or `FAN_REPORT_DFID_NAME`, then a `FAN_MARK_FILESYSTEM` mark | work: the watcher of §15.1 can use fanotify on it |

### Modes

- A file created with mode 0644 reads 0644, one created with 0600 reads 0600, a folder made with 0755 reads 0755: the driver writes the mode as WSL extended attributes (`$LXMOD`, `$LXUID`, `$LXGID`, which `llistxattr` shows).
- `chmod` 0640, 0600, 0755, 0444 and 0000, and `chown 1000:1000`, held through a cold cache, a remount, and through ntfs3. ntfs-3g ignores these attributes and reads every one as 0777, root.
- `chmod 4755` holds 0755: setuid, setgid and sticky are dropped. A mode with no write bit sets the read-only attribute (`system.ntfs_attrib` 0x1, as ntfs-3g reads it).
- In Atoll's own suite, a mode applied by a pass (§15.4) reads back exactly ("ntfs holds 0600 for 0600").

### After a change of driver

The same files, written by each driver with `chmod` and a time, read by each driver (a disk moving from Debian 13 with ntfs3 to Debian 14 with `ntfs`):

| | Written by ntfs3 or `ntfs` | Written by ntfs-3g (no WSL mode) |
|---|---|---|
| inode, size, modification and change times, birth time | the same through all three drivers | the same |
| mode through ntfs3 | as set | 0755 |
| mode through `ntfs`, default | as set | **0777** |
| mode through `ntfs`, `umask=022` | as set | 0755 |
| mode through ntfs-3g | 0777 | 0777 |

So a copy stays `there` (§15.10) across the change: identity and times do not move, and nothing is decided. But for a file Windows or ntfs-3g wrote, the walk now sees another mode than `copies.mode` holds. The next change of that copy, a `touch` for example, moves its change time, and §15.4 then applies 0777 to the other copies as a `chmod` the user made. `umask=022` makes `ntfs` read such files as ntfs3 does. Folders differ the same way, and also in `st_size` (the index size on `ntfs`, 0 or 4096 on ntfs3), which Atoll does not record for folders.

## Power cuts and failing devices

### Durability

A copy of the image taken right after the calls, mounted read-only by ntfs-3g and by `ntfs` (they agreed except where noted). The same sequence ran on ntfs3 as a control:

| Sequence | `ntfs` | ntfs3 |
|---|---|---|
| create, write 1 MiB, no fsync | absent | not run |
| create, write, `fsync` the file and its folder | present, content right | not run |
| rename `tmp/x` → `d/x`, `fsync` the target folder only | both names, each with one link | the same |
| the same, `fsync` the file at its new name only | both names, each with one link | the same |
| the same, `fsync` the file, the target folder, then the source folder (Atoll's order, §14.2) | `d/x` only | the same |
| the same, `fsync` the source folder only | **neither name** | **neither name** |
| the same, `fsync` the target folder then the source folder, not the file | **neither name** (`ntfs` reads the target name as `EIO`) | `d/x` only |
| `link`, `fsync` the new name and its folder | both names, two links | not run |
| `link` then `unlink` the old name, each synced | the new name, with two links recorded | not run |
| `utimensat`, `chmod`, `fsync` the file | both kept | not run |
| `mkdir`, `fsync` it and its parent | present | not run |
| `mkdir`, `fsync` the parent only | absent (`ENOENT` or `EIO`) | absent (`ENOENT` or `EIO`) |
| `unlink`, `fsync` the folder | gone; once, ntfs-3g read the name as `EIO` | gone |
| a file written and `fsync`ed, then 40 s with no `sync` | present; the volume is marked dirty (ntfs3 refuses it without `force`) | present; the volume reads clean |

- The driver keeps no journal: it empties `$LogFile` at a read-write mount (source). A move is durable only once the moved file and both folders are synced, and syncing the source folder before the rest loses the file in a cut. Atoll syncs the file, then the target, then the source (§14.2, §17.1), which is safe on both drivers. In the sequence without the file's own sync, ntfs3 kept the file and `ntfs` did not; Atoll already makes that sync (`SyncEntry`).
- A `sync` (`syncfs`) clears the volume's dirty flag on the disk, and the next change sets it again, written with the next writeback (source: `ntfs_sync_fs`). So a disk dropped soon after a `sync` reads clean, though writes after that `sync` may be half done.

### A device that fails

dm-error under a mounted volume, as `TestDeviceFailures` does, with `errors=continue` (the default) and `errors=remount-ro`, then a `scsi_debug` disk deleted as a USB unplug:

| | `ntfs` | ntfs3 |
|---|---|---|
| right after the failure, from the cache: `mkdir`, create, `link`, `unlink`, rename | succeed | `mkdir`, create and rename succeed |
| `fsync` of a new or moved file | `EIO` | `EIO` |
| `fsync` of a folder | `EIO`, twice in a row; then **0** once a file's `fsync` has given `EIO` (seen in both orders) | `EIO` every time |
| `syncfs` | `EIO` | 0 |
| through a cold cache: `lstat` of the media folder, of a file, of an absent name, `readdir`, read, `mkdir`, create | `EIO`, never `ENOENT`; after the unplug too | not run |
| `statfs` | works | not run |
| superblock options | unchanged with `errors=continue`; `ro` from the first write error with `errors=remount-ro` | |
| Atoll's mount check and its confirmation | before the failure `readonly`/`unsupported-driver`; through a cold cache `missing`/`device-unreadable`; after the unplug `missing`/`device-gone` | |

- Invariant 4 holds: a failing device gave `EIO`, never `ENOENT`, and the checks of §10 found the disk `missing`.
- A folder's `fsync` that answers 0 on a dead device is a weaker guarantee than ntfs3's. Atoll stops at the first `EIO` of a session, and every failure point of its own suite got one (below), but a later folder sync does not repeat the error.
- After the device came back, the volume mounted again on ntfs3 and `ntfs`, holding what was synced before the failure.

## Dirty and hibernated volumes

A `hiberfil.sys` at the top of a volume made by ntfs-3g stands for Windows hibernated. Two ways of mounting: a fresh copy of the image for each driver, or one image mounted by ntfs3, `ntfs`, `ntfs` with `errors=remount-ro` and ntfs-3g in turn, each writing a file:

| Volume | ntfs3 | `ntfs`, default (`errors=continue`) | `ntfs`, `errors=remount-ro` | ntfs-3g |
|---|---|---|---|---|
| `hiberfil.sys` of 10 bytes, a fresh copy each | **read-write** | **read-write** | read-only, "Windows is hibernated" | not run |
| `hiberfil.sys` of 100 bytes, or of 8 KiB starting with `hibr` or `HIBR`, in turn | **read-write** | **read-write** | read-only, "Volume is dirty": the mount before left it so | read-only |
| `hiberfil.sys` of 8 KiB of zeros, in turn | read-write | read-write | read-write | read-write |
| marked dirty (by `ntfs`, below), a fresh copy each | refused | **read-write** | read-only, "Volume is dirty" | read-write |

- The source makes a hibernated, dirty or unreadable-journal volume read-only only under `errors=remount-ro`; under the default it carries on, read-write (`load_system_files`, `fs/ntfs/super.c`). Writing to a volume that Windows left hibernated, including by Fast Startup, loses what Windows writes back when it resumes.
- After a read-write mount of a hibernated volume, `ntfs` left the volume marked dirty at unmount ("Volume has errors. Leaving volume marked dirty"). That is how the last row's volume was made.
- With `errors=remount-ro`, a hibernated or dirty disk shows `ro` in its superblock options, which Atoll already reads as `readonly`/`read-only-mount` (§10).
- §22 says Atoll never forces the mount of a dirty volume. With `ntfs` and its default, the mount is forced without anyone asking. ntfs3 also writes to a hibernated volume: that gap is not new.

## `windows_names` and the trash

A volume holding `a:b.mkv`, `c:d.mkv` and a folder `D:x`, written without `windows_names`, then mounted with it:

| | `ntfs` | ntfs3 | ntfs-3g |
|---|---|---|---|
| `lstat`, read `a:b.mkv` | work | work | work |
| `renameat2(NOREPLACE)` to `trash/a:b.mkv` | `EINVAL` | `EINVAL` | `EINVAL` (the flag) |
| `renameat2(NOREPLACE)` to `trash/ok.mkv` | works | works | `EINVAL` (the flag) |
| `link` `c:d.mkv` → `trash/c:d.mkv`, `link` a good name → `trash/x:y` | **work** | `EINVAL` | `EINVAL` |
| `unlink c:d.mkv`, `rmdir D:x` | **`EINVAL`** | work | work |
| create `e:f` | `EINVAL` | `EINVAL` | `EINVAL` |

- The source checks `windows_names` in `create`, `mkdir`, `rename`, `unlink`, `rmdir`, `symlink` and `mknod`, and not in `link`.
- Atoll reads `EINVAL` from `renameat2` as maybe the flag, and lets `link` decide (`IsFlagRefused`, §14.2). On `ntfs` the link succeeds, so the copy gets a second name in the trash, and the `unlink` of the old name then fails. In Atoll's own suite, `atoll prune` of such a copy ended `io-error`, with its operation left `planned` and the trash not empty, where ntfs3 and ntfs-3g end `name-refused` with the operation `undone` (`TestPruneOfARefusedName`). Nothing is lost: both names hold the file.
- Without `windows_names`, none of this applies, but the driver then accepts most names Windows refuses (above).

## Atoll's own suite on `ntfs`

The scratch copy ran these `test/loopdev` tests on `ntfs`, with no other option: `TestNameCorpus` (with and without `windows_names`), `TestTheTimePrecisionOfEachFilesystem`, `TestARepriseOfATimeOnEachDriver`, `TestAPassAppliesAModeOnEachDriver`, `TestARepriseOfAModeOnEachDriver`, `TestAFailedMountIsAMissingDisk`, `TestRenamesOnEachDriver`, `TestRenamesFoundOnEachDriver`, `TestDeletionsOnEachDriver`, `TestADeviceThatFailsDuringARename`, `TestADeviceThatFailsDuringAFolderRename`, `TestReplicateOnEachDriver`, `TestAPassCopiesOnEachDriver`, `TestAPassRefreshesOnEachDriver`, `TestAPassAppliesATimeOnEachDriver`, `TestADeviceThatFailsDuringACopy`, `TestScanOnEachDriver` (birth time known), `TestScanOfAFailingDisk`, `TestPruneOnEachDriver`, `TestPruneByHandlesPastEveryLimit`, `TestPruneOfARefusedName`, `TestConfirmationOfAFailingDevice`, `TestADeviceThatFailsDuringAnOperation` and `TestADeviceThatFailsDuringARestore`.

All passed but four:

- `TestADeviceThatFailsDuringACopy`, `…DuringAnOperation` and `…DuringARestore`, each at the point `target-synced`: the device fails after the target folder's sync and before the source's. The reprise found "a name and its file's record disagree" and left the operation `planned`, as §17.2 says it must. These tests already exempt ntfs3 from their final checks for exactly this (`skipWhereNamesDisagree`); they do not name `ntfs`.
- `TestPruneOfARefusedName`: the `windows_names` difference above.

## Decisions for the maintainer

1. **Support level.**
   - **A. Keep it `readonly`** (today). No spec change. A Debian 14 user writes to NTFS through ntfs-3g, which `mount -t ntfs` reaches when the `ntfs-3g` package is installed: FUSE, no `RENAME_NOREPLACE` (link and unlink), birth time from an attribute. §22 could say that Debian 14 has no ntfs3.
   - **B. Support it like ntfs3.** `online` when the superblock options hold `iocharset=utf8` and not `nocase`, `readonly` with the existing reasons `iocharset` and `nocase` otherwise, so the closed list of reasons (§29.3) does not change. Touches §10 (the second table: `unsupported-driver` no longer for `ntfs`; `nocase` and `iocharset` for the types `ntfs3` and `ntfs`), §15.8 (refused variants, `windows_names` recommendation), §22 (the NTFS row and the paragraph on the type `ntfs`), `conformance/vectors/disk_check.json` (the rows of type `ntfs`, and new ones), `judgeWrites`, and `test/loopdev` (the driver lists, with ntfs3's exemptions for a name and a record that disagree). Points 2 to 6 then need an answer.
   - **C. In between**: B, plus `readonly` unless `errors=remount-ro` (point 2), or `readonly` until upstream fixes `link` and `windows_names` (points 3 and 4).
2. **`errors=continue`.** Require `errors=remount-ro` (a new reason, for example `errors-continue`: §10, the closed list of §29.3, the `reason` enums of `api/schemas/daemon`, `disk-list` and `scan`), or only recommend it in §22, or leave it. Required, a hibernated or dirty volume would show `ro` and be `readonly` through the existing `read-only-mount`.
3. **Hibernated volumes on any driver.** ntfs3 and `ntfs` by default both mount them read-write. Should a mount check also make an NTFS disk `readonly` when it has a `hiberfil.sys` at its top that is smaller than 4 KiB or whose first 4 KiB are not all zeros, the rule of `ntfs`'s source (a new reason, §10, §29.3)? This would be the only check that reads a file of the disk, and it concerns ntfs3 as much as `ntfs`.
4. **`windows_names` with `link`.** On `ntfs`, the flag of `renameat2` is always supported, so `EINVAL` from it is a refused name. Options: treat `EINVAL` from `renameat2` as `name-refused` on the type `ntfs` without trying `link` (§14.2 step 6, §17.1, `IsFlagRefused`); or recommend no `windows_names` with `ntfs` (§15.8, §22); or keep the planned operation as it is. A copy already under a refused name cannot be removed under `windows_names` either way.
5. **Modes after a change of driver.** Recommend `umask=022` for `ntfs` in §22, so it reads what ntfs3 reads; and/or let a change of a disk's mount type re-read every copy's mode into `copies.mode` without applying it (§10, §15.4), as for a difference already there.
6. **`link` and the modification time.** An upstream bug (ntfs3 does not do it). Report it upstream, and document in §22 that a hard link on such a disk is applied to the other copies as a change of time.
7. **Tests.** Whatever the choice, add `ntfs` to `test/loopdev`, and consider booting Debian sid's 7.3.3 in `test/qemu` beside Ubuntu's mainline, as it is the kernel without ntfs3.

The author's suggestion, for what it is worth: C, with `errors=remount-ro` required (point 2), `EINVAL` from `renameat2` read as a refused name on `ntfs` (point 4), `umask=022` recommended (point 5), the `link` bug reported upstream (point 6), and point 3 decided apart, as it concerns ntfs3 already.

## Not measured

- Volumes that Windows wrote: names in the Win32 namespace with real short names, a real hibernation or Fast Startup, a `$LogFile` that Windows left unclean (the source empties it at a read-write mount), compressed or encrypted files, sparse files.
- mergerfs over `ntfs`, and the renames through it of §15.2.
- Debian's own 7.3.3 kernel, `acl`, performance, large files, long runs.
