package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
)

// humanTexts gives, for each human output that writes a name, what it writes for one: the text of each place a person
// is shown a name, each through shownPath, shownB64 or pathText.
func humanTexts(t *testing.T) map[string]func(name string) string {
	t.Helper()
	return map[string]func(name string) string{
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
		"dupes": func(name string) string {
			return dupesText(&dupesOutput{Groups: []dupeGroupOutput{{Paths: []pathItem{{Path: name}}}}})
		},
		"daemon": func(name string) string {
			var b bytes.Buffer
			daemon := &textDaemon{w: &b}
			if err := daemon.Decision(&jobs.Decision{Kind: "rename", Path: name, To: name}); err != nil {
				t.Fatal(err)
			}
			if err := daemon.Operation(&jobs.Operation{Disk: "t1", Kind: "trash", Path: name, Outcome: "done"}); err != nil {
				t.Fatal(err)
			}
			return b.String()
		},
	}
}

// A name written to a terminal writes no line of its own, and no control character: a file may be named with a line
// feed or an escape sequence, and a line that reads "moved to the trash" under it, or a screen cleared, is then the
// name's doing. Such a name is shown in base64, as one that is not UTF-8 is, and as an alert shows it (specs.md §23).
// The human output of scan, status, names check, guard list and the disk commands wrote it as it is.
func TestANameWritesNoLineOfItsOwn(t *testing.T) {
	const evil = "a\n  moved to the trash: every film\x1b[2J\r"
	texts := humanTexts(t)
	texts["a server's answer"] = func(name string) string { return shownText(name) + "\n" }
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

// A name that holds a format character, which reorders what follows it or hides itself, is shown in base64 in every
// human output, as one with a control character is (G16, the maintainer's choice of 2026-11-22): written as it is,
// "Films/Trailer", U+202E RIGHT-TO-LEFT OVERRIDE, then "vkm.exe" reads as Films/Trailerexe.mkv. Its --json output
// keeps it in path, as format version 1 has it (specs.md §29.3).
func TestANameThatReordersTextIsShownInBase64(t *testing.T) {
	const (
		override = "Films/Trailer\xe2\x80\xaevkm.exe"
		shown    = "(base64) RmlsbXMvVHJhaWxlcuKArnZrbS5leGU="
	)
	hiding := []string{
		override,
		"a\xe2\x81\xa6b\xe2\x81\xa9c", // U+2066 LEFT-TO-RIGHT ISOLATE, U+2069 POP DIRECTIONAL ISOLATE
		"Films/a\xe2\x80\x8b.mkv",     // U+200B ZERO WIDTH SPACE
		"Photos/a\xe2\x80\x8db.jpg",   // U+200D ZERO WIDTH JOINER
		"\xef\xbb\xbfFilms.mkv",       // U+FEFF ZERO WIDTH NO-BREAK SPACE
		"Docu\xc2\xadmentaires",       // U+00AD SOFT HYPHEN
		"a\U000e0067\U000e007fb.png",  // tag characters, past U+FFFF
	}
	isFormat := func(r rune) bool { return unicode.Is(unicode.Cf, r) }
	for what, text := range humanTexts(t) {
		for _, name := range hiding {
			if got := text(name); strings.ContainsFunc(got, isFormat) {
				t.Errorf("%s: a format character reaches the terminal: %+q", what, got)
			}
		}
		if got := text(override); !strings.Contains(got, shown) {
			t.Errorf("%s: %+q, without %q", what, got, shown)
		}
		// U+2028 LINE SEPARATOR is Zl, U+034F COMBINING GRAPHEME JOINER and U+FE0F VARIATION SELECTOR-16 are Mn: no format
		// character, so as they are.
		for _, name := range []string{"a\xe2\x80\xa8b", "a\xcd\x8fb", "Photos/\xe2\x9d\xa4\xef\xb8\x8f.jpg"} {
			if got := text(name); !strings.Contains(got, name) {
				t.Errorf("%s: %+q, without %+q as it is", what, got, name)
			}
		}
	}
	// A server's answer has each format character replaced, as each control character is.
	if got := shownText("refused: \xe2\x80\xaedetaler"); got != "refused: \xef\xbf\xbddetaler" {
		t.Errorf("%+q", got)
	}
	// --json gives the name in path, its bytes as they are.
	path, b64 := encodePath(override)
	raw, err := json.Marshal(statusFile{Path: path, PathB64: b64})
	if err != nil || b64 != "" || !strings.Contains(string(raw), `"path":"`+override+`"`) {
		t.Errorf("%s, %q, %v", raw, b64, err)
	}
	if decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(shown, "(base64) ")); string(decoded) != override {
		t.Errorf("%q", decoded)
	}
}

// errorTexts gives, for each place a line for a person writes an error, a warning or what a disk's line says of it,
// what it writes for a text: standard error, through report; the problems, warnings and labels of rules check; the
// message of a disk's line of scan, catalog verify, disk list and the daemon's text output.
func errorTexts(t *testing.T) map[string]func(text string) string {
	t.Helper()
	missing := disks.Verdict{State: disks.Missing, Reason: disks.IOError}
	return map[string]func(text string) string{
		"standard error": func(text string) string {
			var b strings.Builder
			report(&b, text)
			return b.String()
		},
		"rules check": func(text string) string {
			var b bytes.Buffer
			out := checkOutput{Problems: []checkProblem{{Message: text}, {Key: "a", Message: text}}, Warnings: []checkWarning{{Key: "a", Message: text}},
				Hidden: []checkHidden{{Rule: text, By: text}}, Unreached: []string{text}}
			if err := printCheck(&b, out); err != nil {
				t.Fatal(err)
			}
			return b.String()
		},
		"scan": func(text string) string {
			var b bytes.Buffer
			reason, message := string(missing.Reason), text
			if err := printScan(&b, scanOutput{Disks: []scanDisk{{ID: "t1", State: string(missing.State), Reason: &reason, Message: &message}}}); err != nil {
				t.Fatal(err)
			}
			return b.String()
		},
		"disk list": func(text string) string {
			var b bytes.Buffer
			v := missing
			v.Message = text
			if err := printDiskList(&b, diskListFrom([]config.Disk{{ID: "t1", Mount: "/mnt/t1", FS: "ext4"}}, []disks.Verdict{v})); err != nil {
				t.Fatal(err)
			}
			return b.String()
		},
		"daemon": func(text string) string {
			var b bytes.Buffer
			v := missing
			v.Message = text
			if err := (&textDaemon{w: &b}).Disks([]jobs.DiskLine{{ID: "t1", Verdict: v}}); err != nil {
				t.Fatal(err)
			}
			return b.String()
		},
		"catalog verify": func(text string) string {
			var b bytes.Buffer
			if err := printVerify(&b, &catalogVerifyOutput{Problems: []string{text}, Disks: []scanDisk{}}, "catalog.db"); err != nil {
				t.Fatal(err)
			}
			return b.String()
		},
	}
}

// An error, a warning, or what a disk's line says of it, may hold a path or an argument as it is, which an error from
// below the CLI writes with %s, or a label of the configuration: each control or format character of it, and each byte
// that is not UTF-8, is escaped, so that the line writes no line of its own and reorders or hides nothing
// (specs.md §25, G16). Written as it was, "Trailer", U+202E RIGHT-TO-LEFT OVERRIDE, then "vkm.exe" read as
// Trailerexe.mkv, and a line feed began a line the error never wrote.
func TestAnErrorIsWrittenEscaped(t *testing.T) {
	const (
		evil = "open /mnt/t1/media/Films/Trailer\xe2\x80\xaevkm.exe\n  moved to the trash: every film\x1b[2J\xff"
		want = `open /mnt/t1/media/Films/Trailer\u{202E}vkm.exe\u{000A}  moved to the trash: every film\u{001B}[2J\x{FF}`
	)
	unshowable := func(r rune) bool { return r != '\n' && unicode.In(r, unicode.Cc, unicode.Cf) }
	for what, text := range errorTexts(t) {
		got, plain := text(evil), text("a")
		if strings.Count(got, "\n") != strings.Count(plain, "\n") {
			t.Errorf("%s: %d lines for an error with a line feed, %d for a plain one: %+q", what, strings.Count(got, "\n"), strings.Count(plain, "\n"), got)
		}
		if strings.ContainsFunc(got, unshowable) || !utf8.ValidString(got) {
			t.Errorf("%s: a control or format character, or a byte that is not UTF-8, reaches the terminal: %+q", what, got)
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s: %+q, without %+q", what, got, want)
		}
		// A plain text, accents and backslashes included, and what %q escaped already, are written as they are.
		for _, plain := range []string{`disque t1 : sa corbeille n'a pas pu être relue`, `Films\Amélie`, `unknown argument "\u202e"`} {
			if got := text(plain); !strings.Contains(got, plain) {
				t.Errorf("%s: %+q, without %+q as it is", what, got, plain)
			}
		}
	}
}

// An error that comes from below the CLI, here the system's, which names the configuration's path as it is, reaches
// standard error escaped, and the exit code is the error's.
func TestAnErrorFromBelowTheCLIIsWrittenEscaped(t *testing.T) {
	file := filepath.Join(t.TempDir(), "Trailer\xe2\x80\xaevkm.toml")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", file, "status"}, &stdout, &stderr); code != exitError {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got := stderr.String()
	if strings.ContainsFunc(got, func(r rune) bool { return unicode.Is(unicode.Cf, r) }) || !strings.Contains(got, `Trailer\u{202E}vkm.toml`) {
		t.Errorf("%+q", got)
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
