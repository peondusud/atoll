package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/solver"
	"github.com/peondusud/atoll/internal/watch"
)

type statusArgs struct {
	under  string
	asJSON bool
}

// parseStatus reads the arguments of status (specs.md §25).
func parseStatus(args []string) (statusArgs, error) {
	var out statusArgs
	given := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" {
			if err := setFlag(&out.asJSON, a); err != nil {
				return out, err
			}
			continue
		}
		value, isPath := strings.CutPrefix(a, "--path=")
		switch {
		case isPath:
		case a != "--path":
			return out, fmt.Errorf(T("unknown argument %q"), a)
		case i+1 == len(args):
			return out, errors.New(T("--path needs a path"))
		default:
			i++
			value = args[i]
		}
		if given {
			return out, errors.New(T("--path comes twice"))
		}
		given = true
		// Only its form counts here: a name a filesystem refuses may still be on a disk.
		if _, err := names.CheckPath(value, names.Ext4); err != nil {
			return out, fmt.Errorf("--path: %w", err)
		}
		out.under = value
	}
	return out, nil
}

// statusDisk is one disk of api/schemas/status.schema.json.
type statusDisk struct {
	ID       string  `json:"id"`
	State    string  `json:"state"`
	Mount    string  `json:"mount"`
	FS       string  `json:"fs"`
	Role     string  `json:"role"`
	Capacity *int64  `json:"capacity_bytes"`
	Free     *int64  `json:"free_bytes"`
	LastSeen *string `json:"last_seen"`
	Copies   int     `json:"copies"`
	Bytes    int64   `json:"bytes"`
	// NamesDisagree counts the operations left planned as a name and its file's record disagree on the disk's
	// volume, which only a repair of it resolves; left out when there is none (specs.md §17.2).
	NamesDisagree int `json:"names_disagree,omitempty"`
	// LinksUnnamed counts the links of the guard whose data nothing names, as its last run found them; left out when
	// there is none (specs.md §17).
	LinksUnnamed int `json:"links_unnamed,omitempty"`
}

// statusCopy is one copy of api/schemas/status.schema.json.
type statusCopy struct {
	Disk      string  `json:"disk"`
	State     string  `json:"state"`
	Size      *int64  `json:"size"`
	Mtime     *string `json:"mtime"`
	Created   *string `json:"created"`
	CreatedBy string  `json:"created_by"`
	FirstSeen string  `json:"first_seen"`
	Verified  *string `json:"verified"`
	OldPath   string  `json:"old_path,omitempty"`
	OldB64    string  `json:"old_path_b64,omitempty"`
}

// statusFile is one file of api/schemas/status.schema.json.
type statusFile struct {
	Path      string       `json:"path,omitempty"`
	PathB64   string       `json:"path_b64,omitempty"`
	State     string       `json:"state"`
	Size      int64        `json:"size"`
	Mtime     string       `json:"mtime"`
	Blake3    *string      `json:"blake3"`
	Replicas  int          `json:"replicas"`
	Mode      string       `json:"mode"`
	Rule      *string      `json:"rule"`
	Original  *string      `json:"original"`
	FirstSeen string       `json:"first_seen"`
	Copies    []statusCopy `json:"copies"`
}

// statusSummary is api/schemas/status.schema.json but for its files, which go out as they are read.
type statusSummary struct {
	Disks        []statusDisk   `json:"disks"`
	FilesTotal   int            `json:"files_total"`
	FilesAtLevel int            `json:"files_at_level"`
	BytesTotal   int64          `json:"bytes_total"`
	BytesAtLevel int64          `json:"bytes_at_level"`
	BytesPlaced  int64          `json:"bytes_placed"`
	ByState      map[string]int `json:"by_state"`
}

// statusWriter writes the files as they are read, then what is known once all have been.
type statusWriter interface {
	file(f *catalog.FileRow) error
	end(s *statusSummary) error
}

func runStatus(configPath string, args []string, stdout, stderr io.Writer) int {
	parsed, err := parseStatus(args)
	if err != nil {
		return usage(stderr, "status: "+err.Error())
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	if err := writeStatus(context.Background(), c, parsed, stdout); err != nil {
		return failure(stderr, err)
	}
	return exitOK
}

func writeStatus(ctx context.Context, c *config.Config, parsed statusArgs, stdout io.Writer) (err error) {
	db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	// One write for each file would cost more than reading it.
	buffered := bufio.NewWriter(stdout)
	var writer statusWriter = textStatus{buffered}
	if parsed.asJSON {
		writer = newJSONStatus(buffered)
	}
	misplaced, err := misplacedFiles(ctx, db, c)
	if err != nil {
		return err
	}
	status, err := watch.ReadStatus(ctx, db, parsed.under, misplaced, writer.file)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if err := writer.end(statusSummaryFrom(status)); err != nil {
		return err
	}
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf(T("writing the output: %w"), err)
	}
	return nil
}

// misplacedFiles gives the files with a misplaced copy, from a plan of the whole pool that reads the catalog only
// (specs.md §13.3). With no strict setting there is none, and no plan to make.
func misplacedFiles(ctx context.Context, db *sql.DB, c *config.Config) (map[string]bool, error) {
	if !c.StrictPlacement() {
		return nil, nil //nolint:nilnil // nothing misplaced is an answer
	}
	m, err := disks.NewMachine()
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	misplaced, err := jobs.MisplacedFiles(ctx, db, c, m.Now())
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return misplaced, nil
}

// timeText writes a time of specs.md §29.3: both versions write the same bytes.
func timeText(ns int64) string {
	return time.Unix(0, ns).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func isUTF8(s string) bool {
	return utf8.ValidString(s)
}

func nullTime(ns sql.NullInt64) *string {
	if !ns.Valid {
		return nil
	}
	text := timeText(ns.Int64)
	return &text
}

func nullInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

func nullText(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

func statusSummaryFrom(s *watch.Status) *statusSummary {
	out := &statusSummary{
		Disks:      make([]statusDisk, 0, len(s.Disks)),
		FilesTotal: s.FilesTotal, FilesAtLevel: s.FilesAtLevel, BytesTotal: s.BytesTotal, BytesAtLevel: s.BytesAtLevel, BytesPlaced: s.BytesPlaced,
		ByState: map[string]int{},
	}
	for _, state := range []solver.State{
		solver.StateOK, solver.StatePending, solver.StateDegraded, solver.StateUnprotected, solver.StateUnavailable,
		solver.StateCorrupt, solver.StateConflict, solver.StateNameIncompatible, solver.StateNameCollision,
	} {
		out.ByState[string(state)] = s.ByState[string(state)]
	}
	for i := range s.Disks {
		d := &s.Disks[i]
		disk := statusDisk{ID: d.ID, State: d.State, Mount: d.Mount, FS: d.FS, Role: d.Role, Copies: d.Copies, Bytes: d.Bytes, NamesDisagree: d.NamesDisagree, LinksUnnamed: d.LinksUnnamed}
		if d.Seen {
			lastSeen := timeText(d.LastSeenNS)
			disk.Capacity, disk.Free, disk.LastSeen = &d.Capacity, &d.Free, &lastSeen
		}
		out.Disks = append(out.Disks, disk)
	}
	return out
}

func statusFileFrom(f *catalog.FileRow) *statusFile {
	file := &statusFile{
		State: f.State, Size: f.Size, Mtime: timeText(f.MtimeNS), Replicas: f.ReplicasTarget, Mode: f.Mode,
		Rule: nullText(f.Rule), Original: nullText(f.Original), FirstSeen: timeText(f.FirstSeenNS),
		Copies: make([]statusCopy, 0, len(f.Copies)),
	}
	if utf8.ValidString(f.Path) {
		file.Path = f.Path
	} else {
		file.PathB64 = base64.StdEncoding.EncodeToString([]byte(f.Path))
	}
	if f.Hash != nil {
		text := hex.EncodeToString(f.Hash)
		file.Blake3 = &text
	}
	for i := range f.Copies {
		c := &f.Copies[i]
		item := statusCopy{
			Disk: c.Disk, State: c.State, Size: nullInt(c.Size), Mtime: nullTime(c.MtimeNS), Created: nullTime(c.BtimeNS),
			CreatedBy: c.CreatedBy, FirstSeen: timeText(c.FirstSeenNS), Verified: nullTime(c.VerifiedNS),
		}
		if c.OldPath != nil {
			item.OldPath, item.OldB64 = encodePath(*c.OldPath)
		}
		file.Copies = append(file.Copies, item)
	}
	return file
}

// jsonList writes a JSON object whose list goes out as it comes, and the rest after it: what the
// rest says is known only at the end, and holding a large list back until then would not fit in the
// memory of specs.md §26. The order of keys means nothing in JSON.
type jsonList struct {
	w      io.Writer
	key    string
	buffer bytes.Buffer
	enc    *json.Encoder
	listed bool
}

func newJSONList(w io.Writer, key string) *jsonList {
	j := &jsonList{w: w, key: key}
	j.enc = json.NewEncoder(&j.buffer)
	j.enc.SetEscapeHTML(false)
	j.enc.SetIndent("    ", "  ")
	return j
}

func (j *jsonList) item(v any) error {
	start := ",\n    "
	if !j.listed {
		start = "{\n  \"" + j.key + "\": [\n    "
	}
	j.listed = true
	j.buffer.Reset()
	if err := j.enc.Encode(v); err != nil {
		return fmt.Errorf(T("writing the output: %w"), err)
	}
	return writeText(j.w, start+strings.TrimSuffix(j.buffer.String(), "\n"))
}

// end writes the rest, an object whose keys join those of the list.
func (j *jsonList) end(rest any) error {
	start := "{\n  \"" + j.key + "\": [],"
	if j.listed {
		start = "\n  ],"
	}
	var text strings.Builder
	if err := writeJSON(&text, rest); err != nil {
		return err
	}
	// The object is open already, so its own opening brace goes.
	return writeText(j.w, start+strings.TrimPrefix(text.String(), "{"))
}

// close ends an object that holds only its list.
func (j *jsonList) close() error {
	if j.listed {
		return writeText(j.w, "\n  ]\n}\n")
	}
	return writeText(j.w, "{\n  \""+j.key+"\": []\n}\n")
}

// jsonStatus writes api/schemas/status.schema.json.
type jsonStatus struct {
	*jsonList
}

func newJSONStatus(w io.Writer) jsonStatus {
	return jsonStatus{newJSONList(w, "files")}
}

func (j jsonStatus) file(f *catalog.FileRow) error {
	return j.item(statusFileFrom(f))
}

func (j jsonStatus) end(s *statusSummary) error {
	return j.jsonList.end(s)
}

// textStatus writes for a reader, the summary last, where a long list leaves it in sight.
type textStatus struct {
	w io.Writer
}

func (t textStatus) file(f *catalog.FileRow) error {
	var text strings.Builder
	printStatusFile(&text, statusFileFrom(f))
	return writeText(t.w, text.String())
}

func (t textStatus) end(s *statusSummary) error {
	var text strings.Builder
	for i := range s.Disks {
		d := &s.Disks[i]
		fmt.Fprintf(&text, T("%s: %s (%s, %s), %d copies, %d bytes"), d.ID, d.State, d.Mount, d.FS, d.Copies, d.Bytes)
		if d.LastSeen != nil {
			fmt.Fprintf(&text, T(", %d of %d bytes free, seen %s"), *d.Free, *d.Capacity, *d.LastSeen)
		}
		text.WriteString("\n")
		if d.LinksUnnamed > 0 {
			fmt.Fprintf(&text, T("  %d links of the guard hold data nothing names: they stay in Atoll/links, for you to look at\n"), d.LinksUnnamed)
		}
		if d.NamesDisagree > 0 {
			fmt.Fprintf(&text, T("  %d operations stay planned: names and records disagree on its volume, which needs a repair with chkdsk /f on Windows\n"), d.NamesDisagree)
		}
	}
	fmt.Fprintf(&text, T("files: %d, at their level: %d%s\n"), s.FilesTotal, s.FilesAtLevel, share(int64(s.FilesAtLevel), int64(s.FilesTotal)))
	fmt.Fprintf(&text, T("bytes: %d, at their level: %d%s\n"), s.BytesTotal, s.BytesAtLevel, share(s.BytesAtLevel, s.BytesTotal))
	fmt.Fprintf(&text, T("bytes correctly placed: %d%s\n"), s.BytesPlaced, share(s.BytesPlaced, s.BytesTotal))
	for _, state := range []string{"unavailable", "corrupt", "conflict", "name-incompatible", "name-collision", "degraded", "pending", "unprotected", "ok"} {
		if n := s.ByState[state]; n > 0 {
			fmt.Fprintf(&text, "  %s: %d\n", state, n)
		}
	}
	return writeText(t.w, text.String())
}

func share(part, total int64) string {
	if total == 0 {
		return ""
	}
	return fmt.Sprintf(" (%.1f %%)", 100*float64(part)/float64(total))
}

func printStatusFile(text *strings.Builder, f *statusFile) {
	fmt.Fprintf(text, T("%s: %s, %d bytes, modified %s, wants %d copies\n"), shownB64(f.Path, f.PathB64), f.State, f.Size, f.Mtime, f.Replicas+1)
	for i := range f.Copies {
		c := &f.Copies[i]
		fmt.Fprintf(text, "  %s: %s", c.Disk, c.State)
		if c.Mtime != nil {
			fmt.Fprintf(text, T(", modified %s"), *c.Mtime)
		}
		if c.Created != nil {
			fmt.Fprintf(text, T(", created %s"), *c.Created)
		}
		if f.Original != nil && *f.Original == c.Disk {
			text.WriteString(T(", original"))
		}
		text.WriteString("\n")
	}
}
