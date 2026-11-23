package watch

import (
	"testing"

	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

// kernelNTFS is a bench whose disk t2 is NTFS, mounted with the driver of the given type, as the mount check asks of it.
type kernelNTFS struct {
	*bench
	fstype string
}

func (k *kernelNTFS) Mount(mount string) disks.MountSeen {
	seen := k.bench.Mount(mount)
	if mount == k.dirs["t2"] {
		seen.Entry.FSType = k.fstype
		seen.Entry.SuperOptions = append(seen.Entry.SuperOptions, "iocharset=utf8", "errors=remount-ro")
	}
	return seen
}

// TestAWildcardNameLeftOnTheTypeNTFS: a copy at a name holding a wildcard character, on a disk of the type ntfs, which
// cannot move or remove it without setting its volume read-only, is left as it is (specs.md §15.8, §22): a scan records
// it, and its file is name-incompatible, though another copy of it is ok; a name with no wildcard there is a copy as
// any. On ntfs3, which moves and removes such a name, the same copy counts.
func TestAWildcardNameLeftOnTheTypeNTFS(t *testing.T) {
	for fstype, want := range map[string]string{"ntfs": "name-incompatible", "ntfs3": "ok"} {
		t.Run(fstype, func(t *testing.T) {
			b := newBench(t, "t1", "t2")
			b.config.Disks[1].FS = names.NTFS
			m := &kernelNTFS{bench: b, fstype: fstype}
			for _, disk := range []string{"t1", "t2"} {
				b.write(disk, "Who?/q?.mkv", "content")
				b.write(disk, "plain.mkv", "content")
			}
			if _, err := Scan(t.Context(), b.db, b.config, m, Options{Full: true}); err != nil {
				t.Fatal(err)
			}
			if got := b.count("SELECT count(*) FROM copies"); got != 4 {
				t.Errorf("%d copies recorded, want 4", got)
			}
			for path, state := range map[string]string{"Who?/q?.mkv": want, "plain.mkv": "ok"} {
				if got := b.count("SELECT count(*) FROM files WHERE lpath = CAST(? AS BLOB) AND state = ?", path, state); got != 1 {
					t.Errorf("%s is not %s", path, state)
				}
			}
		})
	}
}
