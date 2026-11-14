package catalog

import (
	"database/sql"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The reads the guards make of the events log, at every pass for each disk, search it through an index: over the
// events of their kinds, and a count over its window only, never over the whole log, which only grows (specs.md §9,
// §15.6). Without one, a pass with nothing to do took 0.8 s over a million events, against 64 ms.
func TestGuardReadsUseAnIndex(t *testing.T) {
	b := openForOps(t)
	for name, query := range map[string]string{
		"guard answer": guardAnswerQuery, "guard end": guardEndQuery, "guard since": guardSinceQuery, "copies gone": copiesGoneQuery,
		"modify answer": modifyAnswerQuery, "modify guard since": modifyGuardSinceQuery, "modified count": modifiedCountQuery,
	} {
		plan := planOf(t, b.sql(), name, query)
		searched := false
		for _, step := range plan {
			if strings.HasPrefix(step, "SCAN events") {
				t.Errorf("%s reads the whole log: %q", name, plan)
			}
			searched = searched || strings.HasPrefix(step, "SEARCH events USING INDEX events_by_kind")
		}
		if !searched {
			t.Errorf("%s: %q", name, plan)
		}
	}
}

// What a purge by slices and a restore read of the trash, they read through its indexes, never the whole of it: the
// entries whose time has come by their expiry, those of a path and below it by path, and one entry by its row
// (specs.md §17). A purge listed every entry of the trash, with its paths, to find the few that were due.
func TestTheTrashIsReadThroughItsIndexes(t *testing.T) {
	b := openForOps(t)
	for name, want := range map[string]struct{ query, step string }{
		"expired":      {trashExpiredSQL, "SEARCH t USING INDEX trash_by_expiry"},
		"due":          {trashDueSQL, "SEARCH t USING INDEX trash_by_expiry"},
		"below a path": {trashRangeSQL, "SEARCH t USING INDEX trash_by_path"},
		"by row":       {trashByRowSQL, "SEARCH t USING INTEGER PRIMARY KEY (rowid=?)"},
	} {
		plan := planOf(t, b.sql(), name, want.query)
		found := false
		for _, step := range plan {
			if strings.HasPrefix(step, "SCAN t") || strings.Contains(step, "TEMP B-TREE") {
				t.Errorf("%s reads or sorts the whole trash: %q", name, plan)
			}
			found = found || strings.HasPrefix(step, want.step)
		}
		if !found {
			t.Errorf("%s: %q", name, plan)
		}
	}
}

// Where the journal put the file of an inode is read through the index of the operations by disk and inode, never
// over the whole journal, which holds an operation for each copy ever made (specs.md §9).
func TestTheJournalIsAskedByInode(t *testing.T) {
	b := openForOps(t)
	if plan := planOf(t, b.sql(), "places", placesOfSQL); len(plan) != 1 || !strings.HasPrefix(plan[0], "SEARCH ops USING INDEX ops_by_key (disk_id=? AND file_key=?)") {
		t.Errorf("%q", plan)
	}
	if plan := planOf(t, b.sql(), "on disk", opOnDiskSQL); !slices.ContainsFunc(plan, func(step string) bool {
		return strings.HasPrefix(step, "SEARCH ops USING INTEGER PRIMARY KEY (rowid=?)")
	}) {
		t.Errorf("%q", plan)
	}
}

// Looking for the operations that name a part of tmp/ goes from its id on, never over the whole journal (specs.md
// §14.2): read at each pass, that took 0.35 s over a million copies.
func TestPartStatesReadFromTheirID(t *testing.T) {
	plan := planOf(t, openForOps(t).sql(), "part states", partStatesQuery)
	if len(plan) != 1 || !strings.HasPrefix(plan[0], "SEARCH ops USING INTEGER PRIMARY KEY (rowid>?)") {
		t.Errorf("%q", plan)
	}
}

// The state of a repair, which names the chunks it left in tmp/, is read by its id (specs.md §14.2, §16.1).
func TestRepairStatesReadFromTheirID(t *testing.T) {
	plan := planOf(t, openForOps(t).sql(), "repair states", repairStateQuery)
	if len(plan) != 1 || !strings.HasPrefix(plan[0], "SEARCH ops USING INTEGER PRIMARY KEY (rowid=?)") {
		t.Errorf("%q", plan)
	}
}

// The files a plan reads as leaving are looked for over its page of paths, and their jobs by file, never over the
// whole catalog or every job: a plan reads every file at each pass (specs.md §13.4).
func TestLeavingReadsItsPage(t *testing.T) {
	plan := planOf(t, openForOps(t).sql(), "leaving", leavingQuery)
	var files, jobs bool
	for _, step := range plan {
		files = files || strings.HasPrefix(step, "SEARCH f USING") && strings.HasSuffix(step, "INDEX sqlite_autoindex_files_1 (lpath>? AND lpath<?)")
		jobs = jobs || strings.HasPrefix(step, "SEARCH j ") && strings.HasSuffix(step, "USING INDEX jobs_by_file (file_id=?)")
	}
	if len(plan) != 2 || !files || !jobs {
		t.Errorf("%q", plan)
	}
}

// placeholders gives a value for each parameter of a query: ?1 to ?N, or one for each ?.
func placeholders(query string) []any {
	n := 0
	for _, m := range regexp.MustCompile(`\?(\d*)`).FindAllStringSubmatch(query, -1) {
		if m[1] == "" {
			n++
		} else if i, _ := strconv.Atoi(m[1]); i > n {
			n = i
		}
	}
	args := make([]any, n)
	for i := range args {
		args[i] = int64(0)
	}
	return args
}

// planOf gives what SQLite says it does for a query, one step a line.
func planOf(t *testing.T, db *sql.DB, name, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, placeholders(query)...)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return plan
}
