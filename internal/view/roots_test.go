package view

import (
	"errors"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/fsx"
)

// A disk's root is the one its last check gave; the one it replaces is closed, but only once no use of it is under
// way, so that a read never opens a copy through a descriptor closed under it (specs.md §21.2).
func TestDiskRoots(t *testing.T) {
	roots := NewDiskRoots()
	defer roots.Close()
	open := func() fsx.Dir {
		dir, err := fsx.OpenDir(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return dir
	}
	if err := roots.Root("t1", func(fsx.Dir) error { return nil }); !errors.Is(err, ErrUnreadable) {
		t.Errorf("a disk with no root: %v", err)
	}
	first := open()
	roots.Set("t1", first)
	roots.Set("t2", open())
	entered, release, replaced := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = roots.Root("t1", func(root fsx.Dir) error {
			close(entered)
			<-release
			if _, err := root.Info(); err != nil {
				t.Errorf("the root closed while in use: %v", err)
			}
			return nil
		})
	}()
	<-entered
	second := open()
	go func() {
		roots.Set("t1", second)
		close(replaced)
	}()
	select {
	case <-replaced:
		t.Error("the root was replaced while in use")
	case <-time.After(50 * time.Millisecond):
	}
	// A disk whose open hangs, as one in trouble may, holds back none of the others, while its own replacement waits.
	other := make(chan error, 1)
	go func() { other <- roots.Root("t2", func(fsx.Dir) error { return nil }) }()
	select {
	case err := <-other:
		if err != nil {
			t.Errorf("another disk: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("another disk waits for the replacement of t1's root")
	}
	close(release)
	<-replaced
	if _, err := first.Info(); err == nil {
		t.Error("the root replaced is still open")
	}
	var got fsx.Dir
	if err := roots.Root("t1", func(root fsx.Dir) error { got = root; return nil }); err != nil || got != second {
		t.Errorf("the root after its replacement: %v", err)
	}
	roots.Drop("t1")
	if _, err := second.Info(); err == nil {
		t.Error("the root dropped is still open")
	}
	if err := roots.Root("t1", func(fsx.Dir) error { return nil }); !errors.Is(err, ErrUnreadable) {
		t.Errorf("a disk whose root was dropped: %v", err)
	}
}
