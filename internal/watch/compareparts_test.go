package watch

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/fsx"
)

// A file's row is written again when any one of what a walk decides of it changed: its size, its time, its replicas,
// its rule, its mode or its original; with none of them, it is left as it is (specs.md §15.10).
func TestAFileRowChangesWithAnyOfItsFields(t *testing.T) {
	row := catalog.FileRow{Size: 5, MtimeNS: 7, ReplicasTarget: 1, Rule: sql.NullString{String: "r", Valid: true}, Mode: "immediate",
		Original: sql.NullString{String: "t1", Valid: true}}
	same := func() *catalog.FileUpdate {
		return &catalog.FileUpdate{Size: row.Size, MtimeNS: row.MtimeNS, ReplicasTarget: row.ReplicasTarget, Rule: row.Rule, Mode: row.Mode,
			Original: row.Original, State: "ok"}
	}
	if fileChanged(&row, same()) {
		t.Error("nothing changed, and the row is written again")
	}
	for _, tc := range []struct {
		name   string
		change func(u *catalog.FileUpdate)
	}{
		{"size", func(u *catalog.FileUpdate) { u.Size++ }},
		{"time", func(u *catalog.FileUpdate) { u.MtimeNS++ }},
		{"replicas", func(u *catalog.FileUpdate) { u.ReplicasTarget++ }},
		{"rule", func(u *catalog.FileUpdate) { u.Rule.String = "other" }},
		{"mode", func(u *catalog.FileUpdate) { u.Mode = "manual" }},
		{"original", func(u *catalog.FileUpdate) { u.Original.String = "t2" }},
	} {
		u := same()
		tc.change(u)
		if !fileChanged(&row, u) {
			t.Errorf("its %s changed alone, and the row is not written", tc.name)
		}
	}
}

// Two looks at one open file are of one version only when its size, its modification time and its change time are
// each the same (specs.md §15.10); a copy is what the walk recorded only when each of them is what its row says.
func TestALookKnowsAVersionByEachOfItsTimes(t *testing.T) {
	look := fsx.Stat{Size: 5, Mtime: fsx.Time{Sec: 1, Nsec: 2}, Ctime: fsx.Time{Sec: 3, Nsec: 4}}
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	row := catalog.CopyRow{Size: valid(5), MtimeNS: valid(1_000_000_002), CtimeNS: valid(3_000_000_004)}
	if !sameVersion(&look, &look) || !matchesRow(&look, &row) {
		t.Fatal("one look, or the row it gave")
	}
	for _, tc := range []struct {
		name   string
		change func(st *fsx.Stat)
	}{
		{"size", func(st *fsx.Stat) { st.Size++ }},
		{"modification time", func(st *fsx.Stat) { st.Mtime.Nsec++ }},
		{"change time", func(st *fsx.Stat) { st.Ctime.Nsec++ }},
	} {
		other := look
		tc.change(&other)
		if sameVersion(&look, &other) || matchesRow(&other, &row) {
			t.Errorf("its %s changed alone, and it is the same", tc.name)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(r *catalog.CopyRow)
	}{
		{"size", func(r *catalog.CopyRow) { r.Size.Valid = false }},
		{"modification time", func(r *catalog.CopyRow) { r.MtimeNS.Valid = false }},
		{"change time", func(r *catalog.CopyRow) { r.CtimeNS.Valid = false }},
	} {
		other := row
		tc.change(&other)
		if matchesRow(&look, &other) {
			t.Errorf("a row with no %s matches", tc.name)
		}
	}
}

// What a walk counts of a disk: each file found, with its bytes, and each file a rule ignores apart.
func TestAWalkCountsWhatItFound(t *testing.T) {
	b := newBenchWith(t, "[[rule]]\nlabel = \"skip\"\next = [\"nfo\"]\naction = \"ignore\"\n", "t1")
	b.write("t1", "a", "x")
	b.write("t1", "b", "yyy")
	b.write("t1", "x.nfo", "n")
	b.write("t1", "y.nfo", "n")
	out := b.scan(Options{})
	r := out.Disks[0]
	if r.Files != 2 || r.Bytes != 4 || r.Ignored != 2 {
		t.Errorf("%d files of %d bytes, %d ignored", r.Files, r.Bytes, r.Ignored)
	}
}

// A folder's row names another folder when its name holds none, or one whose inode or creation time is not the row's,
// each alone; what a look could not read of the folder found says nothing against it (specs.md §15.12).
func TestAFolderRowKnowsAnotherFolder(t *testing.T) {
	key := func(s string) *string { return &s }
	birth := func(n int64) *int64 { return &n }
	row := &catalog.EntryIdentity{FileKey: sql.NullString{String: "7", Valid: true}, BtimeNS: sql.NullInt64{Int64: 9, Valid: true}}
	for _, tc := range []struct {
		name  string
		found *FoundEntry
		other bool
	}{
		{"nothing", nil, true},
		{"the folder", &FoundEntry{FileKey: key("7"), BtimeNS: birth(9)}, false},
		{"another inode", &FoundEntry{FileKey: key("8"), BtimeNS: birth(9)}, true},
		{"another creation time", &FoundEntry{FileKey: key("7"), BtimeNS: birth(10)}, true},
		{"no inode read", &FoundEntry{BtimeNS: birth(9)}, false},
		{"no creation time read", &FoundEntry{FileKey: key("7")}, false},
	} {
		if got := isAnotherFolder(tc.found, row); got != tc.other {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

// A folder holds its file when any file found below it is at home, as its row says: the first or a later one, and
// the folder's files the last of the walk's or not.
func TestAFolderHoldsItsFileWhereverItIs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []FoundFile
		holds bool
	}{
		{"a later file at home", []FoundFile{{Path: "a"}, {Path: "d/x"}, {Path: "d/y", home: true}, {Path: "e"}}, true},
		{"none at home, last of the walk", []FoundFile{{Path: "a"}, {Path: "d/x"}, {Path: "d/y"}}, false},
		{"none below it", []FoundFile{{Path: "a", home: true}, {Path: "e", home: true}}, false},
	} {
		if got := holdsItsFile(&Walked{Files: tc.files}, "d"); got != tc.holds {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

// A list of names, as the placement of a policy holds it, is one string and back again: none is no name at all.
func TestNamesJoinedAndSplit(t *testing.T) {
	if got := splitNames(joinNames(nil)); got != nil {
		t.Errorf("none: %q", got)
	}
	if got := splitNames(joinNames([]string{"ssd", "usb"})); !slices.Equal(got, []string{"ssd", "usb"}) {
		t.Errorf("two: %q", got)
	}
}
