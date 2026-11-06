package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
)

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("no room") }

// TestStatusLooksAtNoDisk: it runs with no control file and no disk, in the delivered build too, and
// it says so when it cannot write what it read.
func TestStatusLooksAtNoDisk(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "atoll.toml")
	file := filepath.Join(dir, "catalog.db")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n" +
		"[[disk]]\nid = \"t1\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b\"\nmount = " + strconv.Quote(filepath.Join(dir, "nowhere")) + "\nfs = \"ext4\"\n"
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"status"}, {"status", "--json"}} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"--config", config}, args...), &stdout, &stderr); code != exitOK {
			t.Errorf("%v: exit %d: %s", args, code, stderr.String())
		}
		if stdout.Len() == 0 {
			t.Errorf("%v: nothing printed", args)
		}
		stderr.Reset()
		if code := run(append([]string{"--config", config}, args...), brokenWriter{}, &stderr); code != exitError {
			t.Errorf("%v on a broken output: exit %d", args, code)
		}
		if !strings.Contains(stderr.String(), "no room") {
			t.Errorf("%v on a broken output: %q", args, stderr.String())
		}
	}
}

// One command that writes at a time: what only reads goes on (specs.md §25).
func TestTheCatalogLock(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "atoll.toml")
	file := filepath.Join(dir, "catalog.db")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n"
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	release, err := fsx.LockFile(file)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, args := range [][]string{{"scan"}, {"prune"}, {"prune", "--yes"}, {"prune", "--dry-run"}, {"trash", "restore", "a"}, {"trash", "purge"}} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"--config", config}, args...), &stdout, &stderr); code != exitError {
			t.Errorf("%v: exit %d", args, code)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), fsx.ErrBusy.Error()) {
			t.Errorf("%v: %q, %q", args, stdout.String(), stderr.String())
		}
	}
	for _, args := range [][]string{{"status"}, {"trash", "list"}, {"disk", "list"}} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"--config", config}, args...), &stdout, &stderr); code != exitOK {
			t.Errorf("%v: exit %d, %s", args, code, stderr.String())
		}
	}
}

// TestUsageInProcess runs in this process, as a panic would also exit with 2.
func TestUsageInProcess(t *testing.T) {
	for _, args := range [][]string{
		{"--config"}, {"trash", "purge", "--older-than"}, {"status", "x"}, {"status", "--path"}, {"status", "--path="}, {"status", "--path", "a", "--path", "b"},
		{"status", "--json", "--json"}, {"status", "--path=/a"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitUsage || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Errorf("%v: exit %d, %q, %q", args, code, stdout.String(), stderr.String())
		}
	}
}

// TestStatusFile holds what no scenario can reach until jobs run: a copy Atoll created, and a copy
// with no size or time yet. Times are in UTC whatever the zone: scenarios run with TZ=UTC.
func TestStatusFile(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("elsewhere", 3600)
	t.Cleanup(func() { time.Local = local })
	file := statusFileFrom(&catalog.FileRow{
		Path: "a", State: "pending", Size: 1, MtimeNS: -1, ReplicasTarget: 2, Mode: "immediate", FirstSeenNS: 1,
		Hash: []byte{0xab, 0x01},
		Copies: []catalog.CopyRow{{
			Disk: "t2", CreatedBy: "atoll", State: "pending", FirstSeenNS: 2,
			BtimeNS: sql.NullInt64{Int64: 3, Valid: true}, VerifiedNS: sql.NullInt64{Int64: 4, Valid: true},
		}},
	})
	if file.Mtime != "1969-12-31T23:59:59.999999999Z" || file.FirstSeen != "1970-01-01T00:00:00.000000001Z" {
		t.Errorf("times: %q, %q", file.Mtime, file.FirstSeen)
	}
	if file.Blake3 == nil || *file.Blake3 != "ab01" {
		t.Errorf("hash: %v", file.Blake3)
	}
	if file.Replicas != 2 || file.Mode != "immediate" || file.Rule != nil || file.Original != nil {
		t.Errorf("policy: %+v", file)
	}
	c := file.Copies[0]
	if c.CreatedBy != "atoll" || c.Size != nil || c.Mtime != nil {
		t.Errorf("copy: %+v", c)
	}
	if c.Created == nil || *c.Created != "1970-01-01T00:00:00.000000003Z" || c.Verified == nil || *c.Verified != "1970-01-01T00:00:00.000000004Z" ||
		c.FirstSeen != "1970-01-01T00:00:00.000000002Z" {
		t.Errorf("copy times: %+v", c)
	}
}

// TestStatusFilesFirst: however many files there are, the output is one JSON document.
func TestStatusFilesFirst(t *testing.T) {
	summary := &statusSummary{Disks: []statusDisk{}, ByState: map[string]int{}, FilesTotal: 3}
	for files := range 3 {
		var out bytes.Buffer
		j := newJSONStatus(&out)
		for i := range files {
			if err := j.file(&catalog.FileRow{Path: string(rune('a' + i)), State: "ok", Mode: "deferred"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := j.end(summary); err != nil {
			t.Fatal(err)
		}
		var read struct {
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
			Disks []any `json:"disks"`
			Total int   `json:"files_total"`
		}
		if err := json.Unmarshal(out.Bytes(), &read); err != nil {
			t.Fatalf("%d files: %v\n%s", files, err, out.String())
		}
		if len(read.Files) != files || read.Files == nil || read.Disks == nil || read.Total != 3 {
			t.Errorf("%d files: %+v", files, read)
		}
		for i := range read.Files {
			if read.Files[i].Path != string(rune('a'+i)) {
				t.Errorf("%d files: %+v", files, read)
			}
		}
		if !bytes.HasSuffix(out.Bytes(), []byte("}\n")) {
			t.Errorf("%d files: %q", files, out.String())
		}
	}
}
