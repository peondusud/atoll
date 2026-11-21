package ops

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/integrity"
	"github.com/peondusud/atoll/internal/watch"
)

// What Atoll writes in place, or makes and removes beside a copy, loses nothing wherever it fails, and says so: the
// parts of a copy, the folders below the root, the link guard's links, the chunks a repair saves and writes, a copy's
// metadata, and what it removes of its own (specs.md §14.2, §16.1, §17, §17.5).
func TestWritesLoseNothingWhereverTheyFail(t *testing.T) {
	sweep(t, sweepCase{name: "a part made", prepare: func(t *testing.T, mount string) swept {
		op := &Op{ID: 5, Disk: "t1", Kind: catalog.OpCreate, Path: "a", Path2: "5.part"}
		var part *Part
		return swept{
			op: func(d *Disk) error {
				var err error
				if part, err = d.MakePart(op); err == nil {
					_ = part.Close()
				}
				return err
			},
			done: func() bool {
				st := statAt(t, mount, "Atoll/tmp/5.part")
				return part != nil && st.Ino == part.Stat.Ino && st.Mode&0o777 == 0o600
			},
			// A driver that refuses the mode leaves the folder of the parts as it is, with a warning.
			best: func() bool { return statAt(t, mount, "Atoll/tmp").Mode&0o077 == 0 },
		}
	}})
	for _, like := range []bool{false, true} {
		sweep(t, sweepCase{name: "a folder made below the root, like another " + map[bool]string{false: "none", true: "one"}[like],
			prepare: func(t *testing.T, mount string) swept {
				put(t, mount, "media/Films/other", "x")
				var model *fsx.Stat
				if like {
					model = &fsx.Stat{Kind: fsx.KindDir, Mode: 0o700, Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
				}
				want := uint32(FolderMode)
				if like {
					want = 0o700
				}
				var made fsx.Stat
				return swept{
					op: func(d *Disk) error {
						var err error
						made, err = d.MakeRootFolder("Films/New", model)
						return err
					},
					done: func() bool {
						st := statAt(t, mount, "media/Films/New")
						return st.Kind == fsx.KindDir && st.Mode&0o777 == want && made.Ino == st.Ino
					},
				}
			}})
	}
	sweep(t, sweepCase{name: "a folder removed", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Films/other", "x")
		if err := os.Mkdir(filepath.Join(mount, "media", "Films", "Old"), 0o755); err != nil {
			t.Fatal(err)
		}
		op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpRmdir, Path: "Films/Old", FileKey: keyAt(t, mount, "media/Films/Old")}
		return swept{
			op:   func(d *Disk) error { return d.RemoveFolder(op) },
			done: func() bool { return absent(mount, "media/Films/Old") },
			kept: func() bool { return holding(mount, "media/Films/other", "x") },
		}
	}})
	sweep(t, sweepCase{name: "an entry purged", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "Atoll/trash/2026-01-01/7/a", "old")
		op := &Op{ID: 8, Disk: "t1", Kind: catalog.OpUnlink, Path: "a", Path2: "2026-01-01/7/a", FileKey: keyAt(t, mount, "Atoll/trash/2026-01-01/7/a")}
		var purged Purged
		return swept{
			op:   func(d *Disk) error { var err error; purged, err = d.PurgeEntry(op); return err },
			done: func() bool { return absent(mount, "Atoll/trash/2026-01-01/7/a") && purged.Nlink == 1 },
		}
	}})
	sweep(t, sweepCase{name: "a link of the guard made", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Films/a", "film")
		op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpLink, Path: "Films/a", Link: 42, FileKey: keyAt(t, mount, "media/Films/a")}
		var links *catalog.Links
		return swept{
			op: func(d *Disk) error {
				folder, err := d.OpenLinks(true)
				if err != nil {
					return err
				}
				defer folder.Close()
				if links, err = d.MakeLink(op, folder); err != nil {
					return err
				}
				return folder.Sync()
			},
			done: func() bool { return sameFile(t, mount, "media/Films/a", "Atoll/links/42") && links != nil },
			// The links folder unread, the link counts include the guard's own, with a warning (specs.md §17): one more
			// name only makes what depends on it more careful.
			best: func() bool { return links.Nlink == 1 && statAt(t, mount, "Atoll/links").Mode&0o077 == 0 },
			kept: func() bool { return holding(mount, "media/Films/a", "film") },
		}
	}})
	sweep(t, sweepCase{name: "a link of the guard removed", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Films/a", "film")
		if err := os.MkdirAll(filepath.Join(mount, "Atoll", "links"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(mount, "media", "Films", "a"), filepath.Join(mount, "Atoll", "links", "42")); err != nil {
			t.Fatal(err)
		}
		op := &Op{ID: 7, Disk: "t1", Kind: catalog.OpUnlink, Path: "Films/a", Link: 42, FileKey: keyAt(t, mount, "media/Films/a")}
		return swept{
			op: func(d *Disk) error {
				folder, err := d.OpenLinks(false)
				if err != nil {
					return err
				}
				defer folder.Close()
				if _, err := d.DropLink(op, folder); err != nil {
					return err
				}
				return folder.Sync()
			},
			done: func() bool { return absent(mount, "Atoll/links/42") },
			kept: func() bool { return holding(mount, "media/Films/a", "film") },
		}
	}})
	sweep(t, sweepCase{name: "a bad chunk saved", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Films/a", "film")
		op := &Op{ID: 9, Disk: "t1", Kind: catalog.OpRepair, Path: "Films/a", Path2: "2026-01-01/9.chunks/Films/a"}
		copyStat := statAt(t, mount, "media/Films/a")
		var saved map[int64]SavedChunk
		return swept{
			op: func(d *Disk) error {
				if err := d.SaveChunk(op, 0, []byte("film"), &copyStat); err != nil {
					return err
				}
				if err := d.SyncChunks(op); err != nil {
					return err
				}
				var err error
				saved, err = d.SavedChunks(op)
				return err
			},
			done: func() bool {
				return holding(mount, "Atoll/trash/2026-01-01/9.chunks/Films/a/00000000", "film") &&
					saved[0] == SavedChunk{Size: 4, Sum: integrity.ChunkSum([]byte("film"))} &&
					statAt(t, mount, "Atoll/trash/2026-01-01/9.chunks/Films/a").Mode&0o077 == 0
			},
			// Saving a chunk never touches the copy it comes from.
			kept: func() bool { return holding(mount, "media/Films/a", "film") },
		}
	}})
	sweep(t, sweepCase{name: "the chunks of an entry purged", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "Atoll/trash/2026-01-01/9.chunks/Films/a/00000000", "c0")
		put(t, mount, "Atoll/trash/2026-01-01/9.chunks/Films/a/00000003", "c3")
		op := &Op{ID: 10, Disk: "t1", Kind: catalog.OpUnlink, Path: "Films/a", Path2: "2026-01-01/9.chunks/Films/a"}
		const folder = "Atoll/trash/2026-01-01/9.chunks/Films/a"
		return swept{
			op: func(d *Disk) error { return d.PurgeChunks(op) },
			// No chunk is left once it says so: the folder that held them may stay, empty, which harms nothing.
			done: func() bool { return absent(mount, folder+"/00000000") && absent(mount, folder+"/00000003") },
			best: func() bool { return absent(mount, folder) },
		}
	}})
	// A chunk written in place that cannot be read back is for a confirmation of the disk to tell apart from a sector
	// given no other place (specs.md §16.1): the repair gives the error as it is.
	sweep(t, sweepCase{name: "a chunk written in place", raw: true, prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Films/a", "fi1m")
		row := rowAt(t, mount, "Films/a")
		before := statAt(t, mount, "media/Films/a")
		good := integrity.ChunkSum([]byte("film"))
		return swept{
			op: func(d *Disk) error {
				f, _, err := d.OpenRepair(row)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				if err := d.WriteChunk(f, 0, []byte("film"), good[:]); err != nil {
					return err
				}
				_, err = d.SetTimeBack(f, before.Mtime)
				return err
			},
			done: func() bool {
				return holding(mount, "media/Films/a", "film") && statAt(t, mount, "media/Films/a").Mtime == before.Mtime
			},
		}
	}})
	metadata := []struct {
		name string
		set  func(d *Disk, row *catalog.KnownCopy) error
		is   func(st *fsx.Stat) bool
	}{
		{"a time set", func(d *Disk, row *catalog.KnownCopy) error {
			_, err := d.SetMtime(row, fsx.Time{Sec: 1000, Nsec: 5})
			return err
		}, func(st *fsx.Stat) bool { return st.Mtime == fsx.Time{Sec: 1000, Nsec: 5} }},
		{"a mode set", func(d *Disk, row *catalog.KnownCopy) error {
			_, err := d.SetMode(row, 0o600)
			return err
		}, func(st *fsx.Stat) bool { return st.Mode&0o777 == 0o600 }},
		{"an owner set", func(d *Disk, row *catalog.KnownCopy) error {
			_, err := d.SetOwner(row, uint32(os.Getuid()), uint32(os.Getgid()))
			return err
		}, func(st *fsx.Stat) bool { return st.Uid == uint32(os.Getuid()) }},
	}
	for _, m := range metadata {
		sweep(t, sweepCase{name: m.name, prepare: func(t *testing.T, mount string) swept {
			put(t, mount, "media/Films/a", "film")
			row := rowAt(t, mount, "Films/a")
			return swept{
				op:   func(d *Disk) error { return m.set(d, row) },
				done: func() bool { return m.is(new(statAt(t, mount, "media/Films/a"))) },
				kept: func() bool { return holding(mount, "media/Films/a", "film") },
			}
		}})
	}
	sweep(t, sweepCase{name: "extended attributes given", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Films/a", "film")
		if err := setXattr(filepath.Join(mount, "media", "Films", "a"), "user.old", "1"); err != nil {
			t.Skipf("no user. extended attributes here: %v", err)
		}
		row := rowAt(t, mount, "Films/a")
		return swept{
			op: func(d *Disk) error {
				_, _, err := d.GiveXattrs(row, map[string][]byte{"user.new": []byte("2")})
				return err
			},
			done: func() bool {
				got := xattrsAt(t, mount, "media/Films/a")
				return len(got) == 1 && string(got["user.new"]) == "2"
			},
			kept: func() bool { return holding(mount, "media/Films/a", "film") },
		}
	}})
	sweep(t, sweepCase{name: "a catalog put on a disk", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "Atoll/catalog/catalog.db", "before")
		return swept{
			op:   func(d *Disk) error { return d.PutCatalog(strings.NewReader("after")) },
			done: func() bool { return holding(mount, "Atoll/catalog/catalog.db", "after") },
			// At any moment the disk holds the one before or the new one (specs.md §9).
			kept: func() bool {
				return holding(mount, "Atoll/catalog/catalog.db", "before") || holding(mount, "Atoll/catalog/catalog.db", "after")
			},
		}
	}})
	// disk add stops at any error a marker gives.
	sweep(t, sweepCase{name: "a marker written", raw: true, prepare: func(t *testing.T, mount string) swept {
		return swept{
			op: func(d *Disk) error { return d.WriteMarker("pool_id = \"p\"\n") },
			done: func() bool {
				return holding(mount, "Atoll/disk.toml", "pool_id = \"p\"\n") && absent(mount, "Atoll/disk.toml.new")
			},
		}
	}})
	sweep(t, sweepCase{name: "a marker written with the rename ntfs-3g refuses", driver: asNTFS3G, raw: true,
		prepare: func(t *testing.T, mount string) swept {
			return swept{
				op:   func(d *Disk) error { return d.WriteMarker("pool_id = \"p\"\n") },
				done: func() bool { return holding(mount, "Atoll/disk.toml", "pool_id = \"p\"\n") },
			}
		}})
}

// What Atoll reads of its own work folder and of the library, before it writes there, says what it found or fails:
// never an absence for a disk it cannot read (invariant 4).
func TestLooksSayNothingOfWhatTheyCannotRead(t *testing.T) {
	sweep(t, sweepCase{name: "a source opened and looked at again", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Films/a", "film")
		row := rowAt(t, mount, "Films/a")
		still := false
		return swept{
			op: func(d *Disk) error {
				f, st, err := d.OpenSource("Films/a", row)
				if err != nil {
					return err
				}
				_ = f.Close()
				still, err = d.SourceStill("Films/a", &st)
				return err
			},
			done: func() bool { return still },
		}
	}})
	sweep(t, sweepCase{name: "an entry of the trash opened and looked at again", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "Atoll/trash/2026-01-01/7/a", "old")
		key := keyAt(t, mount, "Atoll/trash/2026-01-01/7/a")
		still := false
		return swept{
			op: func(d *Disk) error {
				f, st, err := d.OpenTrashSource("2026-01-01/7/a", key)
				if err != nil {
					return err
				}
				_ = f.Close()
				still, err = d.TrashSourceStill("2026-01-01/7/a", &st)
				return err
			},
			done: func() bool { return still },
		}
	}})
	sweep(t, sweepCase{name: "a part opened again", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "Atoll/tmp/5.part", "half")
		op := &Op{ID: 5, Disk: "t1", Kind: catalog.OpCreate, Path: "a", Path2: "5.part"}
		var part *Part
		return swept{
			op: func(d *Disk) error {
				var err error
				if part, err = d.OpenPart(op); err == nil {
					_ = part.Close()
				}
				return err
			},
			done: func() bool { return part != nil && keyOf(&part.Stat) == keyAt(t, mount, "Atoll/tmp/5.part") },
		}
	}})
	sweep(t, sweepCase{name: "the parts listed and cleaned", prepare: func(t *testing.T, mount string) swept {
		for _, name := range []string{"3.part", "4.part", "6.part", "notes"} {
			put(t, mount, "Atoll/tmp/"+name, name)
		}
		var listed []string
		return swept{
			op: func(d *Disk) error {
				var err error
				if listed, err = d.TmpNames(); err != nil {
					return err
				}
				if err := d.CleanTmp(map[string]string{"3.part": "done", "4.part": "planned", "6.part": "undone"}, map[string]bool{"6.part": true}); err != nil {
					return err
				}
				return d.DropPart("6.part")
			},
			done: func() bool {
				slices.Sort(listed)
				return slices.Equal(listed, []string{"3.part", "4.part", "6.part", "notes"}) && absent(mount, "Atoll/tmp/3.part") &&
					absent(mount, "Atoll/tmp/6.part") && holding(mount, "Atoll/tmp/4.part", "4.part") && holding(mount, "Atoll/tmp/notes", "notes")
			},
			// A part an operation left planned names, and what is no part of Atoll's, stay.
			kept: func() bool {
				return holding(mount, "Atoll/tmp/4.part", "4.part") && holding(mount, "Atoll/tmp/notes", "notes")
			},
		}
	}})
	sweep(t, sweepCase{name: "what stands on the way of a path", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Films/a", "film")
		var taken, takenFile bool
		var missing, missingTarget []string
		var target string
		var folder, file *fsx.Stat
		return swept{
			op: func(d *Disk) error {
				var err error
				if taken, missing, err = d.LookAt("Films/New/Deeper/b"); err != nil {
					return err
				}
				if takenFile, _, err = d.LookAt("Films/a"); err != nil {
					return err
				}
				if target, missingTarget, err = d.LookAtTarget("Films/New/b"); err != nil {
					return err
				}
				if folder, err = d.FolderInfo("Films"); err != nil {
					return err
				}
				file, err = d.FolderInfo("Films/a")
				return err
			},
			done: func() bool {
				return !taken && slices.Equal(missing, []string{"Films/New", "Films/New/Deeper"}) && takenFile &&
					target == TargetFree && slices.Equal(missingTarget, []string{"Films/New"}) && folder != nil && file == nil
			},
		}
	}})
	sweep(t, sweepCase{name: "the links of the guard read", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "Atoll/trash/2026-01-01/7/a", "old")
		if err := os.MkdirAll(filepath.Join(mount, "Atoll", "links"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(mount, "Atoll", "trash", "2026-01-01", "7", "a"), filepath.Join(mount, "Atoll", "links", "42")); err != nil {
			t.Fatal(err)
		}
		put(t, mount, "Atoll/links/notes", "not a link of Atoll's")
		key := keyAt(t, mount, "Atoll/links/42")
		var own []int64
		var listed []LinkEntry
		var highest int64
		return swept{
			op: func(d *Disk) error {
				var err error
				if own, err = d.OwnLinksOf("2026-01-01/7/a", key); err != nil {
					return err
				}
				if listed, err = d.Links(); err != nil {
					return err
				}
				highest, err = d.HighestLinkID()
				return err
			},
			done: func() bool { return len(listed) == 2 && listed[0].Name == "42" && listed[0].File && highest == 42 },
			// The links folder unread, an entry of the trash has none of the guard's, with a warning.
			best: func() bool { return slices.Equal(own, []int64{42}) },
		}
	}})
	// A catalog rebuild, and a new catalog, stop at any error the work folder gives.
	sweep(t, sweepCase{name: "the highest operation the work folder names", raw: true, prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "Atoll/trash/2026-01-01/7/a", "old")
		put(t, mount, "Atoll/trash/2026-01-02/9.chunks/b/00000000", "c")
		put(t, mount, "Atoll/tmp/12.3.part", "c")
		var highest int64
		var found []FoundEntry
		return swept{
			op: func(d *Disk) error {
				var err error
				if highest, err = d.HighestOpID(); err != nil {
					return err
				}
				found, err = d.FoundTrash(func(string, string) {}, watch.Unbounded)
				return err
			},
			done: func() bool { return highest == 12 && len(found) == 2 },
		}
	}})
	sweep(t, sweepCase{name: "a marker read", raw: true, prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "Atoll/disk.toml", "pool_id = \"p\"\n")
		var text []byte
		return swept{
			op: func(d *Disk) error {
				var err error
				text, _, err = d.ReadMarker()
				return err
			},
			done: func() bool { return bytes.Equal(text, []byte("pool_id = \"p\"\n")) },
		}
	}})
}

// xattrsAt gives the user. extended attributes of a name below a mount.
func xattrsAt(t *testing.T, mount, rel string) map[string][]byte {
	t.Helper()
	full := filepath.Join(mount, filepath.FromSlash(rel))
	dir, err := fsx.OpenDir(filepath.Dir(full))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	got, err := dir.Xattrs(filepath.Base(full), fsx.UserNamespace)
	if err != nil {
		t.Fatal(err)
	}
	return maps.Clone(got)
}

// setXattr sets a user. extended attribute of a file.
func setXattr(path, name, value string) error {
	dir, err := fsx.OpenDir(filepath.Dir(path))
	if err != nil {
		return err //nolint:wrapcheck // a test's
	}
	defer func() { _ = dir.Close() }()
	return dir.SetXattr(filepath.Base(path), name, []byte(value), 0) //nolint:wrapcheck // a test's
}
