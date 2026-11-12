package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/value"
)

// daemonDisk is one disk of api/schemas/daemon.schema.json.
type daemonDisk struct {
	ID      string  `json:"id"`
	State   string  `json:"state"`
	Reason  *string `json:"reason"`
	Message *string `json:"message"`
	Walked  bool    `json:"walked"`
}

// daemonDecision is one decision of api/schemas/daemon.schema.json.
type daemonDecision struct {
	Path     string `json:"path,omitempty"`
	PathB64  string `json:"path_b64,omitempty"`
	Decision string `json:"decision"`
	To       string `json:"to,omitempty"`
	ToB64    string `json:"to_b64,omitempty"`
}

// daemonOperation is one operation of api/schemas/daemon.schema.json.
type daemonOperation struct {
	Disk    string `json:"disk"`
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	To      string `json:"to,omitempty"`
	ToB64   string `json:"to_b64,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Source  string `json:"source,omitempty"`
	Outcome string `json:"outcome"`
}

// daemonGuard is one guard of api/schemas/daemon.schema.json.
type daemonGuard struct {
	Disk  string `json:"disk"`
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
}

// daemonRest is what is known of a pass only at its end: the disks, decisions and operations go out as
// they come (specs.md §26).
type daemonRest struct {
	Guards  []daemonGuard `json:"guards"`
	Waiting int64         `json:"waiting"`
	NextDue *string       `json:"next_due"`
}

func runDaemon(configPath string, args []string, stdout, stderr io.Writer) int {
	var asJSON, dryRun, untilIdle, foreground bool
	for _, a := range args {
		var flag *bool
		switch a {
		case "--json":
			flag = &asJSON
		case "--dry-run":
			flag = &dryRun
		case "--until-idle":
			flag = &untilIdle
		case "--foreground":
			flag = &foreground
		default:
			return usage(stderr, fmt.Sprintf("daemon: unknown argument %q", a))
		}
		if err := setFlag(flag, a); err != nil {
			return usage(stderr, "daemon: "+err.Error())
		}
	}
	if !untilIdle && asJSON {
		return usage(stderr, "daemon: --json goes with --until-idle")
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if !untilIdle {
		return runForGood(c, !dryRun, stderr)
	}
	buffered := bufio.NewWriter(stdout)
	var sink daemonSink = &textDaemon{w: buffered}
	if asJSON {
		sink = &jsonDaemon{w: buffered, dryRun: dryRun}
	}
	outcome, err := runPass(c, !dryRun, sink, stderr)
	if err == nil {
		err = sink.end(outcome)
	}
	if err == nil {
		err = buffered.Flush()
	}
	if err != nil {
		return failure(stderr, err)
	}
	switch {
	case dryRun:
		return exitOK
	case outcome.Partial:
		return exitPartial
	case outcome.Held:
		return exitConfirm
	default:
		return exitOK
	}
}

// runForGood runs the daemon until SIGTERM or SIGINT (specs.md §15.13). Atoll never detaches itself:
// --foreground changes nothing.
func runForGood(c *config.Config, apply bool, stderr io.Writer) int {
	machine, err := disks.NewMachine()
	if err != nil {
		return failure(stderr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log := slog.New(slog.NewTextHandler(stderr, nil))
	if err := daemon.Run(ctx, daemon.Options{Config: c, Machine: machine, Apply: apply, Log: log}); err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

func runPass(c *config.Config, apply bool, sink jobs.Sink, stderr io.Writer) (outcome *jobs.Outcome, err error) {
	ctx := context.Background()
	machine, err := disks.NewMachine()
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if release, err = lockCatalog(ctx, c, stderr); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	outcome, err = jobs.RunTo(ctx, db, c, machine, apply, sink)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return outcome, nil
}

// daemonSink writes what a pass does as it does it.
type daemonSink interface {
	jobs.Sink
	end(o *jobs.Outcome) error
}

func daemonDiskOf(d *jobs.DiskLine) daemonDisk {
	line := daemonDisk{ID: d.ID, State: string(d.Verdict.State), Walked: d.Walked}
	if d.Verdict.State != disks.Online {
		reason, message := string(d.Verdict.Reason), d.Verdict.Message
		line.Reason, line.Message = &reason, &message
	}
	return line
}

func daemonDecisionOf(d *jobs.Decision) daemonDecision {
	item := daemonDecision{Decision: d.Kind}
	item.Path, item.PathB64 = encodePath(d.Path)
	if d.To != "" {
		item.To, item.ToB64 = encodePath(d.To)
	}
	return item
}

func daemonOperationOf(op *jobs.Operation) daemonOperation {
	item := daemonOperation{Disk: op.Disk, Kind: op.Kind, Reason: op.Reason, Source: op.Source, Outcome: op.Outcome}
	item.Path, item.PathB64 = encodePath(op.Path)
	if op.To != "" {
		item.To, item.ToB64 = encodePath(op.To)
	}
	return item
}

func daemonRestOf(o *jobs.Outcome) daemonRest {
	rest := daemonRest{Guards: []daemonGuard{}, Waiting: o.Waiting}
	for i := range o.Guards {
		rest.Guards = append(rest.Guards, daemonGuard{Disk: o.Guards[i].Disk, Kind: o.Guards[i].Kind, Count: o.Guards[i].Count})
	}
	if o.NextDue.Valid {
		due := timeText(o.NextDue.Int64)
		rest.NextDue = &due
	}
	return rest
}

// jsonDaemon writes api/schemas/daemon.schema.json as a pass goes. The order of keys means nothing in JSON.
type jsonDaemon struct {
	w      io.Writer
	dryRun bool
	buffer bytes.Buffer
	// list is the list now open, and items says whether it holds any.
	list  string
	items bool
}

func (j *jsonDaemon) encoded(v any, indent string) (string, error) {
	j.buffer.Reset()
	enc := json.NewEncoder(&j.buffer)
	enc.SetEscapeHTML(false)
	enc.SetIndent(indent, "  ")
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("writing the output: %w", err)
	}
	return strings.TrimSuffix(j.buffer.String(), "\n"), nil
}

func (j *jsonDaemon) Disks(list []jobs.DiskLine) error {
	lines := make([]daemonDisk, 0, len(list))
	for i := range list {
		lines = append(lines, daemonDiskOf(&list[i]))
	}
	text, err := j.encoded(lines, "  ")
	if err != nil {
		return err
	}
	return writeText(j.w, fmt.Sprintf("{\n  \"dry_run\": %v,\n  \"disks\": %s", j.dryRun, text))
}

// open closes the list that is open, and opens another. Every decision comes before the first operation.
func (j *jsonDaemon) open(list string) error {
	if j.list == list {
		return nil
	}
	closing := ""
	if j.list != "" {
		closing = "]"
		if j.items {
			closing = "\n  ]"
		}
	}
	j.list, j.items = list, false
	if list == "" {
		return writeText(j.w, closing)
	}
	return writeText(j.w, closing+",\n  \""+list+"\": [")
}

func (j *jsonDaemon) item(list string, v any) error {
	if err := j.open(list); err != nil {
		return err
	}
	text, err := j.encoded(v, "    ")
	if err != nil {
		return err
	}
	start := "\n    "
	if j.items {
		start = ",\n    "
	}
	j.items = true
	return writeText(j.w, start+text)
}

func (j *jsonDaemon) Decision(d *jobs.Decision) error {
	return j.item("decisions", daemonDecisionOf(d))
}

func (j *jsonDaemon) Operation(op *jobs.Operation) error {
	if j.list == "" {
		if err := j.open("decisions"); err != nil {
			return err
		}
	}
	return j.item("operations", daemonOperationOf(op))
}

func (j *jsonDaemon) end(o *jobs.Outcome) error {
	for _, list := range []string{"decisions", "operations"} {
		if j.list == "operations" && list == "decisions" {
			continue
		}
		if err := j.open(list); err != nil {
			return err
		}
	}
	if err := j.open(""); err != nil {
		return err
	}
	var text strings.Builder
	if err := writeJSON(&text, daemonRestOf(o)); err != nil {
		return err
	}
	// The object is open already, so its own opening brace goes.
	return writeText(j.w, ","+strings.TrimPrefix(text.String(), "{"))
}

// textDaemon writes for a reader, as a pass goes.
type textDaemon struct {
	w io.Writer
}

func (t *textDaemon) Disks(list []jobs.DiskLine) error {
	var text strings.Builder
	for i := range list {
		if d := daemonDiskOf(&list[i]); d.Reason != nil {
			fmt.Fprintf(&text, "%s: %s: %s: %s\n", d.ID, d.State, *d.Reason, *d.Message)
		}
	}
	return writeText(t.w, text.String())
}

func (t *textDaemon) Decision(d *jobs.Decision) error {
	line := d.Kind + " " + shownPath(d.Path)
	if d.To != "" {
		line += " -> " + shownPath(d.To)
	}
	return writeText(t.w, line+"\n")
}

func (t *textDaemon) Operation(op *jobs.Operation) error {
	line := op.Disk + ": " + op.Kind + " " + shownPath(op.Path)
	if op.To != "" {
		line += " -> " + shownPath(op.To)
	}
	if op.Source != "" {
		line += " (from " + op.Source + ")"
	}
	return writeText(t.w, line+": "+op.Outcome+"\n")
}

func (t *textDaemon) end(o *jobs.Outcome) error {
	var text strings.Builder
	if o.DryRun {
		text.WriteString("a dry run: nothing was written\n")
	}
	for i := range o.Guards {
		g := &o.Guards[i]
		if g.Kind == jobs.GuardModifications {
			fmt.Fprintf(&text, "%s: the mass-modification guard holds what changed after %d files modified: atoll guard list\n", g.Disk, g.Count)
			continue
		}
		fmt.Fprintf(&text, "%s: the mass-deletion guard holds %d copies: atoll guard list\n", g.Disk, g.Count)
	}
	if o.NextDue.Valid {
		fmt.Fprintf(&text, "%d copies wait until %s\n", o.Waiting, timeText(o.NextDue.Int64))
	}
	return writeText(t.w, text.String())
}

func shownB64(path, b64 string) string {
	if b64 != "" {
		return "(base64) " + b64
	}
	return path
}

// guardItem is one disk of api/schemas/guard-list.schema.json.
type guardItem struct {
	Disk    string     `json:"disk"`
	Kind    string     `json:"kind"`
	Tripped bool       `json:"tripped"`
	Count   int64      `json:"count"`
	Paths   []pathItem `json:"paths"`
}

// guardAnswer is the output of api/schemas/guard-answer.schema.json.
type guardAnswer struct {
	Disk string  `json:"disk"`
	Kind string  `json:"kind"`
	Done bool    `json:"done"`
	UpTo *string `json:"up_to"`
}

func runGuard(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, "guard: want list, confirm or reject")
	}
	switch args[0] {
	case "list":
		return guardList(configPath, args[1:], stdout, stderr)
	case "confirm", "reject":
		return guardAct(configPath, args[0], args[1:], stdout, stderr)
	default:
		return usage(stderr, fmt.Sprintf("guard: unknown command %q", args[0]))
	}
}

func guardList(configPath string, args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf("guard list: unknown argument %q", a))
		}
		if err := setFlag(&asJSON, a); err != nil {
			return usage(stderr, "guard list: "+err.Error())
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	items, err := readGuards(c)
	if err != nil {
		return failure(stderr, err)
	}
	if asJSON {
		err = writeJSON(stdout, struct {
			Disks []guardItem `json:"disks"`
		}{items})
	} else {
		var text strings.Builder
		for i := range items {
			it := &items[i]
			state := "clear"
			if it.Tripped {
				state = "tripped"
			}
			fmt.Fprintf(&text, "%s, %s: %s, %d\n", it.Disk, it.Kind, state, it.Count)
			printPaths(&text, "  held", it.Paths)
		}
		err = writeText(stdout, text.String())
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

func readGuards(c *config.Config) (items []guardItem, err error) {
	ctx := context.Background()
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	settings, err := guardSettingsOf(c)
	if err != nil {
		return nil, err
	}
	machineNow, err := nowOf()
	if err != nil {
		return nil, err
	}
	items = []guardItem{}
	for i := range c.Disks {
		item, err := readGuard(ctx, db, &c.Disks[i], settings, machineNow)
		if err != nil {
			return nil, err
		}
		modified, err := readModifyGuard(ctx, db, c.Disks[i].ID, settings, machineNow)
		if err != nil {
			return nil, err
		}
		items = append(items, *item, *modified)
	}
	return items, nil
}

// readModifyGuard says what the mass-modification guard of a disk holds, from events: tripped while a modify-guard
// event stands, with the files it counted, as which files wait only a look can tell (specs.md §15.6).
func readModifyGuard(ctx context.Context, db *sql.DB, disk string, s *guardSettings, now int64) (*guardItem, error) {
	read, err := catalog.ReadModifyGuard(ctx, db, disk, now, s.modWindow)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	paths := []string{}
	if read.Stands {
		if paths, err = catalog.ModifiedPaths(ctx, db, disk, now, s.modWindow, read.AnswerID); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
	}
	return &guardItem{Disk: disk, Kind: jobs.GuardModifications, Tripped: read.Stands, Count: read.Count, Paths: pathItems(paths)}, nil
}

// nowOf reads the clock as a command that looks at no disk does: in a conformance build, that is the
// control file's.
func nowOf() (int64, error) {
	machine, err := disks.NewMachine()
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	return machine.Now(), nil
}

type guardSettings struct {
	files     int64
	percent   value.Percent
	window    int64
	modWindow int64
}

func guardSettingsOf(c *config.Config) (*guardSettings, error) {
	effective := c.Effective()
	files, _ := effective["deletes.mass_guard.files"].(int64)
	text, _ := effective["deletes.mass_guard.percent"].(string)
	percent, err := value.ParsePercent(text)
	if err != nil {
		return nil, fmt.Errorf("deletes.mass_guard.percent: %w", err)
	}
	text, _ = effective["deletes.mass_guard.window"].(string)
	window, err := value.ParseDuration(text)
	if err != nil {
		return nil, fmt.Errorf("deletes.mass_guard.window: %w", err)
	}
	text, _ = effective["modifies.mass_guard.window"].(string)
	modWindow, err := value.ParseDuration(text)
	if err != nil {
		return nil, fmt.Errorf("modifies.mass_guard.window: %w", err)
	}
	return &guardSettings{files: files, percent: percent, window: window.Nanoseconds(), modWindow: modWindow.Nanoseconds()}, nil
}

func readGuard(ctx context.Context, db *sql.DB, d *config.Disk, s *guardSettings, now int64) (*guardItem, error) {
	read, err := catalog.ReadGuard(ctx, db, d.ID, now, s.window)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	refused, err := jobs.RefusedCopies(ctx, db, d)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	wishes := int64(len(refused))
	count := read.Gone + read.Events + wishes
	rows, err := catalog.CopyCount(ctx, db, d.ID)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	// A hold that stands stands until a pass finds it over or someone answers: confirm and reject go by that too.
	tripped := read.HasGuard || solver.GuardTrips(&solver.GuardInput{Count: count, CountPercent: read.GoneLeft + read.EventsLeft + wishes, Rows: rows + read.Events, Files: s.files, Percent: s.percent})
	paths := []string{}
	if tripped {
		if paths, err = catalog.GuardPaths(ctx, db, d.ID, read.UpTo); err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		for i := range refused {
			paths = append(paths, refused[i].Place)
		}
		slices.Sort(paths)
	}
	return &guardItem{Disk: d.ID, Kind: jobs.GuardDeletions, Tripped: tripped, Count: count, Paths: pathItems(paths)}, nil
}

func guardAct(configPath, what string, args []string, stdout, stderr io.Writer) int {
	asJSON := false
	disk, kind := "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			if err := setFlag(&asJSON, a); err != nil {
				return usage(stderr, "guard: "+err.Error())
			}
		case a == "--disk" && i+1 < len(args) && disk == "" && args[i+1] != "":
			i++
			disk = args[i]
		case strings.HasPrefix(a, "--disk=") && disk == "" && a != "--disk=":
			disk = strings.TrimPrefix(a, "--disk=")
		case a == "--kind" && i+1 < len(args) && kind == "":
			i++
			kind = args[i]
		case strings.HasPrefix(a, "--kind=") && kind == "":
			kind = strings.TrimPrefix(a, "--kind=")
		default:
			return usage(stderr, fmt.Sprintf("guard %s: unknown argument %q", what, a))
		}
	}
	if disk == "" {
		return usage(stderr, fmt.Sprintf("guard %s: want --disk", what))
	}
	switch kind {
	case "":
		kind = jobs.GuardDeletions
	case jobs.GuardDeletions, jobs.GuardModifications:
	default:
		return usage(stderr, fmt.Sprintf("guard %s: --kind is deletions or modifications, not %q", what, kind))
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if !hasDisk(c, disk) {
		return failure(stderr, fmt.Errorf("guard: no disk %q in the configuration", disk))
	}
	upTo, err := actOnGuard(c, what, kind, disk, stderr)
	if err != nil {
		return failure(stderr, err)
	}
	answer := guardAnswer{Disk: disk, Kind: kind, Done: upTo.Valid}
	if upTo.Valid {
		at := timeText(upTo.Int64)
		answer.UpTo = &at
	}
	if asJSON {
		err = writeJSON(stdout, answer)
	} else if upTo.Valid {
		err = writeText(stdout, fmt.Sprintf("%s: %s up to %s\n", disk, what, *answer.UpTo))
	} else {
		err = writeText(stdout, fmt.Sprintf("%s: nothing to %s\n", disk, what))
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

func actOnGuard(c *config.Config, what, kind, id string, stderr io.Writer) (upTo sql.NullInt64, err error) {
	ctx := context.Background()
	now, err := nowOf()
	if err != nil {
		return upTo, err
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return upTo, fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if release, err = lockCatalog(ctx, c, stderr); err != nil {
		return upTo, fmt.Errorf("%w", err)
	}
	if kind == jobs.GuardModifications {
		event := catalog.EventModifyGuardConfirm
		if what == "reject" {
			event = catalog.EventModifyGuardReject
		}
		if upTo, err = catalog.AnswerModifyGuard(ctx, db, id, event, now); err != nil {
			return upTo, fmt.Errorf("catalog: %w", err)
		}
		return upTo, nil
	}
	var disk *config.Disk
	for i := range c.Disks {
		if c.Disks[i].ID == id {
			disk = &c.Disks[i]
		}
	}
	refused, err := jobs.RefusedCopies(ctx, db, disk)
	if err != nil {
		return upTo, fmt.Errorf("%w", err)
	}
	if what == "confirm" {
		upTo, err = catalog.ConfirmGuard(ctx, db, id, now, refused)
		if err != nil {
			return upTo, fmt.Errorf("catalog: %w", err)
		}
		return upTo, nil
	}
	upTo, err = catalog.RejectGuard(ctx, db, id, now, refused)
	if err != nil {
		return upTo, fmt.Errorf("catalog: %w", err)
	}
	return upTo, nil
}
