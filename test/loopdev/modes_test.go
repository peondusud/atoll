//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/view"
)

// TestBranchModes: mergerfs takes at runtime the modes a draining and a failed disk get, NC and RO, and keeps them:
// a file created through the view lands on neither, and a file only on the RO branch is still read (specs.md
// §18, §21.1).
func TestBranchModes(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBench(t, v, at)
	pool := viewOf(t, b)
	keep, other := filepath.Join(b.keep, "media"), filepath.Join(at, "media")
	mustDo(t, os.WriteFile(filepath.Join(other, "only-there.mkv"), []byte("x"), 0o644))
	// A third branch, RW, so that a create has somewhere to go.
	third := filepath.Join(t.TempDir(), "third")
	mustDo(t, os.MkdirAll(third, 0o755))
	want := keep + "=NC:" + third + "=RW:" + other + "=RO"
	mustDo(t, view.WriteBranches(pool, want))
	got, err := view.ReadBranches(pool)
	mustDo(t, err)
	if got != want {
		t.Errorf("branches %q, want %q", got, want)
	}
	mustDo(t, os.WriteFile(filepath.Join(pool, "new.mkv"), []byte("y"), 0o644))
	if _, err := os.Stat(filepath.Join(third, "new.mkv")); err != nil {
		t.Errorf("a new file did not land on the RW branch: %v", err)
	}
	for _, branch := range []string{keep, other} {
		if _, err := os.Stat(filepath.Join(branch, "new.mkv")); err == nil {
			t.Errorf("a new file landed on %s", branch)
		}
	}
	if data, err := os.ReadFile(filepath.Join(pool, "only-there.mkv")); err != nil || string(data) != "x" {
		t.Errorf("a file only on the RO branch: %q, %v", data, err)
	}
}
