package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// atollAt runs a command with the configuration at a path of the test's, and gives its exit code and what it wrote.
func atollAt(configPath string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"--config", configPath}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// atoll init writes a configuration from its flags, with the names quoted as TOML wants them, and the catalog; run
// again it keeps both; with flags once the configuration exists, or without them before, it refuses (specs.md §25).
func TestInitWritesWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "etc", "atoll.toml")
	if code, _, stderr := atollAt(file, "init"); code != exitError || !strings.Contains(stderr, "--pool-id and --view-mount") {
		t.Errorf("init with nothing: %d, %s", code, stderr)
	}
	catalog := filepath.Join(dir, "a \"catalog\"\\.db")
	code, stdout, stderr := atollAt(file, "init", "--pool-id", "p", "--view-mount="+filepath.Join(dir, "pool"), "--catalog", catalog, "--json")
	if code != exitOK || !strings.Contains(stdout, `"configuration": "created"`) || !strings.Contains(stdout, `"catalog": "created"`) {
		t.Fatalf("init: %d\n%s%s", code, stdout, stderr)
	}
	if _, err := os.Stat(catalog); err != nil {
		t.Errorf("the catalog: %v", err)
	}
	text, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), `catalog = "`+strings.ReplaceAll(strings.ReplaceAll(catalog, `\`, `\\`), `"`, `\"`)+`"`) {
		t.Errorf("the configuration:\n%s", text)
	}
	if code, stdout, _ := atollAt(file, "init"); code != exitOK || stdout != "configuration: kept\ncatalog: kept\n" {
		t.Errorf("init again: %d, %q", code, stdout)
	}
	if code, _, stderr := atollAt(file, "init", "--pool-id", "q", "--view-mount", "/srv/q"); code != exitError || !strings.Contains(stderr, "already exists") {
		t.Errorf("init with flags over a configuration: %d, %s", code, stderr)
	}
	// What the flags refuse, before anything is read.
	for _, args := range [][]string{
		{"init", "--pool-id", "p"}, {"init", "--catalog", "c"}, {"init", "--pool-id"}, {"init", "--pool-id", "p", "--pool-id", "q", "--view-mount", "/v"},
		{"init", "--json", "--json"}, {"init", "--bogus"}, {"init", "--pool-id", "", "--view-mount", "/v"},
	} {
		if code, _, _ := atollAt(filepath.Join(dir, "new.toml"), args...); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if got := tomlString("a\x01\x7f\"\\"); got != `"a\u0001\u007F\"\\"` {
		t.Errorf("tomlString: %s", got)
	}
}

// What the rules give, checked and explained, in text and in JSON, with a rule that ignores, one that asks three copies,
// and one an earlier rule hides whole (specs.md §11, §25).
func TestTheRulesCheckedAndExplained(t *testing.T) {
	b := newPoolBench(t, 2, "[[rule]]\nlabel = \"samples\"\npath = \"**/Sample/**\"\naction = \"ignore\"\n"+
		"[[rule]]\nlabel = \"films\"\npath = \"Films/**\"\nreplicas = 2\n"+
		"[[rule]]\nlabel = \"hidden\"\npath = \"Films/Action/**\"\nreplicas = 1\n")
	b.write(1, "Films/a.mkv", "a film")
	b.write(1, "Films/Sample/s.mkv", "a sample")
	b.ok(exitOK, "init")
	b.ok(exitOK, "scan")
	out := b.ok(exitOK, "rules", "check")
	t.Logf("rules check:\n%s", out)
	if !strings.Contains(out, "hidden") {
		t.Errorf("rules check says nothing of the hidden rule:\n%s", out)
	}
	b.json(exitOK, "rules", "check")
	for _, path := range []string{"Films/a.mkv", "Films/Sample/s.mkv", "Series/x.mkv"} {
		out := b.ok(exitOK, "rules", "explain", path)
		t.Logf("rules explain %s:\n%s", path, out)
		b.json(exitOK, "rules", "explain", path, "--size", "10MB")
	}
	if code, _, stderr := b.atoll("rules", "explain", "/abs"); code != exitUsage {
		t.Errorf("an absolute path: %d, %s", code, stderr)
	}
}

// Two files that hold the same bytes under different paths are listed as duplicates, in text and in JSON, a path and a
// least size keeping part of them (specs.md §16, §25).
func TestDuplicates(t *testing.T) {
	b := newPoolBench(t, 2, "")
	b.write(1, "Films/a.mkv", "the same film")
	b.write(2, "Films/Old/a copy.mkv", "the same film")
	b.write(1, "Films/b.mkv", "another")
	b.ok(exitOK, "init")
	b.ok(exitOK, "scan", "--full")
	out := b.ok(exitOK, "dupes")
	if !strings.Contains(out, "Films/a.mkv") || !strings.Contains(out, "Films/Old/a copy.mkv") || strings.Contains(out, "b.mkv") {
		t.Errorf("dupes:\n%s", out)
	}
	got := b.json(exitOK, "dupes", "--path", "Films/Old")
	t.Logf("dupes --path: %v", got)
	if out := b.ok(exitOK, "dupes", "--min-size", "1MB"); strings.Contains(out, "a.mkv") {
		t.Errorf("dupes past 1 MB:\n%s", out)
	}
}
