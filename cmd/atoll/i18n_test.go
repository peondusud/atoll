package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// translated gives every message the CLI marks for translation: each T("…") of its sources, and what help says of
// each command.
func translated(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "T" || len(call.Args) != 1 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				// A variable holds what it translates, as the guard's answers and help do: those are checked by
				// hand below.
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: %v", fset.Position(lit.Pos()), err)
			}
			keys[text] = fset.Position(lit.Pos()).String()
			return true
		})
	}
	for _, h := range commandHelps() {
		keys[h.does] = "help of " + h.name
	}
	for _, text := range []string{"%s: confirm up to %s\n", "%s: reject up to %s\n", "%s: nothing to confirm\n", "%s: nothing to reject\n"} {
		keys[text] = "guard's answer"
	}
	return keys
}

var verbPattern = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z%]`)

// Every message marked for translation has a French one, with the same verbs in the same order and the same line
// breaks, and the dictionary holds nothing else (specs.md §25).
func TestEveryMessageHasItsFrench(t *testing.T) {
	keys := translated(t)
	for key, where := range keys {
		fr, ok := frenchMessages[key]
		if !ok {
			t.Errorf("%s: no French for %q", where, key)
			continue
		}
		if a, b := verbPattern.FindAllString(key, -1), verbPattern.FindAllString(fr, -1); !slices.Equal(a, b) {
			t.Errorf("%q: verbs %v, the French has %v", key, a, b)
		}
		if strings.Count(key, "\n") != strings.Count(fr, "\n") || strings.HasSuffix(key, "\n") != strings.HasSuffix(fr, "\n") {
			t.Errorf("%q: the French breaks its lines otherwise: %q", key, fr)
		}
		if strings.TrimSpace(fr) == "" {
			t.Errorf("%q: an empty translation", key)
		}
	}
	for key := range frenchMessages {
		if _, ok := keys[key]; !ok {
			t.Errorf("a French message nothing asks for: %q", key)
		}
	}
}

// The first of LC_ALL, LC_MESSAGES and LANG that is set and not empty decides (specs.md §25).
func TestWantsFrench(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{"LANG": "fr_FR.UTF-8"}, true},
		{map[string]string{"LANG": "fr"}, true},
		{map[string]string{"LANG": "en_US.UTF-8"}, false},
		{map[string]string{"LANG": "C"}, false},
		{map[string]string{"LANG": "fr_FR.UTF-8", "LC_MESSAGES": "en_GB.UTF-8"}, false},
		{map[string]string{"LANG": "en_US.UTF-8", "LC_MESSAGES": "fr_CA.UTF-8"}, true},
		{map[string]string{"LC_ALL": "C.UTF-8", "LC_MESSAGES": "fr_FR.UTF-8"}, false},
		{map[string]string{"LC_ALL": "", "LANG": "fr_BE.UTF-8"}, true},
		{map[string]string{"LANG": "french"}, true},
		{map[string]string{"LANG": "FR_fr"}, false},
	} {
		if got := wantsFrench(func(name string) string { return tc.env[name] }); got != tc.want {
			t.Errorf("%v: %v", tc.env, got)
		}
	}
}

func runHelp(args ...string) (code int, stdout, stderr string) {
	var out, errs bytes.Buffer
	code = run(args, &out, &errs)
	return code, out.String(), errs.String()
}

// Help lists every command, and says more of one, wherever --help comes before a "--", whatever else is there; an
// unknown command there is a usage error (specs.md §25).
func TestHelp(t *testing.T) {
	code, out, errs := runHelp("--help")
	if code != exitOK || errs != "" || !strings.Contains(out, "Commands:") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	for _, h := range commandHelps() {
		if !strings.Contains(out, "  "+h.name+" ") {
			t.Errorf("%s is not listed", h.name)
		}
	}
	for _, args := range [][]string{{"-h"}, {"help"}, {"--config", "/nowhere", "--help"}} {
		if code, got, _ := runHelp(args...); code != exitOK || got != out {
			t.Errorf("%v: %d", args, code)
		}
	}
	trash := "atoll trash list [--json]\n"
	for _, args := range [][]string{
		{"help", "trash"}, {"trash", "--help"}, {"trash", "purge", "--help"}, {"trash", "restore", "x", "-h", "--yes"},
		{"--config=/nowhere", "trash", "--bogus", "--help"},
	} {
		if code, got, errs := runHelp(args...); code != exitOK || !strings.HasPrefix(got, trash) || !strings.Contains(got, "\nlist the trash") || errs != "" {
			t.Errorf("%v: %d %q %q", args, code, got, errs)
		}
	}
	for _, args := range [][]string{{"help", "nosuch"}, {"help", "trash", "list"}, {"nosuch", "--help"}} {
		if code, got, _ := runHelp(args...); code != exitUsage || got != "" {
			t.Errorf("%v: %d %q", args, code, got)
		}
	}
	// An unknown subcommand is a usage error there too: its command's help was written, with exit 0.
	for _, args := range [][]string{{"trash", "bogus", "--help"}, {"disk", "bogus", "-h"}, {"catalog", "verifi", "--help", "--json"}} {
		if code, got, errs := runHelp(args...); code != exitUsage || got != "" || !strings.Contains(errs, args[1]) {
			t.Errorf("%v: %d %q %q", args, code, got, errs)
		}
	}
	// A command with no subcommand takes whatever its arguments hold.
	for _, args := range [][]string{{"scan", "bogus", "--help"}, {"daemon", "--help"}, {"status", "x", "y", "-h"}} {
		if code, got, errs := runHelp(args...); code != exitOK || !strings.HasPrefix(got, "atoll "+args[0]+" ") || errs != "" {
			t.Errorf("%v: %d %q %q", args, code, got, errs)
		}
	}
	// After "--", --help is a path.
	if code, got, _ := runHelp("--config", "/nowhere", "trash", "restore", "--", "--help"); code == exitOK || got != "" {
		t.Errorf("a path named --help: %d %q", code, got)
	}
}

// With French asked for, what the CLI writes for people is in French, and its words for scripts are not (specs.md
// §25).
func TestTheCLIInFrench(t *testing.T) {
	french = true
	t.Cleanup(func() { french = false })
	code, out, _ := runHelp("--help")
	if code != exitOK || !strings.Contains(out, "Commandes :") || !strings.Contains(out, "  trash ") {
		t.Errorf("%d %q", code, out)
	}
	if code, _, errs := runHelp("trash"); code != exitUsage || errs != "atoll: trash : list, restore ou purge attendu\n" {
		t.Errorf("%d %q", code, errs)
	}
	if code, _, errs := runHelp("--config", filepath.Join(t.TempDir(), "atoll.toml"), "trash", "purge", "--dry-run", "--yes"); code != exitUsage ||
		!strings.Contains(errs, "--dry-run et --yes ne vont pas ensemble") {
		t.Errorf("%d %q", code, errs)
	}
}

// A time in the human output of rules explain is written as the configuration writes it (specs.md §25).
func TestRulesExplainWritesDurations(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "atoll.toml")
	if err := os.WriteFile(config, []byte("pool_id = \"p\"\n[view]\nmount = \"/srv/pool\"\n[defaults]\nsettle = \"90m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runHelp("--config", config, "rules", "explain", "Films/a.mkv")
	if code != exitOK || !strings.Contains(out, "\nsettle = 90m (") || !strings.Contains(out, "\ncache_min_age = 7d (") || strings.Contains(out, "_ns") {
		t.Errorf("%d %q %q", code, out, errs)
	}
}
