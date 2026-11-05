# Name compatibility across filesystems

This report backs the name rules of `specs.md` §15.8. It is research, not a contract: the spec, the name corpus (`conformance/corpus/names/`) and the vectors (`conformance/vectors/name_compat.json`) are the contract.

Atoll copies a file to another disk at the same relative path, byte for byte, and never renames it. So before every copy it must know whether the target filesystem can hold the name, and whether every system that will read the disk later can open it. The question here: which names can each pool filesystem hold, for Linux now and for the Windows version later?

## Summary

- ext4, XFS and btrfs hold any component of 1 to 255 bytes, except `/` and NUL. Invalid UTF-8, control characters, `:`, a trailing dot and `CON` all work. Measured on Linux 6.18 with loop images.
- The Linux NTFS drivers hold almost anything, but Windows does not. Without `windows_names`, ntfs3 and ntfs-3g accept `:`, `*`, control characters, a trailing dot and `CON`, and Windows cannot open such names. Even with `windows_names`, both drivers accept `CON .txt`, `COM¹`, `LPT³.nfo` and `CONIN$`.
- ntfs3 on Linux 7.3 reads `a:b` as the stream `b` of the file `a`, through its new `ads` option, which is on by default. Measured:
  - creating `a:b` when `a` exists writes into `a`;
  - a rename onto `m:n` replaces the file `m`;
  - `windows_names` stops none of this.

  So Atoll must never write a `:` to NTFS.
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

## Linux filesystems

Measured on the sandbox kernel 6.18 with loop images (ext4, XFS, btrfs) and FUSE (ntfs-3g and lowntfs-3g 2022.10.3). ntfs3 and ext4 `casefold` were measured in qemu with the Ubuntu mainline kernels 7.2 and 7.3.

| Filesystem | Length | Bytes refused | Notes |
|---|---|---|---|
| ext4 | 255 bytes (`EXT4_NAME_LEN`, `fs/ext4/ext4.h`) | `/`, NUL | `casefold` folders compare after NFD, full case folding and removal of default-ignorable code points (Unicode 12.1). A variant name opens the other file, and a case-only rename does nothing. |
| XFS | 255 bytes (`MAXNAMELEN` 256 with the NUL, `fs/xfs/libxfs/xfs_types.h`) | `/`, NUL | `ascii-ci` folds ASCII and Latin-1 bytes only. It is deprecated: off by default from September 2025, to be removed in 2030. |
| btrfs | 255 bytes (`BTRFS_NAME_LEN`) | `/`, NUL | A directory item full of names with the same crc32c gives `EOVERFLOW` (source only): a transient write error, not a name verdict. |
| ntfs3 | 255 UTF-16 units | invalid UTF-8 (with `iocharset=utf8`) | `windows_names` (Linux 6.2 and later) refuses control characters, `\ / : * ? " < > \|`, a trailing dot or space, and `CON PRN AUX NUL COM1-9 LPT1-9` alone or before a `.` (`fs/ntfs3/fsntfs.c`). `nocase` folds through `$UpCase`. `ads` (7.3) splits names at `:`. |
| ntfs-3g | 255 UTF-16 units | invalid UTF-8, except encoded surrogates | `windows_names` refuses the same set as ntfs3 (man page `ntfs-3g.8`). A CESU-8 pair is stored as one supplementary character. `RENAME_NOREPLACE` gives `EINVAL`, so the final rename uses `link` and `unlink`. |
| lowntfs-3g | 255 bytes and 255 UTF-16 units | as ntfs-3g | `strlen(name) < 256` in `src/lowntfs-3g.c`. |

`FS_IOC_GETFLAGS` reports `FS_CASEFOLD_FL` for ext4 `casefold` folders and for ntfs3 `nocase`. It does so for XFS `ascii-ci` only from Linux 7.2. Before that, `xfs_info` shows `ascii-ci=1`.

ntfs-3g 2022.10.3, with and without `windows_names`, holds `Film.mkv` beside `film.mkv` and `Films/` beside `films/`, and renames a third file to `FILM.MKV` beside them. So the driver does not protect against case collisions, and `windows_names` does not either. Measured on an image file in a container (Linux 6.6, WSL2). ntfs3 was not measured for this, as that kernel does not have it.

## Windows

Sources:
- Microsoft's [Naming Files, Paths, and Namespaces](https://learn.microsoft.com/en-us/windows/win32/fileio/naming-a-file), read from its source in [MicrosoftDocs/win32](https://github.com/MicrosoftDocs/win32/blob/docs/desktop-src/FileIO/naming-a-file.md) and its git history;
- the .NET article on file path formats;
- Wine's `RtlIsDosDeviceName_U` (`dlls/ntdll/path.c`) and its 44-row test table, which Wine also runs on real Windows;
- ReactOS `sdk/lib/rtl/path.c`;
- Go's `internal/filepathlite/path_windows.go` and CPython's `ntpath.py`.

No real Windows machine was available.

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

- **Renames to an incompatible name.** A user may rename a file to a name that another copy's filesystem refuses (§15.3, §6.1). What happens to the copy left at the old name?
- **Trash layout.** `<logical path>.chunks/` cannot hold a file name of 249 bytes or more (§17).
- **Total path length.** mergerfs opens `<branch>/<path>` as one string, limited to 4096 bytes. Windows applications without long-path support stop at 259 units.
- **Output of `atoll names check`.** Its JSON schema, sort key and exit code are not defined, nor the precedence between `name-incompatible`, `name-collision` and `degraded`.
- **8.3 short names.** Both Linux drivers refuse to create a name equal to another file's short name. ntfs-3g's `stat` of that name returns the other file. So existence on NTFS must never be decided by `stat` alone.
- **Mount checks.**
  - ntfs3 must run with `iocharset=utf8`, and its `ads` option should be reported.
  - A listed entry whose `lstat` fails is a name the driver cannot read: ntfs3 drops unpaired surrogates written by Windows or ntfs-3g. It must never count as a missing file.
