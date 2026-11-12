//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// TestNestedSubvolume: a btrfs subvolume below the root is another device under the same mount, so the walk lists
// it in other_mounts and records nothing below it, as for another mount (specs.md §15.10).
func TestNestedSubvolume(t *testing.T) {
	needRoot(t)
	v := newVolume(t, names.Btrfs)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBench(t, v, at)
	must(t, "btrfs", "subvolume", "create", filepath.Join(at, "media", "sub"))
	mustDo(t, os.WriteFile(filepath.Join(at, "media", "sub", "inside.mkv"), []byte("x"), 0o644))
	mustDo(t, os.WriteFile(filepath.Join(at, "media", "beside.mkv"), []byte("y"), 0o644))
	out, err := watch.Scan(t.Context(), b.db, b.config, b.machine, watch.Options{})
	mustDo(t, err)
	for _, r := range out.Disks {
		if r.ID == "v" && !slices.Equal(r.OtherMounts, []string{"sub"}) {
			t.Errorf("other mounts: %q", r.OtherMounts)
		}
	}
	if got := b.text("SELECT group_concat(CAST(lpath AS TEXT), ',') FROM (SELECT lpath FROM files ORDER BY lpath)"); got != "beside.mkv" {
		t.Errorf("files recorded: %s", got)
	}
}
