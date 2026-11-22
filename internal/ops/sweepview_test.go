package ops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
)

// What Atoll's own view does on a disk on a user's behalf loses nothing wherever it fails, and says so, as Atoll's own
// operations do (specs.md §21.2): each folder missing on the way made as the view presents it, nothing replaced, and
// what it makes or moves durable before it returns.
func TestUserOperationsLoseNothingWhereverTheyFail(t *testing.T) {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	like := func(string) (*fsx.Stat, error) { return &fsx.Stat{Mode: 0o750, Uid: uid, Gid: gid}, nil }
	mode := func(m uint32) *uint32 { return &m }
	size := func(n int64) *int64 { return &n }
	at := func(sec int64) *fsx.Time { return &fsx.Time{Sec: sec} }
	symlink := func(t *testing.T, mount, rel, target string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(mount, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	mkdir := func(t *testing.T, mount, rel string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(mount, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, driver := range []struct {
		name string
		fail func(call, name string) error
	}{{"ext4", nil}, {"ntfs-3g", asNTFS3G}} {
		t.Run(driver.name, func(t *testing.T) {
			sweep(t, sweepCase{name: "a file made", driver: driver.fail, prepare: func(t *testing.T, mount string) swept {
				var made fsx.Stat
				return swept{
					op: func(d *Disk) error {
						w, st, err := d.CreateUserFile("Series/S01/e01", 0o640, uid, gid, like)
						if err == nil {
							made = st
							_ = w.Close()
						}
						return err
					},
					done: func() bool {
						st := statAt(t, mount, "media/Series/S01/e01")
						return st.Ino == made.Ino && st.Mode&0o777 == 0o640 && statAt(t, mount, "media/Series").Mode&0o777 == 0o750
					},
				}
			}})
			sweep(t, sweepCase{name: "a file written in place", driver: driver.fail, prepare: func(t *testing.T, mount string) swept {
				put(t, mount, "media/Series/e01", "episode")
				var opened fsx.Stat
				return swept{
					op: func(d *Disk) error {
						w, st, err := d.OpenUserFile("Series/e01")
						if err == nil {
							opened = st
							_ = w.Close()
						}
						return err
					},
					done: func() bool { return opened.Ino == statAt(t, mount, "media/Series/e01").Ino },
					kept: func() bool { return holding(mount, "media/Series/e01", "episode") },
				}
			}})
			sweep(t, sweepCase{name: "a folder and a symbolic link made", driver: driver.fail, prepare: func(t *testing.T, mount string) swept {
				return swept{
					op: func(d *Disk) error {
						if _, err := d.MakeUserFolder("Series/S02", 0o700, uid, gid, like); err != nil {
							return err
						}
						if _, err := d.MakeUserSymlink("Series/latest", "S02", uid, gid, like); err != nil {
							return err
						}
						target, err := d.ReadLinkRoot("Series/latest")
						if err == nil && target != "S02" {
							t.Errorf("the link leads to %q", target)
						}
						return err
					},
					done: func() bool {
						target, err := os.Readlink(filepath.Join(mount, "media", "Series", "latest"))
						return err == nil && target == "S02" && statAt(t, mount, "media/Series/S02").Mode&0o777 == 0o700
					},
				}
			}})
			sweep(t, sweepCase{name: "a file renamed", driver: driver.fail, prepare: func(t *testing.T, mount string) swept {
				put(t, mount, "media/Series/e01", "episode")
				key := keyAt(t, mount, "media/Series/e01")
				return swept{
					op: func(d *Disk) error { return d.RenameUser("Series/e01", "Films/New/e01", false, key, like) },
					done: func() bool {
						return holding(mount, "media/Films/New/e01", "episode") && absent(mount, "media/Series/e01")
					},
					kept: func() bool {
						return holding(mount, "media/Films/New/e01", "episode") || holding(mount, "media/Series/e01", "episode")
					},
				}
			}})
			sweep(t, sweepCase{name: "a folder renamed", driver: driver.fail, prepare: func(t *testing.T, mount string) swept {
				put(t, mount, "media/Series/S01/e01", "episode")
				key := keyAt(t, mount, "media/Series/S01")
				return swept{
					op: func(d *Disk) error { return d.RenameUser("Series/S01", "Films/New/S01", true, key, like) },
					done: func() bool {
						return holding(mount, "media/Films/New/S01/e01", "episode") && absent(mount, "media/Series/S01")
					},
					kept: func() bool {
						return holding(mount, "media/Films/New/S01/e01", "episode") || holding(mount, "media/Series/S01/e01", "episode")
					},
				}
			}})
			// A link refused for anything but the disk is a name the driver does not make: the program copies instead.
			sweep(t, sweepCase{name: "a file given a second name", driver: driver.fail, refusal: ErrNoLink, prepare: func(t *testing.T, mount string) swept {
				put(t, mount, "media/Series/e01", "episode")
				key := keyAt(t, mount, "media/Series/e01")
				return swept{
					op: func(d *Disk) error {
						_, err := d.LinkUser("Series/e01", "Films/New/e01", key, like)
						return err
					},
					done: func() bool { return sameFile(t, mount, "media/Series/e01", "media/Films/New/e01") },
					kept: func() bool { return holding(mount, "media/Series/e01", "episode") },
				}
			}})
			sweep(t, sweepCase{name: "an empty folder and a symbolic link removed", driver: driver.fail, prepare: func(t *testing.T, mount string) swept {
				put(t, mount, "media/Series/e01", "episode")
				mkdir(t, mount, "media/Series/Empty")
				symlink(t, mount, "media/Series/latest", "e01")
				return swept{
					op: func(d *Disk) error {
						if err := d.RemoveUser("Series/Empty", true); err != nil {
							return err
						}
						if err := d.RemoveUser("Series/latest", false); err != nil {
							return err
						}
						return d.TouchParent("Series/e01", fsx.Time{Sec: 7})
					},
					done: func() bool { return absent(mount, "media/Series/Empty") && absent(mount, "media/Series/latest") },
					// A folder's time that cannot be set protects nothing: a warning says so.
					best: func() bool { return statAt(t, mount, "media/Series").Mtime == fsx.Time{Sec: 7} },
					// A regular file is never removed this way: it goes to the trash.
					kept: func() bool { return holding(mount, "media/Series/e01", "episode") },
				}
			}})
		})
	}
	// A new file the view moves to this disk once a write ran out of space on another: its part placed at its path, the
	// folders on the way made as the view presents them (specs.md §21.2).
	sweep(t, sweepCase{name: "a part the view places", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "Atoll/tmp/5.part", "episode")
		op := &Op{ID: 5, Disk: "t1", Kind: catalog.OpCreate, Path: "Series/S01/e01", Path2: "5.part", FileKey: keyAt(t, mount, "Atoll/tmp/5.part")}
		var placed fsx.Stat
		return swept{
			op: func(d *Disk) error {
				var err error
				placed, err = d.PlaceUserPart(op, "Series/S01/e01", like)
				return err
			},
			done: func() bool {
				return holding(mount, "media/Series/S01/e01", "episode") && absent(mount, "Atoll/tmp/5.part") &&
					placed.Ino == statAt(t, mount, "media/Series/S01/e01").Ino && statAt(t, mount, "media/Series").Mode&0o777 == 0o750
			},
			kept: func() bool {
				return holding(mount, "media/Series/S01/e01", "episode") || holding(mount, "Atoll/tmp/5.part", "episode")
			},
		}
	}})
	// The copy a move of the view left on a disk whose trash had no room for it, removed at its path once the same
	// bytes are on another disk, and the look a reprise of that takes (specs.md §21.2).
	sweep(t, sweepCase{name: "a copy left removed", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Series/e01", "episode")
		put(t, mount, "media/Series/e02", "other")
		row := rowAt(t, mount, "Series/e01")
		op := &Op{ID: 6, Disk: "t1", Kind: catalog.OpUnlink, Path: "Series/e01", FileKey: row.FileKey.String}
		before := statAt(t, mount, "media/Series").Mtime
		gone := false
		return swept{
			op: func(d *Disk) error {
				if err := d.RemoveLeft(op, row); err != nil {
					return err
				}
				var err error
				gone, _, err = d.LeftAt(op)
				return err
			},
			done: func() bool {
				return gone && absent(mount, "media/Series/e01") && statAt(t, mount, "media/Series").Mtime == before
			},
			kept: func() bool { return holding(mount, "media/Series/e02", "other") },
		}
	}})
	sweep(t, sweepCase{name: "attributes changed", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Series/e01", "episode")
		mkdir(t, mount, "media/Series/S01")
		symlink(t, mount, "media/Series/latest", "e01")
		return swept{
			op: func(d *Disk) error {
				if _, err := d.SetUserAttrs("Series/e01", &UserAttrs{UID: &uid, GID: &gid, Mode: mode(0o600), Size: size(3), Atime: at(5), Mtime: at(6)}); err != nil {
					return err
				}
				if _, err := d.SetUserAttrs("Series/S01", &UserAttrs{UID: &uid, Mode: mode(0o700), Mtime: at(8)}); err != nil {
					return err
				}
				_, err := d.SetUserAttrs("Series/latest", &UserAttrs{GID: &gid})
				return err
			},
			done: func() bool {
				file, folder := statAt(t, mount, "media/Series/e01"), statAt(t, mount, "media/Series/S01")
				return holding(mount, "media/Series/e01", "epi") && file.Mode&0o777 == 0o600 && file.Mtime == fsx.Time{Sec: 6} &&
					folder.Mode&0o777 == 0o700 && folder.Mtime == fsx.Time{Sec: 8}
			},
		}
	}})
	sweep(t, sweepCase{name: "extended attributes set and removed", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Series/e01", "episode")
		if err := setXattr(filepath.Join(mount, "media", "Series", "e01"), "user.old", "1"); err != nil {
			t.Skipf("no user. extended attributes here: %v", err)
		}
		var read map[string][]byte
		return swept{
			op: func(d *Disk) error {
				if err := d.SetUserXattr("Series/e01", "user.new", []byte("2"), 0); err != nil {
					return err
				}
				if err := d.RemoveUserXattr("Series/e01", "user.old"); err != nil {
					return err
				}
				var err error
				read, _, err = d.UserXattrs("Series/e01")
				return err
			},
			done: func() bool {
				got := xattrsAt(t, mount, "media/Series/e01")
				return len(got) == 1 && string(got["user.new"]) == "2" && len(read) == 1
			},
			kept: func() bool { return holding(mount, "media/Series/e01", "episode") },
		}
	}})
	sweep(t, sweepCase{name: "what the view looks at", prepare: func(t *testing.T, mount string) swept {
		put(t, mount, "media/Series/e01", "episode")
		put(t, mount, "Atoll/trash/2026-01-01/7/a", "old")
		var root, file, entry bool
		return swept{
			op: func(d *Disk) error {
				var err error
				if _, root, err = d.StatUser(""); err != nil {
					return err
				}
				if _, file, err = d.StatUser("Series/e01"); err != nil {
					return err
				}
				_, entry, err = d.StatTrash("2026-01-01/7/a")
				return err
			},
			done: func() bool { return root && file && entry },
		}
	}})
}
