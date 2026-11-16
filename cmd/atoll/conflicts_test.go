package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// A file corrupt that holds a conflict shows corrupt, and atoll conflicts list gives it, marked so, in text and in
// JSON; atoll conflicts resolve refuses it, as a repair comes first (specs.md §4, §15.7, §16). The scenarios cannot
// make a copy rot, which a write does not: the rows are written here.
func TestConflictsOfACorruptFile(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "atoll.toml")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(filepath.Join(dir, "catalog.db")) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n"
	for i, id := range []string{"t1", "t2"} {
		mount := filepath.Join(dir, id)
		if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		text += "[[disk]]\nid = \"" + id + "\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3" + strconv.Itoa(i) + "\"\nmount = " + strconv.Quote(mount) + "\nfs = \"ext4\"\n"
	}
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", config, "init"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("init: exit %d, %q", code, stderr.String())
	}
	db, err := catalog.OpenPool(t.Context(), filepath.Join(dir, "catalog.db"), "p")
	if err != nil {
		t.Fatal(err)
	}
	var list []catalog.Disk
	for i, id := range []string{"t1", "t2"} {
		list = append(list, catalog.Disk{ID: id, UUID: "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3" + strconv.Itoa(i), FS: "ext4", Mount: filepath.Join(dir, id), Root: "media", Workdir: "Atoll", Role: "data", Position: i, State: "online"})
	}
	if err := catalog.SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO files(id, lpath, size, mtime_ns, blake3, replicas_target, mode, state, conflict_held, first_seen_ns, updated_ns)
			VALUES (1, x'61', 2, 2, zeroblob(32), 1, 'deferred', 'corrupt', 1, 0, 0), (2, x'62', 2, 2, zeroblob(32), 1, 'deferred', 'conflict', 0, 0, 0)`,
		`INSERT INTO copies(file_id, disk_id, size, mtime_ns, created_by, first_seen_ns, verified_ns, state)
			VALUES (1, 't1', 2, 2, 'user', 0, NULL, 'ok'), (1, 't2', 2, 2, 'user', 0, NULL, 'corrupt'), (2, 't1', 2, 2, 'user', 0, NULL, 'ok'),
				(2, 't2', 2, 3, 'user', 0, NULL, 'ok')`,
	} {
		if _, err := db.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	if code := run([]string{"--config", config, "conflicts", "list", "--json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("list: exit %d, %q", code, stderr.String())
	}
	var out struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || len(out.Files) != 2 {
		t.Fatalf("%q, %v", stdout.String(), err)
	}
	if out.Files[0]["path"] != "a" || out.Files[0]["corrupt"] != true || out.Files[1]["path"] != "b" {
		t.Errorf("listed: %q", stdout.String())
	}
	if _, has := out.Files[1]["corrupt"]; has {
		t.Errorf("a conflict alone is marked corrupt: %q", stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"--config", config, "conflicts", "list"}, &stdout, &stderr); code != exitOK ||
		!strings.HasPrefix(stdout.String(), "a (corrupt too: atoll repair first)\n") || !strings.Contains(stdout.String(), "\nb\n") {
		t.Errorf("list: exit %d, %q", code, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--config", config, "conflicts", "resolve", "a", "--keep", "t1", "--yes"}, &stdout, &stderr); code != exitError ||
		stdout.Len() != 0 || !strings.Contains(stderr.String(), "the file is corrupt too: repair it first") {
		t.Errorf("resolve: exit %d, %q, %q", code, stdout.String(), stderr.String())
	}
}
