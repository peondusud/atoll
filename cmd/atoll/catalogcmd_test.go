package main

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// A rebuild whose old catalog could not take its name once the swap went through says that the new catalog is in
// place, where the old one stays, where its log went, and the rename that puts them together, before a rebuild run
// again removes it; one whose swap failed says that the new catalog is not in place (specs.md §9, §25).
func TestARebuildSaysWhereEachCatalogIs(t *testing.T) {
	const (
		file  = "/var/lib/atoll/catalog.db"
		fresh = file + ".rebuild"
		old   = file + ".old-20261120T063000Z"
	)
	for _, tc := range []struct {
		name      string
		err       error
		cause     error
		want, not []string
	}{
		{"its log set aside", fmt.Errorf("catalog: %w", &catalog.OldNotSetAside{File: file, At: fresh, Want: old,
			Log: []string{old + "-wal", old + "-shm"}, Err: syscall.EIO}), syscall.EIO,
			[]string{"in place at " + file, "stays at " + fresh, "renamed to " + old, old + "-wal, " + old + "-shm", "rename it to " + old,
				"a rebuild run again would remove", "input/output error"},
			[]string{"not yet in place"}},
		{"with no log", fmt.Errorf("catalog: %w", &catalog.OldNotSetAside{File: file, At: fresh, Want: old, Err: syscall.ENOSPC}), syscall.ENOSPC,
			[]string{"in place at " + file, "stays at " + fresh, "rename it to " + old, "a rebuild run again would remove", "no space left on device"},
			[]string{"not yet in place", "-wal", "its log"}},
		{"the swap failed", fmt.Errorf("catalog: %w", syscall.EINVAL), syscall.EINVAL,
			[]string{"the new catalog is " + fresh + ", not yet in place"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := notInPlace(fresh, tc.err)
			for _, part := range tc.want {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%q says nothing of %q", err, part)
				}
			}
			for _, part := range tc.not {
				if strings.Contains(err.Error(), part) {
					t.Errorf("%q says %q", err, part)
				}
			}
			if !errors.Is(err, tc.cause) {
				t.Errorf("%q lost its cause", err)
			}
		})
	}
}
