# Name compatibility across filesystems

This report backs the name rules of `specs.md` §15.8. It is research, not a contract: the spec, the name corpus (`conformance/corpus/names/`) and the vectors (`conformance/vectors/name_compat.json`) are the contract.

Atoll copies a file to another disk at the same relative path, byte for byte, and never renames it. So before every copy it must know whether the target filesystem can hold the name, and whether every system that will read the disk later can open it. The question here: which names can each pool filesystem hold, for Linux now and for the Windows version later?

## Summary

- ext4, XFS and btrfs hold any component of 1 to 255 bytes, except `/` and NUL. Invalid UTF-8, control characters, `:`, a trailing dot and `CON` all work. Measured on Linux 6.18 with loop images.
- The Linux NTFS drivers hold almost anything, but Windows does not. Without `windows_names`, ntfs3 and ntfs-3g accept `:`, `*`, control characters, a trailing dot and `CON`, and Windows cannot open such names. Even with `windows_names`, both drivers accept `CON .txt`, `COM¹`, `LPT³.nfo` and `CONIN$`.
- ntfs3 on Linux 7.3 reads `a:b` as the stream `b` of the file `a`, through its new `ads` option, which is on by default. Measured on 7.3.1, re-reading after dropping the caches:
  - creating `a:b` when `a` exists adds the stream; the content of `a` stays;
  - a rename onto `m:n` replaces the content of the file `m`, also through mergerfs, where it reaches a file with no other copy;
  - `windows_names` stops none of this, and `ads=0` stops all of it.

  So Atoll must never write a `:` to NTFS, and it refuses `ads`.
- Windows 10 reserves device names even with an extension (`CON.txt`, `NUL.tar.gz`, `CON .txt`). Windows 11 no longer reserves a device name followed by an extension, according to Microsoft's .NET documentation; its exact rule is not established, and secondary sources report that Explorer and the SMB client still refuse some bare names. Microsoft removed `COM0` and `LPT0` from its list in April 2025. The superscripts `¹ ² ³` count as port digits.
- Names that are not valid UTF-8 cannot go to NTFS safely: ntfs3 refuses encoded surrogates, ntfs-3g stores them, and ntfs-3g silently rewrites a CESU-8 pair, which changes the name.
- lowntfs-3g refuses components of more than 255 bytes, and mount information cannot tell it apart from ntfs-3g.
- A UTF-16 length is never more than the UTF-8 length for any character (checked over every scalar value). So on NTFS the 255-byte limit always binds before the 255-unit limit.

## Decisions

The maintainer chose (§15.8):

| Point | Decision |
|---|---|
| Which NTFS disks follow the Windows rules | All of them, in both versions. A name like `a:b.mkv` on ext4 never gets an NTFS replica. |
| Reserved names | The Windows 10 rule, the strictest: the part before the first `.`, without trailing U+0020, compared ignoring the case of ASCII letters, against `CON PRN AUX NUL CONIN$ CONOUT$ COM1-9 COM¹²³ LPT1-9 LPT¹²³`. |
| Variants (ext4 `casefold`, XFS `ascii-ci`, ntfs3 `nocase`, lowntfs-3g `ignore_case`) | Refused at `disk add`; a case-insensitive folder found later makes the disk name-incompatible below it. On NTFS, at most 255 bytes per component as well, for lowntfs-3g. |
| Verdicts and corpus | Every component is checked, folders included. Reasons form a closed list: `too-long`, `invalid-utf8`, `control-character`, `forbidden-character`, `trailing-dot-or-space`, `reserved-name`. The corpus is one JSON file in the vector format. |
| Case collisions on NTFS targets (§15.7) | A hard constraint of the solver, checked with `lpath_key` at every component against the target's existing and planned names. `entries` gets an `lpath_key` column, so folders are looked up the same way as files. |
| Renames to a name a disk refuses (§15.3) | The copy left at the old name goes to the trash with the reason `renamed`, which is purged as late as `deleted`. A copy changed meanwhile stays, as a `conflict`. The solver looks for another compatible disk; failing that, `name-incompatible` or `name-collision` and an alert. The mass-deletion guard counts these copies. |
| `windows_names` | Still recommended, with its cost stated in §15.8. |
| ntfs3 `ads` (§10) | Refused at `disk add`. Found later at a mount check, the disk becomes `readonly`, with an alert. The user mounts with `ads=0`. |
| Chunks in the trash (§17) | `Atoll/trash/<date>/<op-id>.chunks/<logical path>/<index>`, with the index in decimal, from 0, with at least 8 digits. |
| Path past the limit of a mergerfs branch (§15.8) | A hard constraint of the solver: Atoll never writes such a copy. Another disk, failing that `name-incompatible` and an alert. A copy already past the limit is only reported. |
| Path past 259 UTF-16 units (§15.8) | Only listed by `atoll names check`: no state, no alert, no effect on placement. |
| `atoll names check` (§15.9) | One finding per name to fix, the shortest path with the problem, in a flat sorted list. Always checks `ext4` and `ntfs`, whatever the pool holds. Exit code 0 whenever it ran. |
| The state a file shows (§4) | The first that applies: `corrupt`, `conflict`, `unavailable`, `name-incompatible`, `name-collision`, `degraded`, `pending`, `unprotected`, `ok`. Placement and job order read the copies, never the state. |

## Linux filesystems

Measured on the sandbox kernel 6.18 with loop images (ext4, XFS, btrfs) and FUSE (ntfs-3g and lowntfs-3g 2022.10.3). ntfs3 and ext4 `casefold` were measured in qemu with the Ubuntu mainline kernels 7.2 and 7.3.

| Filesystem | Length | Bytes refused | Notes |
|---|---|---|---|
| ext4 | 255 bytes (`EXT4_NAME_LEN`, `fs/ext4/ext4.h`) | `/`, NUL | `casefold` folders compare after NFD, full case folding and removal of default-ignorable code points (Unicode 12.1). A variant name opens the other file, and a case-only rename does nothing. |
| XFS | 255 bytes (`MAXNAMELEN` 256 with the NUL, `fs/xfs/libxfs/xfs_types.h`) | `/`, NUL | `ascii-ci` folds ASCII and Latin-1 bytes only. It is deprecated: off by default from September 2025, to be removed in 2030. |
| btrfs | 255 bytes (`BTRFS_NAME_LEN`) | `/`, NUL | A directory item full of names with the same crc32c gives `EOVERFLOW` (source only): a transient write error, not a name verdict. |
| ntfs3 | 255 UTF-16 units | invalid UTF-8 (with `iocharset=utf8`) | `windows_names` (Linux 6.2 and later) refuses control characters, `\ / : * ? " < > \|`, a trailing dot or space, and `CON PRN AUX NUL COM1-9 LPT1-9` alone or before a `.` (`fs/ntfs3/fsntfs.c`). `nocase` folds through `$UpCase`. `ads` (7.3) splits names at `:`; it shows in `/proc/mounts` when on, and `ads=0` or `ads=no` turns it off (`ads=off` and `noads` are refused, and Linux 6.12 refuses `ads` in any form). |
| ntfs-3g | 255 UTF-16 units | invalid UTF-8, except encoded surrogates | `windows_names` refuses the same set as ntfs3 (man page `ntfs-3g.8`). A CESU-8 pair is stored as one supplementary character. `RENAME_NOREPLACE` gives `EINVAL`, so the final rename uses `link` and `unlink`. |
| lowntfs-3g | 255 bytes and 255 UTF-16 units | as ntfs-3g | `strlen(name) < 256` in `src/lowntfs-3g.c`. |

`FS_IOC_GETFLAGS` reports `FS_CASEFOLD_FL` for ext4 `casefold` folders and for ntfs3 `nocase`. It does so for XFS `ascii-ci` only from Linux 7.2. Before that, `xfs_info` shows `ascii-ci=1`.

ntfs-3g 2022.10.3, with and without `windows_names`, holds `Film.mkv` beside `film.mkv` and `Films/` beside `films/`, and renames a third file to `FILM.MKV` beside them. So the driver does not protect against case collisions, and `windows_names` does not either. Measured on an image file in a container (Linux 6.6, WSL2). ntfs3 does the same, with and without `windows_names`, on Linux 6.12.115 and 7.3.1, measured in qemu.

## mergerfs

What happens when one branch refuses a rename? Measured with mergerfs 2.40.2-5 (Debian 13), ntfs-3g 2022.10.3 and ext4, on image files in a container (Linux 6.6, WSL2), with the settings of §21.1. Each case ran with the ext4 branch first and with the NTFS branch first; the order changed nothing. ntfs3 on Linux 6.12.115 (Debian 13), and on 7.3.1 with `ads=0`, gave the same results in qemu.

| Rename through the merged view | `windows_names` | The application sees | ext4 branch | NTFS branch | Merged view |
|---|---|---|---|---|---|
| File on both, to `f:x.mkv` | yes | success | new name | **copy removed** | new name |
| File on both, to `f:x.mkv` | no | success | new name | new name, which Windows cannot open | new name |
| File on both, to a name with the byte 0xFF | yes and no | success | new name | **copy removed** | new name |
| Folder on both, to `D:x` | yes | success | new name | old folder, with its files | **both folders** |
| Folder on both, to `D:x` | no | success | new name | new name | new name |
| File only on NTFS, to `f:x.mkv` | yes | `EINVAL` | no file | unchanged | old name |
| File only on NTFS, to `f:x.mkv` | no | success | no file | new name | new name |
| File on both, to `OTHER.MKV`, with `Other.mkv` only on NTFS | yes and no | success | new name | both names | both names |
| Control: file on both, to a name both accept | yes and no | success | new name | new name | new name |

The [mergerfs documentation](https://github.com/trapexit/mergerfs/blob/master/mkdocs/docs/config/rename_and_link.md), read from its main branch, states the rule: one successful rename makes the whole rename a success, the source is then removed from the branches that failed, and a removal that fails does so silently. So a folder that is not empty stays. The page mentions no option that changes this.

So the last copy of a file is not lost in these cases: when every branch refuses, nothing changes. But a replica can vanish with no trash and no error to the application. Not measured: newer mergerfs releases.

### ntfs3 with `ads`

`ads` is worse, because the driver accepts the name. Measured in qemu on Ubuntu's mainline Linux 7.3.1, with `ads` on by default, and the same with and without `windows_names`. Each read below came after `drop_caches`.

| Rename | The application sees | NTFS branch afterwards | Merged view |
|---|---|---|---|
| Through the view: `x.mkv` on both, to `m1:n`, where `m1` is another file with its only copy on NTFS | success | `m1` holds the content of `x.mkv`; **its own content is gone** | `m1`, with the wrong content, and `m1:n` |
| Through the view: `y.mkv` on both, to `m2:n`, where `m2` is another file on both | success | `m2` holds the content of `y.mkv`, with a new inode and a new change time; the ext4 copy is intact | `m2` and `m2:n` |
| Through the view: `f.mkv` on both, to `f:x.mkv`, with no file `f` | success | a file `f`, with the content | `f` and `f:x.mkv` |
| Through the view: `g.mkv` only on NTFS, to `g:x.mkv`, with no file `g` | success | a file `g`, with the content | `g` |

- A plain read straight after the rename still shows the old content of `m`: it comes from the page cache. The first run of this measurement read that and wrongly found `m` intact. The damage shows after `drop_caches` or a remount.
- The second row is a trap for §15.4: a copy with new content and a moved change time looks like a user's modification, which would send the good copies to the trash and refresh them from the bad one. This is deduced from the spec; there is no code to test yet.
- A real name `a:b.mkv` written by ntfs-3g is listed, but `stat` and `rename` give `ENOENT` under `ads`.
- With `ads=0`, Linux 7.3.1 behaves as 6.12: `m` stays intact in all these cases, and the table above applies.

Method: a Debian 13 container with no KVM boots each kernel in qemu, from an initramfs built from the container's own files, on image files in the guest's RAM. A container alone cannot do this, as it shares Docker Desktop's kernel (Linux 6.6, WSL2), which has no ntfs3.

## Total path length

### mergerfs

Measured with mergerfs 2.40.2-5 and ext4, on image files in a container (Linux 6.6, WSL2), with the settings of §21.1. The path was walked one folder at a time, so `PATH_MAX` never applied to the caller. Disk A has a prefix of 11 bytes, and disk B of 212. The file fits on A and not on B.

| `<mount>/<root>/<path>` | Result through the view |
|---|---|
| 4095 bytes, only copy on A | listed, `stat` and read work |
| 4096 bytes, only copy on A | listed, `stat` gives `ENOENT` |

| Copies, past the limit on B only | Result through the view |
|---|---|
| On both, A first | served from A |
| On both, B first | served from A: mergerfs moves on to the next branch |
| Only on B, as when A has died | listed, `stat` gives `ENOENT` |
| Only on B, with its folder past the limit too | the folder cannot be entered: `ENOENT` |
| On both, `unlink` | succeeds, removes A's copy, leaves B's. The file stays listed, and `stat` gives `ENOENT` |
| On both, `rename` | succeeds, renames A's copy, leaves B's. The view lists both names |

The error is `ENOENT`, not `ENAMETOOLONG`, so through the view nothing distinguishes such a copy from a missing file. An application has its own limit on `<view mount>/<path>`, also 4095 bytes in one string; Atoll does not check that one.

### Windows

Measured on Windows 11, build 26300, with `LongPathsEnabled` = 1, in a temporary folder on the system disk, on files with an exact full-path length:

| Program | 259 units | 260 units | 300 units |
|---|---|---|---|
| `attrib`, `findstr`, `certutil -hashfile`, `fc`, `xcopy`, `Scripting.FileSystemObject` | works | fails | fails |
| `cmd /c type`, `cmd /c more <`, PowerShell 5.1 and 7, through `[IO.File]` | works | works | works |

So 259 is exact, and `LongPathsEnabled` does not lift the limit for programs that do not declare long-path support. Not measured: Explorer, a mapped network drive, and a machine with the setting off.

## The trash and long or refused names

Measured on ext4, ntfs3 (Linux 6.12.115 and 7.3.1) and ntfs-3g, with `windows_names` on NTFS:

| Layout for overwritten chunks | Name of 248 bytes | Name of 249 bytes | ASCII name of 255 bytes | 127 × `é` + 1 letter (255 bytes) |
|---|---|---|---|---|
| `<op-id>/<logical path>.chunks/<index>` | works | `ENAMETOOLONG` | not possible | not possible |
| `<op-id>/<logical path>/<index>` | not run | not run | works | not run |
| `<op-id>.chunks/<logical path>/<index>` | not run | not run | works | works |

A copy under a name its driver refuses cannot reach the trash under that name. With `a:b.mkv` written before the disk was mounted with `windows_names`, the file can still be listed and read, but `rename` into `Atoll/trash/` and `mkdir` under that name give `EINVAL` on ntfs-3g and on ntfs3 (6.12, and 7.3.1 with `ads=0`). Without `windows_names`, the rename works.

## Windows

Sources:
- Microsoft's [Naming Files, Paths, and Namespaces](https://learn.microsoft.com/en-us/windows/win32/fileio/naming-a-file), read from its source in [MicrosoftDocs/win32](https://github.com/MicrosoftDocs/win32/blob/docs/desktop-src/FileIO/naming-a-file.md) and its git history;
- the .NET article on file path formats;
- Wine's `RtlIsDosDeviceName_U` (`dlls/ntdll/path.c`) and its 44-row test table, which Wine also runs on real Windows;
- ReactOS `sdk/lib/rtl/path.c`;
- Go's `internal/filepathlite/path_windows.go` and CPython's `ntpath.py`.

No real Windows machine was available for these rules. The maintainer's machine has since been used for the path lengths, under "Total path length".

- Forbidden in names: U+0000–U+001F and `< > : " / \ | ? *`. U+007F is allowed.
- A name must not end with a space or a period: Win32 path parsing strips them. Names made only of dots or spaces cannot be opened through Win32.
- At most 255 UTF-16 units per component. `MAX_PATH` is 260 units, including the terminator, for applications without long-path support; `\\?\` allows about 32,767.
- Device names:
  - On Windows 10 and earlier, the last component is cut at its first `.` or `:`, trailing spaces are removed, and the rest is compared without case with `AUX CON NUL PRN`, `COM` or `LPT` followed by a digit 1 to 9 (or `¹ ² ³`), and, from Windows 8, `CONIN$` and `CONOUT$`.
  - A port of Wine's algorithm reproduces all 44 rows of Wine's table.
  - Windows 11 no longer treats a device name followed by an extension as a device: the .NET article says the old rule applies "prior to Windows 11". The exact Windows 11 algorithm and its first build are not established.
- Microsoft's list history:
  - `COM0` and `LPT0` were added in November 2022 and removed in April 2025 (commit `63e70903`: "Fix COM0 and LPT0 incorrectly listed as reserved").
  - The superscripts were added in October 2023.
  - Wine's row `c:\lpt0.txt` is not a device on any tested version.
- NTFS compares names without case through `$UpCase`. Per-folder case sensitivity exists since Windows 10 build 17107, off by default.

The rule Atoll keeps (§15.8) is the Windows 10 superset, as a fixed table. The Rust version must not call `RtlIsDosDeviceName_U` or `GetFullPathName`, whose answers depend on the Windows version.

## Verification

- An independent Python implementation of the §15.8 rules, written from the spec text, generated the corpus (101 names) and the whole-path vectors (27 cases).
- The Go implementation (`internal/names/compat.go`) agrees with it on the corpus, on the vectors, and on 300,000 random names mixing ASCII specials, reserved-name pieces, superscripts, NBSP, fullwidth forms and invalid UTF-8: no disagreement.
- Mutation checks: removing `CONIN$`, the superscripts, the space trimming, the trailing-space rule, U+001F, the byte limit on NTFS or the ASCII case folding each makes the tests fail. A later review found mutants that survived (trimming one space only, Unicode uppercase, dropped port digits, reserved names checked only on valid UTF-8); the corpus now pins each of them.
- `sudo make test-loopdev` creates every name the corpus accepts on real ext4, XFS, btrfs and NTFS loop images, as a file and as a folder, each in a fresh folder, and checks the listing byte for byte and `lstat`. On ext4, XFS and btrfs it also checks that every refused name is refused. NTFS runs on every driver present (ntfs3, ntfs-3g, lowntfs-3g), with and without `windows_names`. In this sandbox it passed on ext4, XFS, btrfs, ntfs-3g and lowntfs-3g; ntfs3 is not in this kernel.
- A second review replayed the corpus on ntfs3 (Linux 7.2 and 7.3, with and without `windows_names` and `ads`), ntfs-3g and lowntfs-3g: every name the corpus accepts was created, listed, found by `stat`, and read back by the other drivers. Two spec-only ports agreed with Go on 5.4 million names and paths. The review also found lowntfs-3g `-o ignore_case`, which lists names in lowercase and is invisible in mount information: it is now a refused variant.

## Open questions

These are listed in `specs.md` §30:

- **Renames through Atoll's own view (phase 2a).** Should a rename that a copy's filesystem refuses fail with an error, which mergerfs cannot do, instead of following §15.3?
- **8.3 short names.** Both Linux drivers refuse to create a name equal to another file's short name. ntfs-3g's `stat` of that name returns the other file. So existence on NTFS must never be decided by `stat` alone.
- **Mount checks.**
  - ntfs3 must run with `iocharset=utf8`.
  - A listed entry whose `lstat` fails is a name the driver cannot read: ntfs3 drops unpaired surrogates written by Windows or ntfs-3g. It must never count as a missing file.
