package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/layout"
)

const markUUID = "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d"

// markBench is a pool of one disk, t2, on a folder the machine takes for its mount, and its catalog.
type markBench struct {
	t          *testing.T
	configPath string
	catalog    string
	mount      string
	machine    *fakeDisk
}

// newMarkBench makes the pool; more is added to t2's [[disk]], and create says whether its catalog is made.
func newMarkBench(t *testing.T, more string, create bool) *markBench {
	t.Helper()
	dir := t.TempDir()
	b := &markBench{t: t, configPath: filepath.Join(dir, "atoll.toml"), catalog: filepath.Join(dir, "catalog.db"), mount: filepath.Join(dir, "t2")}
	mustMkdir(t, filepath.Join(b.mount, "media"))
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(b.catalog) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n" +
		"[[disk]]\nid = \"t2\"\nuuid = \"" + markUUID + "\"\nmount = " + strconv.Quote(b.mount) + "\nfs = \"ext4\"\n" + more
	if err := os.WriteFile(b.configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if create {
		if _, err := catalog.Create(t.Context(), b.catalog, "p"); err != nil {
			t.Fatal(err)
		}
	}
	b.machine = &fakeDisk{mount: b.mount, fsType: "ext4", options: []string{"rw"}}
	return b
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// markerText is the marker of t2 in a pool.
func (b *markBench) markerText(pool string) string {
	b.t.Helper()
	text, err := layout.MarkerText(layout.Marker{PoolID: pool, DiskID: "t2", FSUUID: markUUID})
	if err != nil {
		b.t.Fatal(err)
	}
	return text
}

func (b *markBench) markerPath() string { return filepath.Join(b.mount, "Atoll", layout.MarkerName) }

// write puts a marker on t2.
func (b *markBench) write(text string) {
	b.t.Helper()
	mustMkdir(b.t, filepath.Dir(b.markerPath()))
	if err := os.WriteFile(b.markerPath(), []byte(text), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// onDisk gives what t2 holds at its marker's name, "" for nothing.
func (b *markBench) onDisk() string {
	data, err := os.ReadFile(b.markerPath())
	if err != nil {
		return ""
	}
	return string(data)
}

// synced records t2 in the catalog, as a scan or a pass would, and with seen its own marker seen, as an earlier disk
// add would have.
func (b *markBench) synced(seen bool) {
	b.t.Helper()
	db, err := catalog.OpenPool(b.t.Context(), b.catalog, "p")
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	row := catalog.Disk{ID: "t2", UUID: markUUID, FS: "ext4", Mount: b.mount, Root: "media", Workdir: "Atoll", Role: "data", State: "online"}
	if err := catalog.SyncDisks(b.t.Context(), db, []catalog.Disk{row}); err != nil {
		b.t.Fatal(err)
	}
	if !seen {
		return
	}
	if err := catalog.SetMarkerSeen(b.t.Context(), db, "t2", markUUID, 1); err != nil {
		b.t.Fatal(err)
	}
}

func (b *markBench) seenAs() { b.synced(true) }

// seen says whether the catalog records t2's own marker seen, and t2's state there.
func (b *markBench) seen() (bool, string) {
	b.t.Helper()
	db, err := catalog.OpenPool(b.t.Context(), b.catalog, "p")
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	list, err := catalog.MarkersSeen(b.t.Context(), db)
	if err != nil {
		b.t.Fatal(err)
	}
	rows, err := catalog.Disks(b.t.Context(), db)
	if err != nil {
		b.t.Fatal(err)
	}
	state := ""
	for i := range rows {
		if rows[i].ID == "t2" {
			state = rows[i].State
		}
	}
	return list["t2"] == markUUID, state
}

func (b *markBench) machineFn() func() (disks.Machine, error) {
	return func() (disks.Machine, error) { return b.machine, nil }
}

// mark runs atoll disk mark with these arguments.
func (b *markBench) mark(args ...string) (code int, stdout, stderr string) {
	b.t.Helper()
	var out, errs bytes.Buffer
	code = diskMarkOn(b.configPath, args, b.machineFn(), &out, &errs)
	return code, out.String(), errs.String()
}

// markJSON runs atoll disk mark --json, checks its output against api/schemas/disk-mark.schema.json, and reads it.
func (b *markBench) markJSON(want int, args ...string) diskMarkOutput {
	b.t.Helper()
	code, out, errs := b.mark(append(args, "--json")...)
	if code != want {
		b.t.Fatalf("%v: exit %d, want %d: %s", args, code, want, errs)
	}
	conformsTo(b.t, "disk-mark.schema.json", []byte(out))
	var got diskMarkOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		b.t.Fatal(err)
	}
	return got
}

// conformsTo checks a --json output against what its schema says of each value, as far as Atoll's schemas say it:
// the keys required, no other, a value of each enum or const, of its type or types, through $ref to $defs, and the
// allOf of if and then that ties a reason to its state.
func conformsTo(t *testing.T, name string, data []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("%s: %v: %s", name, err, data)
	}
	if problem := (schemaCheck{root: schema}).value(schema, value, "$"); problem != "" {
		t.Errorf("%s: %s in %s", name, problem, data)
	}
}

type schemaCheck struct{ root map[string]any }

// resolve gives the schema a $ref names, "#/$defs/<name>".
func (c schemaCheck) resolve(schema map[string]any) map[string]any {
	if ref, ok := schema["$ref"].(string); ok {
		defs, _ := c.root["$defs"].(map[string]any)
		if def, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any); ok {
			return def
		}
	}
	return schema
}

// typeOf names a JSON value's type as a schema does.
func typeOf(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if v == float64(int64(v)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	default:
		return "object"
	}
}

func (c schemaCheck) value(schema map[string]any, value any, at string) string {
	schema = c.resolve(schema)
	if enum, ok := schema["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return fmt.Sprint(e) == fmt.Sprint(value) }) {
		return fmt.Sprintf("%s: %v is not one of %v", at, value, enum)
	}
	if want, ok := schema["const"]; ok && fmt.Sprint(want) != fmt.Sprint(value) {
		return fmt.Sprintf("%s: %v, want %v", at, value, want)
	}
	var types []string
	switch kind := schema["type"].(type) {
	case string:
		types = []string{kind}
	case []any:
		for _, k := range kind {
			types = append(types, fmt.Sprint(k))
		}
	}
	got := typeOf(value)
	if len(types) > 0 && !slices.Contains(types, got) && (got != "integer" || !slices.Contains(types, "number")) {
		return fmt.Sprintf("%s: a %s, want %v", at, got, types)
	}
	switch v := value.(type) {
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		if required, ok := schema["required"].([]any); ok {
			for _, r := range required {
				if _, ok := v[fmt.Sprint(r)]; !ok {
					return fmt.Sprintf("%s: no %s", at, r)
				}
			}
		}
		for key, inner := range v {
			sub, ok := properties[key].(map[string]any)
			if !ok {
				if open, set := schema["additionalProperties"].(bool); set && !open {
					return fmt.Sprintf("%s: %s is no property of the schema", at, key)
				}
				continue
			}
			if problem := c.value(sub, inner, at+"."+key); problem != "" {
				return problem
			}
		}
		all, _ := schema["allOf"].([]any)
		for _, each := range all {
			rule, _ := each.(map[string]any)
			cond, _ := rule["if"].(map[string]any)
			then, _ := rule["then"].(map[string]any)
			if cond == nil || then == nil || c.value(cond, value, at) != "" {
				continue
			}
			if problem := c.value(then, value, at); problem != "" {
				return problem
			}
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		for i, inner := range v {
			if problem := c.value(items, inner, fmt.Sprintf("%s[%d]", at, i)); problem != "" {
				return problem
			}
		}
	}
	return ""
}

// disk mark of a disk whose own marker was seen and is gone, missing no-marker: without --yes it says what it would
// write and exits with 3, with --dry-run it says so and exits with 0, both writing nothing; with --yes it writes the
// marker as disk add writes it, nothing left beside it, records the disks' states, and the disk is online again;
// asked again, it is nothing to do (specs.md §10, §25).
func TestDiskMarkWritesTheMarkerOfADiskWhoseMarkerWent(t *testing.T) {
	b := newMarkBench(t, "", true)
	b.seenAs()
	asked := b.markJSON(exitConfirm, "t2")
	if asked.State != "missing" || asked.Reason == nil || *asked.Reason != "no-marker" || asked.Written || asked.DryRun || asked.HadMarker ||
		asked.Marker != (diskShowMarker{PoolID: "p", DiskID: "t2", FSUUID: markUUID}) {
		t.Errorf("without --yes: %+v", asked)
	}
	if dry := b.markJSON(exitOK, "--dry-run", "t2"); !dry.DryRun || dry.Written || dry.State != "missing" {
		t.Errorf("with --dry-run: %+v", dry)
	}
	if got := b.onDisk(); got != "" {
		t.Fatalf("written without --yes: %q", got)
	}
	done := b.markJSON(exitOK, "t2", "--yes")
	if !done.Written || done.HadMarker || len(done.Warnings) != 0 {
		t.Errorf("with --yes: %+v", done)
	}
	if got := b.onDisk(); got != b.markerText("p") {
		t.Errorf("the marker written: %q", got)
	}
	if names, err := os.ReadDir(filepath.Dir(b.markerPath())); err != nil || len(names) != 1 {
		t.Errorf("beside the marker: %v, %v", names, err)
	}
	if seen, state := b.seen(); !seen || state != "online" {
		t.Errorf("the catalog: seen %v, t2 %s", seen, state)
	}
	again := b.markJSON(exitOK, "t2", "--yes")
	if !again.HadMarker || again.Written || again.State != "online" || again.Reason != nil {
		t.Errorf("asked again: %+v", again)
	}
}

// A disk of a pool from before, its marker never seen: online without a marker, disk mark gives it one and records it
// seen; one that holds its own marker already is nothing to do, recorded seen with --yes alone (specs.md §10, §25).
func TestDiskMarkMarksADiskOfAPoolFromBefore(t *testing.T) {
	b := newMarkBench(t, "", true)
	if got := b.markJSON(exitOK, "t2", "--yes"); got.State != "online" || got.Reason != nil || !got.Written {
		t.Errorf("no marker, none seen: %+v", got)
	}
	if seen, _ := b.seen(); !seen {
		t.Error("its marker written, not recorded seen")
	}

	b = newMarkBench(t, "", true)
	b.synced(false)
	b.write(b.markerText("p"))
	if got := b.markJSON(exitOK, "t2"); !got.HadMarker || got.Written || got.State != "online" {
		t.Errorf("its own marker, without --yes: %+v", got)
	}
	if seen, _ := b.seen(); seen {
		t.Error("recorded seen without --yes")
	}
	if got := b.markJSON(exitOK, "t2", "--yes"); !got.HadMarker || got.Written {
		t.Errorf("its own marker, with --yes: %+v", got)
	}
	if seen, _ := b.seen(); !seen {
		t.Error("its own marker, with --yes: not recorded seen")
	}
	if got := b.onDisk(); got != b.markerText("p") {
		t.Errorf("its own marker changed: %q", got)
	}
	if code, out, _ := b.mark("t2", "--yes"); code != exitOK || !strings.Contains(out, "nothing to write") {
		t.Errorf("its own marker, said: %d %q", code, out)
	}
}

// disk mark refuses, with exit 1, nothing on standard output and nothing written: a disk missing for another reason
// than no-marker, readonly, failed, that holds another pool's marker or something that is no marker this version
// reads, whose marker cannot be read, or that the configuration lacks; and a usage error is exit 2 (specs.md §25).
func TestDiskMarkRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		more    string
		prepare func(b *markBench)
		args    []string
		want    int
		says    string
	}{
		{"missing for another reason", "", func(b *markBench) { b.machine.mount = b.mount + "-elsewhere" }, nil, exitError, "missing"},
		{"another identifier there", "", func(b *markBench) { b.machine.id = "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b" }, nil, exitError, "wrong-identifier"},
		{"readonly", "", func(b *markBench) { b.machine.options = []string{"ro"} }, nil, exitError, "read-only-mount"},
		{"readonly, its marker gone", "", func(b *markBench) { b.seenAs(); b.machine.options = []string{"ro"} }, nil, exitError, "read-only"},
		{"failed", "state = \"failed\"\n", nil, nil, exitError, "failed"},
		{"another pool's marker", "", func(b *markBench) { b.write(b.markerText("q")) }, nil, exitError, "other-marker"},
		{"a file this version does not read", "", func(b *markBench) { b.write("format_version = 9\n") }, nil, exitError, "other-marker"},
		{"a folder at its name", "", func(b *markBench) { mustMkdir(b.t, b.markerPath()) }, nil, exitError, "marker"},
		{"its marker unreadable", "", func(b *markBench) {
			b.write(b.markerText("q"))
			b.machine.fail = func(call, name string) error {
				if call == "openfile" && name == layout.MarkerName {
					return syscall.EACCES
				}
				return nil
			}
		}, nil, exitError, "marker"},
		{"a disk the configuration lacks", "", nil, []string{"t9", "--yes"}, exitError, "t9"},
		{"--dry-run with --yes", "", nil, []string{"t2", "--dry-run", "--yes"}, exitUsage, "--dry-run"},
		{"no disk", "", nil, []string{"--yes"}, exitUsage, "disk"},
		{"an unknown argument", "", nil, []string{"t2", "--force"}, exitUsage, "--force"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newMarkBench(t, tc.more, true)
			if tc.prepare != nil {
				tc.prepare(b)
			}
			before := b.onDisk()
			args := tc.args
			if args == nil {
				args = []string{"t2", "--yes", "--json"}
			}
			code, out, errs := b.mark(args...)
			if code != tc.want || out != "" || !strings.Contains(errs, tc.says) {
				t.Errorf("exit %d, want %d; stdout %q; stderr %q, want %q in it", code, tc.want, out, errs, tc.says)
			}
			if got := b.onDisk(); got != before {
				t.Errorf("the marker was %q, is %q", before, got)
			}
		})
	}
	// A configuration that cannot be read, and a machine that cannot be had.
	b := newMarkBench(t, "", true)
	if code := diskMarkOn(filepath.Join(t.TempDir(), "none.toml"), []string{"t2"}, b.machineFn(), &bytes.Buffer{}, &bytes.Buffer{}); code != exitError {
		t.Errorf("no configuration: exit %d", code)
	}
	noMachine := func() (disks.Machine, error) { return nil, syscall.ENODEV }
	if code := diskMarkOn(b.configPath, []string{"t2"}, noMachine, &bytes.Buffer{}, &bytes.Buffer{}); code != exitError {
		t.Errorf("no machine: exit %d", code)
	}
}

// The human output says the state, then what was or would be written, in English or in French (specs.md §25).
func TestDiskMarkSaysWhatItDid(t *testing.T) {
	b := newMarkBench(t, "", true)
	b.seenAs()
	code, out, _ := b.mark("t2")
	if code != exitConfirm || out != "disk t2: missing (no-marker)\nits marker would be written at "+b.markerPath()+": nothing written\n" {
		t.Errorf("without --yes: %d %q", code, out)
	}
	if code, out, _ = b.mark("t2", "--yes"); code != exitOK || out != "disk t2: missing (no-marker)\nits marker is written at "+b.markerPath()+"\n" {
		t.Errorf("with --yes: %d %q", code, out)
	}
	french = true
	t.Cleanup(func() { french = false })
	reason := "no-marker"
	for out, want := range map[*diskMarkOutput]string{
		{Disk: "t2", State: "missing", Reason: &reason}:                "disque t2 : missing (no-marker)\nson marqueur serait écrit en /m : rien n'a été écrit\n",
		{Disk: "t2", State: "missing", Reason: &reason, Written: true}: "disque t2 : missing (no-marker)\nson marqueur est écrit en /m\n",
		{Disk: "t2", State: "online", HadMarker: true}:                 "disque t2 : online\nil porte son propre marqueur en /m : rien à écrire\n",
	} {
		if got := diskMarkText(out, "/m"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// disk list asks each disk's marker, with the catalog's record: a disk whose own marker was seen and is gone is
// missing, no-marker; a catalog that cannot be read is a warning, and the check then takes none as seen (specs.md §10,
// §25).
func TestDiskListSaysADiskWithoutItsMarkerMissing(t *testing.T) {
	b := newMarkBench(t, "", true)
	b.seenAs()
	list := func(b *markBench, args ...string) (int, string, string) {
		var out, errs bytes.Buffer
		code := diskListOn(b.configPath, args, b.machineFn(), &out, &errs)
		return code, out.String(), errs.String()
	}
	code, out, errs := list(b, "--json")
	if code != exitOK || errs != "" {
		t.Fatalf("%d %q", code, errs)
	}
	conformsTo(t, "disk-list.schema.json", []byte(out))
	var got diskListOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Disks) != 1 || got.Disks[0].State != "missing" || got.Disks[0].Reason == nil || *got.Disks[0].Reason != "no-marker" {
		t.Errorf("%+v", got.Disks)
	}
	if code, out, _ = list(b); code != exitOK || !strings.HasPrefix(out, "t2: missing ("+b.mount+", ext4): no-marker: ") {
		t.Errorf("as text: %d %q", code, out)
	}
	b.write(b.markerText("p"))
	if code, out, _ = list(b); code != exitOK || out != "t2: online ("+b.mount+", ext4)\n" {
		t.Errorf("its marker back: %d %q", code, out)
	}
	// No catalog: none taken as seen, with a warning.
	b = newMarkBench(t, "", false)
	if code, out, errs = list(b); code != exitOK || out != "t2: online ("+b.mount+", ext4)\n" || !strings.Contains(errs, "warning: which disks' own marker was seen") {
		t.Errorf("no catalog: %d %q %q", code, out, errs)
	}
	for _, args := range [][]string{{"--json", "--json"}, {"--all"}} {
		if code, out, _ := list(b, args...); code != exitUsage || out != "" {
			t.Errorf("%v: %d %q", args, code, out)
		}
	}
	noMachine := func() (disks.Machine, error) { return nil, syscall.ENODEV }
	if code := diskListOn(b.configPath, nil, noMachine, &bytes.Buffer{}, &bytes.Buffer{}); code != exitError {
		t.Errorf("no machine: exit %d", code)
	}
}

// runDisk sends each subcommand to its own, and says a usage error of none or of an unknown one (specs.md §25).
func TestRunDiskSendsEachSubcommand(t *testing.T) {
	config := filepath.Join(t.TempDir(), "none.toml")
	for _, tc := range []struct {
		args []string
		want int
	}{
		{nil, exitUsage},
		{[]string{"bogus"}, exitUsage},
		{[]string{"mark"}, exitUsage},
		{[]string{"mark", "t2"}, exitError},
		{[]string{"list", "--all"}, exitUsage},
		{[]string{"show"}, exitUsage},
		{[]string{"add"}, exitUsage},
		{[]string{"remove"}, exitUsage},
		{[]string{"fail"}, exitUsage},
		{[]string{"replace"}, exitUsage},
	} {
		var out, errs bytes.Buffer
		if code := runDisk(config, tc.args, &out, &errs); code != tc.want || out.Len() != 0 {
			t.Errorf("%v: exit %d, want %d; %q %q", tc.args, code, tc.want, out.String(), errs.String())
		}
	}
}

// The check of an output against its schema refuses what the schema refuses: a reason of another state, a key it
// lacks or does not know, a value of another type.
func TestTheSchemaCheckRefuses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "schemas", "disk-list.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	disk := `{"id": "t2", "uuid": "u", "fs": "ext4", "mount": "/m", "role": "data", "state": %q, "reason": %s, "message": %s}`
	online := fmt.Sprintf(disk, "online", "null", "null")
	for text, ok := range map[string]bool{
		fmt.Sprintf(disk, "missing", `"no-marker"`, `"m"`):     true,
		fmt.Sprintf(disk, "readonly", `"other-marker"`, `"m"`): true,
		online: true,
		fmt.Sprintf(disk, "readonly", `"no-marker"`, `"m"`): false,
		fmt.Sprintf(disk, "online", `"ads"`, `"m"`):         false,
		fmt.Sprintf(disk, "lost", "null", "null"):           false,
		`{"id": "t2"}`: false,
		strings.Replace(online, `"role": "data"`, `"role": "data", "extra": 1`, 1): false,
		strings.Replace(online, `"id": "t2"`, `"id": 2`, 1):                        false,
	} {
		var value any
		if err := json.Unmarshal([]byte(`{"disks": [`+text+`]}`), &value); err != nil {
			t.Fatal(err)
		}
		if problem := (schemaCheck{root: schema}).value(schema, value, "$"); (problem == "") != ok {
			t.Errorf("%s: %q", text, problem)
		}
	}
}
