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
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/metrics"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/telemetry"
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

// daemonGuard is one guard of api/schemas/daemon.schema.json. Window is there for a mass-modification guard its day
// tripped alone, so that every other line stays as it was.
type daemonGuard struct {
	Disk   string `json:"disk"`
	Kind   string `json:"kind"`
	Count  int64  `json:"count"`
	Window string `json:"window,omitempty"`
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
			return usage(stderr, fmt.Sprintf(T("daemon: unknown argument %q"), a))
		}
		if err := setFlag(flag, a); err != nil {
			return usage(stderr, "daemon: "+err.Error())
		}
	}
	if !untilIdle && asJSON {
		return usage(stderr, T("daemon: --json goes with --until-idle"))
	}
	// The daemon that runs for good listens for a hangup before it reads anything: one that comes while its
	// configuration loads, as a disk command run then sends, ends a process that does not listen (specs.md §15.13).
	var hangups chan os.Signal
	if !untilIdle {
		hangups = make(chan os.Signal, 1)
		signal.Notify(hangups, syscall.SIGHUP)
		defer signal.Stop(hangups)
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if !untilIdle {
		return runForGood(configPath, c, !dryRun, stderr, hangups)
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

// runForGood runs the daemon until SIGTERM or SIGINT, reading the configuration again on SIGHUP (specs.md
// §15.13), which hangups has given since before the configuration was first read. Atoll never detaches itself:
// --foreground changes nothing.
func runForGood(configPath string, c *config.Config, apply bool, stderr io.Writer, hangups <-chan os.Signal) int {
	machine, err := newMachine()
	if err != nil {
		return failure(stderr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	reload := make(chan struct{}, 1)
	go func() {
		for range hangups {
			select {
			case reload <- struct{}{}:
			default:
			}
		}
	}()
	logs := logsFor(stderr)
	log := slog.New(logs)
	logWarnings(log, c)
	// The local API answers with the configuration the daemon runs with, the one a reload takes too (specs.md §25.1).
	// What the loop records, /metrics gives (specs.md §23.1), and the export sends, as the log, when [telemetry] asks
	// it, from that configuration too (§23.2). Its answers to a guard go through the loop, which runs each under the
	// catalog's lock, and reload it as SIGHUP does.
	rec := metrics.NewRecorder(atollVersion())
	tel := telemetry.NewManager(telemetry.Options{Logs: logs, Metrics: rec, Version: atollVersion()})
	applyTelemetry(ctx, tel, c, log)
	defer stopTelemetry(tel, log)
	api := newAPIServer(c, machine, log, rec)
	work := make(chan *daemon.CatalogWork)
	api.answerThrough(work, reload, apply)
	stopAPI := api.start(ctx, c)
	defer stopAPI()
	load := func() (*config.Config, error) {
		next, err := load(configPath)
		if err == nil {
			api.current.Store(next)
			applyTelemetry(ctx, tel, next, log)
			logWarnings(log, next)
		}
		return next, err
	}
	opts := daemon.Options{Config: c, Machine: machine, Apply: apply, Log: log, Reload: reload, Load: load, Metrics: rec, Catalog: work}
	if err := daemon.Run(ctx, opts); err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

// applyTelemetry gives a process that runs for good the format of its log and the export [telemetry] asks (specs.md
// §23.2). An export that cannot start leaves the process running without it, as a collector that is gone does.
func applyTelemetry(ctx context.Context, tel *telemetry.Manager, c *config.Config, log *slog.Logger) {
	settings, err := c.Telemetry()
	if err == nil {
		err = tel.Apply(ctx, settings)
	}
	if err != nil {
		log.Warn("the OTLP export cannot start: the process runs without it", "error", err)
	}
}

// stopTelemetry sends what the export holds, the last metrics and the records queued, within a few seconds at most.
func stopTelemetry(tel *telemetry.Manager, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryStop)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		log.Warn("the OTLP export did not send all it held before the stop", "error", err)
	}
}

// telemetryStop is the most a stop waits for the export: a collector that is gone must not hold up systemctl stop.
const telemetryStop = 5 * time.Second

func runPass(c *config.Config, apply bool, sink jobs.Sink, stderr io.Writer) (outcome *jobs.Outcome, err error) {
	ctx := context.Background()
	machine, err := newMachine()
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
		g := &o.Guards[i]
		rest.Guards = append(rest.Guards, daemonGuard{Disk: g.Disk, Kind: g.Kind, Count: g.Count, Window: g.Window})
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
		return "", fmt.Errorf(T("writing the output: %w"), err)
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
			// The message of a mount check may hold an error of the system, which names a path as it is (specs.md §25).
			fmt.Fprintf(&text, "%s: %s: %s: %s\n", d.ID, d.State, *d.Reason, names.Escaped(*d.Message))
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
		text.WriteString(T("a dry run: nothing was written\n"))
	}
	for i := range o.Guards {
		g := &o.Guards[i]
		if g.Kind == jobs.GuardModifications && g.Window == jobs.WindowDay {
			fmt.Fprintf(&text, T("%s: the mass-modification guard holds what changed after %d files modified within a day: atoll guard list\n"), g.Disk, g.Count)
			continue
		}
		if g.Kind == jobs.GuardModifications {
			fmt.Fprintf(&text, T("%s: the mass-modification guard holds what changed after %d files modified: atoll guard list\n"), g.Disk, g.Count)
			continue
		}
		fmt.Fprintf(&text, T("%s: the mass-deletion guard holds %d copies: atoll guard list\n"), g.Disk, g.Count)
	}
	if o.NextDue.Valid {
		fmt.Fprintf(&text, T("%d copies wait until %s\n"), o.Waiting, timeText(o.NextDue.Int64))
	}
	return writeText(t.w, text.String())
}

// shownB64 is shownPath for a path as an output holds it: in path, or in base64 when it is not UTF-8.
func shownB64(path, b64 string) string {
	if b64 != "" {
		return names.Base64Shown + b64
	}
	return shownPath(path)
}

// guardItem is one disk of api/schemas/guard-list.schema.json. Window is there for a mass-modification guard that
// stands as its day tripped it, whose count and paths are then the day's (specs.md §15.6).
type guardItem struct {
	Disk    string     `json:"disk"`
	Kind    string     `json:"kind"`
	Tripped bool       `json:"tripped"`
	Count   int64      `json:"count"`
	Paths   []pathItem `json:"paths"`
	Window  string     `json:"window,omitempty"`
}

// guardAnswer is the output of api/schemas/guard-answer.schema.json. DryRun and Paths are there with --dry-run alone, so
// that an answer's output stays as it was.
type guardAnswer struct {
	Disk   string      `json:"disk"`
	Kind   string      `json:"kind"`
	Done   bool        `json:"done"`
	UpTo   *string     `json:"up_to"`
	DryRun bool        `json:"dry_run,omitempty"`
	Paths  *[]pathItem `json:"paths,omitempty"`
}

// guardRequest is an answer to a guard: what atoll guard confirm and reject are given, and what the local API's
// POST /v1/guards/{id} is given (specs.md §15.6, §25.1).
type guardRequest struct {
	what   string // "confirm" or "reject"
	kind   string // jobs.GuardDeletions or jobs.GuardModifications
	disk   string
	dryRun bool
	// upTo, which the local API alone gives, is the time of the guard event the answer is for: a guard that stands
	// since another time is not answered (errGuardMoved).
	upTo sql.NullInt64
}

// errGuardMoved says that the guard an answer is for is not the one that stands: nothing was answered.
var errGuardMoved = errors.New("the guard that stands is not the one the answer is for: nothing was answered")

func runGuard(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, T("guard: want list, confirm or reject"))
	}
	switch args[0] {
	case "list":
		return guardList(configPath, args[1:], stdout, stderr)
	case "confirm", "reject":
		return guardAct(configPath, args[0], args[1:], stdout, stderr)
	default:
		return usage(stderr, fmt.Sprintf(T("guard: unknown command %q"), args[0]))
	}
}

func guardList(configPath string, args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf(T("guard list: unknown argument %q"), a))
		}
		if err := setFlag(&asJSON, a); err != nil {
			return usage(stderr, "guard list: "+err.Error())
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	items, err := readGuards(context.Background(), c)
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
			day := ""
			if it.Window == jobs.WindowDay {
				day = T(" within a day")
			}
			fmt.Fprintf(&text, "%s, %s: %s, %d%s\n", it.Disk, it.Kind, state, it.Count, day)
			printPaths(&text, "  held", it.Paths)
		}
		err = writeText(stdout, text.String())
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

func readGuards(ctx context.Context, c *config.Config) (items []guardItem, err error) {
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
		item, _, err := readGuard(ctx, db, &c.Disks[i], settings, machineNow)
		if err != nil {
			return nil, err
		}
		modified, _, err := readModifyGuard(ctx, db, c.Disks[i].ID, settings, machineNow)
		if err != nil {
			return nil, err
		}
		items = append(items, *item, *modified)
	}
	return items, nil
}

// readModifyGuard says what the mass-modification guard of a disk holds, from events: tripped while a modify-guard
// event stands, with the files it counted, as which files wait only a look can tell (specs.md §15.6); over its day
// when its day tripped it, and over its window otherwise. It gives the time of the event that stands, which an answer
// would go up to, invalid with none.
func readModifyGuard(ctx context.Context, db *sql.DB, disk string, s *guardSettings, now int64) (*guardItem, sql.NullInt64, error) {
	read, err := catalog.ReadModifyGuard(ctx, db, disk, now, s.modWindow, s.dayWindow)
	if err != nil {
		return nil, sql.NullInt64{}, fmt.Errorf("catalog: %w", err)
	}
	item := &guardItem{Disk: disk, Kind: jobs.GuardModifications, Tripped: read.Stands, Count: read.Count, Paths: []pathItem{}}
	window, after := s.modWindow, read.AnswerID
	if read.Stands && read.Window == jobs.WindowDay {
		item.Count, item.Window = read.DayCount, jobs.WindowDay
		window, after = s.dayWindow, read.DayAnswerID
	}
	if read.Stands {
		paths, err := catalog.ModifiedPaths(ctx, db, disk, now, window, after)
		if err != nil {
			return nil, sql.NullInt64{}, fmt.Errorf("catalog: %w", err)
		}
		item.Paths = pathItems(paths)
	}
	stands := sql.NullInt64{Int64: read.GuardNS, Valid: read.Stands}
	return item, stands, nil
}

// nowOf reads the clock as a command that looks at no disk does: in a conformance build, that is the
// control file's.
func nowOf() (int64, error) {
	machine, err := newMachine()
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
	dayWindow int64
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
	text, _ = effective["modifies.day_guard.window"].(string)
	dayWindow, err := value.ParseDuration(text)
	if err != nil {
		return nil, fmt.Errorf("modifies.day_guard.window: %w", err)
	}
	return &guardSettings{files: files, percent: percent, window: window.Nanoseconds(), modWindow: modWindow.Nanoseconds(), dayWindow: dayWindow.Nanoseconds()}, nil
}

// readGuard says what the mass-deletion guard of a disk holds, from rows and events (specs.md §15.6). It gives the time
// of the guard event that stands, which an answer would go up to, invalid with none.
func readGuard(ctx context.Context, db *sql.DB, d *config.Disk, s *guardSettings, now int64) (*guardItem, sql.NullInt64, error) {
	read, err := catalog.ReadGuard(ctx, db, d.ID, now, s.window)
	if err != nil {
		return nil, sql.NullInt64{}, fmt.Errorf("catalog: %w", err)
	}
	refused, err := jobs.RefusedCopies(ctx, db, d)
	if err != nil {
		return nil, sql.NullInt64{}, fmt.Errorf("%w", err)
	}
	wishes := int64(len(refused))
	count := read.Gone + read.Events + wishes
	rows, err := catalog.CopyCount(ctx, db, d.ID)
	if err != nil {
		return nil, sql.NullInt64{}, fmt.Errorf("catalog: %w", err)
	}
	// A hold that stands stands until a pass finds it over or someone answers: confirm and reject go by that too.
	tripped := read.HasGuard || solver.GuardTrips(&solver.GuardInput{Count: count, CountPercent: read.GoneLeft + read.EventsLeft + wishes, Rows: rows + read.Events, Files: s.files, Percent: s.percent})
	paths := []string{}
	if tripped {
		if paths, err = catalog.GuardPaths(ctx, db, d.ID, read.UpTo); err != nil {
			return nil, sql.NullInt64{}, fmt.Errorf("catalog: %w", err)
		}
		for i := range refused {
			paths = append(paths, refused[i].Place)
		}
		slices.Sort(paths)
	}
	stands := sql.NullInt64{Int64: read.GuardNS, Valid: read.HasGuard}
	return &guardItem{Disk: d.ID, Kind: jobs.GuardDeletions, Tripped: tripped, Count: count, Paths: pathItems(paths)}, stands, nil
}

func guardAct(configPath, what string, args []string, stdout, stderr io.Writer) int {
	asJSON, dryRun := false, false
	disk, kind := "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			if err := setFlag(&asJSON, a); err != nil {
				return usage(stderr, "guard: "+err.Error())
			}
		case a == "--dry-run":
			if err := setFlag(&dryRun, a); err != nil {
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
			return usage(stderr, fmt.Sprintf(T("guard %s: unknown argument %q"), what, a))
		}
	}
	if disk == "" {
		return usage(stderr, fmt.Sprintf(T("guard %s: want --disk"), what))
	}
	switch kind {
	case "":
		kind = jobs.GuardDeletions
	case jobs.GuardDeletions, jobs.GuardModifications:
	default:
		return usage(stderr, fmt.Sprintf(T("guard %s: --kind is deletions or modifications, not %q"), what, kind))
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if !hasDisk(c, disk) {
		return failure(stderr, fmt.Errorf(T("guard: no disk %q in the configuration"), disk))
	}
	req := &guardRequest{what: what, kind: kind, disk: disk, dryRun: dryRun}
	answer, err := actOnGuard(c, req, stderr)
	if err != nil {
		return failure(stderr, err)
	}
	if answer.Done {
		tellOfAnswer(configPath, c, kind, stderr)
	}
	if asJSON {
		err = writeJSON(stdout, answer)
	} else {
		err = writeText(stdout, guardText(req, answer))
	}
	if err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

// guardText is the human output of an answer to a guard, or of its dry run.
func guardText(req *guardRequest, answer *guardAnswer) string {
	var text strings.Builder
	if req.dryRun {
		text.WriteString(T("a dry run: nothing was written\n"))
	}
	switch {
	case req.dryRun && answer.UpTo != nil && req.what == "reject":
		fmt.Fprintf(&text, T("%s: a reject would go up to %s\n"), req.disk, *answer.UpTo)
	case req.dryRun && answer.UpTo != nil:
		fmt.Fprintf(&text, T("%s: a confirm would go up to %s\n"), req.disk, *answer.UpTo)
	case answer.UpTo != nil:
		format := "%s: confirm up to %s\n"
		if req.what == "reject" {
			format = "%s: reject up to %s\n"
		}
		fmt.Fprintf(&text, T(format), req.disk, *answer.UpTo)
	default:
		format := "%s: nothing to confirm\n"
		if req.what == "reject" {
			format = "%s: nothing to reject\n"
		}
		fmt.Fprintf(&text, T(format), req.disk)
	}
	if answer.Paths != nil {
		printPaths(&text, "  held", *answer.Paths)
	}
	return text.String()
}

// tellOfAnswer tells the daemon that runs for good of an answer to a guard, as systemctl reload atoll would, so that
// its pass takes the answer at once: an answer to the mass-modification guard holds for the guard's window only
// (specs.md §15.6). The answer is recorded either way: a daemon that could not be told, or none running, is a line of
// standard error, which says how long a pass has to start.
func tellOfAnswer(configPath string, c *config.Config, kind string, stderr io.Writer) {
	told, err := tellDaemon(configPath, c)
	if err == nil && told > 0 {
		return
	}
	window := ""
	if kind == jobs.GuardModifications {
		window, _ = c.Effective()["modifies.mass_guard.window"].(string)
	}
	switch {
	case err != nil && window != "":
		report(stderr, fmt.Sprintf(T("the daemon was not told, %v: the answer holds for %s, for a pass that starts by then (systemctl reload atoll)"), err, window))
	case err != nil:
		report(stderr, fmt.Sprintf(T("the daemon was not told, %v: its next pass takes the answer (systemctl reload atoll)"), err))
	case window != "":
		report(stderr, fmt.Sprintf(T("no daemon runs: the answer holds for %s, for a pass that starts by then"), window))
	}
}

// actOnGuard answers a guard as atoll guard confirm and reject do: on the catalog, under its lock, which it waits for
// as every command does.
func actOnGuard(c *config.Config, req *guardRequest, stderr io.Writer) (answer *guardAnswer, err error) {
	ctx := context.Background()
	now, err := nowOf()
	if err != nil {
		return nil, err
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
	return answerGuardOn(ctx, db, c, req, now)
}

// answerGuardOn answers a guard of a disk on the catalog, whose lock the caller holds; or, with a dry run, it says what
// the answer would be about, the time it would go up to and what the guard holds, as atoll guard list says it, and
// writes nothing. It is what atoll guard confirm and reject do, and what the local API's answer does, by this same code
// (specs.md §15.6, §25.1). An answer writes its event before anything it lets go happens, in the transaction that
// records it.
func answerGuardOn(ctx context.Context, db *sql.DB, c *config.Config, req *guardRequest, now int64) (*guardAnswer, error) {
	answer := &guardAnswer{Disk: req.disk, Kind: req.kind}
	if req.dryRun || req.upTo.Valid {
		item, stands, err := readOneGuard(ctx, db, c, req, now)
		if err != nil {
			return nil, err
		}
		if req.upTo.Valid && stands.Valid && stands.Int64 != req.upTo.Int64 {
			return nil, fmt.Errorf("%w: it stands since %s, not %s", errGuardMoved, timeText(stands.Int64), timeText(req.upTo.Int64))
		}
		if req.dryRun {
			held := []pathItem{}
			if stands.Valid {
				at := timeText(stands.Int64)
				answer.UpTo, held = &at, item.Paths
			}
			answer.DryRun, answer.Paths = true, &held
			return answer, nil
		}
	}
	upTo, err := recordGuardAnswer(ctx, db, c, req, now)
	if err != nil {
		return nil, err
	}
	if upTo.Valid {
		at := timeText(upTo.Int64)
		answer.Done, answer.UpTo = true, &at
	}
	return answer, nil
}

// readOneGuard reads one guard of a disk as atoll guard list does, with the time of the event that stands.
func readOneGuard(ctx context.Context, db *sql.DB, c *config.Config, req *guardRequest, now int64) (*guardItem, sql.NullInt64, error) {
	settings, err := guardSettingsOf(c)
	if err != nil {
		return nil, sql.NullInt64{}, err
	}
	if req.kind == jobs.GuardModifications {
		return readModifyGuard(ctx, db, req.disk, settings, now)
	}
	return readGuard(ctx, db, configDisk(c, req.disk), settings, now)
}

// recordGuardAnswer records a confirm or a reject, with the time of the guard event it goes up to, invalid when no
// guard stood and nothing was answered (specs.md §15.6).
func recordGuardAnswer(ctx context.Context, db *sql.DB, c *config.Config, req *guardRequest, now int64) (upTo sql.NullInt64, err error) {
	if req.kind == jobs.GuardModifications {
		event := catalog.EventModifyGuardConfirm
		if req.what == "reject" {
			event = catalog.EventModifyGuardReject
		}
		if upTo, err = catalog.AnswerModifyGuard(ctx, db, req.disk, event, now); err != nil {
			return upTo, fmt.Errorf("catalog: %w", err)
		}
		return upTo, nil
	}
	refused, err := jobs.RefusedCopies(ctx, db, configDisk(c, req.disk))
	if err != nil {
		return upTo, fmt.Errorf("%w", err)
	}
	if req.what == "confirm" {
		upTo, err = catalog.ConfirmGuard(ctx, db, req.disk, now, refused)
		if err != nil {
			return upTo, fmt.Errorf("catalog: %w", err)
		}
		return upTo, nil
	}
	upTo, err = catalog.RejectGuard(ctx, db, req.disk, now, refused)
	if err != nil {
		return upTo, fmt.Errorf("catalog: %w", err)
	}
	return upTo, nil
}

// configDisk gives the disk of the configuration with that id, nil when it lists none.
func configDisk(c *config.Config, id string) *config.Disk {
	for i := range c.Disks {
		if c.Disks[i].ID == id {
			return &c.Disks[i]
		}
	}
	return nil
}
