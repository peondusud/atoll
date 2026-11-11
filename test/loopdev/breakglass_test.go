//go:build loopdev

package loopdev

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/view"
)

// TestBreakGlass: a pool used through mergerfs mounted from the line atoll mergerfs-config prints, then Atoll
// stopped and removed, its catalog and configuration gone, and plain mergerfs mounted again from that line at the
// same mount point. The view lists and holds the same as before, which is the library as the user left it, and no
// disk holds a hidden file (CLAUDE.md, break glass; specs.md G1, R-02, §21.1, §27). The pool has no stale copy and
// no conflict when Atoll goes, as §27 asks.
func TestBreakGlass(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	for _, tc := range trashDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			// A modified copy waits for its settle, ten minutes by default, from its change time (specs.md §15.4): the
			// copy back from an atomic save would wait as long.
			b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")

			// The library as it was before Atoll, on both disks, each file on one.
			write(t, b.keep, "Films/A.mkv", "a")
			write(t, b.keep, "Series/S01/e01.mkv", "e01")
			write(t, b.at, "Films/B.mkv", "b")
			write(t, b.at, "Music/x.flac", "x")
			b.scan(false)
			catchUp(b)

			// The merged view, from the line Atoll prints, and what a user does through it.
			line, pool := fstabLine(b)
			mountView(t, line, pool)
			mustDo(t, os.Rename(filepath.Join(pool, "Films/A.mkv"), filepath.Join(pool, "Films/A (2001).mkv")))
			mustDo(t, os.Remove(filepath.Join(pool, "Films/B.mkv")))
			saved := filepath.Join(pool, "Series/S01/e01.mkv")
			mustDo(t, os.WriteFile(saved+".part", []byte("e01, cut"), 0o644))
			mustDo(t, os.Rename(saved+".part", saved)) // an atomic save
			mustDo(t, os.MkdirAll(filepath.Join(pool, "Music/New"), 0o755))
			mustDo(t, os.WriteFile(filepath.Join(pool, "Music/New/y.flac"), []byte("y"), 0o644))
			catchUp(b)

			want := map[string]string{"Films/A (2001).mkv": "a", "Series/S01/e01.mkv": "e01, cut", "Music/x.flac": "x", "Music/New/y.flac": "y"}
			before, folders := snapshot(t, pool)
			if !maps.Equal(before, want) {
				t.Fatalf("through the view: %v, want %v", before, want)
			}
			// Each disk is a normal tree that holds the library: every file on both, as it is in the view, and nothing else.
			for _, top := range []string{b.keep, b.at} {
				if got, _ := snapshot(t, filepath.Join(top, "media")); !maps.Equal(got, want) {
					t.Errorf("%s: %v", top, got)
				}
			}

			// Atoll stopped and removed: its catalog and configuration gone.
			must(t, "umount", pool)
			mustDo(t, b.db.Close())
			file, _ := b.config.Effective()["catalog"].(string)
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if err := os.Remove(file + suffix); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}

			// Plain mergerfs from the same line at the same mount point: the same view.
			mountView(t, line, pool)
			after, again := snapshot(t, pool)
			if !maps.Equal(after, before) || !slices.Equal(again, folders) {
				t.Errorf("after: %v %v, before: %v %v", after, again, before, folders)
			}
			must(t, "umount", pool)
			for _, top := range []string{b.keep, b.at} {
				if hidden := hiddenNames(t, top); len(hidden) > 0 {
					t.Errorf("%s holds hidden names: %v", top, hidden)
				}
			}
		})
	}
}

// needMergerfs skips where mergerfs is missing, but in the qemu guest, where run.sh installs it: there, its absence
// is a broken guest, not a machine without it.
func needMergerfs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mergerfs"); err != nil {
		if os.Getenv("ATOLL_QEMU") != "" {
			t.Fatal("no mergerfs in the qemu guest: test/qemu/run.sh installs it")
		}
		t.Skip("no mergerfs")
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// catchUp runs the passes and the copies until every file is ok: the pool then has no stale copy, no pending one and
// no conflict. The copies run at once, as atoll replicate --now: a file saved a moment ago would wait for its settle.
func catchUp(b *passBench) {
	b.t.Helper()
	var outcomes []string
	for range 5 {
		b.settle()
		if _, err := jobs.Replicate(b.t.Context(), b.db, b.config, b.machine, jobs.ReplicateOptions{Now: true}, func(item *jobs.ReplicateItem) error {
			outcomes = append(outcomes, item.Path+" "+item.Source+">"+item.Disk+" "+item.Outcome)
			return nil
		}); err != nil {
			b.t.Fatal(err)
		}
		if b.text("SELECT count(*) FROM files WHERE state <> 'ok'")+b.text("SELECT count(*) FROM copies WHERE state <> 'ok'") == "00" {
			return
		}
	}
	b.t.Fatalf("copies run: %v; files not ok: %s; copies not ok: %s; jobs: %s", outcomes,
		b.text("SELECT group_concat(CAST(lpath AS TEXT) || ' ' || state, '; ') FROM files WHERE state <> 'ok'"),
		b.text("SELECT group_concat(disk_id || ' ' || state, '; ') FROM copies WHERE state <> 'ok'"),
		b.text("SELECT group_concat(kind || ' ' || state || ' ' || COALESCE(src_disk, '-') || '>' || COALESCE(dst_disk, '-'), '; ') FROM jobs"))
}

// fstabLine gives the line atoll mergerfs-config prints for the bench, from what its catalog recorded, and the
// view's mount point.
func fstabLine(b *passBench) (string, string) {
	b.t.Helper()
	list, err := catalog.Disks(b.t.Context(), b.db)
	mustDo(b.t, err)
	capacities := map[string]int64{}
	for _, d := range list {
		if d.HasCapacity {
			capacities[d.ID] = d.Capacity
		}
	}
	minFree, err := b.config.MinFree()
	mustDo(b.t, err)
	m, err := view.NewMergerfs(b.config, minFree, capacities)
	mustDo(b.t, err)
	for _, branch := range m.Branches {
		if branch.MinFree == nil {
			b.t.Fatalf("disk %s: no minfreespace", branch.Disk)
		}
	}
	return m.Fstab(), m.Mount
}

// mountView mounts mergerfs from a line of fstab, through mount(8) and mount.mergerfs, as a boot would.
func mountView(t *testing.T, line, at string) {
	t.Helper()
	mustDo(t, os.MkdirAll(at, 0o755))
	table := filepath.Join(t.TempDir(), "fstab")
	mustDo(t, os.WriteFile(table, []byte(line+"\n"), 0o644))
	must(t, "mount", "-T", table, at)
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "umount", at).Run() })
}

// snapshot gives the content of each file below root, and its folders, by path relative to it.
func snapshot(t *testing.T, root string) (map[string]string, []string) {
	t.Helper()
	files := map[string]string{}
	var folders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			folders = append(folders, rel)
			return nil
		}
		data, err := os.ReadFile(path)
		files[rel] = string(data)
		return err
	})
	mustDo(t, err)
	return files, folders
}

// hiddenNames lists every name below top that starts with a dot.
func hiddenNames(t *testing.T, top string) []string {
	t.Helper()
	var hidden []string
	err := filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
		if err == nil && path != top && strings.HasPrefix(d.Name(), ".") {
			hidden = append(hidden, path)
		}
		return err
	})
	mustDo(t, err)
	return hidden
}
