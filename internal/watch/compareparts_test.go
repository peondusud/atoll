package watch

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/solver"
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

// A file takes the size and date of its original's copy, and of no other, whichever comes after it: the original is the
// in-sync copy with the oldest creation time (specs.md §7).
func TestAFileTakesItsOriginalsSizeAndDate(t *testing.T) {
	b := newBench(t, "t1", "t2")
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	f := &catalog.FileRow{Path: "a", Size: 1, MtimeNS: 1, Mode: "immediate", State: "ok", Copies: []catalog.CopyRow{
		{Disk: "t1", State: "ok", Size: valid(5), MtimeNS: valid(10), BtimeNS: valid(100), FirstSeenNS: 100},
		{Disk: "t2", State: "ok", Size: valid(7), MtimeNS: valid(20), BtimeNS: valid(200), FirstSeenNS: 200},
	}}
	state := map[string]solver.DiskState{"t1": solver.DiskOnline, "t2": solver.DiskOnline}
	update, err := settleFile(b.config, f, map[string]int{"t1": 0, "t2": 1}, state)
	if err != nil || update.Original.String != "t1" || update.Size != 5 || update.MtimeNS != 10 {
		t.Errorf("%+v, %v", update, err)
	}
}

// A file takes what its rule says, its label, its replicas and its mode; one a rule now ignores keeps what it was, and
// the pool's colocate (specs.md §12, §15.10).
func TestAFileTakesWhatItsRuleSays(t *testing.T) {
	b := newBenchWith(t, "[placement]\ncolocate = \"folder\"\n[[rule]]\nlabel = \"two\"\npath = \"keep/**\"\nreplicas = 2\nmode = \"manual\"\ncolocate = \"none\"\n"+
		"[[rule]]\nlabel = \"skip\"\npath = \"tmp/**\"\naction = \"ignore\"\n", "t1")
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	row := func(path string) *catalog.FileRow {
		return &catalog.FileRow{Path: path, Size: 4, MtimeNS: 1, ReplicasTarget: 1, Mode: "immediate", State: "ok",
			Rule: sql.NullString{String: "before", Valid: true}, Copies: []catalog.CopyRow{
				{Disk: "t1", State: "ok", Size: valid(4), MtimeNS: valid(1), FirstSeenNS: 1},
			}}
	}
	state := map[string]solver.DiskState{"t1": solver.DiskOnline}
	update, err := settleFile(b.config, row("keep/a"), map[string]int{"t1": 0}, state)
	if err != nil || update.ReplicasTarget != 2 || update.Mode != "manual" || update.Rule.String != "two" {
		t.Errorf("a rule's file: %+v, %v", update, err)
	}
	update, err = settleFile(b.config, row("tmp/a"), map[string]int{"t1": 0}, state)
	if err != nil || update.ReplicasTarget != 1 || update.Mode != "immediate" || update.Rule.String != "before" {
		t.Errorf("an ignored file: %+v, %v", update, err)
	}
	if got := colocateOf(b.config, row("keep/a")); got != "none" {
		t.Errorf("a rule's file is colocated %q", got)
	}
	if got := colocateOf(b.config, row("tmp/a")); got != "folder" {
		t.Errorf("an ignored file is colocated %q", got)
	}
}

// A file renamed while the walk went is one found at a name no row holds whose inode the walk also found at a name a
// row holds, or a row it could not reach says, as the same file: with one link, or as many as the other and another
// change time; two names with as many links and one change time are two links of one file. A first look, with rows for
// none of what it found, has nothing to compare, and asks the catalog nothing (specs.md §15.10).
func TestAFileRenamedWhileTheWalkWentIsKnown(t *testing.T) {
	b := newBench(t, "t1")
	d := &b.config.Disks[0]
	birth := func(n int64) *int64 { return &n }
	file := func(path string, nlink, ctime, btime int64) FoundFile {
		return FoundFile{Path: path, Size: 4, MtimeNS: 1, CtimeNS: ctime, BtimeNS: birth(btime), FileKey: "1:7", Nlink: nlink}
	}
	straddled := func(db *sql.DB, files []FoundFile, rowless []int, unreached ...solver.FileLook) ([]bool, error) {
		w := &Walked{Files: files}
		c := &compared{unreached: unreached}
		for _, i := range rowless {
			w.Files[i].rowless = true
			c.rowless = append(c.rowless, &w.Files[i])
		}
		err := c.markStraddled(t.Context(), db, d, w)
		out := make([]bool, len(w.Files))
		for i := range w.Files {
			out[i] = w.Files[i].straddled
		}
		return out, err
	}
	unreached := func(ctime, btime int64) solver.FileLook {
		return solver.FileLook{Inode: "1:7", Size: 4, MtimeNS: 1, CtimeNS: ctime, BtimeNS: birth(btime)}
	}
	for _, tc := range []struct {
		name      string
		files     []FoundFile
		rowless   []int
		unreached []solver.FileLook
		want      []bool
	}{
		{"one link at both names", []FoundFile{file("a", 1, 5, 9), file("b", 1, 5, 9)}, []int{1}, nil, []bool{false, true}},
		{"two links at both names, another change time", []FoundFile{file("a", 2, 5, 9), file("b", 2, 6, 9)}, []int{1}, nil, []bool{false, true}},
		{"two links of one file", []FoundFile{file("a", 2, 5, 9), file("b", 2, 5, 9)}, []int{1}, nil, []bool{false, false}},
		{"a link more at its row", []FoundFile{file("a", 3, 5, 9), file("b", 2, 6, 9)}, []int{1}, nil, []bool{false, false}},
		{"its inode used again", []FoundFile{file("a", 1, 5, 9), file("b", 1, 5, 8)}, []int{1}, nil, []bool{false, false}},
		{"a name no row holds alone", []FoundFile{file("b", 1, 5, 9)}, []int{0}, nil, []bool{false}},
		{"a row not reached, one link", []FoundFile{file("b", 1, 5, 9)}, []int{0}, []solver.FileLook{unreached(5, 9)}, []bool{true}},
		{"a row not reached, another change time", []FoundFile{file("b", 2, 6, 9)}, []int{0}, []solver.FileLook{unreached(5, 9)}, []bool{true}},
		{"a row not reached, a link of it", []FoundFile{file("b", 2, 5, 9)}, []int{0}, []solver.FileLook{unreached(5, 9)}, []bool{false}},
		{"a row not reached, its inode used again", []FoundFile{file("b", 1, 5, 9)}, []int{0}, []solver.FileLook{unreached(5, 8)}, []bool{false}},
		{"rows for all, none straddled", []FoundFile{file("a", 1, 5, 9)}, nil, nil, []bool{false}},
	} {
		got, err := straddled(b.db, tc.files, tc.rowless, tc.unreached...)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, %v, want %v", tc.name, got, err, tc.want)
		}
	}
	closed, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		files   []FoundFile
		rowless []int
	}{
		{"a first look", []FoundFile{file("a", 1, 5, 9), file("b", 1, 5, 9)}, []int{0, 1}},
		{"rows for all", []FoundFile{file("a", 1, 5, 9)}, nil},
		{"nothing found", nil, nil},
	} {
		if _, err := straddled(closed, tc.files, tc.rowless); err != nil {
			t.Errorf("%s asks the catalog: %v", tc.name, err)
		}
	}
	if _, err := straddled(closed, []FoundFile{file("a", 1, 5, 9), file("b", 1, 5, 9)}, []int{1}); err == nil {
		t.Error("the ignored names of a catalog that fails, and no error")
	}
}

// A verdict sets a copy's clock only when that changes it: a copy gone, moved, uncertain or Atoll's own starts it, or
// changes its kind, and one there again stops it (specs.md §15.12).
func TestACopysClockIsSetOnlyWhenItChanges(t *testing.T) {
	gone := func(kind string) *catalog.KnownCopy {
		k := &catalog.KnownCopy{FileID: 3}
		if kind != "" {
			k.GoneNS = sql.NullInt64{Int64: 1, Valid: true}
			k.GoneKind = sql.NullString{String: kind, Valid: true}
		}
		return k
	}
	for _, tc := range []struct {
		name string
		k    *catalog.KnownCopy
		v    solver.CopyVerdict
		want []catalog.CopyClock
	}{
		{"gone, from there", gone(""), solver.VerdictGone, []catalog.CopyClock{{FileID: 3, Kind: "gone"}}},
		{"gone, from moved", gone("moved"), solver.VerdictGone, []catalog.CopyClock{{FileID: 3, Kind: "gone"}}},
		{"gone again", gone("gone"), solver.VerdictGone, nil},
		{"moved again", gone("moved"), solver.VerdictMoved, nil},
		{"there again", gone("gone"), solver.VerdictThere, []catalog.CopyClock{{FileID: 3}}},
		{"there still", gone(""), solver.VerdictThere, nil},
		{"edited, from uncertain", gone("uncertain"), solver.VerdictEdited, []catalog.CopyClock{{FileID: 3}}},
		{"unknown", gone("gone"), solver.VerdictUnknown, nil},
	} {
		c := &compared{}
		c.clock(tc.k, tc.v)
		if !slices.Equal(c.clocks, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, c.clocks, tc.want)
		}
	}
}

// A row whose copy is there takes the mode and owner the walk found when it lacks any of the three, and a row whose
// copy sits at another name than its file's says so (specs.md §9, §15.10).
func TestARowThereTakesWhatItLacks(t *testing.T) {
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	known := func(path, filePath string, mode, uid, gid sql.NullInt64) *catalog.KnownCopy {
		return &catalog.KnownCopy{Path: path, FilePath: filePath, FileID: 3, Size: valid(4), MtimeNS: valid(1), CtimeNS: valid(5),
			BtimeNS: valid(9), FileKey: sql.NullString{String: "1:7", Valid: true}, Mode: mode, UID: uid, GID: gid}
	}
	at := func(path string) *FoundFile {
		nine := int64(9)
		return &FoundFile{Path: path, Size: 4, MtimeNS: 1, CtimeNS: 5, BtimeNS: &nine, FileKey: "1:7", Nlink: 1, Mode: 0o644, UID: 1000, GID: 100}
	}
	none := sql.NullInt64{}
	for _, tc := range []struct {
		name          string
		mode, uid, gd sql.NullInt64
		takes         bool
	}{
		{"all three", valid(0o600), valid(1), valid(2), false},
		{"no mode", none, valid(1), valid(2), true},
		{"no owner", valid(0o600), none, valid(2), true},
		{"no group", valid(0o600), valid(1), none, true},
	} {
		c := &compared{r: &Report{}, found: func(*FoundFile) {}}
		c.row(known("a", "a", tc.mode, tc.uid, tc.gd), at("a"))
		want := []catalog.CopyMode(nil)
		if tc.takes {
			want = []catalog.CopyMode{{FileID: 3, Mode: 0o644, UID: 1000, GID: 100}}
		}
		if c.r.Known != 1 || !slices.Equal(c.modes, want) || c.behind != nil {
			t.Errorf("%s: known %d, modes %+v, behind %v", tc.name, c.r.Known, c.modes, c.behind)
		}
	}
	c := &compared{r: &Report{}, found: func(*FoundFile) {}}
	c.row(known("b", "a", valid(0o600), valid(1), valid(2)), at("b"))
	if !c.behind["a"] || len(c.behind) != 1 {
		t.Errorf("a copy at another name than its file's: behind %v", c.behind)
	}
}

// A copy moved says whether it kept its size and date, which only a row that holds both can say; a copy replaced by a
// regular file says that file's change time, and one replaced by something else says nothing of it (specs.md §15.4,
// §15.12).
func TestAVerdictSaysWhatTheCopyBecame(t *testing.T) {
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	invalid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n} }
	known := func(size, mtime sql.NullInt64) knownRow {
		return compactRow(&catalog.KnownCopy{Path: "a", FilePath: "a", FileID: 3, Size: size, MtimeNS: mtime, CtimeNS: valid(5),
			FileKey: sql.NullString{String: "1:7", Valid: true}})
	}
	for _, tc := range []struct {
		name        string
		size, mtime sql.NullInt64
		same        bool
	}{
		{"kept both", valid(4), valid(1), true},
		{"another size", valid(5), valid(1), false},
		{"no size", invalid(4), valid(1), false},
		{"another date", valid(4), valid(2), false},
		{"no date", valid(4), invalid(1), false},
	} {
		c := &compared{r: &Report{}}
		target := &FoundFile{Path: "b", Size: 4, MtimeNS: 1, CtimeNS: 6, Nlink: 1, claims: 1}
		c.finish(&openRow{k: known(tc.size, tc.mtime), verdict: solver.VerdictMoved, target: target}, nil)
		want := CopySeen{FileID: 3, Verdict: solver.VerdictMoved, Target: "b", CtimeNS: 6, Nlink: 1, SameDate: tc.same}
		if len(c.r.Seen) != 1 || c.r.Seen[0] != want || !target.isTarget || !slices.Equal(c.r.Moved, []MovedCopy{{Path: "a", To: "b"}}) {
			t.Errorf("%s: %+v, %+v", tc.name, c.r.Seen, c.r.Moved)
		}
	}
	for _, at := range []*FoundFile{{Path: "a", CtimeNS: 8}, nil} {
		c := &compared{r: &Report{}}
		c.finish(&openRow{k: known(valid(4), valid(1)), at: at, verdict: solver.VerdictReplaced}, nil)
		want := CopySeen{FileID: 3, Verdict: solver.VerdictReplaced}
		if at != nil {
			want.CtimeNS, want.Regular = 8, true
		}
		if len(c.r.Seen) != 1 || c.r.Seen[0] != want || !slices.Equal(c.r.Changed, []string{"a"}) {
			t.Errorf("replaced by %+v: %+v, %v", at, c.r.Seen, c.r.Changed)
		}
	}
}

// A look limited to names that no longer finds a copy no event said was deleted asks for a walk of the whole disk,
// whatever the copies after it say (specs.md §15.13).
func TestALookLimitedToNamesAsksForAWholeWalkOnce(t *testing.T) {
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	row := func(path string, id int64) knownRow {
		return compactRow(&catalog.KnownCopy{Path: path, FilePath: path, FileID: id, Size: valid(4), MtimeNS: valid(1), CtimeNS: valid(5),
			FileKey: sql.NullString{String: "1:7", Valid: true}})
	}
	c := &compared{r: &Report{}, named: &namedLook{names: []string{"a", "b"}, deleted: []string{"b"}}}
	c.finish(&openRow{k: row("a", 1), verdict: solver.VerdictGone}, nil)
	c.finish(&openRow{k: row("b", 2), verdict: solver.VerdictGone}, nil)
	if !c.r.Whole || len(c.r.Seen) != 2 || c.r.Seen[0].Verdict != solver.VerdictUnknown || c.r.Seen[1].Verdict != solver.VerdictGone {
		t.Errorf("whole %v, %+v", c.r.Whole, c.r.Seen)
	}
}

// A row below a folder renamed to a name the walk did not enter says nothing of its file; one below a folder renamed
// elsewhere, or below no folder renamed, is no such row (specs.md §15.12).
func TestARowCarriedOutOfReachSaysNothing(t *testing.T) {
	moves := []FolderMove{{From: "d", To: "e"}}
	notReached := map[string]bool{"e/m": true}
	for path, want := range map[string]bool{"d/m/x": true, "d/m": true, "d/y": false, "f/m/x": false, "dm/x": false} {
		if got := carriedOutOfReach(moves, notReached, path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
	if carriedOutOfReach(nil, notReached, "e/m/x") {
		t.Error("no folder renamed, and a row carried")
	}
}

// Atoll's own folders hold a copy when they hold the same file, whichever of what they hold of its inode it is; a
// look that does not ask, or has nothing gone to ask of, asks nothing (specs.md §15.12).
func TestAtollsOwnFoldersHoldTheSameFile(t *testing.T) {
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	k := &catalog.KnownCopy{Path: "a", FileID: 3, Size: valid(4), MtimeNS: valid(1), CtimeNS: valid(5),
		FileKey: sql.NullString{String: "1:7", Valid: true}}
	other, same := solver.FileLook{Inode: "1:7", Size: 5, MtimeNS: 1}, solver.FileLook{Inode: "1:7", Size: 4, MtimeNS: 1}
	if !isOwn(k, map[string][]solver.FileLook{"1:7": {other, same}}) {
		t.Error("the second of an inode's files, and not Atoll's own")
	}
	if isOwn(k, map[string][]solver.FileLook{"1:7": {other}, "1:8": {same}}) {
		t.Error("another file, and Atoll's own")
	}
	b := newBench(t, "t1")
	d := &b.config.Disks[0]
	var asked []map[string]bool
	look := &Look{Own: func(_ *config.Disk, inodes map[string]bool) (map[string][]solver.FileLook, error) {
		asked = append(asked, inodes)
		return map[string][]solver.FileLook{"1:7": {same}}, nil
	}}
	c := &compared{}
	if own, err := c.ownFiles(d, &Look{}, map[string]bool{"1:7": true}); own != nil || err != nil {
		t.Errorf("a look that does not ask: %v, %v", own, err)
	}
	if own, err := c.ownFiles(d, look, map[string]bool{}); own != nil || err != nil || asked != nil {
		t.Errorf("nothing gone: %v, %v, %v", own, err, asked)
	}
	if own, err := c.ownFiles(d, look, map[string]bool{"1:7": true}); len(own["1:7"]) != 1 || err != nil || len(asked) != 1 || !asked[0]["1:7"] {
		t.Errorf("a copy gone: %v, %v, %v", own, err, asked)
	}
	failing := &Look{Own: func(*config.Disk, map[string]bool) (map[string][]solver.FileLook, error) { return nil, syscall.EIO }}
	if _, err := c.ownFiles(d, failing, map[string]bool{"1:7": true}); !errors.Is(err, syscall.EIO) {
		t.Errorf("own folders that fail: %v", err)
	}
}

// A new file a rule ignores is an ignored entry, but below a folder renamed and not yet decided or one kept, which keep
// their rows; any other gets a row of its own, unless it sits below a folder renamed or strictly below one kept, and
// each folder that holds new files is said once (specs.md §15.10, §15.12).
func TestANewFileGetsARowWhereNothingWaits(t *testing.T) {
	b := newBenchWith(t, "[[rule]]\nlabel = \"skip\"\npath = \"tmp/**\"\naction = \"ignore\"\n", "t1")
	type outcome struct {
		entries, copies, ignored, fresh int
		found                           bool
	}
	fresh := func(path string, tops, kept map[string]bool) outcome {
		r, out := &Report{}, &catalog.Walk{Kept: kept}
		found := false
		freshFile(b.config, &FoundFile{Path: path, Size: 4, MtimeNS: 1, FileKey: "1:7", Nlink: 1}, r, out, tops, func(*FoundFile) { found = true })
		return outcome{len(out.Entries), len(out.Copies), r.Ignored, r.New, found}
	}
	for _, tc := range []struct {
		name       string
		path       string
		tops, kept map[string]bool
		want       outcome
	}{
		{"ignored", "tmp/x", nil, nil, outcome{1, 0, 1, 0, false}},
		{"ignored below a folder renamed", "tmp/x", map[string]bool{"tmp": true}, nil, outcome{0, 0, 1, 0, false}},
		{"ignored below a folder kept", "tmp/x", nil, map[string]bool{"tmp": true}, outcome{0, 0, 1, 0, false}},
		{"new", "d/x", nil, nil, outcome{0, 1, 0, 1, true}},
		{"new below a folder renamed", "d/x", map[string]bool{"d": true}, nil, outcome{0, 0, 0, 0, true}},
		{"new strictly below a folder kept", "d/x", nil, map[string]bool{"d": true}, outcome{0, 0, 0, 0, true}},
		{"new at a name kept", "d/x", nil, map[string]bool{"d/x": true}, outcome{0, 1, 0, 1, true}},
	} {
		if got := fresh(tc.path, tc.tops, tc.kept); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	r, out := &Report{}, &catalog.Walk{}
	for _, path := range []string{"d/x", "d/y", "z", "e/f/g"} {
		freshFile(b.config, &FoundFile{Path: path, Size: 4, MtimeNS: 1, FileKey: "1:7", Nlink: 1}, r, out, nil, func(*FoundFile) {})
	}
	if !slices.Equal(r.NewFolders, []string{"d", "", "e/f"}) {
		t.Errorf("new folders %q", r.NewFolders)
	}
}

// A manual file is held until it is released, and a pending copy is running only while its job runs: a copy there is
// never running (specs.md §12, §13.4, §14.1).
func TestAPlanHoldsWhatIsManualAndKnowsWhatRuns(t *testing.T) {
	b := newBenchWith(t, "[[rule]]\nlabel = \"keep\"\npath = \"keep/**\"\nmode = \"manual\"\n", "t1", "t2")
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	row := func(path string) *catalog.FileRow {
		return &catalog.FileRow{ID: 4, Path: path, Size: 4, MtimeNS: 1, ReplicasTarget: 1, Mode: "immediate", State: "ok", Copies: []catalog.CopyRow{
			{Disk: "t1", State: "ok", Size: valid(4), MtimeNS: valid(1), FirstSeenNS: 1},
			{Disk: "t2", State: "pending", FirstSeenNS: 2},
		}}
	}
	kit := func(released func(string) bool, running map[int64][]string) *planKit {
		return &planKit{c: b.config, position: map[string]int{"t1": 0, "t2": 1},
			state:      map[string]solver.DiskState{"t1": solver.DiskOnline, "t2": solver.DiskOnline},
			placements: map[placementKey]*solver.Placement{}, released: released, running: running}
	}
	var none solver.Residency
	for _, tc := range []struct {
		name     string
		path     string
		released func(string) bool
		hold     bool
	}{
		{"manual", "keep/a", nil, true},
		{"manual, released", "keep/a", func(p string) bool { return p == "keep/a" }, false},
		{"manual, another released", "keep/a", func(p string) bool { return p == "keep/b" }, true},
		{"immediate", "a", nil, false},
	} {
		if file, _, err := kit(tc.released, nil).planFile(row(tc.path), false, none); err != nil || file.Hold != tc.hold {
			t.Errorf("%s: hold %v, %v", tc.name, file.Hold, err)
		}
	}
	for _, tc := range []struct {
		name    string
		running map[int64][]string
		want    []bool
	}{
		{"its job runs", map[int64][]string{4: {"t2"}}, []bool{false, true}},
		{"a job on the disk of its copy there", map[int64][]string{4: {"t1"}}, []bool{false, false}},
		{"another file's job", map[int64][]string{5: {"t2"}}, []bool{false, false}},
	} {
		file, _, err := kit(nil, tc.running).planFile(row("a"), false, none)
		if err != nil || len(file.Copies) != 2 || file.Copies[0].Running != tc.want[0] || file.Copies[1].Running != tc.want[1] {
			t.Errorf("%s: %+v, %v", tc.name, file.Copies, err)
		}
	}
}

// A file corrupt keeps that state while a copy of it is corrupt; once none is, it takes the state its copies give it,
// and the conflict it held, found while it was corrupt, comes first; a conflict holds whatever the plan says (specs.md
// §4, §16).
func TestCorruptHoldsWhileACopyIs(t *testing.T) {
	file := func(state solver.CopyState) solver.File {
		return solver.File{Path: "a", Copies: []solver.Copy{{Disk: "t1", State: solver.CopyOK}, {Disk: "t2", State: state}}}
	}
	results := []solver.Result{{Path: "a", States: []solver.State{solver.StateDegraded}}}
	for _, tc := range []struct {
		name  string
		copy  solver.CopyState
		state string
		held  bool
		want  solver.State
	}{
		{"corrupt, a copy corrupt", solver.CopyCorrupt, "corrupt", false, solver.StateCorrupt},
		{"corrupt, none now", solver.CopyOK, "corrupt", false, solver.StateDegraded},
		{"corrupt, none now, a conflict held", solver.CopyOK, "corrupt", true, solver.StateConflict},
		{"degraded, a copy corrupt", solver.CopyCorrupt, "ok", false, solver.StateDegraded},
		{"a conflict", solver.CopyOK, "conflict", false, solver.StateConflict},
	} {
		_, states, _, err := applyResults([]solver.File{file(tc.copy)}, []settled{{id: 1, state: tc.state, held: tc.held}}, results)
		got := tc.state
		if len(states) == 1 {
			got = states[0].State
		}
		if err != nil || got != string(tc.want) {
			t.Errorf("%s: %s, %v, want %s", tc.name, got, err, tc.want)
		}
	}
}

// The status sums the bytes of every file and of every copy, of those at their level and of those placed, whether
// under keeps all or a part, while every copy counts for its disk (specs.md §13.3, §15.11).
func TestStatusSumsEveryFile(t *testing.T) {
	b := newBench(t, "t1", "t2")
	for path, text := range map[string]string{"d/a": "x", "d/b": "yyy"} {
		b.write("t1", path, text)
		b.write("t2", path, text)
	}
	b.write("t1", "e", "zzzzz")
	b.scan(Options{})
	for _, tc := range []struct {
		under                  string
		misplaced              map[string]bool
		files                  int
		total, atLevel, placed int64
	}{
		{"", nil, 3, 9, 4, 9},
		{"", map[string]bool{"d/b": true}, 3, 9, 4, 6},
		{"d", nil, 2, 4, 4, 4},
	} {
		s, err := ReadStatus(t.Context(), b.db, tc.under, tc.misplaced, func(*catalog.FileRow) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if s.FilesTotal != tc.files || s.BytesTotal != tc.total || s.FilesAtLevel != 2 || s.BytesAtLevel != tc.atLevel || s.BytesPlaced != tc.placed {
			t.Errorf("under %q, misplaced %v: %+v", tc.under, tc.misplaced, s)
		}
		if s.Disks[0].Copies != 3 || s.Disks[0].Bytes != 9 || s.Disks[1].Copies != 2 || s.Disks[1].Bytes != 4 {
			t.Errorf("under %q, the disks: %+v", tc.under, s.Disks)
		}
	}
}

// A copy that cannot be relied on, before others that can, leaves them counted (specs.md §15.11).
func TestStatusCountsTheCopiesAfterOneItCannotRelyOn(t *testing.T) {
	at := map[string]int{"t1": 0, "t2": 1, "t3": 2}
	s := &Status{Disks: []DiskStatus{{Disk: catalog.Disk{ID: "t1", State: "online"}}, {Disk: catalog.Disk{ID: "t2", State: "missing"}},
		{Disk: catalog.Disk{ID: "t3", State: "online"}}, {Disk: catalog.Disk{ID: "t4", State: "online"}}}, ByState: map[string]int{}}
	at["t4"] = 3
	f := &catalog.FileRow{Size: 7, ReplicasTarget: 1, State: "ok", Copies: []catalog.CopyRow{
		{Disk: "t1", State: "stale"}, {Disk: "gone", State: "ok"}, {Disk: "t2", State: "ok"}, {Disk: "t3", State: "ok"}, {Disk: "t4", State: "ok"},
	}}
	if !s.atLevel(f, at) {
		t.Error("two copies to rely on, after three it cannot, and the file is not at its level")
	}
}

// The plan hands its copies to Queue as soon as a batch holds copyBatch of them, and a job that is no copy, before one
// that is, leaves that one in (specs.md §14.1, §26).
func TestTheQueueTakesABatchOnceFull(t *testing.T) {
	var batches []int
	q := &copyQueue{queue: func(jobs []PlannedJob) error {
		batches = append(batches, len(jobs))
		return nil
	}}
	for i := range copyBatch {
		q.files = append(q.files, solver.File{Path: "f", Placement: &solver.Placement{}})
		q.remembered = append(q.remembered, settled{id: int64(i + 1), state: "ok"})
	}
	for i := range copyBatch {
		jobs := []solver.Job{{Kind: solver.JobRefresh, Path: "f", Src: "a", Dst: "b"}, {Kind: solver.JobReplicate, Path: "f", Src: "a", Dst: "c"}}
		if err := q.add(i, jobs); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(batches, []int{copyBatch}) || len(q.batch) != 0 {
		t.Errorf("batches of %v, %d left", batches, len(q.batch))
	}
}

// A name heard again counts once, a folder or deleted once any of its events said so, and the names after it are kept
// with it; maxNames names exactly are still names (specs.md §15.13).
func TestNamesHeardAgainCountOnce(t *testing.T) {
	h := map[string]Heard{}
	AddHeard(h, map[string]Heard{"t1": {Names: []Named{{Path: "a"}, {Path: "b"}}}}, 3)
	AddHeard(h, map[string]Heard{"t1": {Names: []Named{{Path: "a", Folder: true}, {Path: "c", Deleted: true}}}}, 3)
	if want := []Named{{Path: "a", Folder: true}, {Path: "b"}, {Path: "c", Deleted: true}}; !slices.Equal(h["t1"].Names, want) || h["t1"].Whole {
		t.Errorf("%+v", h["t1"])
	}
}

// A debounce of zero is one a configuration read again may give: a group is then ripe at its own event; a negative one
// is refused, and the one before stays (specs.md §15.1).
func TestADebounceOfZeroIsOne(t *testing.T) {
	g, err := NewGrouping(1_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SetDebounce(0); err != nil {
		t.Fatalf("a debounce of zero: %v", err)
	}
	if err := g.SetDebounce(-1); err == nil {
		t.Error("a negative debounce, and no error")
	}
	if err := g.Add(Event{Disk: "t1", Path: "a", Kind: KindWritten, Origin: OriginUser, AtNS: 5}); err != nil {
		t.Fatal(err)
	}
	if ripe, waiting, _, _ := g.Ripe(5); len(ripe) != 1 || waiting != 0 {
		t.Errorf("at its event: %d ripe, %d waiting", len(ripe), waiting)
	}
}

// A look at a copy finds what its row says only when its size, its modification time, its change time and its inode are
// all the row's, the row holding each (specs.md §15.10).
func TestAFileIsUnchangedOnlyAsItsRowSaysOfAll(t *testing.T) {
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	row := func() *catalog.KnownCopy {
		return &catalog.KnownCopy{Size: valid(4), MtimeNS: valid(1), CtimeNS: valid(5), FileKey: sql.NullString{String: "7", Valid: true}}
	}
	f := &FoundFile{Size: 4, MtimeNS: 1, CtimeNS: 5, FileKey: "7"}
	if !Unchanged(f, row()) {
		t.Error("all as its row says, and changed")
	}
	for name, change := range map[string]func(k *catalog.KnownCopy){
		"another size":              func(k *catalog.KnownCopy) { k.Size.Int64 = 5 },
		"no size":                   func(k *catalog.KnownCopy) { k.Size.Valid = false },
		"another modification time": func(k *catalog.KnownCopy) { k.MtimeNS.Int64 = 2 },
		"no modification time":      func(k *catalog.KnownCopy) { k.MtimeNS.Valid = false },
		"another change time":       func(k *catalog.KnownCopy) { k.CtimeNS.Int64 = 6 },
		"no change time":            func(k *catalog.KnownCopy) { k.CtimeNS.Valid = false },
		"another inode":             func(k *catalog.KnownCopy) { k.FileKey.String = "8" },
		"no inode":                  func(k *catalog.KnownCopy) { k.FileKey.Valid = false },
	} {
		k := row()
		change(k)
		if Unchanged(f, k) {
			t.Errorf("%s, and unchanged", name)
		}
	}
}

// A scan confirms a disk past the cache before it writes what its walk found when that says a copy moved, went, or is
// uncertain or Atoll's own, and never for a copy there, edited, replaced or not reached (specs.md §10, §15.10).
func TestWhatAScanConfirmsBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		v    solver.CopyVerdict
		want bool
	}{
		{solver.VerdictMoved, true}, {solver.VerdictGone, true}, {solver.VerdictUncertain, true}, {solver.VerdictOwn, true},
		{solver.VerdictThere, false}, {solver.VerdictEdited, false}, {solver.VerdictReplaced, false}, {solver.VerdictUnknown, false},
	} {
		r := &Report{Seen: []CopySeen{{FileID: 1, Verdict: solver.VerdictThere}, {FileID: 2, Verdict: tc.v}}}
		if got := needsConfirming(r); got != tc.want {
			t.Errorf("%s: %v", tc.v, got)
		}
	}
	if needsConfirming(&Report{}) {
		t.Error("nothing seen, and confirmed")
	}
}

// The verdict about a copy is found by its file, and none for a file the look said nothing of.
func TestTheVerdictOfACopyIsFoundByItsFile(t *testing.T) {
	list := []CopySeen{{FileID: 9, Verdict: solver.VerdictGone}, {FileID: 3, Verdict: solver.VerdictMoved}, {FileID: 5, Verdict: solver.VerdictThere}}
	SortSeen(list)
	for id, want := range map[int64]solver.CopyVerdict{3: solver.VerdictMoved, 5: solver.VerdictThere, 9: solver.VerdictGone} {
		if got := SeenOf(list, id); got == nil || got.Verdict != want || got.FileID != id {
			t.Errorf("%d: %+v", id, got)
		}
	}
	if got := SeenOf(list, 4); got != nil {
		t.Errorf("a file not seen: %+v", got)
	}
	if got := SeenOf(list, 5); got != &list[1] {
		t.Error("not the verdict of the list itself")
	}
}

// A row the walk could not get to says nothing of its copy: unknown, as its row when the name holds it unchanged or a
// reprise is to tell of it; a file of another inode at its name is one a copy may have gone to (specs.md §15.10).
func TestARowNotReachedSaysNothingOfItsCopy(t *testing.T) {
	valid := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	k := &catalog.KnownCopy{Path: "d/a", FilePath: "d/a", FileID: 3, Size: valid(4), MtimeNS: valid(1), CtimeNS: valid(5), BtimeNS: valid(9),
		FileKey: sql.NullString{String: "7", Valid: true}}
	nine, eight := int64(9), int64(8)
	for _, tc := range []struct {
		name    string
		at      *FoundFile
		planned map[string]bool
		asRow   bool
		others  int
	}{
		{"as its row", &FoundFile{Path: "d/a", Size: 4, MtimeNS: 1, CtimeNS: 5, BtimeNS: &nine, FileKey: "7"}, nil, true, 0},
		{"edited", &FoundFile{Path: "d/a", Size: 6, MtimeNS: 2, CtimeNS: 6, BtimeNS: &nine, FileKey: "7"}, nil, false, 0},
		{"another file", &FoundFile{Path: "d/a", Size: 4, MtimeNS: 1, CtimeNS: 5, BtimeNS: &eight, FileKey: "8"}, nil, false, 1},
		{"nothing found", nil, nil, false, 0},
		{"nothing found, planned", nil, map[string]bool{"d": true}, true, 0},
	} {
		found := 0
		c := &compared{r: &Report{}, notReached: map[string]bool{"d": true}, planned: tc.planned, found: func(*FoundFile) { found++ }}
		c.row(k, tc.at)
		want := CopySeen{FileID: 3, Verdict: solver.VerdictUnknown, AsRow: tc.asRow}
		unreached := 0
		if tc.at == nil {
			unreached = 1
		} else if found != 1 {
			t.Errorf("%s: found %d times", tc.name, found)
		}
		if len(c.r.Seen) != 1 || c.r.Seen[0] != want || len(c.others) != tc.others || len(c.unreached) != unreached || len(c.open) != 0 {
			t.Errorf("%s: seen %+v, %d others, %d unreached, %d open", tc.name, c.r.Seen, len(c.others), len(c.unreached), len(c.open))
		}
	}
}
