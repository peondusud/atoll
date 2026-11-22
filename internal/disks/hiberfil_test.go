package disks

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
)

// TestReadHiberfil: what the top of a disk holds at hiberfil.sys, read through a handle on it and never followed
// elsewhere, its first 4 KiB at most; an error that says the disk is unknown is never a file absent (invariant 4), and
// reading writes nothing, not even the file's access time (specs.md §10).
func TestReadHiberfil(t *testing.T) {
	top := t.TempDir()
	at := filepath.Join(top, HiberfilName)
	open := func(t *testing.T) fsx.Dir {
		t.Helper()
		dir, err := fsx.OpenDir(top)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dir.Close() })
		return dir
	}
	clear := func(t *testing.T) {
		t.Helper()
		if err := os.RemoveAll(at); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("absent", func(t *testing.T) {
		if got := ReadHiberfil(open(t)); got.File != HiberfilAbsent || got.Hibernated() {
			t.Errorf("%+v", got)
		}
	})
	t.Run("ten bytes", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		if err := os.WriteFile(at, []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := ReadHiberfil(open(t))
		if got.File != HiberfilFile || got.Size != 10 || string(got.Head) != "0123456789" || !got.Hibernated() {
			t.Errorf("%+v", got)
		}
	})
	t.Run("its first 4 KiB", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		content := make([]byte, 3*HiberfilHeader)
		content[HiberfilHeader] = 1
		if err := os.WriteFile(at, content, 0o644); err != nil {
			t.Fatal(err)
		}
		got := ReadHiberfil(open(t))
		if got.File != HiberfilFile || got.Size != int64(len(content)) || !bytes.Equal(got.Head, content[:HiberfilHeader]) || got.Hibernated() {
			t.Errorf("%s, %d bytes read, hibernated %v", got.File, len(got.Head), got.Hibernated())
		}
	})
	t.Run("the magic of a hibernation", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		content := append([]byte("HIBR"), make([]byte, 2*HiberfilHeader)...)
		if err := os.WriteFile(at, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ReadHiberfil(open(t)); got.File != HiberfilFile || !got.Hibernated() {
			t.Errorf("%s, hibernated %v", got.File, got.Hibernated())
		}
	})
	t.Run("a folder", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		if err := os.Mkdir(at, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := ReadHiberfil(open(t)); got.File != HiberfilNotAFile || !got.Hibernated() {
			t.Errorf("%+v", got)
		}
	})
	t.Run("a symbolic link to zeros", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		zeros := filepath.Join(t.TempDir(), "zeros")
		if err := os.WriteFile(zeros, make([]byte, 2*HiberfilHeader), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(zeros, at); err != nil {
			t.Fatal(err)
		}
		if got := ReadHiberfil(open(t)); got.File != HiberfilNotAFile || !got.Hibernated() {
			t.Errorf("not followed: %+v", got)
		}
	})
	t.Run("a FIFO, without waiting", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		if err := syscall.Mkfifo(at, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ReadHiberfil(open(t)); got.File != HiberfilNotAFile {
			t.Errorf("%+v", got)
		}
	})
	for _, tc := range []struct {
		name string
		err  error
		want HiberfilAnswer
	}{
		{"EIO", syscall.EIO, HiberfilIOError},
		{"ENOTCONN", syscall.ENOTCONN, HiberfilIOError},
		{"ENODEV", syscall.ENODEV, HiberfilIOError},
		{"EACCES", syscall.EACCES, HiberfilUnreadable},
		{"EMFILE", syscall.EMFILE, HiberfilUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := fsx.Fault(open(t), func(call, name string) error {
				if call == "readstart" && name == HiberfilName {
					return tc.err
				}
				return nil
			})
			got := ReadHiberfil(dir)
			if got.File != tc.want || got.Note == "" {
				t.Errorf("%+v", got)
			}
			if v := judgeHiberfil(got); (tc.want == HiberfilIOError) != (v.State == Missing) || v.State == Online {
				t.Errorf("judged %+v", v)
			}
		})
	}
	t.Run("nothing written", func(t *testing.T) {
		t.Cleanup(func() { clear(t) })
		if err := os.WriteFile(at, make([]byte, 2*HiberfilHeader), 0o644); err != nil {
			t.Fatal(err)
		}
		// An access time a day and more ago, before the modification: any read would move it under relatime.
		old := time.Now().Add(-72 * time.Hour)
		if err := os.Chtimes(at, old, old.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		before := accessTime(t, at)
		if got := ReadHiberfil(open(t)); got.File != HiberfilFile || got.Hibernated() {
			t.Fatalf("%s, hibernated %v", got.File, got.Hibernated())
		}
		if after := accessTime(t, at); !after.Equal(before) {
			t.Errorf("the access time moved from %v to %v", before, after)
		}
		if _, err := os.ReadFile(at); err != nil {
			t.Fatal(err)
		}
		if accessTime(t, at).Equal(before) {
			t.Log("this filesystem keeps no access time: what was asked of it could not be seen here")
		}
	})
}

func accessTime(t *testing.T, path string) time.Time {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return time.Unix(st.Atim.Sec, st.Atim.Nsec) //nolint:unconvert // int32 on some platforms
}

// TestHiberfilIsReadOncePerMount: a machine that runs for long reads hiberfil.sys once for each mount, so that the
// daemon's check every 30 s wakes no disk for it, and again once the disk is mounted anew, which a unique mount id
// tells, or a check that found nothing mounted there in between. An error is never kept (specs.md §10).
func TestHiberfilIsReadOncePerMount(t *testing.T) {
	hibernated := HiberfilSeen{File: HiberfilFile, Size: 10, Head: make([]byte, 10)}
	resumed := HiberfilSeen{File: HiberfilFile, Size: 8192, Head: make([]byte, HiberfilHeader)}
	var (
		c       hiberfilCache
		unique  = map[string]uint64{"/mnt/t1": 7, "/mnt/t2": 9}
		hasIt   = true
		onDisk  = hibernated
		reads   int
		failing error
		during  func()
	)
	uniqueOf := func(path string) (uint64, bool, error) {
		if failing != nil {
			return 0, false, failing
		}
		return unique[path], hasIt, nil
	}
	ask := func(mount string, entry *MountEntry) HiberfilSeen {
		return c.answer(mount, entry, uniqueOf, func() HiberfilSeen {
			reads++
			if during != nil {
				during()
			}
			return onDisk
		})
	}
	entry := &MountEntry{ID: 300, Major: 8, Minor: 17}
	step := func(name string, mount string, e *MountEntry, want HiberfilSeen, wantReads int) {
		t.Helper()
		got := ask(mount, e)
		if got.File != want.File || got.Size != want.Size || got.Note != want.Note || reads != wantReads {
			t.Errorf("%s: %+v after %d reads, want %+v after %d", name, got, reads, want, wantReads)
		}
	}
	step("the first check", "/mnt/t1", entry, hibernated, 1)
	onDisk = resumed
	step("the next check, the same mount", "/mnt/t1", entry, hibernated, 1)
	step("another disk", "/mnt/t2", &MountEntry{ID: 301, Major: 8, Minor: 33}, resumed, 2)
	unique["/mnt/t1"] = 8
	step("mounted anew, its unique id told", "/mnt/t1", entry, resumed, 3)
	step("and kept", "/mnt/t1", entry, resumed, 3)

	hasIt, onDisk = false, hibernated
	step("a kernel with no unique id", "/mnt/t1", entry, hibernated, 4)
	step("kept by the id of the table", "/mnt/t1", entry, hibernated, 4)
	onDisk = resumed
	step("another id of the table", "/mnt/t1", &MountEntry{ID: 302, Major: 8, Minor: 17}, resumed, 5)
	step("another device", "/mnt/t1", &MountEntry{ID: 302, Major: 8, Minor: 18}, resumed, 6)
	onDisk = hibernated
	step("the same id given again, unseen", "/mnt/t1", &MountEntry{ID: 302, Major: 8, Minor: 18}, resumed, 6)
	c.forget("/mnt/t1")
	step("once a check found nothing mounted there", "/mnt/t1", &MountEntry{ID: 302, Major: 8, Minor: 18}, hibernated, 7)

	hasIt = true
	for _, e := range []HiberfilSeen{{File: HiberfilIOError, Note: "EIO"}, {File: HiberfilUnreadable, Note: "EACCES"}} {
		c.forget("/mnt/t1")
		onDisk = e
		step("an error", "/mnt/t1", entry, e, reads+1)
		onDisk = resumed
		step("not kept", "/mnt/t1", entry, resumed, reads+1)
		step("a reading kept", "/mnt/t1", entry, resumed, reads)
	}

	c.forget("/mnt/t1")
	during = func() { unique["/mnt/t1"]++ }
	step("mounted anew during the read", "/mnt/t1", entry, mountChanged, reads+1)
	during = nil
	step("read again", "/mnt/t1", entry, resumed, reads+1)

	failing = syscall.EIO
	step("the unique id does not answer", "/mnt/t1", entry, HiberfilSeen{File: HiberfilIOError, Note: hiberfilError(syscall.EIO).Note}, reads)
	failing = syscall.EACCES
	step("nor that way", "/mnt/t1", entry, HiberfilSeen{File: HiberfilUnreadable, Note: hiberfilError(syscall.EACCES).Note}, reads)
}

// TestHiberfilCacheAtOnce: the daemon's check and the local API ask at once.
func TestHiberfilCacheAtOnce(t *testing.T) {
	var c hiberfilCache
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			mount := []string{"/mnt/a", "/mnt/b"}[i%2]
			for range 100 {
				got := c.answer(mount, &MountEntry{ID: uint64(i % 2)}, func(string) (uint64, bool, error) { return 1, true, nil },
					func() HiberfilSeen { return HiberfilSeen{File: HiberfilAbsent} })
				if got.File != HiberfilAbsent {
					t.Error(got)
					return
				}
				if i%4 == 0 {
					c.forget(mount)
				}
			}
		})
	}
	wg.Wait()
}

// TestJudgeHiberfil: each answer, and what a check makes of it; an answer no machine gives is no reading, and the disk
// is written to no more.
func TestJudgeHiberfil(t *testing.T) {
	for _, tc := range []struct {
		seen   HiberfilSeen
		state  State
		reason Reason
	}{
		{HiberfilSeen{File: HiberfilAbsent}, Online, ""},
		{HiberfilSeen{File: HiberfilFile, Size: HiberfilHeader, Head: make([]byte, HiberfilHeader)}, Online, ""},
		{HiberfilSeen{File: HiberfilFile, Size: HiberfilHeader - 1, Head: make([]byte, HiberfilHeader-1)}, Readonly, Hibernated},
		{HiberfilSeen{File: HiberfilNotAFile}, Readonly, Hibernated},
		{HiberfilSeen{File: HiberfilIOError, Note: "EIO"}, Missing, IOError},
		{HiberfilSeen{File: HiberfilUnreadable, Note: "EACCES"}, Readonly, Hibernated},
		{HiberfilSeen{}, Readonly, Hibernated},
		{HiberfilSeen{File: "what"}, Readonly, Hibernated},
	} {
		got := judgeHiberfil(tc.seen)
		if got.State != tc.state || got.Reason != tc.reason || (got.State == Online) != (got.Message == "") {
			t.Errorf("%+v: %+v, want %s %q", tc.seen, got, tc.state, tc.reason)
		}
	}
}
