//go:build loopdev

package loopdev

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/trash"
)

// TestWhatTheTypeNTFSDoesWithAWildcardName pins what the type ntfs does with a name holding one of " * < > ?, the
// wildcard characters of Windows, that ntfs-3g wrote, mounted as the mount check asks (errors=remount-ro), with and
// without windows_names: each operation on a fresh volume, its volume synced, then a folder made at its top, which
// says whether the volume is still writable (measured on Linux 7.3.5, docs/research/ntfs-driver.md). Taking such a name
// out of its folder, by a rename or an unlink, meets an error that sets the whole volume read-only, windows_names or not
// for a rename, and so does a write of its content once written back, whatever the options; every other operation
// leaves it writable, a read, a change of mode, owner, time or attribute, a link to it, a rename of a name below a
// folder of such a name, a file made there, and a rename to such a name without windows_names. So Atoll
// leaves a copy at such a path on that type as it is (specs.md §15.8, §22): a kernel that changes this fails here.
func TestWhatTheTypeNTFSDoesWithAWildcardName(t *testing.T) {
	needRoot(t)
	ntfsType(t)
	const file, folder, inner = "q?.mkv", "d*x", "f.mkv"
	in := func(dir string, parts ...string) string {
		return filepath.Join(append([]string{dir, "media"}, parts...)...)
	}
	toTrash := func(dir, name string) string { return filepath.Join(dir, "Atoll", "trash", name) }
	// What a call gives, and the volume after it. An unlink that sets the volume read-only gives EROFS itself.
	type answer struct {
		err      []error // what the call may give, nil for nothing
		readOnly bool    // the volume read-only after it
	}
	ok := answer{}
	probes := []struct {
		name          string
		do            func(dir string) error
		plain, wnames answer
	}{
		{"lstat and list", func(dir string) error {
			if _, err := os.Lstat(in(dir, file)); err != nil {
				return err
			}
			_, err := os.ReadDir(in(dir))
			return err
		}, ok, ok},
		{"read with O_NOATIME", func(dir string) error { return readAll(in(dir, file), unix.O_NOATIME) }, ok, ok},
		{"read past relatime", func(dir string) error {
			old := time.Unix(1500000000, 0)
			if err := os.Chtimes(in(dir, "ok.mkv"), old, old); err != nil {
				return err
			}
			return readAll(in(dir, file), 0)
		}, ok, ok},
		{"open to write", func(dir string) error {
			f, err := os.OpenFile(in(dir, file), os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			return f.Close()
		}, ok, ok},
		{"fsync", func(dir string) error {
			f, err := os.Open(in(dir, file))
			if err != nil {
				return err
			}
			return errors.Join(f.Sync(), f.Close())
		}, ok, ok},
		{"a write of its content", func(dir string) error { return os.WriteFile(in(dir, file), []byte("rewritten"), 0o644) },
			answer{nil, true}, answer{nil, true}},
		{"chmod", func(dir string) error { return os.Chmod(in(dir, file), 0o600) }, ok, ok},
		{"utimes", func(dir string) error {
			when := time.Unix(1600000000, 0)
			return os.Chtimes(in(dir, file), when, when)
		}, ok, ok},
		{"chown", func(dir string) error { return os.Lchown(in(dir, file), 1000, 1000) }, ok, ok},
		{"setxattr", func(dir string) error { return unix.Lsetxattr(in(dir, file), "user.x", []byte("y"), 0) }, ok, ok},
		{"a move to the trash", func(dir string) error {
			return unix.Renameat2(unix.AT_FDCWD, in(dir, file), unix.AT_FDCWD, toTrash(dir, "x.mkv"), unix.RENAME_NOREPLACE)
		}, answer{[]error{syscall.ERANGE}, true}, answer{[]error{syscall.ERANGE}, true}},
		{"a link to the trash", func(dir string) error { return os.Link(in(dir, file), toTrash(dir, "x.mkv")) }, ok, ok},
		{"unlink", func(dir string) error { return os.Remove(in(dir, file)) },
			answer{[]error{syscall.ERANGE, syscall.EROFS}, true}, answer{[]error{syscall.EINVAL}, false}},
		{"a read below such a folder", func(dir string) error { return readAll(in(dir, folder, inner), unix.O_NOATIME) }, ok, ok},
		{"a move out of such a folder", func(dir string) error {
			return unix.Renameat2(unix.AT_FDCWD, in(dir, folder, inner), unix.AT_FDCWD, toTrash(dir, "y.mkv"), unix.RENAME_NOREPLACE)
		}, ok, ok},
		{"a file made in such a folder", func(dir string) error { return os.WriteFile(in(dir, folder, "new.mkv"), []byte("new"), 0o644) }, ok, ok},
		{"a rename to such a name", func(dir string) error {
			return unix.Renameat2(unix.AT_FDCWD, in(dir, "ok.mkv"), unix.AT_FDCWD, in(dir, "w?.mkv"), unix.RENAME_NOREPLACE)
		}, ok, answer{[]error{syscall.EINVAL}, false}},
	}
	for _, options := range []string{"", ",windows_names"} {
		for _, p := range probes {
			t.Run(p.name+options, func(t *testing.T) {
				want := p.plain
				if options != "" {
					want = p.wnames
				}
				img, dir := ntfsImageWrittenBy3g(t, file, folder, inner)
				if err := ntfsKernel(options)(img, dir); err != nil {
					t.Skipf("cannot mount: %v", err)
				}
				t.Cleanup(func() { _ = syscall.Unmount(dir, 0) })
				err := p.do(dir)
				// What the call left dirty is written back first: an error the driver meets there sets the volume
				// read-only as one it meets in the call does.
				synced := syncVolume(dir)
				after := os.Mkdir(filepath.Join(dir, "after"), 0o755)
				t.Logf("%s%s: %v; synced: %v; a folder made after it: %v", p.name, options, err, synced, after)
				matched := err == nil && want.err == nil
				for _, e := range want.err {
					matched = matched || errors.Is(err, e)
				}
				if !matched {
					t.Errorf("%v, want %v", err, want.err)
				}
				if readOnly := errors.Is(after, syscall.EROFS); readOnly != want.readOnly || !readOnly && after != nil {
					t.Errorf("a folder made after it: %v, want the volume read-only %v", after, want.readOnly)
				}
			})
		}
	}
	t.Run("windows_names among the superblock options", func(t *testing.T) {
		img, dir := ntfsImageWrittenBy3g(t, file, folder, inner)
		if err := ntfsKernel(",windows_names")(img, dir); err != nil {
			t.Skipf("cannot mount: %v", err)
		}
		t.Cleanup(func() { _ = syscall.Unmount(dir, 0) })
		table, err := os.ReadFile("/proc/self/mountinfo")
		mustDo(t, err)
		for _, line := range bytes.Split(table, []byte("\n")) {
			if bytes.Contains(line, []byte(dir)) {
				t.Logf("mountinfo: %s", line)
				if !bytes.Contains(line, []byte(",windows_names")) {
					t.Errorf("no windows_names: %s", line)
				}
			}
		}
	})
}

// syncVolume writes back what the volume of a folder holds dirty, as syncfs does.
func syncVolume(dir string) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return unix.Syncfs(fd)
}

func readAll(path string, flags int) error {
	fd, err := unix.Open(path, unix.O_RDONLY|flags, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	_, err = io.Copy(io.Discard, f)
	return errors.Join(err, unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED), f.Close())
}

// ntfsImageWrittenBy3g makes an NTFS image holding media/<file>, media/<folder>/<inner>, media/ok.mkv and an empty
// Atoll/trash, written by ntfs-3g, and gives the image and an empty folder to mount it at.
func ntfsImageWrittenBy3g(t *testing.T, file, folder, inner string) (string, string) {
	t.Helper()
	work := t.TempDir()
	img, dir := filepath.Join(work, "fs.img"), filepath.Join(work, "mnt")
	mustDo(t, os.Mkdir(dir, 0o755))
	f, err := os.Create(img)
	mustDo(t, err)
	mustDo(t, f.Truncate(64<<20))
	mustDo(t, f.Close())
	run(t, append(mkntfs, img)...)
	if err := fuse("ntfs-3g", "")(img, dir); err != nil {
		t.Skipf("cannot mount with ntfs-3g: %v", err)
	}
	for _, sub := range []string{filepath.Join("media", folder), filepath.Join("Atoll", "trash")} {
		mustDo(t, os.MkdirAll(filepath.Join(dir, sub), 0o755))
	}
	for _, name := range []string{file, filepath.Join(folder, inner), "ok.mkv"} {
		mustDo(t, os.WriteFile(filepath.Join(dir, "media", name), []byte("content of "+name), 0o644))
	}
	mustDo(t, syscall.Unmount(dir, 0))
	// ntfs-3g lets go of the image a moment after its unmount.
	time.Sleep(500 * time.Millisecond)
	return img, dir
}

// TestAWildcardNameLeftOnTheTypeNTFS: a disk of the type ntfs, mounted as the mount check asks and without
// windows_names, holding names with a wildcard character that ntfs-3g wrote, beside an ext4 disk holding the same: a
// pass reads, moves and writes nothing of them, and their files are name-incompatible; Atoll's own trash, asked to take
// one as surplus, of a file x1 whose copy on the ext4 disk is verified, refuses its name and journals nothing; and the
// deletion of the ext4 copy of another leaves the copy there where it is, with its row, its job of the deletion refused,
// and nothing made again from it; and an edit made to a third there, by ntfs-3g, reaches the ext4 copy, the copy there
// read as the source of a copy (the maintainer, 2026-11-23), nothing renamed, unlinked, written or journaled on its
// volume. The volume stays writable and the disk online throughout: nothing set it read-only (specs.md §15.8, §22). The
// name with no wildcard beside them is a copy as any.
func TestAWildcardNameLeftOnTheTypeNTFS(t *testing.T) {
	needRoot(t)
	options := driverOptions(t, "ntfs")
	if _, err := exec.LookPath("ntfs-3g"); err != nil {
		t.Skip("no ntfs-3g to write the names")
	}
	v := newVolume(t, names.NTFS)
	at := mountPoint(t)
	mustDo(t, os.MkdirAll(at, 0o755))
	run(t, "ntfs-3g", v.device, at)
	for _, logical := range []string{"Who?/q?.mkv", "plain.mkv", "Single/r?.mkv", "Edit?/e?.mkv"} {
		write(t, at, logical, "film")
	}
	run(t, "umount", at)
	time.Sleep(500 * time.Millisecond)
	mountVolume(t, v, "ntfs", at, options...)
	b := newPassBenchWith(t, v, at, "[[rule]]\nlabel = \"single\"\npath = \"Single/**\"\nreplicas = 0\n")
	for _, logical := range []string{"Who?/q?.mkv", "plain.mkv", "Single/r?.mkv", "Edit?/e?.mkv"} {
		write(t, b.keep, logical, "film")
	}
	writable := func(when string) {
		t.Helper()
		probe := filepath.Join(at, "probe")
		if err := os.Mkdir(probe, 0o755); err != nil {
			t.Fatalf("%s: the volume is not writable: %v", when, err)
		}
		mustDo(t, os.Remove(probe))
		if got := disks.Check(b.machine, &b.config.Disks[1]); got.State != disks.Online {
			t.Fatalf("%s: the disk is %s %s: %s", when, got.State, got.Reason, got.Message)
		}
		wantContent(t, at, "Who?/q?.mkv", "film")
		if got := b.text("SELECT count(*) FROM ops WHERE disk_id = 'v'"); got != "0" {
			t.Errorf("%s: %s operations journaled on the disk", when, got)
		}
	}
	b.scan(true)
	if got := b.text("SELECT group_concat(CAST(lpath AS TEXT) || ' ' || state, ', ') FROM (SELECT * FROM files ORDER BY lpath)"); got != "Edit?/e?.mkv name-incompatible, Single/r?.mkv name-incompatible, Who?/q?.mkv name-incompatible, plain.mkv ok" {
		t.Errorf("after the scan: %s", got)
	}
	b.settle()
	writable("after a pass")

	// Recorded surplus, as a mount by another driver could leave it: the copy on the ext4 disk is verified and enough.
	if _, err := b.db.ExecContext(t.Context(), "UPDATE copies SET state = 'surplus' WHERE disk_id = 'v' AND file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'Single/r?.mkv')"); err != nil {
		t.Fatal(err)
	}
	s, err := trash.Open(t.Context(), b.db, b.config, b.machine, true)
	mustDo(t, err)
	var outcomes []string
	_, err = s.Prune(t.Context(), func(i *trash.PruneItem) error {
		outcomes = append(outcomes, i.Disk+" "+i.Path+" "+i.Outcome)
		return nil
	})
	mustDo(t, errors.Join(err, s.Finish(t.Context())))
	s.Close()
	if len(outcomes) != 1 || outcomes[0] != "v Single/r?.mkv name-refused" {
		t.Errorf("the prune: %v", outcomes)
	}
	writable("after a prune")
	wantContent(t, at, "Single/r?.mkv", "film")

	mustDo(t, os.Remove(filepath.Join(b.keep, "media", "Who?", "q?.mkv")))
	b.settle()
	writable("after a deletion on the other disk")
	if got := b.text("SELECT group_concat(disk_id) FROM copies WHERE file_id = (SELECT id FROM files WHERE CAST(lpath AS TEXT) = 'Who?/q?.mkv')"); got != "v" {
		t.Errorf("the copies left: %q", got)
	}
	if got := b.text("SELECT state FROM files WHERE CAST(lpath AS TEXT) = 'Who?/q?.mkv'"); got != "name-incompatible" {
		t.Errorf("the file is %q", got)
	}
	b.settle()
	if _, err := os.Lstat(filepath.Join(b.keep, "media", "Who?", "q?.mkv")); !os.IsNotExist(err) {
		t.Errorf("the deleted copy made again: %v", err)
	}
	if got := b.text("SELECT group_concat(kind || ' ' || reason || ' ' || dst_disk) FROM jobs"); got != "trash deleted v" {
		t.Errorf("the jobs: %q", got)
	}

	// An edit made to the copy there, by ntfs-3g, as Windows would make it: a write of its content through the type ntfs
	// itself sets the volume read-only once written back (TestWhatTheTypeNTFSDoesWithAWildcardName). The copy on the
	// ext4 disk goes to the trash, overwritten, and is made again from it, once its settle has passed.
	run(t, "umount", at)
	run(t, "ntfs-3g", v.device, at)
	write(t, at, "Edit?/e?.mkv", "a longer film")
	run(t, "umount", at)
	time.Sleep(500 * time.Millisecond)
	mountVolume(t, v, "ntfs", at, options...)
	writable("after an edit by ntfs-3g")
	b.pass()
	b.machine = &later{Machine: b.machine, by: 11 * time.Minute}
	out := b.pass()
	for _, want := range []jobs.Operation{
		{Disk: "k", Kind: jobs.KindTrash, Path: "Edit?/e?.mkv", Reason: "overwritten", Outcome: "trashed"},
		{Disk: "k", Kind: jobs.KindReplicate, Path: "Edit?/e?.mkv", Source: "v", Outcome: jobs.OutcomeReplicated},
	} {
		if !slices.Contains(out.Operations, want) {
			t.Errorf("no %+v in %+v", want, out.Operations)
		}
	}
	dropCaches(t)
	wantContent(t, b.keep, "Edit?/e?.mkv", "a longer film")
	wantContent(t, at, "Edit?/e?.mkv", "a longer film")
	writable("after the edit reached the ext4 disk")
	dropCaches(t)
	wantContent(t, at, "plain.mkv", "film")
	_ = exec.CommandContext(context.Background(), "sync").Run()
}
