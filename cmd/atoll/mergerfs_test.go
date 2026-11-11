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

// TestMergerfsConfigPrintsTheLine: without --json, the command prints the line of --json and a line feed, and nothing
// else, so that it can be appended to /etc/fstab; it reads no disk, and says which branch has no minfreespace of its
// own (specs.md §21.1, §25). The scenarios judge only --json.
func TestMergerfsConfigPrintsTheLine(t *testing.T) {
	for minFree, want := range map[string]int{`["1GiB"]`: exitOK, `["5%"]`: exitPartial} {
		dir := t.TempDir()
		config := filepath.Join(dir, "atoll.toml")
		file := filepath.Join(dir, "catalog.db")
		text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n" +
			"[placement]\nmin_free = " + minFree + "\n" +
			"[[disk]]\nid = \"t1\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b\"\nmount = " + strconv.Quote(filepath.Join(dir, "nowhere")) + "\nfs = \"ext4\"\n"
		if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
			t.Fatal(err)
		}
		var asJSON, plain, stderr bytes.Buffer
		if code := run([]string{"--config", config, "mergerfs-config", "--json"}, &asJSON, &stderr); code != want {
			t.Fatalf("%s: --json: exit %d: %s", minFree, code, stderr.String())
		}
		var out mergerfsOutput
		if err := json.Unmarshal(asJSON.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		stderr.Reset()
		if code := run([]string{"--config", config, "mergerfs-config"}, &plain, &stderr); code != want {
			t.Fatalf("%s: exit %d: %s", minFree, code, stderr.String())
		}
		if plain.String() != out.Fstab+"\n" || !strings.Contains(out.Fstab, "/nowhere/media=RW") {
			t.Errorf("%s: printed %q, the line of --json %q", minFree, plain.String(), out.Fstab)
		}
		if said := strings.Contains(stderr.String(), `disk "t1"`); said != (want == exitPartial) {
			t.Errorf("%s: standard error %q", minFree, stderr.String())
		}
		stderr.Reset()
		if code := run([]string{"--config", config, "mergerfs-config"}, brokenWriter{}, &stderr); code != exitError || !strings.Contains(stderr.String(), "no room") {
			t.Errorf("%s: on a broken output: exit %d, %q", minFree, code, stderr.String())
		}
	}
}
