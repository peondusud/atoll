package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// Without --json, names check writes a finding a line, as --json says it, then how many names it checked; a name
// that is not UTF-8 is in base64 (specs.md §15.9). The scenarios judge only --json.
func TestNamesCheckText(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "atoll.toml")
	file := filepath.Join(dir, "catalog.db")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n" +
		"[[disk]]\nid = \"t1\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b\"\nmount = " + strconv.Quote(filepath.Join(dir, "t1")) + "\nfs = \"ext4\"\n"
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a:b/x.mkv", "Film.mkv", "film.mkv", "\xff.mkv", "ok.mkv"} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO files(lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (?, 0, 0, 1, 'deferred', 'ok', 0, 0)`, []byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", config, "names", "check"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := "Film.mkv: collision with film.mkv\n" +
		"a:b: incompatible on ntfs (forbidden-character)\n" +
		"film.mkv: collision with Film.mkv\n" +
		"(base64) /y5ta3Y=: invalid-utf8\n" +
		"(base64) /y5ta3Y=: incompatible on ntfs (invalid-utf8)\n" +
		"6 names checked, 5 findings\n"
	if stdout.String() != want {
		t.Errorf("%q", stdout.String())
	}
	if code := run([]string{"--config", config, "names", "check"}, brokenWriter{}, &stderr); code != exitError {
		t.Errorf("on a broken output: exit %d", code)
	}
}
