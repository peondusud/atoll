package schema

import (
	"testing"
	"testing/fstest"
)

func TestMigrationsAreConsecutive(t *testing.T) {
	list, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 || list[0].Version != 1 || list[0].Name != "0001_initial.sql" || list[0].SQL == "" {
		t.Errorf("got %d migrations, the first being %+v", len(list), list[0])
	}
}

func TestMigrationRules(t *testing.T) {
	sql := &fstest.MapFile{Data: []byte("CREATE TABLE a(b INTEGER) STRICT;")}
	for _, c := range []struct {
		note  string
		files []string
		want  []int
	}{
		{"none", nil, nil},
		{"one", []string{"0001_a.sql"}, []int{1}},
		{"in order, whatever the listing", []string{"0002_b.sql", "0001_a.sql", "0003_c.sql"}, []int{1, 2, 3}},
		{"starting past 1", []string{"0002_a.sql"}, nil},
		{"starting at 0", []string{"0000_a.sql"}, nil},
		{"a gap", []string{"0001_a.sql", "0003_c.sql"}, nil},
		{"one number twice", []string{"0001_a.sql", "0001_b.sql"}, nil},
		{"no name", []string{"0001.sql"}, nil},
		{"three digits", []string{"001_a.sql"}, nil},
		{"uppercase", []string{"0001_A.sql"}, nil},
		{"a space", []string{"0001_a b.sql"}, nil},
	} {
		files := fstest.MapFS{"migrations/.keep": &fstest.MapFile{Mode: 0o755}}
		delete(files, "migrations/.keep")
		for _, name := range c.files {
			files["migrations/"+name] = sql
		}
		if len(c.files) == 0 {
			files["migrations"] = &fstest.MapFile{Mode: 0o755 | 1<<31}
		}
		list, err := readMigrations(files)
		wantError := c.want == nil && len(c.files) > 0
		if (err != nil) != wantError {
			t.Errorf("%s: got %+v, %v", c.note, list, err)
			continue
		}
		if len(list) != len(c.want) {
			t.Errorf("%s: got %+v", c.note, list)
			continue
		}
		for i, m := range list {
			if m.Version != c.want[i] {
				t.Errorf("%s: got %+v", c.note, list)
			}
		}
	}
}
