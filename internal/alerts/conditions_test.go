package alerts

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
)

// newCatalog makes a catalog, with files in a state for each path given.
func newCatalog(t *testing.T, files map[string]string) *sql.DB {
	t.Helper()
	file := filepath.Join(t.TempDir(), "catalog.db")
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for path, state := range files {
		mode := "deferred"
		if state == "unprotected" {
			mode = "manual"
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO files(lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (?, 0, 0, 1, ?, ?, 0, 0)`, []byte(path), mode, state); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// Each condition holds once, with its count, in key order; what is fine holds nothing (specs.md §23).
func TestHoldings(t *testing.T) {
	f := &Facts{
		Disks: []jobs.DiskLine{
			{ID: "t1", Verdict: disks.Verdict{State: disks.Missing}}, {ID: "t2", Verdict: disks.Verdict{State: disks.Readonly}},
			{ID: "t3", Verdict: disks.Verdict{State: disks.Online}},
		},
		Guards: []jobs.GuardLine{
			{Disk: "t3", Kind: jobs.GuardDeletions, Count: 40}, {Disk: "t3", Kind: jobs.GuardModifications, Count: 7},
			{Disk: "t3", Kind: jobs.GuardDeletions, Count: 41},
		},
		Refused:  Refused{Count: 2, First: []string{"a", "b"}},
		Files:    &catalog.FileAlerts{ByState: map[string]int64{"corrupt": 3, "conflict": 0, "name-collision": 1}, Manual: 1},
		Used:     91,
		Capacity: 100,
	}
	var got []string
	for _, h := range Holdings(f) {
		got = append(got, fmt.Sprintf("%s=%d", h.Key, h.Count))
	}
	want := []string{"disk-missing:t1=1", "disk-readonly:t2=1", "files-corrupt=3", "files-name-collision=1", "guard-deletions:t3=1",
		"guard-modifications:t3=1", "manual-waiting=1", "name-refused=2", "pool-full=1"}
	if !slices.Equal(got, want) {
		t.Errorf("%q", got)
	}
	if h := Holdings(&Facts{Files: &catalog.FileAlerts{ByState: map[string]int64{}}}); len(h) != 0 {
		t.Errorf("all fine: %+v", h)
	}
}

// Gather counts the files behind the alerts, and the space of the data disks that hold their files now: not a
// missing, failed or cache disk, nor one whose capacity no check read (specs.md §23).
func TestGather(t *testing.T) {
	db := newCatalog(t, map[string]string{"c/1": "corrupt", "c/2": "corrupt", "m/1": "unprotected", "ok": "ok"})
	disk := func(id string, position int, role, state string, capacity, free int64) catalog.Disk {
		return catalog.Disk{ID: id, UUID: "u-" + id, FS: "ext4", Mount: "/mnt/" + id, Root: "media", Workdir: "Atoll", Role: role,
			Position: position, State: state, Seen: capacity > 0, Capacity: capacity, Free: free}
	}
	list := []catalog.Disk{
		disk("a", 0, "data", "online", 100, 10), disk("b", 1, "data", "readonly", 100, 0), disk("c", 2, "data", "standby", 100, 50),
		disk("d", 3, "data", "draining", 100, 50), disk("e", 4, "data", "missing", 1000, 0), disk("f", 5, "data", "failed", 1000, 0),
		disk("g", 6, "cache", "online", 1000, 0), disk("h", 7, "data", "online", 0, 0),
	}
	if err := catalog.SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse([]byte("pool_id = \"p\"\ncatalog = \"/srv/catalog.db\"\n[view]\nmount = \"/srv/pool\"\n[defaults]\nmanual_alert_after = \"1h\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	const hour = int64(3600e9)
	outcome := &jobs.Outcome{Disks: []jobs.DiskLine{{ID: "e"}}, Guards: []jobs.GuardLine{{Disk: "a", Kind: jobs.GuardDeletions}}}
	f, err := Gather(t.Context(), db, c, 2*hour, outcome, &Refused{Count: 3, First: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if f.Used != 290 || f.Capacity != 400 || f.SeenByNS != hour || f.ManualAfter != "1h" || f.Files.ByState["corrupt"] != 2 || f.Files.Manual != 1 ||
		f.Refused.Count != 3 || len(f.Disks) != 1 || len(f.Guards) != 1 {
		t.Errorf("%+v, files %+v", f, f.Files)
	}
	// Seen an hour ago at most: not waiting yet.
	if f, _ := Gather(t.Context(), db, c, hour-1, outcome, &Refused{}); f.Files.Manual != 0 {
		t.Errorf("waiting too soon: %+v", f.Files)
	}
}

// The pool is full past 90 % of its capacity, never at it, and with no capacity known it is not.
func TestPoolFull(t *testing.T) {
	for _, tc := range []struct {
		used, capacity int64
		want           bool
	}{
		{0, 0, false}, {5, 0, false}, {90, 100, false}, {91, 100, true}, {9, 10, false}, {10, 10, true},
		{6, 7, false}, {7, 7, true}, {899, 999, false}, {900, 999, true},
		{math.MaxInt64 / 10 * 9, math.MaxInt64, false}, {math.MaxInt64, math.MaxInt64, true},
	} {
		if got := poolFull(tc.used, tc.capacity); got != tc.want {
			t.Errorf("%d of %d: %v", tc.used, tc.capacity, got)
		}
	}
}

// Refused keeps the first ten names in byte order, each once, and counts every refusal.
func TestRefusedAdd(t *testing.T) {
	var r Refused
	for _, p := range []string{"m", "c", "x", "a", "c", "b", "z", "e", "d", "f", "g", "h", "i", "j", "0"} {
		r.Add(p)
	}
	if r.Count != 15 || !slices.Equal(r.First, []string{"0", "a", "b", "c", "d", "e", "f", "g", "h", "i"}) {
		t.Errorf("%+v", r)
	}
}

// An alert stops naming paths at the first that would take its text past maxBody, and counts it among the rest.
func TestListFits(t *testing.T) {
	long, longer := strings.Repeat("a", 3000), strings.Repeat("b", 1000)
	got := list("first line", []string{long, longer, "c"}, 5)
	if got != "first line\n"+long+"\nand 4 more" {
		t.Errorf("%d bytes: %.40q…", len(got), got)
	}
	if got := list("first line", []string{strings.Repeat("z", maxBody)}, 1); got != "first line\nand 1 more" {
		t.Errorf("%.40q", got)
	}
	// Room is kept for the last lines: a path that would leave less than 100 bytes is not named.
	if got := list("first line", []string{strings.Repeat("y", 3950)}, 1); got != "first line\nand 1 more" {
		t.Errorf("%.40q", got)
	}
	if got := list("first line", []string{"a", "b"}, 2); got != "first line\na\nb" {
		t.Errorf("%q", got)
	}
}

func TestShown(t *testing.T) {
	for p, want := range map[string]string{
		"films/été.mkv": "films/été.mkv",
		"a\nb":          "(base64) YQpi",
		"\xff":          "(base64) /w==",
		"a\x7fb":        "(base64) YX9i",
	} {
		if got := Shown(p); got != want {
			t.Errorf("%q: %q", p, got)
		}
	}
}

// What each alert says: its title, its priority, and the files it names, ten at most, in byte order.
func TestDescribe(t *testing.T) {
	files := map[string]string{"a/conflict": "conflict", "m/x": "unprotected"}
	for i := range 12 {
		files[fmt.Sprintf("c/%02d", 11-i)] = "corrupt"
	}
	db := newCatalog(t, files)
	f := &Facts{
		Disks:    []jobs.DiskLine{{ID: "t1", Verdict: disks.Verdict{State: disks.Missing, Message: "nothing is mounted there"}}, {ID: "t2", Verdict: disks.Verdict{State: disks.Readonly}}},
		Guards:   []jobs.GuardLine{{Disk: "t1", Kind: jobs.GuardDeletions, Count: 40}, {Disk: "t2", Kind: jobs.GuardModifications, Count: 7}},
		Refused:  Refused{Count: 1, First: []string{"bad\xff"}},
		Used:     95,
		Capacity: 100,
		SeenByNS: 10, ManualAfter: "7d",
	}
	for _, tc := range []struct {
		key      string
		count    int64
		title    string
		priority int
		body     []string
	}{
		{"disk-missing:t1", 1, "Atoll: disk t1 is missing", 4, []string{"missing: nothing is mounted there."}},
		{"disk-readonly:t2", 1, "Atoll: disk t2 is read-only", 3, []string{"read-only."}},
		{"guard-deletions:t1", 1, "Atoll: the mass-deletion guard of disk t1 tripped", 4, []string{"deletions it found on disk t1 (40)", "atoll guard list"}},
		{"guard-modifications:t2", 1, "Atoll: the mass-modification guard of disk t2 tripped", 4, []string{"modifications it found on disk t2 (7)"}},
		{"files-corrupt", 12, "Atoll: 12 corrupt files", 4, []string{"\nc/00\nc/01\n", "\nc/09\nand 2 more"}},
		{"files-conflict", 1, "Atoll: 1 conflict file", 3, []string{"\na/conflict"}},
		{"manual-waiting", 1, "Atoll: 1 file in manual mode left unprotected", 3, []string{"more than 7d", "\nm/x"}},
		{"files-unavailable", 2, "Atoll: 2 unavailable files", 4, []string{"\nand 2 more"}},
		{"files-name-incompatible", 2, "Atoll: 2 name-incompatible files", 3, []string{"\nand 2 more"}},
		{"pool-full", 1, "Atoll: the pool is 95 % full", 3, []string{"95 of their 100 bytes"}},
		{"name-refused", 1, "Atoll: a driver refused 1 name", 3, []string{"\n(base64) YmFk/w=="}},
	} {
		m, err := Describe(t.Context(), db, f, Holding{Key: tc.key, Count: tc.count})
		if err != nil {
			t.Errorf("%s: %v", tc.key, err)
			continue
		}
		if m.Key != tc.key || m.Title != tc.title || m.Priority != tc.priority {
			t.Errorf("%s: %+v", tc.key, m)
		}
		for _, part := range tc.body {
			if !strings.Contains(m.Body, part) {
				t.Errorf("%s: %q lacks %q", tc.key, m.Body, part)
			}
		}
	}
	if strings.Count(mustDescribe(t, db, f, "files-corrupt", 12).Body, "\nc/") != FirstN {
		t.Error("not ten paths")
	}
	if _, err := Describe(t.Context(), db, f, Holding{Key: "nothing", Count: 1}); err == nil {
		t.Error("an unknown key")
	}
}

func mustDescribe(t *testing.T, db *sql.DB, f *Facts, key string, count int64) *Message {
	t.Helper()
	m, err := Describe(t.Context(), db, f, Holding{Key: key, Count: count})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
