# The FUSE library of Atoll's own view

This report compares two Go libraries for Atoll's own view (`specs.md` §21.2) on what the view needs, for the decision `specs.md` §30 lists: cgofuse, which the spec and `CLAUDE.md` name, and go-fuse. It is research, not a contract: what is decided goes to `specs.md` §21.2 and §29.7, and to `CLAUDE.md`'s stack.

## Why it matters

- The view is mounted by the daemon at `view.mount`, in place of mergerfs, and every media server and user reads the library through it. The library it is built on decides how the binary is built, what the machine must have installed for it to mount, and how each request of the kernel reaches Atoll.
- The view's own logic does not depend on it: what a path presents, a folder's listing and `statfs` come from `internal/view` (`Tree`), from the catalog alone. Each library is a thin side over that tree, read only for now, of some 250 lines: `internal/view/fusefs` (cgofuse) and `internal/view/gofusefs` (go-fuse).

## Method

- Both sides were built over the same tree and mounted, as root, in the qemu guest of `test/qemu/run.sh`, on Debian 13's Linux 6.12.117 and Ubuntu's mainline 7.3.1, by the tests of `test/loopdev`.
- `TestViewMatchesMergerfs` replicates a library on two disks, mounts mergerfs from the line `atoll mergerfs-config` prints and the view beside it, and compares, through each, every name with its kind, mode, owner, size, modification time, symlink target and content; then lets a replica rot, its times kept, and reads through each; then tries four writes; then `statfs`. Six drivers: ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g.
- `TestViewBrowsesNoDisk` lists the whole library through the view once a disk's device answers every read with an error (dm-error) and the page cache is dropped, on ext4, XFS, ntfs3 and ntfs-3g.
- Measured on 2026-11-16, with cgofuse v1.6.0, go-fuse v2.12.1 and Debian 13's libfuse 3.17.2.

## Summary

| | cgofuse v1.6.0 | go-fuse v2.12.1 |
|---|---|---|
| License | MIT | BSD-3-Clause |
| How it reaches the kernel | libfuse3's high-level API, loaded at mount time (`dlopen`) | the kernel's FUSE protocol itself, through `/dev/fuse` |
| Build | cgo, the `fuse3` tag, libfuse3's headers (`libfuse3-dev`) | no cgo: the binary stays static, as today |
| What the machine needs to mount | `libfuse3.so.3` | nothing: `mount(2)` with `CAP_SYS_ADMIN`, which the daemon's unit already gives, or `fusermount` |
| Debian 13, openmediavault 8, Fedora, Arch | **does not mount**: libfuse 3.17 and later is `libfuse3.so.4` | mounts |
| `TestViewMatchesMergerfs` | 12 of 12, once the guest's library was given the name cgofuse asks for | 60 of 60, five times on each driver and kernel |
| `TestViewBrowsesNoDisk` | not run | 8 of 8 |
| Paths | the kernel's path of each request, as a string | inodes: the path is rebuilt from the tree of looked-up nodes (`Inode.Path`) |
| The Rust version | — | the same protocol as `fuser`, which `specs.md` §29.7 plans for it |

## cgofuse and libfuse 3.17

- cgofuse v1.6.0 opens libfuse by one name: `dlopen("libfuse3.so.3", RTLD_NOW)` for the FUSE3 API (`fuse/host_cgo.go`). It tries no other, and the process exits with "cannot find FUSE" when that one is missing.
- libfuse 3.17 changed its soname to `libfuse3.so.4`. Its changelog gives the reason: the ABI is that of 3.10 again, and the version was raised as releases with another ABI had shipped as `.so.3`. Debian 13's `libfuse3-4` holds `libfuse3.so.3.17.2` and `libfuse3.so.4` and no `libfuse3.so.3`; `apt-get install libfuse3-3` finds no package there. Fedora's and Arch's fuse3 packages provide `libfuse3.so.4` too.
- So the view built on cgofuse does not mount on Debian 13, nor on openmediavault 8, which is built on it. In qemu it passed every check of `TestViewMatchesMergerfs` once `libfuse3.so.3` was made a link to `libfuse3.so.4` in the test's guest alone, which is no fix: a link between two sonames is what their authors warn against. A fix is one line in cgofuse, to try `libfuse3.so.4` before `libfuse3.so.3`, carried in a copy of its own (`replace` in `go.mod`) until cgofuse takes it.
- The headers it builds against must be those of the library it loads: built with 3.17's headers, it loads 3.17.

## go-fuse and an interrupted request

- go-fuse gives each request a context that it cancels when the kernel interrupts the request, as the kernel does when a signal reaches the process that waits on it. The Go runtime preempts its own threads with `SIGURG`: a test's `open` on the view was interrupted so, and the view, which passed the context to the catalog, answered `EIO`, which failed the open for good, in 4 of the first 12 runs. Answered `EINTR`, the kernel starts the call again, or gives the process `EINTR`; then 60 of 60 runs passed. cgofuse gives no context, and its side passes none.
- go-fuse numbers inodes itself unless the file system gives each node a number. With a number of its own at each lookup, a name looked up again once the kernel's 1 s were over was another node to the kernel. Each path now has its own number, a hash of it, so that a name looked up again is the node the kernel holds; that was not what failed the open above.

## What neither changes

- Both pass the same tree the same answers: the listings, the attributes and the data are the view's, not the library's, and both match mergerfs.
- Both mount with `ro`, `default_permissions` and `allow_other`, and the kernel keeps attributes and names for 1 s and no absent name, as for mergerfs.
- Writes, renames and deletions through the view are to do on either.

## Recommendation

go-fuse: it mounts where Atoll is to be installed, keeps the binary static and free of a library the machine must hold, and speaks the protocol the Rust version's `fuser` will. cgofuse needs a copy of its own to mount on Debian 13, and cgo for the whole binary.
