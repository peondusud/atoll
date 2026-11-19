package models

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "draw states.md again from states.json")

// machines is states.json.
type machines struct {
	About    string    `json:"about"`
	Machines []machine `json:"machines"`
}

type machine struct {
	Name        string            `json:"name"`
	Title       string            `json:"title"`
	Table       string            `json:"table"`
	Glossary    string            `json:"glossary"`
	States      []string          `json:"states"`
	Transitions []transition      `json:"transitions"`
	Unentered   map[string]string `json:"unentered"`
	Bugs        []bug             `json:"bugs"`
}

type transition struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
	When  string `json:"when"`
	Spec  string `json:"spec"`
}

type bug struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
	Entry string `json:"entry"` // the heading of DONE.md that fixed it, up to its date
	Flaw  string `json:"flaw"`  // or the flaw of README.md still open
}

func readMachines(t *testing.T) machines {
	t.Helper()
	var m machines
	if err := json.Unmarshal([]byte(read(t, "states.json")), &m); err != nil {
		t.Fatalf("states.json: %v", err)
	}
	return m
}

// schemaStates gives the closed list of a table's column state, as the last migration that makes the table says.
func schemaStates(t *testing.T, table string) []string {
	t.Helper()
	files, err := filepath.Glob("../../schema/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("schema/migrations: %v", err)
	}
	slices.Sort(files)
	create := regexp.MustCompile(`(?s)CREATE TABLE ` + table + `\((.*?)\n\)`)
	check := regexp.MustCompile(`\bstate\s+TEXT NOT NULL CHECK \(state IN \(([^)]*)\)\)`)
	var list []string
	for _, f := range files {
		for _, m := range create.FindAllStringSubmatch(read(t, f), -1) {
			if c := check.FindStringSubmatch(m[1]); c != nil {
				list = list[:0]
				for _, v := range strings.Split(c[1], ",") {
					list = append(list, strings.Trim(strings.TrimSpace(v), "'"))
				}
			}
		}
	}
	return list
}

// glossaryStates gives the values a bullet of specs.md §4 lists, in its first sentence: "- **Copy states**: `ok`, ...".
func glossaryStates(t *testing.T, spec, name string) []string {
	t.Helper()
	text, _, err := sectionText(spec, "4")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "- **" + name + "**: "
	for line := range strings.SplitSeq(text, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			first, _, _ := strings.Cut(rest, ". ")
			var out []string
			for _, m := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(first, -1) {
				out = append(out, m[1])
			}
			return out
		}
	}
	t.Fatalf("specs.md §4 has no bullet %q", name)
	return nil
}

// TestStatesKeepTheClosedLists fails once states.json no longer lists the states of the schema and of specs.md §4, or
// says of a transition or a bug what nothing else says.
func TestStatesKeepTheClosedLists(t *testing.T) {
	spec, done, readme := read(t, "../../specs.md"), read(t, "../../DONE.md"), read(t, "README.md")
	for _, m := range readMachines(t).Machines {
		if got := schemaStates(t, m.Table); !slices.Equal(got, m.States) {
			t.Errorf("%s: schema/migrations' closed list of %s.state is %v, states.json says %v", m.Name, m.Table, got, m.States)
		}
		if got := glossaryStates(t, spec, m.Glossary); !slices.Equal(got, m.States) {
			t.Errorf("%s: specs.md §4 lists %v, states.json says %v", m.Name, got, m.States)
		}
		known := append([]string{"[*]"}, m.States...)
		entered := map[string]bool{}
		seen := map[string]bool{}
		for _, tr := range m.Transitions {
			pair := tr.From + " -> " + tr.To
			switch {
			case !slices.Contains(known, tr.From) || !slices.Contains(known, tr.To) || tr.From == tr.To:
				t.Errorf("%s: %s: no such transition", m.Name, pair)
			case seen[pair]:
				t.Errorf("%s: %s twice", m.Name, pair)
			case tr.Label == "" || tr.When == "" || !strings.HasPrefix(tr.Spec, "§"):
				t.Errorf("%s: %s says nothing of when, or of where specs.md says so", m.Name, pair)
			}
			seen[pair], entered[tr.To] = true, true
		}
		for _, s := range m.States {
			_, unentered := m.Unentered[s]
			if entered[s] == unentered {
				t.Errorf("%s: %s is entered by a transition: %v, and said to be entered by none: %v", m.Name, s, entered[s], unentered)
			}
		}
		for _, b := range m.Bugs {
			pair := b.From + " -> " + b.To
			if !seen[pair] {
				t.Errorf("%s: a bug on %s, which is no transition", m.Name, pair)
			}
			switch {
			case (b.Entry == "") == (b.Flaw == ""):
				t.Errorf("%s: the bug %q names neither an entry of DONE.md nor a flaw, or both", m.Name, b.Label)
			case b.Entry != "" && !regexp.MustCompile(`(?m)^## `+regexp.QuoteMeta(b.Entry)+`( |$)`).MatchString(done):
				t.Errorf("%s: DONE.md has no entry %q", m.Name, b.Entry)
			case b.Flaw != "" && !strings.Contains(readme, "### "+b.Flaw+":"):
				t.Errorf("%s: README.md has no %s", m.Name, b.Flaw)
			}
		}
	}
}

// TestStatesDrawing keeps states.md what states.json draws; go test ./conformance/models -run TestStatesDrawing -update
// draws it again.
func TestStatesDrawing(t *testing.T) {
	want := draw(readMachines(t))
	if *update {
		if err := os.WriteFile("states.md", []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := read(t, "states.md"); got != want {
		t.Error("states.md is not what states.json draws: go test ./conformance/models -run TestStatesDrawing -update")
	}
}

func draw(all machines) string {
	var b strings.Builder
	b.WriteString("# Disk and copy states\n\n")
	b.WriteString("Drawn from `states.json` by `go test ./conformance/models -run TestStatesDrawing -update`; edit that file, " +
		"not this one. " + all.About + "\n")
	for _, m := range all.Machines {
		fmt.Fprintf(&b, "\n## %s\n\n```mermaid\nstateDiagram-v2\n", m.Title)
		for _, tr := range m.Transitions {
			mark := ""
			for i, bg := range m.Bugs {
				if bg.From == tr.From && bg.To == tr.To {
					mark += fmt.Sprintf(" [%d]", i+1)
				}
			}
			fmt.Fprintf(&b, "    %s --> %s: %s%s\n", tr.From, tr.To, tr.Label, mark)
		}
		b.WriteString("```\n\n| From | To | When | specs.md |\n|---|---|---|---|\n")
		for _, tr := range m.Transitions {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", cell(tr.From), cell(tr.To), tr.When, tr.Spec)
		}
		if len(m.Unentered) > 0 {
			b.WriteString("\nNo transition enters:\n\n")
			for _, s := range slices.Sorted(maps.Keys(m.Unentered)) {
				fmt.Fprintf(&b, "- `%s`: %s.\n", s, m.Unentered[s])
			}
		}
		if len(m.Bugs) > 0 {
			b.WriteString("\n| Bug | Transition | What went wrong | Where |\n|---|---|---|---|\n")
			for i, bg := range m.Bugs {
				where := "DONE.md, " + bg.Entry
				if bg.Flaw != "" {
					where = "README.md, " + bg.Flaw + ", open"
				}
				fmt.Fprintf(&b, "| [%d] | %s → %s | %s | %s |\n", i+1, cell(bg.From), cell(bg.To), bg.Label, where)
			}
		}
	}
	return b.String()
}

// cell writes a state as a table shows it: [*] is no state, but before the first or after the last.
func cell(s string) string {
	if s == "[*]" {
		return "(none)"
	}
	return "`" + s + "`"
}
