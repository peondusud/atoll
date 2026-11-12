package catalog

import (
	"database/sql"
	"regexp"
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
