package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode"
)

// A name written to a terminal writes no line of its own, and no control character: a file may be named with a line
// feed or an escape sequence, and a line that reads "moved to the trash" under it, or a screen cleared, is then the
// name's doing. Such a name is shown in base64, as one that is not UTF-8 is, and as an alert shows it (specs.md §23).
// The human output of scan, status, names check, guard list and the disk commands wrote it as it is.
func TestANameWritesNoLineOfItsOwn(t *testing.T) {
	const evil = "a\n  moved to the trash: every film\x1b[2J\r"
	texts := map[string]func(name string) string{
		"shownPath": func(name string) string { return shownPath(name) + "\n" },
		"shownB64":  func(name string) string { return shownB64(name, "") + "\n" },
		"pathText":  func(name string) string { return pathText(pathItem{Path: name}) + "\n" },
		"printPaths": func(name string) string {
			var b strings.Builder
			printPaths(&b, "  held", []pathItem{{Path: name}})
			return b.String()
		},
		"names check": func(name string) string {
			var b bytes.Buffer
			if err := writeNamesText(&b, &namesOutput{Checked: 2, Findings: []namesFinding{{Kind: "collision", Path: name, With: []string{name}}}}); err != nil {
				t.Fatal(err)
			}
			return b.String()
		},
		"status": func(name string) string {
			var b strings.Builder
			printStatusFile(&b, &statusFile{Path: name, State: "ok"})
			return b.String()
		},
		"disk fail": func(name string) string {
			return diskChangeText(&diskChangeOutput{Disk: "t1", DependingFirst: pathItems([]string{name})})
		},
		"disk show": func(name string) string {
			return diskShowText(&diskShowOutput{ID: "t1", DependingFirst: pathItems([]string{name})})
		},
		"a server's answer": func(name string) string { return shownText(name) + "\n" },
	}
	for what, text := range texts {
		got, plain := text(evil), text("a")
		if strings.Count(got, "\n") != strings.Count(plain, "\n") {
			t.Errorf("%s: %d lines for a name with a line feed, %d for a plain one: %q", what, strings.Count(got, "\n"), strings.Count(plain, "\n"), got)
		}
		if strings.ContainsFunc(strings.ReplaceAll(got, "\n", ""), unicode.IsControl) {
			t.Errorf("%s: a control character reaches the terminal: %q", what, got)
		}
	}
	// A plain name is shown as it is, accents and spaces included, and one that is not UTF-8 in base64 as before.
	for name, want := range map[string]string{"Films/Amélie (2001).mkv": "Films/Amélie (2001).mkv", "a\xffb": "(base64) Yf9i", "a\tb": "(base64) YQli"} {
		if got := shownPath(name); got != want {
			t.Errorf("%q: %q", name, got)
		}
	}
	if got := shownB64("", "Yf9i"); got != "(base64) Yf9i" {
		t.Errorf("%q", got)
	}
	// A text keeps what can be read of it.
	if got := shownText("refused: \x1b[31mno\x00"); got != "refused: �[31mno�" {
		t.Errorf("%q", got)
	}
}

// The files a disk's commands name as depending on it are paths of specs.md §29.3: path, or path_b64 for a name that
// is not UTF-8, which a plain string cannot hold and wrote as U+FFFD.
func TestDependingFirstIsAPath(t *testing.T) {
	names := pathItems([]string{"a", "b\xff"})
	for _, out := range []any{diskShowOutput{DependingFirst: names}, diskChangeOutput{DependingFirst: names}} {
		text, err := json.Marshal(out)
		if err != nil || !strings.Contains(string(text), `"depending_first":[{"path":"a"},{"path_b64":"Yv8="}]`) {
			t.Errorf("%s, %v", text, err)
		}
	}
}
