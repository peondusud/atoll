package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/view"
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

// omvPool writes a configuration of view.mount and of disks t1, t2… at mounts, more following the line of view.mount,
// and its catalog, with the capacities given recorded, as a scan records them.
func omvPool(t *testing.T, view, more string, capacities map[string]int64, mounts ...string) string {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "atoll.toml")
	file := filepath.Join(dir, "catalog.db")
	text := "pool_id = \"home-media\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(view) + "\n" + more
	uuids := []string{"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b", "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d", "9b2e4d6f-1a3c-4e5f-8a7b-6c5d4e3f2a1b"}
	var list []catalog.Disk
	for i, mount := range mounts {
		id := "t" + strconv.Itoa(i+1)
		text += "[[disk]]\nid = \"" + id + "\"\nuuid = \"" + uuids[i] + "\"\nmount = " + strconv.Quote(mount) + "\nfs = \"ext4\"\n"
		capacity, seen := capacities[id]
		list = append(list, catalog.Disk{ID: id, UUID: uuids[i], FS: "ext4", Mount: mount, Root: "media", Workdir: "Atoll", Role: "data",
			Position: i, State: "online", Seen: seen, Capacity: capacity, Free: capacity, LastSeenNS: 1})
	}
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "home-media"); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "home-media")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return config
}

// TestMergerfsConfigOMV: with --omv, each field of the plugin's form a line, named as the form names them, the same
// values as the omv object of --json; what the form cannot hold as Atoll would want it is said on standard error
// (specs.md §21.1, §25). The scenarios judge only --json.
func TestMergerfsConfigOMV(t *testing.T) {
	config := omvPool(t, "/srv/pool", "[placement]\nmin_free = [\"5%\"]\n", map[string]int64{"t1": 1000000, "t2": 4 << 30}, "/mnt/t1", "/mnt/t2")
	var out, stderr bytes.Buffer
	if code := run([]string{"--config", config, "mergerfs-config", "--omv"}, &out, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := "Name: pool\nPaths:\n/mnt/t1/media=RW\n/mnt/t2/media=RW\nCreate policy: mfs\nMinimum free space: 205\nUnit: Megabytes\n" +
		"Options: category.search=ff,category.action=epall,moveonenospc=false,rename-exdev=passthrough,link-exdev=passthrough,cache.attr=1,cache.entry=1,cache.negative_entry=0\n" +
		"Mount point: /srv/mergerfs/pool\n"
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
	// 5% of 4 GiB, 214748364 bytes, is no whole count of G, M or K, and past 99999 in K: rounded up, 205M.
	for _, said := range []string{"214748364 bytes", "205M", "view.mount is /srv/pool", "/srv/mergerfs/pool"} {
		if !strings.Contains(stderr.String(), said) {
			t.Errorf("standard error %q, without %q", stderr.String(), said)
		}
	}
	var asJSON bytes.Buffer
	stderr.Reset()
	if code := run([]string{"--config", config, "mergerfs-config", "--omv", "--json"}, &asJSON, &stderr); code != exitOK {
		t.Fatalf("--json: exit %d: %s", code, stderr.String())
	}
	var o mergerfsOutput
	if err := json.Unmarshal(asJSON.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	if o.OMV == nil || o.OMV.RPC.Name != "pool" || o.OMV.RPC.Paths != "/mnt/t1/media=RW\n/mnt/t2/media=RW" || *o.OMV.RPC.MinFree != 205 || *o.OMV.RPC.MinFreeUnit != "M" ||
		!slices.Equal(o.OMV.Warnings, []string{"minfreespace-rounded", "mount-elsewhere"}) {
		t.Errorf("--json: %s", asJSON.String())
	}
	asJSON.Reset()
	if code := run([]string{"--config", config, "mergerfs-config", "--json"}, &asJSON, &stderr); code != exitOK || strings.Contains(asJSON.String(), "omv") {
		t.Errorf("--json alone: exit %d, %s", code, asJSON.String())
	}

	// In French, the labels the French form shows: openmediavault translates Name and Unit, not the plugin's own.
	french = true
	t.Cleanup(func() { french = false })
	out.Reset()
	stderr.Reset()
	if code := run([]string{"--config", config, "mergerfs-config", "--omv"}, &out, &stderr); code != exitOK ||
		!strings.HasPrefix(out.String(), "Nom : pool\nPaths :\n/mnt/t1/media=RW\n") || !strings.Contains(out.String(), "\nUnité : Megabytes\n") ||
		!strings.Contains(stderr.String(), "attention : view.mount est /srv/pool") {
		t.Errorf("French: exit %d\n%s%s", code, out.String(), stderr.String())
	}
}

// With --omv, a path the plugin refuses names its disk and exits with 1, with nothing on standard output; values that
// differ with view.omv_min_free = "same" print the form without its minimum free space, name the disks and exit with
// 4; a disk with no value of its own exits with 4, as without --omv.
func TestMergerfsConfigOMVRefuses(t *testing.T) {
	omv := func(config string) (int, string, string) {
		var out, stderr bytes.Buffer
		code := run([]string{"--config", config, "mergerfs-config", "--omv"}, &out, &stderr)
		return code, out.String(), stderr.String()
	}
	for mount, char := range map[string]string{"/mnt/it's": `"'"`, "/mnt/tab\there": `"\t"`} {
		code, out, stderr := omv(omvPool(t, "/srv/mergerfs/media", "", nil, "/mnt/t1", mount))
		if code != exitError || out != "" || !strings.Contains(stderr, `disk "t2"`) || !strings.Contains(stderr, char) {
			t.Errorf("%q: exit %d, %q, %q", mount, code, out, stderr)
		}
	}
	code, out, stderr := omv(omvPool(t, "/srv/mergerfs/media", "[placement]\nmin_free = [\"100TiB\"]\n", nil, "/mnt/t1"))
	if code != exitError || out != "" || !strings.Contains(stderr, "109951162777600 bytes") {
		t.Errorf("too large: exit %d, %q, %q", code, out, stderr)
	}

	capacities := map[string]int64{"t1": 10 << 30, "t2": 20 << 30}
	code, out, stderr = omv(omvPool(t, "/srv/mergerfs/media", "omv_min_free = \"same\"\n[placement]\nmin_free = [\"10%\"]\n", capacities, "/mnt/t1", "/mnt/t2", "/mnt/t3"))
	if code != exitPartial || strings.Contains(out, "Minimum free space") || strings.Contains(out, "Unit") || !strings.Contains(out, "Name: media\n") ||
		!strings.Contains(stderr, `"t1" (1073741824 bytes), "t2" (2147483648 bytes)`) || !strings.Contains(stderr, `disk "t3"`) || strings.Contains(stderr, "view.mount") {
		t.Errorf("same: exit %d\n%s%s", code, out, stderr)
	}
	code, out, stderr = omv(omvPool(t, "/srv/mergerfs/media", "[placement]\nmin_free = [\"10%\"]\n", capacities, "/mnt/t1", "/mnt/t2", "/mnt/t3"))
	if code != exitPartial || !strings.Contains(out, "Minimum free space: 2\nUnit: Gigabytes\n") || !strings.Contains(stderr, `disk "t3"`) || strings.Contains(stderr, `"t1"`) {
		t.Errorf("largest: exit %d\n%s%s", code, out, stderr)
	}
	code, out, stderr = omv(omvPool(t, "/srv/mergerfs/media", "[placement]\nmin_free = [\"10%\"]\n", nil, "/mnt/t1"))
	if code != exitPartial || strings.Contains(out, "Minimum free space") || !strings.Contains(stderr, "no disk has a minfreespace of its own") {
		t.Errorf("none: exit %d\n%s%s", code, out, stderr)
	}
}

// The pool's one minimum free space, as the form holds it, rounded up or not, is compared with the largest file, as each
// branch's is without --omv: not larger only warns. 50,000 bytes are held as 49K, 50,176.
func TestMergerfsConfigOMVWarnsOfTheLargestFile(t *testing.T) {
	c, err := config.Parse([]byte("pool_id = \"p\"\n[view]\nmount = \"/srv/mergerfs/p\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	for largest, warns := range map[int64]bool{50000: false, 50175: false, 50176: true, 60000: true} {
		reserve := int64(50000)
		m := &view.Mergerfs{Mount: "/srv/mergerfs/p", Options: []string{"category.create=mfs"},
			Branches: []view.Branch{{Disk: "t1", Path: "/mnt/t1/media", Mode: view.ModeRW, MinFree: &reserve}}}
		out := &mergerfsOutput{Branches: []mergerfsBranch{{Disk: "t1", MinFree: &reserve}}, Largest: &largest}
		var stdout, stderr bytes.Buffer
		if code := runOMV(c, m, out, false, &stdout, &stderr); code != exitOK || !strings.Contains(stdout.String(), "Minimum free space: 49\nUnit: Kilobytes\n") {
			t.Errorf("%d: exit %d, %s", largest, code, stdout.String())
		}
		if said := strings.Contains(stderr.String(), "not larger than the largest file"); said != warns {
			t.Errorf("%d: standard error %q", largest, stderr.String())
		}
	}
}
