package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A runner that passes everything judges nothing, so these give it a fake binary, a shell
// script, and require it to fail where it must.
func fakeBinary(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-atoll")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func scenarioFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type verdictCase struct {
	note   string
	script string
	body   string
	fails  string // what the failure says; empty when the scenario must pass
}

func checkVerdicts(t *testing.T, cases []verdictCase) {
	t.Helper()
	for _, c := range cases {
		err := runFile(fakeBinary(t, c.script), scenarioFile(t, c.body))
		switch {
		case c.fails == "" && err != nil:
			t.Errorf("%s: %v", c.note, err)
		case c.fails != "" && err == nil:
			t.Errorf("%s: passed, want a failure about %q", c.note, c.fails)
		case c.fails != "" && !strings.Contains(err.Error(), c.fails):
			t.Errorf("%s: %v, want a failure about %q", c.note, err, c.fails)
		}
	}
}

// steps makes a scenario of a list of steps.
func steps(list string) string {
	return `{"format_version": 1, "steps": [` + list + `]}`
}

func TestComparingOutput(t *testing.T) {
	const prints = `echo '{"valid": true, "count": 3, "list": ["a", "b"], "message": "any words"}'`
	expect := func(want string) string { return steps(`{"do": "atoll", "stdout_json": ` + want + `}`) }
	checkVerdicts(t, []verdictCase{
		{"it matches", prints, expect(`{"valid": true, "count": 3, "list": ["a", "b"], "message": "${ANY}"}`), ""},
		{"a boolean differs", prints, expect(`{"valid": false, "count": 3, "list": ["a", "b"], "message": "${ANY}"}`), "$.valid"},
		{"an integer differs", prints, expect(`{"valid": true, "count": 4, "list": ["a", "b"], "message": "${ANY}"}`), "$.count"},
		{"3.0 is not 3", prints, expect(`{"valid": true, "count": 3.0, "list": ["a", "b"], "message": "${ANY}"}`), "$.count"},
		{"a key too many in the output", prints, expect(`{"valid": true, "count": 3, "list": ["a", "b"]}`), "unexpected key"},
		{"a key missing from the output", prints, expect(`{"valid": true, "count": 3, "list": ["a", "b"], "message": "${ANY}", "more": 1}`), `no key "more"`},
		{"a list in another order", prints, expect(`{"valid": true, "count": 3, "list": ["b", "a"], "message": "${ANY}"}`), "$.list[0]"},
		{"a list too long", prints, expect(`{"valid": true, "count": 3, "list": ["a"], "message": "${ANY}"}`), "2 items, want 1"},
		{"a string is compared", prints, expect(`{"valid": true, "count": 3, "list": ["a", "b"], "message": "other"}`), "$.message"},
		{"a string for an object", `echo '"x"'`, expect(`{"a": 1}`), "want an object"},
		{"a string for a list", `echo '"x"'`, expect(`[1]`), "want a list"},
		{"a number for a string", `echo '1'`, expect(`"x"`), "want a string"},
		{"${ANY} is for strings", `echo '1'`, expect(`"${ANY}"`), "want a string"},
		{"null is a value: expected", `echo 'null'`, expect(`null`), ""},
		{"null is a value: not expected", `echo '{}'`, expect(`null`), "$:"},
		{"null is a value: not given", `echo 'null'`, expect(`{}`), "want an object"},
		{"nothing printed", `true`, expect(`{}`), "not JSON"},
		{"not JSON", `echo 'hello'`, expect(`{}`), "not JSON"},
		{"two JSON values", `echo '{} {}'`, expect(`{}`), "not JSON"},
		{"no stdout_json: the output is not read", `echo 'hello'`, steps(`{"do": "atoll"}`), ""},
		{"another exit code", "exit 3", steps(`{"do": "atoll"}`), "exit code 3"},
		{"the expected exit code", "exit 3", steps(`{"do": "atoll", "exit": 3}`), ""},
		{"exit 0 when 1 is wanted", "exit 0", steps(`{"do": "atoll", "exit": 1}`), "exit code 0"},
		{"the sandbox in what is expected", `printf '{"p": "%s"}' "$PWD/y"`, expect(`{"p": "${SANDBOX}/y"}`), ""},
		{"the sandbox in what is expected: not it", `echo '{"p": "/other/y"}'`, expect(`{"p": "${SANDBOX}/y"}`), "$.p"},
	})
}

func TestRunningTheBinary(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"the sandbox in the arguments", `[ "$1" = "$PWD/x" ]`, steps(`{"do": "atoll", "args": ["${SANDBOX}/x"]}`), ""},
		{"bytes in an argument", `[ "$1" = "$(printf 'a\377')" ]`, steps(`{"do": "atoll", "args": [{"b64": "Yf8="}]}`), ""},
		{"an argument that is neither", "exit 0", steps(`{"do": "atoll", "args": [{"bytes": "x"}]}`), "argument"},
		{"an argument with no bytes", "exit 0", steps(`{"do": "atoll", "args": [{}]}`), "want a string or b64"},
		{"bad base64 in an argument", "exit 0", steps(`{"do": "atoll", "args": [{"b64": "*"}]}`), "argument"},
		{"no config: no flag", `[ $# -eq 0 ]`, steps(`{"do": "atoll"}`), ""},
		{"a config: its path first", `[ "$1" = "--config" ] && [ "$2" = "$PWD/atoll.toml" ] && [ "$3" = "x" ] && grep -q "$PWD/c" "$2"`, `{"format_version": 1, "config": "a = \"${SANDBOX}/c\"\n", "steps": [{"do": "atoll", "args": ["x"]}]}`, ""},
		{"the control file in the environment", `[ "$ATOLL_CONFORMANCE" = "$PWD/conformance.json" ]`, steps(`{"do": "atoll"}`), ""},
		{"a set time zone", `[ "$TZ" = "UTC" ]`, steps(`{"do": "atoll"}`), ""},
		{"nothing on standard input", `[ "$(cat)" = "" ]`, steps(`{"do": "atoll"}`), ""},
		{"it runs in the sandbox", `[ -f conformance.json ]`, steps(`{"do": "atoll"}`), ""},
		{"the default clock", `grep -q 2026-01-01T00:00:00Z "$ATOLL_CONFORMANCE"`, steps(`{"do": "atoll"}`), ""},
		{"advance moves the clock", `grep -q 2026-10-26T01:10:00Z "$ATOLL_CONFORMANCE"`, `{"format_version": 1, "clock": "2026-10-26T01:00:00Z", "steps": [{"do": "advance", "by": "10m"}, {"do": "atoll"}]}`, ""},
		{"a bad duration", "exit 0", steps(`{"do": "advance", "by": "soon"}`), "duration"},
		{"a bad clock", "exit 0", `{"format_version": 1, "clock": "tonight", "steps": [{"do": "atoll"}]}`, "clock"},
	})
}

func TestRefusingAScenario(t *testing.T) {
	file := `{"do": "expect_tree", "disk": "d", "tree": [`
	checkVerdicts(t, []verdictCase{
		{"an unknown field", "exit 0", `{"format_version": 1, "stepz": [], "steps": [{"do": "atoll"}]}`, "unknown field"},
		{"an unknown field in a step", "exit 0", steps(`{"do": "atoll", "stdout": 1}`), "unknown field"},
		{"an unknown field in an entry", "exit 0", steps(file + `{"path": "a", "txt": "x"}]}`), "unknown field"},
		{"a field on the wrong step", "exit 0", steps(`{"do": "atoll", "tree": []}`), "does not apply"},
		{"a field on the wrong step: advance", "exit 0", steps(`{"do": "advance", "by": "1m", "exit": 1}`), "does not apply"},
		{"a field on the wrong step: expect_tree", "exit 0", steps(`{"do": "expect_tree", "disk": "d", "tree": [], "path": "a"}`), "does not apply"},
		{"a field on the wrong step: delete", "exit 0", steps(`{"do": "delete", "disk": "d", "path": "a", "text": "x"}`), "does not apply"},
		{"a field on the wrong step: mkdir", "exit 0", steps(`{"do": "mkdir", "disk": "d", "path": "a", "size": 1}`), "does not apply"},
		{"a field on the wrong step: rename", "exit 0", steps(`{"do": "rename", "disk": "d", "path": "a", "to": "b", "target": "c"}`), "does not apply"},
		{"a field on the wrong step: symlink", "exit 0", steps(`{"do": "symlink", "disk": "d", "path": "a", "target": "b", "to": "c"}`), "does not apply"},
		{"a field on the wrong step: touch", "exit 0", steps(`{"do": "touch", "disk": "d", "path": "a", "mtime_ns": 1, "by": "1m"}`), "does not apply"},
		{"a field on the wrong step: write", "exit 0", steps(`{"do": "write", "disk": "d", "path": "a", "text": "x", "exit": 1}`), "does not apply"},
		{"another format version", "exit 0", `{"format_version": 2, "steps": [{"do": "atoll"}]}`, "format_version"},
		{"no format version", "exit 0", `{"steps": [{"do": "atoll"}]}`, "format_version"},
		{"no steps", "exit 0", steps(""), "no steps"},
		{"a step it does not know", "exit 0", steps(`{"do": "crash"}`), "does not know"},
		{"a step with no do", "exit 0", steps(`{}`), "no do"},
		{"a do in other case", "exit 0", steps(`{"DO": "atoll"}`), "no do"},
		{"trailing data", "exit 0", steps(`{"do": "atoll"}`) + ` {}`, "trailing"},
		{"nothing runs before a later bad step", `exit 9`, steps(`{"do": "atoll"}, {"do": "crash"}`), "does not know"},
		{"no tree", "exit 0", steps(`{"do": "expect_tree", "disk": "d"}`), "no tree"},
		{"a null tree", "exit 0", steps(`{"do": "expect_tree", "disk": "d", "tree": null}`), "no tree"},
		{"touch with no time", "exit 0", steps(`{"do": "touch", "disk": "d", "path": "a"}`), "no mtime_ns"},
		{"a symlink step with no target", "exit 0", steps(`{"do": "symlink", "disk": "d", "path": "a"}`), "no target"},
		{"write with no content", "exit 0", steps(`{"do": "write", "disk": "d", "path": "a"}`), "no content"},
		{"no content expected", "exit 0", steps(file + `{"path": "a"}]}`), "no content"},
		{"both contents", "exit 0", steps(file + `{"path": "a", "text": "x", "size": 1}]}`), "both"},
		{"a negative size", "exit 0", steps(file + `{"path": "a", "size": -1}]}`), "size -1"},
		{"a file with a target", "exit 0", steps(file + `{"path": "a", "text": "x", "target": "b"}]}`), "no target"},
		{"a folder with content", "exit 0", steps(file + `{"path": "a", "kind": "dir", "text": "x"}]}`), "only a path"},
		{"a folder with a size", "exit 0", steps(file + `{"path": "a", "kind": "dir", "size": 1}]}`), "only a path"},
		{"a folder with a target", "exit 0", steps(file + `{"path": "a", "kind": "dir", "target": "x"}]}`), "only a path"},
		{"a folder with a time", "exit 0", steps(file + `{"path": "a", "kind": "dir", "mtime_ns": 1}]}`), "only a path"},
		{"a symlink with content", "exit 0", steps(file + `{"path": "a", "kind": "symlink", "target": "b", "text": "x"}]}`), "only a path and a target"},
		{"a symlink with a size", "exit 0", steps(file + `{"path": "a", "kind": "symlink", "target": "b", "size": 1}]}`), "only a path and a target"},
		{"a symlink with a time", "exit 0", steps(file + `{"path": "a", "kind": "symlink", "target": "b", "mtime_ns": 1}]}`), "only a path and a target"},
		{"a symlink with no target", "exit 0", steps(file + `{"path": "a", "kind": "symlink"}]}`), "no target"},
		{"an unknown kind", "exit 0", steps(file + `{"path": "a", "kind": "fifo"}]}`), "unknown kind"},
		{"a bad entry in a disk", "exit 0", `{"format_version": 1, "disks": [{"id": "d", "uuid": "u", "fs": "ext4", "capacity": 10, "tree": [{"path": "a"}]}], "steps": [{"do": "atoll"}]}`, "no content"},
	})
}

func TestComparingATree(t *testing.T) {
	onDisk := func(list string) string {
		return `{"format_version": 1, "disks": [{"id": "d", "uuid": "u", "fs": "ext4", "capacity": 10, "tree": [{"path": "media/a", "text": "x"}]}], "steps": [` + list + `]}`
	}
	expect := func(tree string) string { return `{"do": "expect_tree", "disk": "d", "tree": [` + tree + `]}` }
	const a = `{"path": "media/a", "text": "x"}, `
	checkVerdicts(t, []verdictCase{
		{"it matches", "exit 0", onDisk(expect(`{"path": "media/a", "text": "x"}`)), ""},
		{"an unknown disk", "exit 0", onDisk(`{"do": "expect_tree", "disk": "e", "tree": []}`), "unknown disk"},
		{"a file missing from the disk", "exit 0", onDisk(expect(a + `{"path": "media/b", "text": "y"}`)), "not on the disk"},
		{"a file not expected", "exit 0", onDisk(expect("")), "not expected"},
		{"other content", "exit 0", onDisk(expect(`{"path": "media/a", "text": "y"}`)), "other content"},
		{"a kind that differs", "exit 0", onDisk(expect(`{"path": "media/a", "kind": "dir"}`)), "want a dir"},
		{"a folder for a file", "exit 0", onDisk(`{"do": "mkdir", "disk": "d", "path": "media/e"}, ` + expect(a+`{"path": "media/e", "text": "x"}`)), "want a file"},
		{"an empty folder must be listed", "exit 0", onDisk(`{"do": "mkdir", "disk": "d", "path": "media/e"}, ` + expect(`{"path": "media/a", "text": "x"}`)), "not expected"},
		{"and passes when it is", "exit 0", onDisk(`{"do": "mkdir", "disk": "d", "path": "media/e"}, ` + expect(a+`{"path": "media/e", "kind": "dir"}`)), ""},
		{"a folder may be listed", "exit 0", onDisk(expect(a + `{"path": "media", "kind": "dir"}`)), ""},
		{"a * matches one name", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "Atoll/trash/d/42/a", "text": "t"}, ` + expect(a+`{"path": "Atoll/trash/d/*/a", "text": "t"}`)), ""},
		{"a * is one name only", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "Atoll/trash/d/4/2/a", "text": "t"}, ` + expect(a+`{"path": "Atoll/trash/d/*/a", "text": "t"}`)), "not on the disk"},
		{"a * is a whole component", "exit 0", onDisk(expect(`{"path": "media/*a", "text": "x"}`)), "not on the disk"},
		{"a * compares the rest", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "Atoll/trash/d/42/a", "text": "t"}, ` + expect(a+`{"path": "Atoll/trash/e/*/a", "text": "t"}`)), "not on the disk"},
		{"a * does not reach below its name", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "media/sub/f", "text": "t"}, ` + expect(a+`{"path": "media/*", "text": "t"}`)), "want a file"},
		{"a * compares the content", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "Atoll/trash/d/42/a", "text": "t"}, ` + expect(a+`{"path": "Atoll/trash/d/*/a", "text": "u"}`)), "other content"},
		{"one * entry takes one file", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "Atoll/trash/d/1/a", "text": "t"}, {"do": "write", "disk": "d", "path": "Atoll/trash/d/2/a", "text": "t"}, ` + expect(a+`{"path": "Atoll/trash/d/*/a", "text": "t"}`)), "not expected"},
		{"two * entries take two files", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "Atoll/trash/d/1/a", "text": "t"}, {"do": "write", "disk": "d", "path": "Atoll/trash/d/2/a", "text": "t"}, ` + expect(a+`{"path": "Atoll/trash/d/*/a", "text": "t"}, {"path": "Atoll/trash/d/*/a", "text": "t"}`)), ""},
		{"two * entries, in the other order", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "Atoll/trash/d/1/a", "text": "t"}, {"do": "write", "disk": "d", "path": "Atoll/trash/d/2/a", "text": "u"}, ` + expect(a+`{"path": "Atoll/trash/d/*/a", "text": "u"}, {"path": "Atoll/trash/d/*/a", "text": "t"}`)), ""},
		{"size means the pattern", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "media/b", "size": 300}, ` + expect(a+`{"path": "media/b", "size": 300}`)), ""},
		{"size differs", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "media/b", "size": 300}, ` + expect(a+`{"path": "media/b", "size": 299}`)), "other content"},
		{"the pattern wraps at 251", `[ "$(od -An -tu1 -j251 -N1 "$PWD/disks/d/media/b" | tr -d ' ')" = 0 ] && [ "$(od -An -tu1 -j250 -N1 "$PWD/disks/d/media/b" | tr -d ' ')" = 250 ]`, onDisk(`{"do": "write", "disk": "d", "path": "media/b", "size": 300}, {"do": "atoll"}`), ""},
		{"write replaces content in place", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "media/a", "text": "new"}, ` + expect(`{"path": "media/a", "text": "new"}`)), ""},
		{"write with a time", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "media/b", "text": "t", "mtime_ns": 1000000000}, ` + expect(a+`{"path": "media/b", "text": "t", "mtime_ns": 1000000000}`)), ""},
		{"rename", "exit 0", onDisk(`{"do": "rename", "disk": "d", "path": "media/a", "to": "media/b"}, ` + expect(`{"path": "media/b", "text": "x"}`)), ""},
		{"rename to bytes", "exit 0", onDisk(`{"do": "rename", "disk": "d", "path": "media/a", "to_b64": "bWVkaWEvYf8="}, ` + expect(`{"path_b64": "bWVkaWEvYf8=", "text": "x"}`)), ""},
		{"rename with no target", "exit 0", onDisk(`{"do": "rename", "disk": "d", "path": "media/a"}`), "to: no path"},
		{"rename out of the disk", "exit 0", onDisk(`{"do": "rename", "disk": "d", "path": "media/a", "to": "../x"}`), "clean path"},
		{"delete", "exit 0", onDisk(`{"do": "delete", "disk": "d", "path": "media/a"}, ` + expect(`{"path": "media", "kind": "dir"}`)), ""},
		{"delete what is not there", "exit 0", onDisk(`{"do": "delete", "disk": "d", "path": "media/z"}`), "no such file"},
		{"delete a folder that holds a file", "exit 0", onDisk(`{"do": "delete", "disk": "d", "path": "media"}`), "not empty"},
		{"a symlink", "exit 0", onDisk(`{"do": "symlink", "disk": "d", "path": "media/l", "target": "a"}, ` + expect(a+`{"path": "media/l", "kind": "symlink", "target": "a"}`)), ""},
		{"a symlink to elsewhere", "exit 0", onDisk(`{"do": "symlink", "disk": "d", "path": "media/l", "target": "a"}, ` + expect(a+`{"path": "media/l", "kind": "symlink", "target": "b"}`)), "target"},
		{"a symlink is not followed", "exit 0", onDisk(`{"do": "symlink", "disk": "d", "path": "media/l", "target": "a"}, ` + expect(a+`{"path": "media/l", "text": "x"}`)), "want a file"},
		{"touch and mtime", "exit 0", onDisk(`{"do": "touch", "disk": "d", "path": "media/a", "mtime_ns": 1000000000}, ` + expect(`{"path": "media/a", "text": "x", "mtime_ns": 1000000000}`)), ""},
		{"another mtime", "exit 0", onDisk(`{"do": "touch", "disk": "d", "path": "media/a", "mtime_ns": 1000000000}, ` + expect(`{"path": "media/a", "text": "x", "mtime_ns": 2000000000}`)), "mtime"},
		{"a path outside the disk", "exit 0", onDisk(`{"do": "delete", "disk": "d", "path": "../x"}`), "clean path"},
		{"an absolute path", "exit 0", onDisk(`{"do": "delete", "disk": "d", "path": "/etc/x"}`), "clean path"},
		{"a path that is not clean", "exit 0", onDisk(`{"do": "delete", "disk": "d", "path": "media//a"}`), "clean path"},
		{"the top of the disk", "exit 0", onDisk(`{"do": "delete", "disk": "d", "path": "."}`), "clean path"},
		{"both path and path_b64", "exit 0", onDisk(`{"do": "delete", "disk": "d", "path": "x", "path_b64": "eA=="}`), "both"},
		{"no path", "exit 0", onDisk(`{"do": "delete", "disk": "d"}`), "no path"},
		{"bad base64 in a path", "exit 0", onDisk(`{"do": "delete", "disk": "d", "path_b64": "*"}`), "base64"},
		{"an unknown disk for a file step", "exit 0", onDisk(`{"do": "delete", "disk": "e", "path": "x"}`), "unknown disk"},
		{"a name that is not UTF-8", "exit 0", onDisk(`{"do": "write", "disk": "d", "path_b64": "bWVkaWEvYf8=", "text": "n"}, ` + expect(a+`{"path_b64": "bWVkaWEvYf8=", "text": "n"}`)), ""},
		{"a hidden file is listed like any other", "exit 0", onDisk(`{"do": "write", "disk": "d", "path": "media/.n", "text": "n"}, ` + expect(`{"path": "media/a", "text": "x"}`)), "not expected"},
		{"a fifo on the disk", `mkfifo "$PWD/disks/d/media/f"`, onDisk(`{"do": "atoll"}`), "neither a file"},
	})
}

func TestAlwaysChecked(t *testing.T) {
	two := func(list string) string {
		return `{"format_version": 1, "disks": [{"id": "d", "uuid": "u", "fs": "ext4", "capacity": 10, "tree": [{"path": "media/a", "text": "x"}]}, {"id": "e", "uuid": "v", "fs": "ext4", "capacity": 10, "tree": []}], "steps": [` + list + `]}`
	}
	const run = `{"do": "atoll"}`
	checkVerdicts(t, []verdictCase{
		{"what Atoll may write", `mkdir -p "$PWD/disks/d/Atoll/tmp" "$PWD/disks/e/media/f" && touch "$PWD/disks/d/Atoll/tmp/x"`, two(run), ""},
		{"a hidden file in media", `touch "$PWD/disks/d/media/.x"`, two(run), "a hidden name"},
		{"a hidden folder in the workdir", `mkdir -p "$PWD/disks/d/Atoll/.y"`, two(run), "a hidden name"},
		{"a hidden file at the top", `touch "$PWD/disks/d/.x"`, two(run), "a hidden name"},
		{"on the second disk too", `touch "$PWD/disks/e/.x"`, two(run), "a hidden name"},
		{"a file outside media and Atoll", `touch "$PWD/disks/d/stray"`, two(run), "outside"},
		{"a folder outside them", `mkdir "$PWD/disks/e/stray"`, two(run), "outside"},
		{"left between two runs", `case "$1" in make) touch "$PWD/disks/d/stray";; clean) rm "$PWD/disks/d/stray";; esac`, two(`{"do": "atoll", "args": ["make"]}, {"do": "atoll", "args": ["clean"]}`), "outside"},
		{"a tree holds a folder and a symlink", "exit 0", `{"format_version": 1, "disks": [{"id": "d", "uuid": "u", "fs": "ext4", "capacity": 10, "tree": [{"path": "media/e", "kind": "dir"}, {"path": "media/l", "kind": "symlink", "target": "e"}]}], "steps": [{"do": "expect_tree", "disk": "d", "tree": [{"path": "media/e", "kind": "dir"}, {"path": "media/l", "kind": "symlink", "target": "e"}]}]}`, ""},
		{"what the scenario put at the start", "exit 0", `{"format_version": 1, "disks": [{"id": "d", "uuid": "u", "fs": "ext4", "capacity": 10, "tree": [{"path": ".old", "text": "o"}, {"path": "other/x", "text": "o"}]}], "steps": [` + run + `]}`, ""},
		{"what a step put there", "exit 0", two(`{"do": "write", "disk": "d", "path": "other/.n", "text": "o"}, ` + run), ""},
		{"a folder a step made", "exit 0", two(`{"do": "mkdir", "disk": "d", "path": ".m/n"}, ` + run), ""},
		{"a symlink a step made", "exit 0", two(`{"do": "symlink", "disk": "d", "path": ".l", "target": "x"}, ` + run), ""},
		{"a name a rename gave", "exit 0", two(`{"do": "rename", "disk": "d", "path": "media/a", "to": ".r"}, ` + run), ""},
		{"a replica of a user's hidden file", `mkdir -p "$PWD/disks/e/media" && touch "$PWD/disks/e/media/.n"`, two(`{"do": "write", "disk": "d", "path": "media/.n", "text": "o"}, ` + run), ""},
		{"a top is for its own disk only", `touch "$PWD/disks/e/other"`, two(`{"do": "write", "disk": "d", "path": "other/x", "text": "o"}, ` + run), "outside"},
		{"another hidden name beside one the user gave", `touch "$PWD/disks/d/media/.y"`, two(`{"do": "write", "disk": "d", "path": "media/.n", "text": "o"}, ` + run), "a hidden name"},
		{"two disks with one id", "exit 0", `{"format_version": 1, "disks": [{"id": "d", "uuid": "u", "fs": "ext4", "capacity": 10, "tree": []}, {"id": "d", "uuid": "v", "fs": "ext4", "capacity": 10, "tree": []}], "steps": [` + run + `]}`, "twice"},
		{"a disk with no id", "exit 0", `{"format_version": 1, "disks": [{"uuid": "u", "fs": "ext4", "capacity": 10, "tree": []}], "steps": [` + run + `]}`, "no id"},
	})
}

func TestControlFile(t *testing.T) {
	body := `{"format_version": 1, "clock": "2026-10-26T01:00:00+02:00", "disks": [{"id": "d", "uuid": "u", "fs": "ntfs", "capacity": 10, "tree": []}], "steps": [{"do": "atoll", "stdout_json": {"now": "2026-10-26T01:00:00+02:00", "disks": [{"id": "d", "uuid": "u", "fs": "ntfs", "path": "${SANDBOX}/disks/d", "capacity": 10}]}}]}`
	if err := runFile(fakeBinary(t, `cat "$ATOLL_CONFORMANCE"`), scenarioFile(t, body)); err != nil {
		t.Error(err)
	}
}

func TestUnmountAndMount(t *testing.T) {
	const (
		tree     = `[{"path": "media/a", "text": "x"}]`
		unmount  = `{"do": "unmount", "disk": "t1"}`
		mount    = `{"do": "mount", "disk": "t1"}`
		empty    = `{"do": "expect_tree", "disk": "t1", "tree": []}`
		full     = `{"do": "expect_tree", "disk": "t1", "tree": ` + tree + `}`
		run      = `{"do": "atoll"}`
		isListed = `grep -q 'disks/t1"' "$ATOLL_CONFORMANCE"`
		notThere = `! grep -q 'disks/t1"' "$ATOLL_CONFORMANCE"`
	)
	with := func(list string) string {
		return `{"format_version": 1, "disks": [{"id": "t1", "uuid": "u", "fs": "ext4", "capacity": 1, "tree": ` + tree + `}], "steps": [` + list + `]}`
	}
	checkVerdicts(t, []verdictCase{
		{"the folder is left empty", "exit 0", with(unmount + "," + empty), ""},
		{"the tree is not there meanwhile", "exit 0", with(unmount + "," + full), "not on the disk"},
		{"it comes back with its tree", "exit 0", with(unmount + "," + mount + "," + full), ""},
		{"it comes back: not empty", "exit 0", with(unmount + "," + mount + "," + empty), "media/a"},
		{"it stays in the control file at first", isListed, with(run), ""},
		{"it leaves the control file", notThere, with(unmount + "," + run), ""},
		{"it leaves the control file: the opposite", isListed, with(unmount + "," + run), "exit code 1"},
		{"it returns to the control file", isListed, with(unmount + "," + mount + "," + run), ""},
		{"the clock keeps it out", notThere, with(unmount + `,{"do": "advance", "by": "1s"},` + run), ""},
		{"written to the empty folder, by a step", "exit 0", with(unmount + `,{"do": "write", "disk": "t1", "path": "x", "text": "y"},` + mount), "written to the empty folder"},
		{"unmounted twice", "exit 0", with(unmount + "," + unmount), "already unmounted"},
		{"mounted when it is", "exit 0", with(mount), "is not unmounted"},
		{"mounted twice", "exit 0", with(unmount + "," + mount + "," + mount), "is not unmounted"},
		{"unmount: an unknown disk", "exit 0", with(`{"do": "unmount", "disk": "x"}`), "unknown disk"},
		{"mount: an unknown disk", "exit 0", with(`{"do": "mount", "disk": "x"}`), "unknown disk"},
		{"unmount: a field that does not apply", "exit 0", with(`{"do": "unmount", "disk": "t1", "path": "x"}`), "does not apply"},
		{"mount: a field that does not apply", "exit 0", with(`{"do": "mount", "disk": "t1", "by": "1s"}`), "does not apply"},
		{"the binary writes to the empty folder", "echo x > disks/t1/stray", with(unmount + "," + run), "stray"},
	})
}

func TestExpectCatalog(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE t(i INTEGER, s TEXT, n TEXT, b BLOB);
		INSERT INTO t VALUES (1, 'a', NULL, x'ff'), (2, 'b', 'x', x'00')`); err != nil {
		t.Fatal(err)
	}
	box := &sandbox{dir: dir}
	expect := func(query, rows string) error {
		return box.compareCatalog(&step{Query: query, Rows: json.RawMessage(rows)})
	}
	for _, tc := range []struct {
		note, query, rows, fails string
	}{
		{"it matches", "SELECT i, s, n FROM t ORDER BY i", `[[1, "a", null], [2, "b", "x"]]`, ""},
		{"no row", "SELECT i FROM t WHERE i = 9", `[]`, ""},
		{"a row too many", "SELECT i FROM t ORDER BY i", `[[1]]`, "2 items, want 1"},
		{"a row too few", "SELECT i FROM t WHERE i = 1", `[[1], [2]]`, "1 items, want 2"},
		{"another order", "SELECT i FROM t ORDER BY i", `[[2], [1]]`, "$[0][0]"},
		{"an integer differs", "SELECT i FROM t WHERE i = 1", `[[2]]`, "$[0][0]"},
		{"a string differs", "SELECT s FROM t WHERE i = 1", `[["b"]]`, "$[0][0]"},
		{"null is a value", "SELECT n FROM t WHERE i = 1", `[["a"]]`, "$[0][0]"},
		{"${ANY}", "SELECT s FROM t WHERE i = 1", `[["${ANY}"]]`, ""},
		{"${SANDBOX}", "SELECT '" + dir + "/x'", `[["${SANDBOX}/x"]]`, ""},
		{"a BLOB must say how to read it", "SELECT b FROM t WHERE i = 1", `[["x"]]`, "read it with CAST or hex"},
		{"hex reads it", "SELECT hex(b) FROM t WHERE i = 1", `[["FF"]]`, ""},
		{"a query that fails", "SELECT nothing FROM t", `[]`, "query"},
		{"a real number is refused", "SELECT 1.5", `[[1.5]]`, "read it with CAST or hex"},
	} {
		err := expect(tc.query, tc.rows)
		switch {
		case tc.fails == "" && err != nil:
			t.Errorf("%s: %v", tc.note, err)
		case tc.fails != "" && (err == nil || !strings.Contains(err.Error(), tc.fails)):
			t.Errorf("%s: %v, want a failure about %q", tc.note, err, tc.fails)
		}
	}
	if err := (&sandbox{dir: t.TempDir()}).compareCatalog(&step{Query: "SELECT 1", Rows: json.RawMessage(`[]`)}); err == nil || !strings.Contains(err.Error(), "catalog") {
		t.Errorf("no catalog: %v", err)
	}
	checkVerdicts(t, []verdictCase{
		{"no query", "exit 0", steps(`{"do": "expect_catalog", "rows": []}`), "no query"},
		{"no rows", "exit 0", steps(`{"do": "expect_catalog", "query": "SELECT 1"}`), "no rows"},
		{"a field that does not apply", "exit 0", steps(`{"do": "expect_catalog", "query": "SELECT 1", "rows": [], "disk": "a"}`), "does not apply"},
		{"no catalog at all", "exit 0", steps(`{"do": "expect_catalog", "query": "SELECT 1", "rows": []}`), "catalog"},
	})
}

func TestWriteStepExpandsTheSandbox(t *testing.T) {
	const tree = `[{"path": "x", "text": "a"}]`
	body := `{"format_version": 1, "disks": [{"id": "t1", "uuid": "u", "fs": "ext4", "capacity": 1, "tree": ` + tree + `}], "steps": [
		{"do": "write", "disk": "t1", "path": "f", "text": "in ${SANDBOX}/y"},
		{"do": "atoll"}]}`
	checkVerdicts(t, []verdictCase{
		{"it is expanded", `grep -q "in $PWD/y" disks/t1/f`, body, ""},
		{"the token does not stay", `! grep -q 'SANDBOX' disks/t1/f`, body, ""},
	})
}

func TestTimeout(t *testing.T) {
	stepTimeout = 100 * time.Millisecond
	defer func() { stepTimeout = 2 * time.Minute }()
	start := time.Now()
	err := runFile(fakeBinary(t, `echo started; sleep 30`), scenarioFile(t, steps(`{"do": "atoll"}`)))
	if err == nil || !strings.Contains(err.Error(), "no end within") || !strings.Contains(err.Error(), "started") {
		t.Errorf("a binary that never ends: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("it took %s to give up", elapsed)
	}
}

func TestParseDuration(t *testing.T) {
	for _, c := range []struct {
		text string
		want time.Duration
	}{{"0s", 0}, {"10s", 10 * time.Second}, {"5m", 5 * time.Minute}, {"2h", 2 * time.Hour}, {"3d", 72 * time.Hour}, {"1w", 168 * time.Hour}, {"120h", 120 * time.Hour}, {"15250w", 15250 * 168 * time.Hour}} {
		got, err := parseDuration(c.text)
		if err != nil || got != c.want {
			t.Errorf("%s: %v, %v", c.text, got, err)
		}
	}
	for _, text := range []string{"", "m", "5", "5x", "-5m", "1h30m", "1.5h", "15251w", "99999999999999999999s"} {
		if got, err := parseDuration(text); err == nil {
			t.Errorf("%q: accepted as %v", text, got)
		}
	}
}

func TestRunFolder(t *testing.T) {
	ok := fakeBinary(t, "exit 0")
	folder := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(folder, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "not a scenario, and not refused")
	write("a.json", steps(`{"do": "atoll"}`))
	if err := run(ok, folder); err != nil {
		t.Errorf("a good folder: %v", err)
	}
	write("b.json", steps(`{"do": "atoll", "exit": 9}`))
	err := run(ok, folder)
	if err == nil || !strings.Contains(err.Error(), "1 of 2") || !strings.Contains(err.Error(), "b.json") {
		t.Errorf("one failure in two: %v", err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(ok, other); err == nil {
		t.Error("a file that is no scenario was skipped")
	}
	if err := run(filepath.Join(t.TempDir(), "absent"), folder); err == nil {
		t.Error("a binary that does not exist was accepted")
	}
	if err := run(ok, filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a folder that does not exist was accepted")
	}
	if err := run(ok, t.TempDir()); err == nil {
		t.Error("a folder with no scenario was accepted")
	}
}

// Each scenario of the repository is well-formed, whatever binary judges it.
func TestRepositoryScenarios(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "scenarios", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no scenarios")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		s, err := parseScenario(data)
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if s.Spec == "" || s.Description == "" {
			t.Errorf("%s: no spec or no description", file)
		}
	}
}
