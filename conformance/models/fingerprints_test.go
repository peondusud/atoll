// Package models keeps the TLA+ model of this folder in step with specs.md: each section the model formalizes is
// pinned by the SHA-256 of its text in fingerprints.json, so that a change to one of them is a reason to reread the
// model (CLAUDE.md, "Testing rules"). It reads files only, and imports nothing from the implementation.
package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fingerprints is fingerprints.json.
type fingerprints struct {
	Model         string    `json:"model"`
	Normalisation string    `json:"normalisation"`
	Sections      []section `json:"sections"`
}

type section struct {
	Section string `json:"section"`
	Title   string `json:"title"`
	SHA256  string `json:"sha256"`
}

// A heading: one to six '#', a space, and the section's number, "14." for a section and "14.2" for a subsection.
var heading = regexp.MustCompile(`^(#{1,6}) ([0-9][0-9.]*)(?: |$)`)

// sectionText gives one section of a specification as fingerprints.json's normalisation says, and its heading line:
// CRLF read as LF; each line without its trailing spaces and tabs; from the section's heading to the line before the
// next heading of as many '#' or fewer, a heading inside a fenced code block (between lines that begin with ```) not
// counting; without its blank lines at the end; each line ended by LF.
func sectionText(spec, number string) (text, title string, err error) {
	var out []string
	level, fenced := 0, false
	for line := range strings.SplitSeq(strings.ReplaceAll(spec, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		} else if m := heading.FindStringSubmatch(line); m != nil && !fenced {
			switch {
			case title != "" && len(m[1]) <= level:
				return finish(out), title, nil
			case title == "" && strings.TrimSuffix(m[2], ".") == number:
				title, level = line, len(m[1])
			}
		}
		if title != "" {
			out = append(out, line)
		}
	}
	if title == "" {
		return "", "", errors.New("no heading has that number")
	}
	return finish(out), title, nil
}

func finish(lines []string) string {
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n") + "\n"
}

func sum(text string) string {
	s := sha256.Sum256([]byte(text))
	return hex.EncodeToString(s[:])
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSpecsSectionsKeepTheirFingerprint fails once a section the model formalizes is no longer the text the model
// was checked against.
func TestSpecsSectionsKeepTheirFingerprint(t *testing.T) {
	spec := read(t, "../../specs.md")
	var fp fingerprints
	if err := json.Unmarshal([]byte(read(t, "fingerprints.json")), &fp); err != nil {
		t.Fatalf("fingerprints.json: %v", err)
	}
	model := read(t, fp.Model)
	if len(fp.Sections) == 0 || fp.Normalisation == "" {
		t.Fatal("fingerprints.json: no section, or no normalisation")
	}
	var numbers []string
	for _, s := range fp.Sections {
		numbers = append(numbers, s.Section)
		text, title, err := sectionText(spec, s.Section)
		if err != nil {
			t.Errorf("specs.md §%s: %v: the section the model formalizes was renumbered or removed; reread %s against "+
				"specs.md, and say in fingerprints.json which section it formalizes now", s.Section, err, fp.Model)
			continue
		}
		if got := sum(text); got != s.SHA256 || title != s.Title {
			t.Errorf("specs.md §%s (%s) is no longer the text %s was checked against: reread the model against it, "+
				"update the model or confirm it unchanged (make model-check), and give that section in fingerprints.json "+
				"the title %q and the sha256 %q, in the same commit (CLAUDE.md)", s.Section, title, fp.Model, title, got)
		}
		// Cited as itself: §14 in "§14," or "§14 ", never in "§14.2".
		if !regexp.MustCompile(`§` + regexp.QuoteMeta(s.Section) + `($|[^0-9.]|\.[^0-9])`).MatchString(model) {
			t.Errorf("fingerprints.json pins specs.md §%s, which %s never cites", s.Section, fp.Model)
		}
	}
	if !slices.IsSortedFunc(numbers, compareSections) || len(slices.Compact(slices.Clone(numbers))) != len(numbers) {
		t.Errorf("fingerprints.json: sections not in order, or twice: %v", numbers)
	}
}

// compareSections orders section numbers as specs.md does: 9 before 10, 14 before 14.1.
func compareSections(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range min(len(pa), len(pb)) {
		// The heading pattern only takes digits there.
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x - y
		}
	}
	return len(pa) - len(pb)
}

// TestSectionText pins the normalisation on a small text: a subsection belongs to its section, a heading in a code
// block is none, line ends and trailing spaces do not count.
func TestSectionText(t *testing.T) {
	spec := "# T\r\n\r\n## 1. One\r\n\r\na  \r\n\r\n```\r\n## 9. not a heading\r\n```\r\n\r\n### 1.1 Sub\r\n\r\nb\t\r\n\r\n## 2. Two\r\nc\r\n"
	cases := []struct{ number, text, title string }{
		{"1", "## 1. One\n\na\n\n```\n## 9. not a heading\n```\n\n### 1.1 Sub\n\nb\n", "## 1. One"},
		{"1.1", "### 1.1 Sub\n\nb\n", "### 1.1 Sub"},
		{"2", "## 2. Two\nc\n", "## 2. Two"},
	}
	for _, c := range cases {
		text, title, err := sectionText(spec, c.number)
		if err != nil || text != c.text || title != c.title {
			t.Errorf("§%s: %q, %q, %v; want %q, %q", c.number, text, title, err, c.text, c.title)
		}
	}
	if _, _, err := sectionText(spec, "9"); err == nil {
		t.Error("§9 is in a code block, and no heading")
	}
	if a, b := sum("x\n"), sum("x\n"); a != b || len(a) != 64 {
		t.Errorf("sum: %q", a)
	}
}
