package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

// polkitPool writes a configuration of the disks given, each a mount point and its [[disk]] lines past fs, and gives
// its path. Its catalog is in a folder that is not there, which the command must not make.
func polkitPool(t *testing.T, disks ...[2]string) (config, catalogFile string) {
	t.Helper()
	dir := t.TempDir()
	config = filepath.Join(dir, "atoll.toml")
	catalogFile = filepath.Join(dir, "nowhere", "catalog.db")
	text := "pool_id = \"home-media\"\ncatalog = " + strconv.Quote(catalogFile) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n"
	uuids := []string{"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b", "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d", "9b2e4d6f-1a3c-4e5f-8a7b-6c5d4e3f2a1b"}
	for i, d := range disks {
		text += "[[disk]]\nid = \"t" + strconv.Itoa(i+1) + "\"\nuuid = \"" + uuids[i] + "\"\nmount = " + strconv.Quote(d[0]) + "\nfs = \"ext4\"\n" + d[1]
	}
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return config, catalogFile
}

// TestPolkitRulePrintsTheRule: without --json, the command prints the rule of --json and nothing else, so that it can
// be written to /etc/polkit-1/rules.d/ as it is; the same in French, as it is a file to install; it reads the
// configuration alone, and makes no catalog; and it says when it cannot write (specs.md §25). The scenarios judge only
// --json.
func TestPolkitRulePrintsTheRule(t *testing.T) {
	config, catalogFile := polkitPool(t, [2]string{"/srv/dev-disk-by-uuid-1234-ABCD", ""}, [2]string{"/mnt/disk2", "state = \"failed\"\n"},
		[2]string{"/mnt/disk3", "state = \"draining\"\n"})
	var asJSON, stderr bytes.Buffer
	if code := run([]string{"--config", config, "polkit-rule", "--json"}, &asJSON, &stderr); code != exitOK || stderr.Len() != 0 {
		t.Fatalf("--json: exit %d: %s", code, stderr.String())
	}
	var out polkitOutput
	if err := json.Unmarshal(asJSON.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	want := []polkitUnit{{"t1", `srv-dev\x2ddisk\x2dby\x2duuid\x2d1234\x2dABCD.mount`}, {"t3", "mnt-disk3.mount"}}
	if len(out.Units) != len(want) || out.Units[0] != want[0] || out.Units[1] != want[1] {
		t.Errorf("units %v, want %v", out.Units, want)
	}
	if len(out.LeftOut) != 1 || out.LeftOut[0] != (polkitLeftOut{"t2", polkitFailed}) || out.Account != "atoll" {
		t.Errorf("left out %v, account %q", out.LeftOut, out.Account)
	}
	if !strings.Contains(out.Rule, "\t\t\"srv-dev\\\\x2ddisk\\\\x2dby\\\\x2duuid\\\\x2d1234\\\\x2dABCD.mount\", // t1\n") {
		t.Errorf("the rule does not name t1's unit with its backslashes doubled:\n%s", out.Rule)
	}
	for _, french := range []bool{false, true} {
		setFrench(t, french)
		var plain bytes.Buffer
		stderr.Reset()
		if code := run([]string{"--config", config, "polkit-rule"}, &plain, &stderr); code != exitOK || stderr.Len() != 0 {
			t.Fatalf("French %v: exit %d: %s", french, code, stderr.String())
		}
		if plain.String() != out.Rule {
			t.Errorf("French %v: printed %q, the rule of --json %q", french, plain.String(), out.Rule)
		}
	}
	setFrench(t, false)
	if _, err := os.Stat(filepath.Dir(catalogFile)); !os.IsNotExist(err) {
		t.Errorf("the catalog's folder: %v, want none", err)
	}
	stderr.Reset()
	if code := run([]string{"--config", config, "polkit-rule"}, brokenWriter{}, &stderr); code != exitError || !strings.Contains(stderr.String(), "no room") {
		t.Errorf("on a broken output: exit %d, %q", code, stderr.String())
	}
}

func setFrench(t *testing.T, on bool) {
	t.Helper()
	french = on
	t.Cleanup(func() { french = false })
}

// A disk in service whose unit Atoll cannot name is left out, as the daemon mounts it again itself: the rule is
// printed all the same, standard error names the disk, and the command exits with 4. A failed disk left out is no
// such thing.
func TestPolkitRuleLeavesOutAUnitWithNoName(t *testing.T) {
	config, _ := polkitPool(t, [2]string{"/mnt/" + strings.Repeat("a", 300), ""}, [2]string{"/mnt/disk2", "state = \"failed\"\n"})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", config, "polkit-rule"}, &stdout, &stderr); code != exitPartial {
		t.Fatalf("exit %d, want %d: %s", code, exitPartial, stderr.String())
	}
	if !strings.Contains(stdout.String(), "\tvar units = [];\n\t// Left out: t1 (no-unit-name), t2 (failed).\n") {
		t.Errorf("the rule:\n%s", stdout.String())
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, `disk "t1"`) {
		t.Errorf("standard error %q", got)
	}
}

// The rule as polkitd runs it: one action, the account atoll, the verb start; a unit of the list allowed, any other
// mount unit refused, the rest left to the rules after it, deploy/50-atoll.rules among them. It is ASCII, and every
// line of code is outside a comment: no name of a disk or of the pool can end one. Measured by polkitd in qemu
// (test/loopdev/units_test.go, TestThePoolPolkitRuleUnderSystemd).
func TestPolkitRuleAllowsTheListAloneAndRefusesOtherMounts(t *testing.T) {
	config, _ := polkitPool(t, [2]string{"/mnt/disk1", ""})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", config, "polkit-rule"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	rule := stdout.String()
	for i := range len(rule) {
		if c := rule[i]; (c < 0x20 || c >= 0x7f) && c != '\n' && c != '\t' {
			t.Fatalf("byte %#x at %d: the rule is not ASCII", c, i)
		}
	}
	var code []string
	for _, line := range strings.Split(rule, "\n") {
		if text, _, _ := strings.Cut(line, " // "); !strings.HasPrefix(strings.TrimSpace(text), "//") {
			code = append(code, text)
		}
	}
	program := strings.Join(code, "\n")
	for _, want := range []string{"polkit.addRule(function (action, subject) {\n", "\tvar units = [\n\t\t\"mnt-disk1.mount\"\n\t];\n",
		`action.id !== "org.freedesktop.systemd1.manage-units" || subject.user !== "atoll" ||`, `action.lookup("verb") !== "start") {`,
		"\t\treturn polkit.Result.NOT_HANDLED;\n\t}\n\tvar unit = action.lookup(\"unit\") || \"\";\n",
		"\tif (units.indexOf(unit) >= 0) {\n\t\treturn polkit.Result.YES;\n\t}\n",
		"\tif (/\\.mount$/.test(unit)) {\n\t\treturn polkit.Result.NO;\n\t}\n\treturn polkit.Result.NOT_HANDLED;\n});"} {
		if !strings.Contains(program, want) {
			t.Errorf("the rule has no %q:\n%s", want, program)
		}
	}
	for word, n := range map[string]int{"polkit.Result.YES": 1, "polkit.Result.NO;": 1, "action.lookup(\"verb\")": 1, "addRule": 1} {
		if got := strings.Count(program, word); got != n {
			t.Errorf("%s %d times, want %d", word, got, n)
		}
	}
}

// Each name of a mount unit is written as a JavaScript string that reads back as the name: its backslashes doubled,
// as systemd's \x2d would read "-" in JavaScript, and nothing but printable ASCII. JSON reads those literals as
// JavaScript does (specs.md §15.13).
func TestJSStringOfEachMountUnit(t *testing.T) {
	type vector struct {
		Path  string `json:"path"`
		Unit  string `json:"unit"`
		Error bool   `json:"error"`
		Note  string `json:"note"`
	}
	for _, v := range vectors.Load[vector](t, "mount_unit.json") {
		if v.Error {
			continue
		}
		literal := jsString(v.Unit)
		var back string
		if err := json.Unmarshal([]byte(literal), &back); err != nil || back != v.Unit {
			t.Errorf("%q: %s reads %q, %v", v.Unit, literal, back, err)
		}
		if strings.Count(literal, `\`) != 2*strings.Count(v.Unit, `\`) {
			t.Errorf("%q: %s", v.Unit, literal)
		}
	}
	for text, want := range map[string]string{
		`a"b`:            `"a\"b"`,
		"a\x01b\x7f":     `"a\u0001b\u007f"`,
		"\u2028":         `"\u00e2\u0080\u00a8"`,
		`srv-dev\x2dmnt`: `"srv-dev\\x2dmnt"`,
		"":               `""`,
	} {
		if got := jsString(text); got != want {
			t.Errorf("%q: %s, want %s", text, got, want)
		}
	}
}
