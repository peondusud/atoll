package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
)

// atoll mount takes one point at most, or view.readonly_mount, which follows the relations of view.readonly_mount
// (exit 2); a point that is not a folder is one it cannot start on (exit 1). The mount itself is test/loopdev's
// (specs.md §21.2).
func TestMountRefuses(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	pool := filepath.Join(dir, "pool")
	config := func(readonly string) string {
		path := filepath.Join(dir, "atoll"+strconv.Itoa(len(readonly))+".toml")
		text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(pool) + "\n"
		if readonly != "" {
			text += "readonly_mount = " + strconv.Quote(readonly) + "\n"
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	plain := config("")
	afile := filepath.Join(dir, "a-file")
	if err := os.WriteFile(afile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{"--config", plain, "mount"}, exitUsage, "view.readonly_mount"},
		{[]string{"--config", plain, "mount", "/a", "/b"}, exitUsage, "one point"},
		{[]string{"--config", plain, "mount", "--json"}, exitUsage, "--json"},
		{[]string{"--config", plain, "mount", pool}, exitUsage, "view.mount"},
		{[]string{"--config", plain, "mount", filepath.Join(pool, "ro")}, exitUsage, "view.mount"},
		{[]string{"--config", plain, "mount", "relative"}, exitUsage, "absolute"},
		{[]string{"--config", plain, "mount", dir}, exitUsage, "catalog"},
		{[]string{"--config", plain, "mount", filepath.Join(dir, "nowhere")}, exitError, "does not exist"},
		{[]string{"--config", plain, "mount", afile}, exitError, "not a folder"},
		{[]string{"--config", config(filepath.Join(dir, "gone")), "mount"}, exitError, "does not exist"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != tc.code || !strings.Contains(stderr.String(), tc.says) || stdout.Len() != 0 {
			t.Errorf("%v: exit %d, %q, %q", tc.args[2:], code, stdout.String(), stderr.String())
		}
	}
}
