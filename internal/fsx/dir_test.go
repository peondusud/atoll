package fsx

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestFileTime(t *testing.T) {
	for _, tc := range []struct {
		units uint64
		want  Time
	}{
		{132539040001234567, Time{1609430400, 123456700}}, // ntfs-3g wrote it, ntfs3 read it (docs/research/file-times.md)
		{0, Time{-11644473600, 0}},
		{116444736000000000, Time{0, 0}},
		{116444736000000001, Time{0, 100}},
		{116444735999999999, Time{-1, 999999900}},
		{1<<64 - 1, Time{1844674407370 - 11644473600 + 0, 955161500}},
	} {
		if got := FileTime(tc.units); got != tc.want {
			t.Errorf("%d: got %+v, want %+v", tc.units, got, tc.want)
		}
	}
}

func TestDirNeverFollowsASymlink(t *testing.T) {
	top := t.TempDir()
	if err := os.MkdirAll(filepath.Join(top, "real/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "real/file"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"to-dir": "real", "to-file": "real/file"} {
		if err := os.Symlink(target, filepath.Join(top, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := OpenDir(filepath.Join(top, "to-dir")); !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, syscall.ELOOP) {
		t.Errorf("OpenDir through a symlink: %v", err)
	}
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	names, err := dir.Names()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"real", "to-dir", "to-file"}) {
		t.Errorf("names: %v", names)
	}
	if _, err := dir.OpenDir("to-dir"); !IsUnreadableName(err) {
		t.Errorf("OpenDir on a symlink: %v", err)
	}
	if _, err := dir.OpenFile("to-file"); !IsUnreadableName(err) {
		t.Errorf("OpenFile on a symlink: %v", err)
	}
	if _, err := OpenFileAt(dir, "to-dir/file"); !IsUnreadableName(err) {
		t.Errorf("a path through a symlink: %v", err)
	}
	st, err := dir.Stat("to-file")
	if err != nil || st.Kind != KindSymlink {
		t.Errorf("Stat on a symlink: %+v, %v", st, err)
	}
	target, err := dir.ReadLink("to-file")
	if err != nil || target != "real/file" {
		t.Errorf("ReadLink: %q, %v", target, err)
	}
	f, err := OpenFileAt(dir, "real/file")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Info(); err != nil || info.Size != 3 || info.Kind != KindFile {
		t.Errorf("Info: %+v, %v", info, err)
	}
	if _, err := dir.OpenFile("real"); !IsUnreadableName(err) {
		t.Errorf("OpenFile on a folder: %v", err)
	}
}

// TestStatSaysWhatStatxSays: each field, against what the os package reads. A wrong one would be
// compared with itself on every later scan, and nothing else would show it.
func TestStatSaysWhatStatxSays(t *testing.T) {
	top := t.TempDir()
	full := filepath.Join(top, "file")
	if err := os.WriteFile(full, []byte("abcde"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(full, filepath.Join(top, "again")); err != nil {
		t.Fatal(err)
	}
	when := time.Unix(1000000000, 123000000)
	if err := os.Chtimes(full, when, when); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	st, err := dir.Stat("file")
	if err != nil {
		t.Fatal(err)
	}
	var want syscall.Stat_t
	if err := syscall.Stat(full, &want); err != nil {
		t.Fatal(err)
	}
	if st.Kind != KindFile || st.Size != 5 || st.Mode != 0o640 || st.Nlink != 2 || st.Ino != want.Ino {
		t.Errorf("%+v, inode %d", st, want.Ino)
	}
	if st.Mtime != (Time{1000000000, 123000000}) {
		t.Errorf("modification time %+v", st.Mtime)
	}
	if st.Ctime != (Time{want.Ctim.Sec, want.Ctim.Nsec}) || st.Ctime.Sec <= st.Mtime.Sec {
		t.Errorf("change time %+v, want %+v", st.Ctime, want.Ctim)
	}
	file, err := dir.OpenFile("file")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	seen, err := file.Info()
	if err != nil {
		t.Fatal(err)
	}
	if seen.Ino != st.Ino || seen.Ctime != st.Ctime || seen.MountID != st.MountID {
		t.Errorf("an open file: %+v, want %+v", seen, st)
	}
}

// TestSyncEntry: a file and a folder are synced by their name. A name that leads nowhere, to a symlink, or
// to what Atoll may not open says it was not opened, with why, so that only a failed fsync reads as a disk
// lost. The fault wrapper names the entry.
func TestSyncEntry(t *testing.T) {
	top := t.TempDir()
	if err := os.WriteFile(filepath.Join(top, "file"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(top, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(top, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "closed"), []byte("abc"), 0o000); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	for _, name := range []string{"file", "sub"} {
		if err := dir.SyncEntry(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"absent", "link"} {
		if err := dir.SyncEntry(name); !errors.Is(err, ErrNotOpened) || !IsUnreadableName(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if os.Getuid() != 0 {
		if err := dir.SyncEntry("closed"); !errors.Is(err, ErrNotOpened) || !IsDenied(err) {
			t.Errorf("closed: %v", err)
		}
	}
	// A FIFO opens without waiting, and its fsync fails: what that says is the fsync's, not the open's.
	if err := syscall.Mkfifo(filepath.Join(top, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := dir.SyncEntry("pipe"); !errors.Is(err, syscall.EINVAL) || errors.Is(err, ErrNotOpened) {
		t.Errorf("pipe: %v", err)
	}
	var calls []string
	faulty := Fault(dir, func(call, name string) error {
		calls = append(calls, call+" "+name)
		if name == "sub" {
			return syscall.EIO
		}
		return nil
	})
	if err := faulty.SyncEntry("file"); err != nil {
		t.Errorf("file: %v", err)
	}
	if err := faulty.SyncEntry("sub"); !errors.Is(err, syscall.EIO) || errors.Is(err, ErrNotOpened) {
		t.Errorf("sub: %v", err)
	}
	if !slices.Equal(calls, []string{"syncentry file", "syncentry sub"}) {
		t.Errorf("calls %q", calls)
	}
}

func TestOpenFileRefusesAFifoWithoutWaiting(t *testing.T) {
	top := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(top, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if _, err := dir.OpenFile("pipe"); !errors.Is(err, ErrNotARegularFile) {
		t.Errorf("got %v", err)
	}
	st, err := dir.Stat("pipe")
	if err != nil || st.Kind != KindOther {
		t.Errorf("Stat: %+v, %v", st, err)
	}
}

// TestWritableFile: a copy in progress is made by its name and never over one that exists, written,
// read back at any offset, given its times, mode and owner through its handle, and opened again to
// resume it. A symlink, a folder or a FIFO at the name is never opened for writing (specs.md §14.2).
func TestWritableFile(t *testing.T) {
	top := t.TempDir()
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	f, err := dir.CreateFile("1.part", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("W"), 6); err != nil {
		t.Fatal(err)
	}
	when := Time{Sec: 1600000000, Nsec: 123456789}
	if err := f.SetTimes(when, when); err != nil {
		t.Fatal(err)
	}
	if err := f.SetMode(0o640); err != nil {
		t.Fatal(err)
	}
	if err := f.SetOwner(uint32(os.Getuid()), uint32(os.Getgid())); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.DropCache(); err != nil {
		t.Fatal(err)
	}
	st, err := f.Info()
	if err != nil || st.Kind != KindFile || st.Size != 11 || st.Mode != 0o640 || st.Mtime != when {
		t.Errorf("%+v, %v", st, err)
	}
	back := make([]byte, 5)
	if n, err := f.ReadAt(back, 6); n != 5 || err != nil || string(back) != "World" {
		t.Errorf("read back %q, %d, %v", back, n, err)
	}
	if _, err := f.ReadAt(back, 9); !errors.Is(err, io.EOF) {
		t.Errorf("past the end: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.CreateFile("1.part", 0o600); !IsExisting(err) {
		t.Errorf("made over one that exists: %v", err)
	}
	again, err := dir.OpenWritable("1.part")
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Truncate(5); err != nil {
		t.Fatal(err)
	}
	if st, err := again.Info(); err != nil || st.Size != 5 || st.Ino == 0 {
		t.Errorf("truncated: %+v, %v", st, err)
	}
	_ = again.Close()
	if err := os.Symlink("1.part", filepath.Join(top, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(top, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(top, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.OpenWritable("link"); !IsUnreadableName(err) {
		t.Errorf("a symlink: %v", err)
	}
	for _, name := range []string{"sub", "pipe"} {
		if w, err := dir.OpenWritable(name); err == nil || (name == "pipe" && !errors.Is(err, ErrNotARegularFile)) {
			if w != nil {
				_ = w.Close()
			}
			t.Errorf("%s: %v", name, err)
		}
	}
	// What a resume reads of the source: any offset of a file opened for reading.
	r, err := dir.OpenFile("1.part")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if n, err := r.ReadAt(back[:2], 3); n != 2 || err != nil || string(back[:2]) != "lo" {
		t.Errorf("ReadAt: %q, %d, %v", back[:2], n, err)
	}
	var calls []string
	faulty := Fault(dir, func(call, name string) error {
		calls = append(calls, call+" "+name)
		if call == "filesync" {
			return syscall.EIO
		}
		return nil
	})
	w, err := faulty.CreateFile("2.part", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); !errors.Is(err, syscall.EIO) {
		t.Errorf("sync: %v", err)
	}
	_ = w.Close()
	if w, err := faulty.OpenWritable("2.part"); err == nil {
		_ = w.Close()
	}
	if !slices.Equal(calls, []string{"create 2.part", "write 2.part", "filesync 2.part", "openwrite 2.part"}) {
		t.Errorf("calls %q", calls)
	}
}

func TestWriteNewNeverReplaces(t *testing.T) {
	top := t.TempDir()
	path := filepath.Join(top, "file")
	if err := WriteNew(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := WriteNew(path, []byte("second")); err == nil {
		t.Error("it replaced a file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "first" {
		t.Errorf("%q, %v", data, err)
	}
	entries, err := os.ReadDir(top)
	if err != nil || len(entries) != 1 {
		t.Errorf("left behind: %v, %v", entries, err)
	}
	if exists, err := Exists(path); !exists || err != nil {
		t.Errorf("Exists: %v, %v", exists, err)
	}
	if exists, err := Exists(path + "x"); exists || err != nil {
		t.Errorf("Exists: %v, %v", exists, err)
	}
}

func TestSumSizes(t *testing.T) {
	top := t.TempDir()
	for name, size := range map[string]int{"a": 3, "sub/b": 5, "sub/deep/c": 7} {
		full := filepath.Join(top, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a", filepath.Join(top, "link")); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDir(top)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if got, err := SumSizes(dir); got != 15 || err != nil {
		t.Errorf("got %d, %v", got, err)
	}
}
