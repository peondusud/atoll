package view

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// scaleCatalog makes a catalog of 200,000 files on two disks, each with two copies, as a library holds them: 2,000
// shows of 10 seasons of 10 episodes, each folder an entry on both disks (specs.md §26).
func scaleCatalog(tb testing.TB) *sql.DB {
	tb.Helper()
	file := filepath.Join(tb.TempDir(), "catalog.db")
	if _, err := catalog.Create(context.Background(), file, "p"); err != nil {
		tb.Fatal(err)
	}
	db, err := catalog.OpenPool(context.Background(), file, "p")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	var list []catalog.Disk
	for i, id := range []string{"t1", "t2"} {
		list = append(list, catalog.Disk{ID: id, UUID: "u-" + id, FS: "ext4", Mount: "/mnt/" + id, Root: "media", Workdir: "Atoll", Role: "data", Position: i, State: "online"})
	}
	if err := catalog.SyncDisks(context.Background(), db, list); err != nil {
		tb.Fatal(err)
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	file1, err := tx.PrepareContext(ctx, `INSERT INTO files(id, lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
		VALUES (?, ?, 1000, 5, 1, 'deferred', 'ok', 0, 0)`)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = file1.Close() }()
	copy1, err := tx.PrepareContext(ctx, `INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, btime_ns, state, mode, uid, gid, nlink)
		VALUES (?, ?, 1000, 5, 'user', 0, ?, 'ok', 420, 1000, 100, 1)`)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = copy1.Close() }()
	entry1, err := tx.PrepareContext(ctx, `INSERT INTO entries(lpath, disk_id, kind, mode, mtime_ns, created_by, size, uid, gid)
		VALUES (?, ?, 'dir', 493, 5, 'user', 4096, 1000, 100)`)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = entry1.Close() }()
	id := 0
	for show := range 2000 {
		showPath := fmt.Sprintf("Series/Show %04d", show)
		for _, d := range []string{"t1", "t2"} {
			if _, err := entry1.ExecContext(ctx, []byte(showPath), d); err != nil {
				tb.Fatal(err)
			}
		}
		for season := range 10 {
			seasonPath := fmt.Sprintf("%s/S%02d", showPath, season)
			for _, d := range []string{"t1", "t2"} {
				if _, err := entry1.ExecContext(ctx, []byte(seasonPath), d); err != nil {
					tb.Fatal(err)
				}
			}
			for episode := range 10 {
				id++
				if _, err := file1.ExecContext(ctx, id, []byte(fmt.Sprintf("%s/e%02d.mkv", seasonPath, episode))); err != nil {
					tb.Fatal(err)
				}
				for i, d := range []string{"t1", "t2"} {
					if _, err := copy1.ExecContext(ctx, id, d, 100+i); err != nil {
						tb.Fatal(err)
					}
				}
			}
		}
	}
	for _, d := range []string{"t1", "t2"} {
		if _, err := entry1.ExecContext(ctx, []byte("Series"), d); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return db
}

// BenchmarkTreeAtScale: what browsing costs at 200,000 files (specs.md §21.2, §26): what one path presents, the
// listing of the folder of the shows, 2,000 names over 200,000 paths below them, and the listing of one season.
func BenchmarkTreeAtScale(b *testing.B) {
	db := scaleCatalog(b)
	tree := NewTree(db, rootsOf{}, Owner{}, 0)
	ctx := context.Background()
	b.Run("lookup", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			if n, err := tree.Lookup(ctx, fmt.Sprintf("Series/Show %04d/S03/e07.mkv", i%2000)); err != nil || n == nil || n.Disk != "t1" {
				b.Fatalf("%+v, %v", n, err)
			}
		}
	})
	b.Run("list-shows", func(b *testing.B) {
		for b.Loop() {
			if list, err := tree.List(ctx, "Series"); err != nil || len(list) != 2000 {
				b.Fatalf("%d, %v", len(list), err)
			}
		}
	})
	b.Run("list-season", func(b *testing.B) {
		for b.Loop() {
			if list, err := tree.List(ctx, "Series/Show 1234/S05"); err != nil || len(list) != 10 {
				b.Fatalf("%d, %v", len(list), err)
			}
		}
	})
	b.Run("list-root", func(b *testing.B) {
		for b.Loop() {
			if list, err := tree.List(ctx, ""); err != nil || len(list) != 1 {
				b.Fatalf("%d, %v", len(list), err)
			}
		}
	})
}
