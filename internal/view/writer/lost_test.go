package writer

import (
	"errors"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/ops"
	"github.com/peondusud/atoll/internal/view"
)

// Each operation of the view on disks that answer EIO to every call, as a USB disk that dropped does, fails, writes
// nothing and leaves nothing done in the catalog; once the disks are back, the reprise leaves nothing of it planned,
// and the files are as they were (invariant 4, specs.md §21.2).
func TestAnOperationOnDisksLostChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(b *bench) error
	}{
		{"create", func(b *bench) error {
			f, _, err := b.w.Create(b.t.Context(), "Films/new.mkv", 0o644, root)
			return closed(f, err)
		}},
		{"mkdir", func(b *bench) error { _, err := b.w.Mkdir(b.t.Context(), "Films/New", 0o755, root); return err }},
		{"symlink", func(b *bench) error { _, err := b.w.Symlink(b.t.Context(), "Films/l", "a.mkv", root); return err }},
		{"open to write", func(b *bench) error {
			return closed(b.w.Open(b.t.Context(), "Films/a.mkv", b.lookup("Films/a.mkv"), root))
		}},
		{"link", func(b *bench) error {
			_, err := b.w.Link(b.t.Context(), "Films/a.mkv", "Films/b.mkv", root)
			return err
		}},
		{"unlink", func(b *bench) error { return b.w.Unlink(b.t.Context(), "Films/a.mkv", root) }},
		{"rename", func(b *bench) error { return b.w.Rename(b.t.Context(), "Films/a.mkv", "Films/b.mkv", true, root) }},
		{"rename of a folder", func(b *bench) error { return b.w.Rename(b.t.Context(), "Films", "Old", true, root) }},
		{"xattr", func(b *bench) error {
			return b.w.SetXattr(b.t.Context(), "Films/a.mkv", b.lookup("Films/a.mkv"), "user.comment", []byte("x"), 0, root)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, "ext4")
			lost := func(string, string) error { return syscall.EIO }
			b.machine.setFail(b.mount["t1"], lost)
			b.machine.setFail(b.mount["t2"], lost)
			err := tc.do(b)
			b.machine.setFail(b.mount["t1"], nil)
			b.machine.setFail(b.mount["t2"], nil)
			if err == nil {
				t.Fatal("an operation on disks lost did not fail")
			}
			if !errors.Is(err, ops.ErrDiskLost) && !errors.Is(err, syscall.EIO) && !errors.Is(err, view.ErrUnreadable) {
				t.Errorf("a disk lost read as another error: %v", err)
			}
			if err := b.w.Reprise(t.Context()); err != nil {
				t.Fatalf("reprise: %v", err)
			}
			if n := b.journaled(); n != 0 {
				t.Errorf("%d operations of the view left open", n)
			}
			if n := b.planned(); n != 0 {
				t.Errorf("%d operations left planned", n)
			}
			for _, disk := range []string{"t1", "t2"} {
				if got := b.on(disk, "Films/a.mkv"); got != "from "+disk {
					t.Errorf("%s holds %q", disk, got)
				}
				for _, p := range []string{"Films/new.mkv", "Films/b.mkv", "Films/l", "Old/a.mkv"} {
					if got := b.on(disk, p); got != "absent" {
						t.Errorf("%s holds %s: %q", disk, p, got)
					}
				}
			}
		})
	}
}
