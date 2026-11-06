package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // to read a catalog, as a scenario's expectations do (specs.md §29.4)
)

const (
	formatVersion  = 1
	sandboxToken   = "${SANDBOX}"
	anyToken       = "${ANY}"
	anyOrNullToken = "${ANY_OR_NULL}"
	defaultClock   = "2026-01-01T00:00:00Z"
	controlEnv     = "ATOLL_CONFORMANCE"
)

// stepTimeout bounds one run of the binary, so that a daemon that never goes idle fails its
// scenario instead of hanging the suite.
var stepTimeout = 2 * time.Minute

// scenario is one file of conformance/scenarios: see its README.md.
type scenario struct {
	FormatVersion int     `json:"format_version"`
	Spec          string  `json:"spec"`
	Description   string  `json:"description"`
	Clock         string  `json:"clock"`
	Disks         []disk  `json:"disks"`
	Config        *string `json:"config"`
	Steps         []step  `json:"steps"`
}

type disk struct {
	ID       string  `json:"id"`
	UUID     string  `json:"uuid"`
	FS       string  `json:"fs"`
	Capacity int64   `json:"capacity"`
	Tree     []entry `json:"tree"`
}

// entry is what a tree holds at a path.
type entry struct {
	Path    *string `json:"path"`
	PathB64 *string `json:"path_b64"`
	Kind    string  `json:"kind"`
	Text    *string `json:"text"`
	Size    *int    `json:"size"`
	Target  string  `json:"target"`
	MtimeNS *int64  `json:"mtime_ns"`
}

type step struct {
	Do         string            `json:"do"`
	Args       []json.RawMessage `json:"args"`
	Exit       int               `json:"exit"`
	StdoutJSON json.RawMessage   `json:"stdout_json"`
	Disk       string            `json:"disk"`
	Path       *string           `json:"path"`
	PathB64    *string           `json:"path_b64"`
	To         *string           `json:"to"`
	ToB64      *string           `json:"to_b64"`
	Text       *string           `json:"text"`
	Size       *int              `json:"size"`
	Target     string            `json:"target"`
	MtimeNS    *int64            `json:"mtime_ns"`
	By         string            `json:"by"`
	Tree       []entry           `json:"tree"`
	Query      string            `json:"query"`
	Rows       json.RawMessage   `json:"rows"`
}

// stepFields lists what a step may hold besides do, and reports whether the runner knows it.
func stepFields(do string) ([]string, bool) {
	switch do {
	case "atoll":
		return []string{"args", "exit", "stdout_json"}, true
	case "write":
		return []string{"disk", "path", "path_b64", "text", "size", "mtime_ns"}, true
	case "mkdir", "delete":
		return []string{"disk", "path", "path_b64"}, true
	case "rename":
		return []string{"disk", "path", "path_b64", "to", "to_b64"}, true
	case "symlink":
		return []string{"disk", "path", "path_b64", "target"}, true
	case "touch":
		return []string{"disk", "path", "path_b64", "mtime_ns"}, true
	case "advance":
		return []string{"by"}, true
	case "unmount", "mount":
		return []string{"disk"}, true
	case "expect_tree":
		return []string{"disk", "tree"}, true
	case "expect_catalog":
		return []string{"query", "rows"}, true
	default:
		return nil, false
	}
}

// parseScenario refuses what would check nothing, before anything runs.
func parseScenario(data []byte) (*scenario, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s scenario
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode: trailing data after the scenario")
	}
	if s.FormatVersion != formatVersion {
		return nil, fmt.Errorf("format_version %d, want %d", s.FormatVersion, formatVersion)
	}
	if len(s.Steps) == 0 {
		return nil, errors.New("no steps")
	}
	if err := checkSteps(data); err != nil {
		return nil, err
	}
	for i := range s.Disks {
		if err := checkEntries(s.Disks[i].Tree); err != nil {
			return nil, fmt.Errorf("disk %q: %w", s.Disks[i].ID, err)
		}
	}
	for i := range s.Steps {
		st := &s.Steps[i]
		var err error
		switch {
		case st.Do == "expect_tree" && st.Tree == nil:
			err = errors.New("no tree")
		case st.Do == "expect_tree":
			err = checkEntries(st.Tree)
		case st.Do == "expect_catalog" && (st.Query == "" || st.Rows == nil):
			err = errors.New("no query, or no rows")
		case st.Do == "write":
			err = checkEntry(&entry{Text: st.Text, Size: st.Size})
		case st.Do == "touch" && st.MtimeNS == nil:
			err = errors.New("no mtime_ns")
		case st.Do == "symlink" && st.Target == "":
			err = errors.New("no target")
		}
		if err != nil {
			return nil, fmt.Errorf("step %d (%s): %w", i, st.Do, err)
		}
	}
	return &s, nil
}

// checkSteps refuses a step the runner does not know, and a field on a step it does not
// apply to. Both would read as a check that passed.
func checkSteps(data []byte) error {
	var raw struct {
		Steps []map[string]json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	for i, fields := range raw.Steps {
		var do string
		if err := json.Unmarshal(fields["do"], &do); err != nil {
			return fmt.Errorf("step %d: no do", i)
		}
		allowed, known := stepFields(do)
		if !known {
			return fmt.Errorf("step %d: a step the runner does not know: %q", i, do)
		}
		for _, field := range slices.Sorted(maps.Keys(fields)) {
			if field != "do" && !slices.Contains(allowed, field) {
				return fmt.Errorf("step %d (%s): %q does not apply to it", i, do, field)
			}
		}
	}
	return nil
}

func checkEntries(tree []entry) error {
	for i := range tree {
		if err := checkEntry(&tree[i]); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
	}
	return nil
}

// checkEntry refuses a field that means nothing for the kind of an entry.
func checkEntry(e *entry) error {
	switch e.Kind {
	case "", "file":
		switch {
		case e.Target != "":
			return errors.New("a file has no target")
		case e.Text != nil && e.Size != nil:
			return errors.New("both text and size")
		case e.Text == nil && e.Size == nil:
			return errors.New("no content: want text or size")
		case e.Size != nil && *e.Size < 0:
			return fmt.Errorf("size %d", *e.Size)
		}
	case "dir":
		if e.Text != nil || e.Size != nil || e.Target != "" || e.MtimeNS != nil {
			return errors.New("a folder has only a path")
		}
	case "symlink":
		if e.Text != nil || e.Size != nil || e.MtimeNS != nil {
			return errors.New("a symlink has only a path and a target")
		}
		if e.Target == "" {
			return errors.New("no target")
		}
	default:
		return fmt.Errorf("unknown kind %q", e.Kind)
	}
	return nil
}

// sandbox is one run of a scenario, in its own temporary folder.
type sandbox struct {
	bin    string
	dir    string
	s      *scenario
	now    time.Time
	tops   map[string]map[string]bool // by disk id: what the scenario put at the top of it
	hidden map[string]bool            // the hidden names the scenario gave, on any disk
	away   map[string]bool            // the disks an unmount step set aside
}

func runFile(bin, file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	s, err := parseScenario(data)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "atoll-scenario-")
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	box := &sandbox{bin: bin, dir: dir, s: s, tops: map[string]map[string]bool{}, hidden: map[string]bool{}, away: map[string]bool{}}
	if err := box.setUp(); err != nil {
		return fmt.Errorf("set up: %w", err)
	}
	for i := range s.Steps {
		if err := box.do(&s.Steps[i]); err != nil {
			return fmt.Errorf("step %d (%s): %w", i, s.Steps[i].Do, err)
		}
	}
	return nil
}

func (b *sandbox) setUp() error {
	clock := b.s.Clock
	if clock == "" {
		clock = defaultClock
	}
	now, err := time.Parse(time.RFC3339, clock)
	if err != nil {
		return fmt.Errorf("clock: %w", err)
	}
	b.now = now
	for i := range b.s.Disks {
		d := &b.s.Disks[i]
		if d.ID == "" || b.tops[d.ID] != nil {
			return fmt.Errorf("disk %q: no id, or twice", d.ID)
		}
		b.tops[d.ID] = map[string]bool{}
		root := b.diskPath(d.ID)
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("disk %s: %w", d.ID, err)
		}
		for j := range d.Tree {
			rel, err := pathOf(d.Tree[j].Path, d.Tree[j].PathB64)
			if err != nil {
				return fmt.Errorf("disk %s: %w", d.ID, err)
			}
			if err := writeEntry(root, rel, &d.Tree[j]); err != nil {
				return fmt.Errorf("disk %s: %w", d.ID, err)
			}
			b.note(d.ID, rel)
		}
	}
	if b.s.Config != nil {
		config := strings.ReplaceAll(*b.s.Config, sandboxToken, b.dir)
		if err := os.WriteFile(b.configPath(), []byte(config), 0o644); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	return b.writeControl()
}

// note records a path the scenario itself creates, which is no fault of the binary's.
func (b *sandbox) note(id, rel string) {
	top, _, _ := strings.Cut(rel, "/")
	b.tops[id][top] = true
	for name := range strings.SplitSeq(rel, "/") {
		if strings.HasPrefix(name, ".") {
			b.hidden[name] = true
		}
	}
}

func (b *sandbox) diskPath(id string) string {
	return filepath.Join(b.dir, "disks", id)
}

func (b *sandbox) configPath() string {
	return filepath.Join(b.dir, "atoll.toml")
}

func (b *sandbox) controlPath() string {
	return filepath.Join(b.dir, "conformance.json")
}

// writeControl writes the file only a conformance build reads (specs.md §29.4).
func (b *sandbox) writeControl() error {
	type controlDisk struct {
		ID       string `json:"id"`
		UUID     string `json:"uuid"`
		FS       string `json:"fs"`
		Path     string `json:"path"`
		Capacity int64  `json:"capacity"`
	}
	control := struct {
		Now   string        `json:"now"`
		Disks []controlDisk `json:"disks"`
	}{Now: b.now.Format(time.RFC3339), Disks: []controlDisk{}}
	for i := range b.s.Disks {
		d := &b.s.Disks[i]
		if !b.away[d.ID] {
			control.Disks = append(control.Disks, controlDisk{d.ID, d.UUID, d.FS, b.diskPath(d.ID), d.Capacity})
		}
	}
	data, err := json.MarshalIndent(control, "", "  ")
	if err != nil {
		return fmt.Errorf("control file: %w", err)
	}
	if err := os.WriteFile(b.controlPath(), data, 0o644); err != nil {
		return fmt.Errorf("control file: %w", err)
	}
	return nil
}

func (b *sandbox) do(s *step) error {
	switch s.Do {
	case "atoll":
		if err := b.runAtoll(s); err != nil {
			return err
		}
		return b.checkAlways()
	case "advance":
		by, err := parseDuration(s.By)
		if err != nil {
			return err
		}
		b.now = b.now.Add(by)
		return b.writeControl()
	case "unmount":
		return b.unmount(s)
	case "mount":
		return b.mount(s)
	case "expect_tree":
		root, err := b.diskOf(s)
		if err != nil {
			return err
		}
		return compareTree(root, s.Tree)
	case "expect_catalog":
		return b.compareCatalog(s)
	default:
		return b.fileStep(s)
	}
}

// unmount leaves what a failed mount does: an empty folder where the disk was.
func (b *sandbox) unmount(s *step) error {
	root, err := b.diskOf(s)
	if err != nil {
		return err
	}
	if b.away[s.Disk] {
		return fmt.Errorf("disk %q is already unmounted", s.Disk)
	}
	aside := b.asidePath(s.Disk)
	if err := os.MkdirAll(filepath.Dir(aside), 0o755); err != nil {
		return fmt.Errorf("unmount: %w", err)
	}
	if err := os.Rename(root, aside); err != nil {
		return fmt.Errorf("unmount: %w", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		return fmt.Errorf("unmount: %w", err)
	}
	b.away[s.Disk] = true
	return b.writeControl()
}

// mount brings a disk back. A real mount would hide what was written to the empty folder, and
// this refuses it, as nothing may write there (specs.md §10).
func (b *sandbox) mount(s *step) error {
	root, err := b.diskOf(s)
	if err != nil {
		return err
	}
	if !b.away[s.Disk] {
		return fmt.Errorf("disk %q is not unmounted", s.Disk)
	}
	if err := os.Remove(root); err != nil {
		return fmt.Errorf("mount: something was written to the empty folder: %w", err)
	}
	if err := os.Rename(b.asidePath(s.Disk), root); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	delete(b.away, s.Disk)
	return b.writeControl()
}

func (b *sandbox) asidePath(id string) string {
	return filepath.Join(b.dir, "unmounted", id)
}

func (b *sandbox) diskOf(s *step) (string, error) {
	if b.tops[s.Disk] == nil {
		return "", fmt.Errorf("unknown disk %q", s.Disk)
	}
	return b.diskPath(s.Disk), nil
}

// fileStep does to a disk what a user would.
func (b *sandbox) fileStep(s *step) error {
	root, err := b.diskOf(s)
	if err != nil {
		return err
	}
	rel, err := pathOf(s.Path, s.PathB64)
	if err != nil {
		return err
	}
	full := filepath.Join(root, rel)
	switch s.Do {
	case "write":
		b.note(s.Disk, rel)
		text := s.Text
		if text != nil {
			replaced := strings.ReplaceAll(*text, sandboxToken, b.dir)
			text = &replaced
		}
		return writeEntry(root, rel, &entry{Text: text, Size: s.Size, MtimeNS: s.MtimeNS})
	case "mkdir":
		b.note(s.Disk, rel)
		return os.MkdirAll(full, 0o755)
	case "rename":
		to, err := pathOf(s.To, s.ToB64)
		if err != nil {
			return fmt.Errorf("to: %w", err)
		}
		b.note(s.Disk, to)
		return os.Rename(full, filepath.Join(root, to))
	case "delete":
		return os.Remove(full)
	case "symlink":
		b.note(s.Disk, rel)
		return writeEntry(root, rel, &entry{Kind: "symlink", Target: s.Target})
	default: // touch
		when := time.Unix(0, *s.MtimeNS)
		return os.Chtimes(full, when, when)
	}
}

func (b *sandbox) runAtoll(s *step) error {
	args := make([]string, 0, len(s.Args)+2)
	if b.s.Config != nil {
		args = append(args, "--config", b.configPath())
	}
	for _, raw := range s.Args {
		arg, err := b.argument(raw)
		if err != nil {
			return err
		}
		args = append(args, arg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.bin, args...)
	cmd.Dir = b.dir
	// With a set time zone, what "local" means does not depend on the machine.
	cmd.Env = append(os.Environ(), controlEnv+"="+b.controlPath(), "TZ=UTC")
	cmd.WaitDelay = time.Second // a child holding the output open must not outlast the timeout
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("no end within %s; stdout %q; stderr %q", stepTimeout, stdout.String(), stderr.String())
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return fmt.Errorf("run: %w", err)
		}
		code = exit.ExitCode()
	}
	if code != s.Exit {
		return fmt.Errorf("exit code %d, want %d; stdout %q; stderr %q", code, s.Exit, stdout.String(), stderr.String())
	}
	if s.StdoutJSON != nil {
		return b.compareJSON(stdout.Bytes(), s.StdoutJSON)
	}
	return nil
}

// argument reads an item of args: a string, or {"b64": "…"} for bytes that are not UTF-8.
func (b *sandbox) argument(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.ReplaceAll(text, sandboxToken, b.dir), nil
	}
	var item struct {
		B64 *string `json:"b64"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&item); err != nil {
		return "", fmt.Errorf("argument %s: %w", raw, err)
	}
	if item.B64 == nil {
		return "", fmt.Errorf("argument %s: want a string or b64", raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(*item.B64)
	if err != nil {
		return "", fmt.Errorf("argument %s: %w", raw, err)
	}
	return string(decoded), nil
}

// compareCatalog runs a query on the scenario's catalog, read-only, and compares its rows.
func (b *sandbox) compareCatalog(s *step) error {
	want, err := decodeExact(s.Rows)
	if err != nil {
		return fmt.Errorf("rows: %w", err)
	}
	got, err := b.queryCatalog(s.Query)
	if err != nil {
		return err
	}
	data, err := json.Marshal(got)
	if err != nil {
		return fmt.Errorf("query %q: %w", s.Query, err)
	}
	gotValue, err := decodeExact(data)
	if err != nil {
		return fmt.Errorf("query %q: %w", s.Query, err)
	}
	if err := b.sameJSON("$", gotValue, want); err != nil {
		return fmt.Errorf("query %q: %w\nall of it: %s", s.Query, err, data)
	}
	return nil
}

func (b *sandbox) queryCatalog(query string) (rows [][]any, err error) {
	path := filepath.Join(b.dir, "catalog.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	result, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query %q: %w", query, err)
	}
	defer func() { err = errors.Join(err, result.Close()) }()
	columns, err := result.Columns()
	if err != nil {
		return nil, fmt.Errorf("query %q: %w", query, err)
	}
	rows = [][]any{}
	for result.Next() {
		row := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range row {
			targets[i] = &row[i]
		}
		if err := result.Scan(targets...); err != nil {
			return nil, fmt.Errorf("query %q: %w", query, err)
		}
		for i, value := range row {
			switch value.(type) {
			case nil, int64, string:
			default:
				return nil, fmt.Errorf("query %q: column %q holds %T: read it with CAST or hex", query, columns[i], value)
			}
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("query %q: %w", query, err)
	}
	return rows, nil
}

// decodeExact keeps numbers as their text: no float rounds one, and 3.0 is not 3.
func decodeExact(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data")
	}
	return v, nil
}

func (b *sandbox) compareJSON(got []byte, want json.RawMessage) error {
	gotValue, err := decodeExact(got)
	if err != nil {
		return fmt.Errorf("stdout is not JSON: %w: %q", err, got)
	}
	wantValue, err := decodeExact(want)
	if err != nil {
		return fmt.Errorf("stdout_json: %w", err)
	}
	if err := b.sameJSON("$", gotValue, wantValue); err != nil {
		return fmt.Errorf("stdout: %w\nall of it: %s", err, got)
	}
	return nil
}

func (b *sandbox) sameJSON(where string, got, want any) error {
	if want == anyOrNullToken {
		if _, isString := got.(string); isString || got == nil {
			return nil
		}
		return fmt.Errorf("%s: %v, want a string or null", where, got)
	}
	switch w := want.(type) {
	case string:
		text, ok := got.(string)
		if !ok {
			return fmt.Errorf("%s: %v, want a string", where, got)
		}
		if w == anyToken {
			return nil
		}
		if expected := strings.ReplaceAll(w, sandboxToken, b.dir); text != expected {
			return fmt.Errorf("%s: %q, want %q", where, text, expected)
		}
		return nil
	case map[string]any:
		object, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %v, want an object", where, got)
		}
		for _, key := range slices.Sorted(maps.Keys(object)) {
			if _, listed := w[key]; !listed {
				return fmt.Errorf("%s: an unexpected key %q", where, key)
			}
		}
		for _, key := range slices.Sorted(maps.Keys(w)) {
			have, ok := object[key]
			if !ok {
				return fmt.Errorf("%s: no key %q", where, key)
			}
			if err := b.sameJSON(where+"."+key, have, w[key]); err != nil {
				return err
			}
		}
		return nil
	case []any:
		list, ok := got.([]any)
		if !ok {
			return fmt.Errorf("%s: %v, want a list", where, got)
		}
		if len(list) != len(w) {
			return fmt.Errorf("%s: %d items, want %d", where, len(list), len(w))
		}
		for i := range w {
			if err := b.sameJSON(fmt.Sprintf("%s[%d]", where, i), list[i], w[i]); err != nil {
				return err
			}
		}
		return nil
	default:
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("%s: %v, want %v", where, got, want)
		}
		return nil
	}
}

// parseDuration reads a duration of specs.md §29.5. It is written again here because the
// runner imports nothing from internal/.
func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("duration %q", s)
	}
	var unit time.Duration
	switch s[len(s)-1] {
	case 's':
		unit = time.Second
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	case 'w':
		unit = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("duration %q: unknown unit", s)
	}
	var total time.Duration
	for _, c := range s[:len(s)-1] {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("duration %q", s)
		}
		digit := time.Duration(c-'0') * unit
		if total > (math.MaxInt64-digit)/10 {
			return 0, fmt.Errorf("duration %q: too long", s)
		}
		total = total*10 + digit
	}
	return total, nil
}

// pathOf reads a path, given as text or as base64, relative to the top of a disk.
func pathOf(plain, b64 *string) (string, error) {
	var p string
	switch {
	case plain != nil && b64 != nil:
		return "", errors.New("both a path and its base64")
	case plain != nil:
		p = *plain
	case b64 != nil:
		decoded, err := base64.StdEncoding.DecodeString(*b64)
		if err != nil {
			return "", fmt.Errorf("base64: %w", err)
		}
		p = string(decoded)
	default:
		return "", errors.New("no path")
	}
	if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") || path.Clean(p) != p {
		return "", fmt.Errorf("path %q: want a clean path inside the disk", p)
	}
	return p, nil
}

// pattern is size bytes of i mod 251, the pattern of the official BLAKE3 test vectors.
func pattern(size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(i % 251)
	}
	return out
}

// contentOf reads an entry that checkEntry has accepted as a file.
func contentOf(e *entry) []byte {
	if e.Text != nil {
		return []byte(*e.Text)
	}
	return pattern(*e.Size)
}

// writeEntry creates what an entry of a tree describes.
func writeEntry(root, rel string, e *entry) error {
	full := filepath.Join(root, rel)
	if e.Kind == "dir" {
		return os.MkdirAll(full, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if e.Kind == "symlink" {
		return os.Symlink(e.Target, full)
	}
	if err := os.WriteFile(full, contentOf(e), 0o644); err != nil {
		return err
	}
	if e.MtimeNS == nil {
		return nil
	}
	when := time.Unix(0, *e.MtimeNS)
	return os.Chtimes(full, when, when)
}

// found is what is on a disk at a path.
type found struct {
	kind    string
	content []byte
	target  string
	mtimeNS int64
	empty   bool // for a folder
}

// listTree reads a whole disk, by path relative to its top.
func listTree(root string) (map[string]*found, error) {
	out := map[string]*found{}
	err := filepath.WalkDir(root, func(full string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if full == root {
			return nil
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			children, err := os.ReadDir(full)
			if err != nil {
				return err
			}
			out[rel] = &found{kind: "dir", empty: len(children) == 0}
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			out[rel] = &found{kind: "symlink", target: target}
		case info.Mode().IsRegular():
			content, err := os.ReadFile(full)
			if err != nil {
				return err
			}
			out[rel] = &found{kind: "file", content: content, mtimeNS: info.ModTime().UnixNano()}
		default:
			return fmt.Errorf("%q: neither a file, a folder nor a symlink", rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the disk: %w", err)
	}
	return out, nil
}

// matches reports whether a listed path, where a component * matches any one name, names a path.
func matches(listed, actual string) bool {
	if listed == actual {
		return true
	}
	a, b := strings.Split(listed, "/"), strings.Split(actual, "/")
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != "*" && a[i] != b[i] {
			return false
		}
	}
	return true
}

// compareTree compares a whole disk with what a step expects (README.md, "Comparing"). Each
// entry takes one path, so one * entry does not cover two operations.
func compareTree(root string, want []entry) error {
	actual, err := listTree(root)
	if err != nil {
		return err
	}
	paths := slices.Sorted(maps.Keys(actual))
	taken := map[string]bool{}
	for i := range want {
		rel, err := pathOf(want[i].Path, want[i].PathB64)
		if err != nil {
			return err
		}
		var differs error
		hit := false
		for _, p := range paths {
			if taken[p] || !matches(rel, p) {
				continue
			}
			if err := sameEntry(&want[i], actual[p]); err != nil {
				if differs == nil {
					differs = fmt.Errorf("%q: %w", p, err)
				}
				continue
			}
			taken[p], hit = true, true
			break
		}
		switch {
		case hit:
		case differs != nil:
			return differs
		default:
			return fmt.Errorf("%q: not on the disk", rel)
		}
	}
	for _, p := range paths {
		// A folder needs no entry unless it is empty: what is below it is listed.
		if !taken[p] && (actual[p].kind != "dir" || actual[p].empty) {
			return fmt.Errorf("%q: on the disk, and not expected", p)
		}
	}
	return nil
}

func sameEntry(e *entry, f *found) error {
	kind := e.Kind
	if kind == "" {
		kind = "file"
	}
	if f.kind != kind {
		return fmt.Errorf("a %s, want a %s", f.kind, kind)
	}
	switch kind {
	case "symlink":
		if f.target != e.Target {
			return fmt.Errorf("target %q, want %q", f.target, e.Target)
		}
	case "file":
		content := contentOf(e)
		if !bytes.Equal(f.content, content) {
			return fmt.Errorf("%d bytes of other content, want %d", len(f.content), len(content))
		}
		if e.MtimeNS != nil && f.mtimeNS != *e.MtimeNS {
			return fmt.Errorf("mtime %d, want %d", f.mtimeNS, *e.MtimeNS)
		}
	}
	return nil
}

// checkAlways applies what must hold after every run of the binary (specs.md §6).
func (b *sandbox) checkAlways() error {
	for i := range b.s.Disks {
		id := b.s.Disks[i].ID
		listing, err := listTree(b.diskPath(id))
		if err != nil {
			return err
		}
		for _, p := range slices.Sorted(maps.Keys(listing)) {
			if name := path.Base(p); strings.HasPrefix(name, ".") && !b.hidden[name] {
				return fmt.Errorf("disk %s: a hidden name: %q", id, p)
			}
			top, _, _ := strings.Cut(p, "/")
			if top != "media" && top != "Atoll" && !b.tops[id][top] {
				return fmt.Errorf("disk %s: %q, outside media and Atoll", id, p)
			}
		}
	}
	return nil
}
