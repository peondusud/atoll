//go:build loopdev

package loopdev

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/names"
)

// linksOf counts what became of a group of links once their device failed and came back.
type linksOf struct {
	// there are the links found again, each a regular file.
	there int
	// named are those whose file still has its name in media/, and same those that lead to the file at that name.
	named, same int
	// whole are those that read as the file was written.
	whole int
	// counted are those whose file counts as many names as it has.
	counted int
	// broken are the names a look could not conclude of, as a driver with no journal answers of a name and a record
	// that disagree.
	broken int
}

func (l linksOf) String() string {
	return fmt.Sprintf("%d there, %d named in media/, %d the same file, %d whole, %d with the right count of names, %d broken",
		l.there, l.named, l.same, l.whole, l.counted, l.broken)
}

// TestALinkIsDurableOnceItsFolderIsSynced measures what the sync of each file the link guard links buys (specs.md
// §17, §17.1). The guard syncs each file it links, then the folder of the links once for a batch: 200,000 syncs when
// it is turned on over a library, as §17.1 asks of every name on every driver since ntfs3 was measured to lose one
// without it. On a filesystem that journals a name with its folder, the folder's sync alone should do.
//
// A device fails right after the sync of a folder of links, as §17.1 measured a move: links to files long written,
// and to files just written that nothing synced, each made with no sync of its own, then one sync of their folder,
// then links that nothing syncs at all, and the device fails. The same with each file synced first, as the guard
// does, is the reference. What was measured, on Linux 6.12 and 7.3, and what this test holds each kernel to:
//
//   - on ext4, XFS and btrfs, every link the folder's sync covered is there, leads to its file and counts among its
//     names, and the filesystem checks clean: a name is written with its folder. The links nothing synced are gone,
//     so the failure does take away what was not written;
//   - but a file just written comes back empty on ext4 and XFS without the sync of each file, and so do some on
//     btrfs, where that sync had written it: 0 of 300 whole, and 272 to 280 of 300. Its name and its link are there;
//   - an ext4 made without a journal keeps the links, and the count of names of 1 file of 300: it writes a folder
//     without the file a new name leads to. Logged only, as no kernel promises anything there;
//   - ntfs3 loses every link without the sync of each file, and keeps them with it; ntfs-3g keeps them either way.
//     Logged only: §17.1 holds those drivers to what it measured of a move.
//
// So the sync of each file stays: leaving it out is safe for a file whose content is on its disk, on a filesystem
// known to journal, and Atoll knows neither yet (TODO.md).
func TestALinkIsDurableOnceItsFolderIsSynced(t *testing.T) {
	needRoot(t)
	if _, err := exec.LookPath("dmsetup"); err != nil {
		t.Skip("no dmsetup")
	}
	for _, tc := range []struct {
		name   string
		fs     names.Filesystem
		driver string
		mkfs   []string
		check  []string
		// journals says that the filesystem writes a name with its folder: what the measure must show.
		journals bool
	}{
		{"ext4", names.Ext4, "", nil, []string{"e2fsck", "-f", "-n"}, true},
		{"xfs", names.XFS, "", nil, []string{"xfs_repair", "-n"}, true},
		{"btrfs", names.Btrfs, "", nil, []string{"btrfs", "check", "--readonly"}, true},
		{"ext4-no-journal", names.Ext4, "", []string{"-O", "^has_journal"}, []string{"e2fsck", "-f", "-n"}, false},
		{"ntfs3", names.NTFS, "ntfs3", nil, nil, false},
		{"ntfs-3g", names.NTFS, "ntfs-3g", nil, nil, false},
	} {
		for _, way := range []struct {
			name     string
			syncFile bool
		}{{"folder-alone", false}, {"file-then-folder", true}} {
			t.Run(tc.name+"/"+way.name, func(t *testing.T) {
				old, fresh, unsynced, checked := linksAfterAFailure(t, tc.fs, tc.driver, tc.mkfs, tc.check, way.syncFile)
				t.Logf("links to files long written: %s", old)
				t.Logf("links to files just written: %s", fresh)
				t.Logf("links nothing synced: %d of %d there; the filesystem: %s", unsynced.there, linkCount, checked)
				if !tc.journals {
					return
				}
				for what, got := range map[string]linksOf{"long written": old, "just written": fresh} {
					if got.there != linkCount || got.broken != 0 {
						t.Errorf("%d of %d links to files %s came through the failure, %d broken", got.there, linkCount, what, got.broken)
					}
					if got.same != got.named || got.counted != got.there {
						t.Errorf("links to files %s: %s", what, got)
					}
				}
				// A file long written is at its name and whole, whatever became of its link.
				if old.named != linkCount || old.whole != linkCount {
					t.Errorf("files long written: %s", old)
				}
				if checked != "clean" {
					t.Errorf("the filesystem does not check clean: %s", checked)
				}
				if unsynced.there == linkCount {
					t.Errorf("every link nothing synced came through too: the failure took nothing away, and the measure says nothing")
				}
				// What the sync of each file buys where a name is journaled: the content of a file just written.
				if way.syncFile && fresh.whole != linkCount {
					t.Errorf("%d of %d files just written are whole, each synced before the failure", fresh.whole, linkCount)
				}
			})
		}
	}
}

// linkCount is how many files of each kind are linked: enough for a folder of links to hold several blocks.
const linkCount = 300

// linksAfterAFailure makes the links, fails the device right after the sync of their folder, brings it back, and
// counts what is there.
func linksAfterAFailure(t *testing.T, fs names.Filesystem, driver string, mkfs, check []string, syncFile bool) (old, fresh, unsynced linksOf, checked string) {
	t.Helper()
	options := driverOptions(t, driver)
	m := newMapped(t, fs, mkfs...)
	at := mountPoint(t)
	mountVolume(t, m.volume, driver, at, options...)
	media, links := filepath.Join(at, "media"), filepath.Join(at, "Atoll", "links")
	for _, dir := range []string{filepath.Join(media, "old"), filepath.Join(media, "new"), links} {
		mustDo(t, os.MkdirAll(dir, 0o755))
	}
	text := func(kind string, i int) []byte {
		return []byte(strings.Repeat(fmt.Sprintf("%s %04d\n", kind, i), 600))
	}
	name := func(kind string, i int) string { return filepath.Join(media, kind, fmt.Sprintf("%04d.mkv", i)) }
	link := func(group, i int) string { return filepath.Join(links, fmt.Sprintf("%d%04d", group, i)) }
	for i := range linkCount {
		mustDo(t, os.WriteFile(name("old", i), text("old", i), 0o644))
	}
	// Everything so far is on the device: the files long written, and the folders.
	must(t, "sync")
	for i := range linkCount {
		mustDo(t, os.WriteFile(name("new", i), text("new", i), 0o644))
	}
	syncOf := func(path string) {
		t.Helper()
		f, err := os.Open(path)
		mustDo(t, err)
		defer func() { _ = f.Close() }()
		mustDo(t, f.Sync())
	}
	for group, kind := range []string{"old", "new"} {
		for i := range linkCount {
			mustDo(t, os.Link(name(kind, i), link(group+1, i)))
			if syncFile {
				syncOf(link(group+1, i))
			}
		}
	}
	syncOf(links)
	// What nothing syncs: a second link to each file long written.
	for i := range linkCount {
		mustDo(t, os.Link(name("old", i), link(9, i)))
	}
	m.fail(t)

	_ = exec.CommandContext(context.Background(), "umount", "-l", at).Run()
	time.Sleep(time.Second)
	if strings.HasSuffix(driver, "3g") {
		_ = exec.CommandContext(context.Background(), "pkill", "-9", "-f", driver+" "+m.device+" ").Run()
		time.Sleep(time.Second)
	}
	m.heal(t)
	// Dirty, ntfs3 refuses to mount without force.
	if driver == "ntfs3" {
		options = append(options, "force")
	}
	if err := remount(t, m.volume, driver, at, options); err != nil {
		t.Skipf("the volume did not mount again: %v", err)
	}
	dropCaches(t)

	inode := func(path string) (ino, nlink uint64, there bool, err error) {
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return 0, 0, false, nil
		case err != nil:
			return 0, 0, false, err
		case !info.Mode().IsRegular():
			return 0, 0, false, fmt.Errorf("%s is no regular file: %v", path, info.Mode())
		}
		st := info.Sys().(*syscall.Stat_t)         //nolint:forcetypeassert // Linux only
		return st.Ino, uint64(st.Nlink), true, nil //nolint:unconvert // 32 bits on some architectures
	}
	count := func(group int, kind string, others ...int) linksOf {
		var out linksOf
		for i := range linkCount {
			ino, nlink, there, err := inode(link(group, i))
			if err != nil {
				out.broken++
				continue
			}
			if !there {
				continue
			}
			out.there++
			names := uint64(1)
			nameIno, _, named, err := inode(name(kind, i))
			switch {
			case err != nil:
				out.broken++
			case named:
				out.named++
				names++
				if nameIno == ino {
					out.same++
				}
			}
			for _, other := range others {
				if otherIno, _, there, err := inode(link(other, i)); err == nil && there && otherIno == ino {
					names++
				}
			}
			if nlink == names {
				out.counted++
			}
			if data, err := os.ReadFile(link(group, i)); err == nil && string(data) == string(text(kind, i)) {
				out.whole++
			}
		}
		return out
	}
	old, fresh, unsynced = count(1, "old", 9), count(2, "new"), count(9, "old", 1)
	// A file long written is at its name whatever became of its links: counted here where no link came back.
	if old.there == 0 {
		for i := range linkCount {
			if _, _, named, err := inode(name("old", i)); err == nil && named {
				old.named++
			}
		}
	}

	checked = "not checked"
	if check != nil {
		if err := exec.CommandContext(context.Background(), "umount", at).Run(); err != nil {
			return old, fresh, unsynced, "still mounted: " + err.Error()
		}
		out, err := exec.CommandContext(t.Context(), check[0], append(append([]string{}, check[1:]...), m.device)...).CombinedOutput() //nolint:gosec // the test's own tools
		checked = "clean"
		if err != nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			checked = fmt.Sprintf("%v: %s", err, strings.Join(lines[max(0, len(lines)-6):], " | "))
		}
	}
	return old, fresh, unsynced, checked
}
