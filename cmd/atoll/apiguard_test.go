package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/watch"
)

// folderMachine takes the folders of a configuration's disks for mounted disks, as a conformance build does, with a
// clock of its own.
type folderMachine struct {
	disks.Machine
	list []config.Disk
	now  atomic.Int64
}

func (m *folderMachine) Mount(mount string) disks.MountSeen {
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{
		MountPoint: mount, Root: "/", FSType: "ext4", MountOptions: []string{"rw"}, SuperOptions: []string{"rw"},
	}}
}

func (m *folderMachine) Device(_ names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	for i := range m.list {
		if m.list[i].Mount == entry.MountPoint {
			return disks.DeviceSeen{Device: disks.DeviceOK, Identity: &disks.Identity{ID: m.list[i].UUID}}
		}
	}
	return disks.DeviceSeen{Device: disks.DeviceGoneAway}
}

func (m *folderMachine) ReadPastCache(fs names.Filesystem, entry *disks.MountEntry) disks.DeviceSeen {
	return m.Device(fs, entry)
}

func (m *folderMachine) Root(string, string) disks.RootSeen {
	return disks.RootSeen{Folder: disks.FolderOK}
}
func (m *folderMachine) Hiberfil(string, *disks.MountEntry) disks.HiberfilSeen {
	return disks.HiberfilSeen{File: disks.HiberfilAbsent}
}
func (m *folderMachine) Now() int64 { return m.now.Load() }

// guardedPool is a pool of two disks that are folders, each holding a, b and c, whose guard allows one deletion: the
// three are deleted from t1, and a pass holds them, as in conformance/scenarios/daemon-guard.json.
type guardedPool struct {
	configPath string
	c          *config.Config
	m          *folderMachine
}

func newGuardedPool(t *testing.T, base int64) *guardedPool {
	t.Helper()
	top := t.TempDir()
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(filepath.Join(top, "catalog.db")) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(top, "pool")) + "\n" +
		"[deletes.mass_guard]\nfiles = 1\npercent = \"100%\"\n"
	for i, id := range []string{"t1", "t2"} {
		mount := filepath.Join(top, id)
		if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"a", "b", "c"} {
			file := filepath.Join(mount, "media", name)
			if err := os.WriteFile(file, []byte(name), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(file, time.Unix(1_600_000_000, 0), time.Unix(1_600_000_000, 0)); err != nil {
				t.Fatal(err)
			}
		}
		text += "[[disk]]\nid = \"" + id + "\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3" + strconv.Itoa(i) + "\"\nmount = " + strconv.Quote(mount) + "\nfs = \"ext4\"\n"
	}
	p := &guardedPool{configPath: filepath.Join(top, "atoll.toml")}
	if err := os.WriteFile(p.configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	if p.c, err = load(p.configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), catalogPath(p.c), "p"); err != nil {
		t.Fatal(err)
	}
	real, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	p.m = &folderMachine{Machine: real, list: p.c.Disks}
	p.m.now.Store(base)
	db := p.open(t)
	if _, err := watch.Scan(t.Context(), db, p.c, p.m, watch.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	p.close(t, db)
	p.m.now.Add(time.Minute.Nanoseconds())
	for _, name := range []string{"a", "b", "c"} {
		if err := os.Remove(filepath.Join(p.c.Disks[0].Mount, "media", name)); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if o := p.pass(t); !o.Held || len(o.Decisions) != 0 {
			t.Fatalf("the guard does not hold: %+v", o)
		}
		p.m.now.Add(30 * time.Second.Nanoseconds())
	}
	return p
}

// clockBase is where the clocks of a test's pools start: an hour behind the machine's clock, which atoll guard reads, so
// that an answer comes after what the passes recorded; the same for twin pools, whose catalogs then hold the same times.
func clockBase() int64 { return time.Now().Add(-time.Hour).UnixNano() }

func (p *guardedPool) open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := catalog.OpenPool(t.Context(), catalogPath(p.c), "p")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func (p *guardedPool) close(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// pass runs a pass of atoll daemon --until-idle on the pool, as its clock says.
func (p *guardedPool) pass(t *testing.T) *jobs.Outcome {
	t.Helper()
	db := p.open(t)
	defer p.close(t, db)
	o, err := jobs.RunTo(t.Context(), db, p.c, p.m, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// tables gives what an answer writes to the catalog, its times aside: the events, the jobs, the copies and the files.
func (p *guardedPool) tables(t *testing.T) string {
	t.Helper()
	db := p.open(t)
	defer p.close(t, db)
	var out strings.Builder
	for _, query := range []string{
		"SELECT id, level, kind, data FROM events ORDER BY id",
		"SELECT id, kind, file_id, src_disk, dst_disk, state, reason FROM jobs ORDER BY id",
		"SELECT file_id, disk_id, state, gone_kind, gone_ns, CAST(old_lpath AS TEXT) FROM copies ORDER BY file_id, disk_id",
		"SELECT id, CAST(lpath AS TEXT), state FROM files ORDER BY id",
	} {
		for _, line := range queryLines(t, db, query) {
			out.WriteString(line + "\n")
		}
	}
	return out.String()
}

// queryLines gives each row of a query as a line.
func queryLines(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%v", values))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// directly runs an answer of the local API as a command runs it, under the catalog's lock, for the tests that run no
// daemon's loop.
func directly(c *config.Config) func(ctx context.Context, do func(ctx context.Context, db *sql.DB) error) error {
	return func(ctx context.Context, do func(ctx context.Context, db *sql.DB) error) (err error) {
		db, err := catalog.OpenPool(ctx, catalogPath(c), c.PoolID)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		release, err := fsx.WaitLockFile(ctx, catalogPath(c), nil)
		if err != nil {
			return errors.Join(fmt.Errorf("%w", err), db.Close())
		}
		defer release()
		defer func() { err = errors.Join(err, db.Close()) }()
		return do(ctx, db)
	}
}

// answerAPI is a local API that answers guards directly, and counts what it told the daemon.
func answerAPI(t *testing.T, c *config.Config) (*apiServer, *atomic.Int32) {
	t.Helper()
	a := newTestAPI(t, c)
	told := &atomic.Int32{}
	a.onCatalog, a.apply, a.told = directly(c), true, func() { told.Add(1) }
	return a, told
}

func post(h http.Handler, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func runCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return stdout.String(), code
}

// The API's answer is atoll guard confirm's or reject's, through the same code: the same output, byte for byte, its
// dry run as --dry-run's, the same rows and events in the catalog, and then the same pass, which lets the held
// deletions go after a confirm and keeps the files after a reject (specs.md §15.6, §25.1).
func TestTheAPIAnswersAGuardAsTheCommand(t *testing.T) {
	for _, what := range []string{"confirm", "reject"} {
		t.Run(what, func(t *testing.T) {
			base := clockBase()
			byCLI, byAPI := newGuardedPool(t, base), newGuardedPool(t, base)
			a, told := answerAPI(t, byAPI.c)
			h := a.routes()
			before := byAPI.tables(t)
			want, code := runCLI(t, "--config", byCLI.configPath, "guard", what, "--disk", "t1", "--dry-run", "--json")
			w := post(h, "/v1/guards/t1", `{"answer": "`+what+`", "dry_run": true}`, "")
			if code != exitOK || w.Code != http.StatusOK || w.Body.String() != want || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("the dry run: exit %d, %d %s\nwant\n%s", code, w.Code, w.Body.String(), want)
			}
			var dry struct {
				UpTo   *string           `json:"up_to"`
				DryRun bool              `json:"dry_run"`
				Paths  []json.RawMessage `json:"paths"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &dry); err != nil || dry.UpTo == nil || !dry.DryRun || len(dry.Paths) != 3 {
				t.Errorf("the dry run says %s", w.Body.String())
			}
			if after := byAPI.tables(t); after != before || told.Load() != 0 {
				t.Fatalf("the dry run wrote to the catalog, or told the daemon %d times:\n%s\nwas\n%s", told.Load(), after, before)
			}
			want, code = runCLI(t, "--config", byCLI.configPath, "guard", what, "--disk", "t1", "--json")
			w = post(h, "/v1/guards/t1", `{"answer": "`+what+`", "kind": "deletions", "yes": true, "up_to": `+strconv.Quote(*dry.UpTo)+`}`, "")
			if code != exitOK || w.Code != http.StatusOK || w.Body.String() != want || !strings.Contains(want, `"done": true`) {
				t.Fatalf("the answer: exit %d, %d %s\nwant\n%s", code, w.Code, w.Body.String(), want)
			}
			if told.Load() != 1 {
				t.Errorf("the daemon was told %d times", told.Load())
			}
			if cli, api := byCLI.tables(t), byAPI.tables(t); cli != api {
				t.Errorf("the catalogs differ:\n%s\nfrom the API\n%s", cli, api)
			}
			// The guard was answered: a second answer has nothing to answer, as a second command has.
			want, _ = runCLI(t, "--config", byCLI.configPath, "guard", what, "--disk", "t1", "--json")
			if w := post(h, "/v1/guards/t1", `{"answer": "`+what+`", "yes": true}`, ""); w.Code != http.StatusOK || w.Body.String() != want || !strings.Contains(want, `"done": false`) {
				t.Errorf("answered again: %d %s, want %s", w.Code, w.Body.String(), want)
			}
			if told.Load() != 1 {
				t.Errorf("an answer that did nothing told the daemon")
			}
			fromCLI, fromAPI := byCLI.pass(t), byAPI.pass(t)
			if !reflect.DeepEqual(fromCLI.Decisions, fromAPI.Decisions) || !reflect.DeepEqual(fromCLI.Operations, fromAPI.Operations) || fromCLI.Held != fromAPI.Held {
				t.Errorf("the passes differ: %+v %+v\nfrom the API %+v %+v", fromCLI.Decisions, fromCLI.Operations, fromAPI.Decisions, fromAPI.Operations)
			}
			if what == "confirm" && (len(fromAPI.Decisions) != 3 || len(fromAPI.Operations) != 3 || fromAPI.Held) {
				t.Errorf("after a confirm: %+v %+v", fromAPI.Decisions, fromAPI.Operations)
			}
			if what == "reject" && len(fromAPI.Decisions) != 0 {
				t.Errorf("after a reject: %+v", fromAPI.Decisions)
			}
			for _, name := range []string{"a", "b", "c"} {
				_, err := os.Stat(filepath.Join(byAPI.c.Disks[1].Mount, "media", name))
				if gone := os.IsNotExist(err); gone != (what == "confirm") {
					t.Errorf("%s on t2 after a %s: %v", name, what, err)
				}
			}
		})
	}
}

// The answer to the mass-modification guard too, with the window it holds for (specs.md §15.6).
func TestTheAPIAnswersTheModifyGuardAsTheCommand(t *testing.T) {
	for _, what := range []string{"confirm", "reject"} {
		byCLI, c1 := apiPool(t, "[modifies.mass_guard]\nwindow = \"7m\"\n")
		_, c2 := apiPool(t, "[modifies.mass_guard]\nwindow = \"7m\"\n")
		for _, c := range []*config.Config{c1, c2} {
			db, err := catalog.OpenPool(t.Context(), catalogPath(c), "p")
			if err != nil {
				t.Fatal(err)
			}
			if err := catalog.WriteModifyGuardEvent(t.Context(), db, "t1", "", 1_000); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		}
		a, told := answerAPI(t, c2)
		h := a.routes()
		want, _ := runCLI(t, "--config", byCLI, "guard", what, "--disk", "t1", "--kind", "modifications", "--dry-run", "--json")
		if w := post(h, "/v1/guards/t1", `{"answer": "`+what+`", "kind": "modifications", "dry_run": true}`, ""); w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s, the dry run: %d %s, want %s", what, w.Code, w.Body.String(), want)
		}
		want, _ = runCLI(t, "--config", byCLI, "guard", what, "--disk", "t1", "--kind", "modifications", "--json")
		if w := post(h, "/v1/guards/t1", `{"answer": "`+what+`", "kind": "modifications", "yes": true}`, ""); w.Code != http.StatusOK || w.Body.String() != want || told.Load() != 1 {
			t.Errorf("%s: %d %s, want %s, told %d", what, w.Code, w.Body.String(), want, told.Load())
		}
		var kinds [2][]string
		for i, c := range []*config.Config{c1, c2} {
			db, err := catalog.OpenPool(t.Context(), catalogPath(c), "p")
			if err != nil {
				t.Fatal(err)
			}
			kinds[i] = queryLines(t, db, "SELECT kind, data FROM events ORDER BY id")
			_ = db.Close()
		}
		if !reflect.DeepEqual(kinds[0], kinds[1]) || len(kinds[1]) != 2 {
			t.Errorf("%s: the events differ: %v, from the API %v", what, kinds[0], kinds[1])
		}
	}
}

// What the API refuses, it refuses before anything is read or written, as a usage error of the command, with its JSON
// error: a request that is not one answer, with no confirmation, or with a dry run and a confirmation at once; a disk
// the configuration does not list; another method (specs.md §25.1, invariant 17).
func TestTheAPIRefusesAnAnswerItCannotTake(t *testing.T) {
	p := newGuardedPool(t, clockBase())
	a, told := answerAPI(t, p.c)
	h := a.routes()
	before := p.tables(t)
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"no confirmation", `{"answer": "confirm"}`, http.StatusBadRequest},
		{"no confirmation, said", `{"answer": "confirm", "yes": false, "dry_run": false}`, http.StatusBadRequest},
		{"both", `{"answer": "confirm", "yes": true, "dry_run": true}`, http.StatusBadRequest},
		{"no answer", `{"yes": true}`, http.StatusBadRequest},
		{"another answer", `{"answer": "maybe", "yes": true}`, http.StatusBadRequest},
		{"an answer of another case", `{"answer": "Confirm", "yes": true}`, http.StatusBadRequest},
		{"an unknown guard", `{"answer": "confirm", "kind": "renames", "yes": true}`, http.StatusBadRequest},
		{"an unknown field", `{"answer": "confirm", "yes": true, "force": true}`, http.StatusBadRequest},
		{"a field of another case", `{"answer": "confirm", "Yes": true}`, http.StatusBadRequest},
		{"a field given twice", `{"answer": "reject", "answer": "confirm", "yes": true}`, http.StatusBadRequest},
		{"a string for a flag", `{"answer": "confirm", "yes": "true"}`, http.StatusBadRequest},
		{"a time of another form", `{"answer": "confirm", "yes": true, "up_to": "2026-01-01T00:00:00Z"}`, http.StatusBadRequest},
		{"no object", `["confirm"]`, http.StatusBadRequest},
		{"no JSON", `confirm`, http.StatusBadRequest},
		{"two objects", `{"answer": "confirm", "yes": true} {}`, http.StatusBadRequest},
		{"nothing", ``, http.StatusBadRequest},
		{"too long", `{"answer": "confirm", "yes": true, "kind": "deletions"` + strings.Repeat(" ", 4<<10) + `}`, http.StatusRequestEntityTooLarge},
	} {
		w := post(h, "/v1/guards/t1", tc.body, "")
		if w.Code != tc.code || !strings.Contains(w.Body.String(), `"error"`) || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if w := post(h, "/v1/guards/t9", `{"answer": "confirm", "yes": true}`, ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("a disk the configuration does not list: %d %s", w.Code, w.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if w := ask(h, method, "/v1/guards/t1", ""); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
			t.Errorf("%s: %d %q", method, w.Code, w.Header().Get("Allow"))
		}
	}
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/guards/t1", strings.NewReader(`{"answer": "confirm", "yes": true}`))
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnsupportedMediaType || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("a body of type %q: %d %s", contentType, w.Code, w.Body.String())
		}
	}
	if w := post(h, "/v1/guards", `{"answer": "confirm", "yes": true}`, ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/guards: %d", w.Code)
	}
	if after := p.tables(t); after != before || told.Load() != 0 {
		t.Errorf("a refused request wrote to the catalog, or told the daemon %d times:\n%s", told.Load(), after)
	}
	// A charset with the type is the type.
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/guards/t1", strings.NewReader(`{"answer": "confirm", "dry_run": true}`))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("with a charset: %d %s", w.Code, w.Body.String())
	}
}

// up_to says which guard an answer is for: one that stands since another time is not answered, so that a confirm
// given on what a dry run showed never lets go of what tripped since (specs.md §25.1).
func TestTheAPIAnswersTheGuardItWasShown(t *testing.T) {
	p := newGuardedPool(t, clockBase())
	a, told := answerAPI(t, p.c)
	h := a.routes()
	before := p.tables(t)
	w := post(h, "/v1/guards/t1", `{"answer": "confirm", "yes": true, "up_to": "2001-01-01T00:00:00.000000000Z"}`, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("another guard: %d %s", w.Code, w.Body.String())
	}
	if w := post(h, "/v1/guards/t1", `{"answer": "confirm", "dry_run": true, "up_to": "2001-01-01T00:00:00.000000000Z"}`, ""); w.Code != http.StatusConflict {
		t.Errorf("a dry run for another guard: %d %s", w.Code, w.Body.String())
	}
	if after := p.tables(t); after != before || told.Load() != 0 {
		t.Errorf("an answer for another guard was given:\n%s", after)
	}
	w = post(h, "/v1/guards/t1", `{"answer": "confirm", "dry_run": true}`, "")
	var dry guardAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &dry); err != nil || dry.UpTo == nil {
		t.Fatalf("%v %s", err, w.Body.String())
	}
	w = post(h, "/v1/guards/t1", `{"answer": "confirm", "yes": true, "up_to": `+strconv.Quote(*dry.UpTo)+`}`, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"done": true`) {
		t.Errorf("the guard shown: %d %s", w.Code, w.Body.String())
	}
	// Answered: nothing stands, and an answer for that guard has nothing to answer.
	w = post(h, "/v1/guards/t1", `{"answer": "confirm", "yes": true, "up_to": `+strconv.Quote(*dry.UpTo)+`}`, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"done": false`) || told.Load() != 1 {
		t.Errorf("then: %d %s, told %d", w.Code, w.Body.String(), told.Load())
	}
}

// Over TCP, an answer asks for the token as a question does, and a token file other accounts can read answers
// questions and dry runs only: one of them could let go of what the guard holds (specs.md §25.1).
func TestTheAPIAnswerOverTCP(t *testing.T) {
	p := newGuardedPool(t, clockBase())
	token := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(token, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(p.configPath)
	if err != nil {
		t.Fatal(err)
	}
	text = append(text, []byte("[api]\nlisten = \"127.0.0.1:7341\"\ntoken_file = "+strconv.Quote(token)+"\n")...)
	if err := os.WriteFile(p.configPath, text, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := load(p.configPath)
	if err != nil {
		t.Fatal(err)
	}
	a, told := answerAPI(t, c)
	tcp := a.withToken(a.routes())
	before := p.tables(t)
	for _, header := range []string{"", "Bearer", "Bearer s3cre", "bearer s3cret"} {
		if w := post(tcp, "/v1/guards/t1", `{"answer": "confirm", "yes": true}`, header); w.Code != http.StatusUnauthorized || w.Body.Len() != 0 {
			t.Errorf("%q: %d %s", header, w.Code, w.Body.String())
		}
	}
	if err := os.Chmod(token, 0o644); err != nil {
		t.Fatal(err)
	}
	if w := post(tcp, "/v1/guards/t1", `{"answer": "confirm", "yes": true}`, "Bearer s3cret"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "api.token_file") {
		t.Errorf("a token other accounts can read: %d %s", w.Code, w.Body.String())
	}
	if w := post(tcp, "/v1/guards/t1", `{"answer": "confirm", "dry_run": true}`, "Bearer s3cret"); w.Code != http.StatusOK {
		t.Errorf("a dry run with a token other accounts can read: %d %s", w.Code, w.Body.String())
	}
	if w := ask(tcp, http.MethodGet, "/v1/guards", "Bearer s3cret"); w.Code != http.StatusOK {
		t.Errorf("a question with a token other accounts can read: %d", w.Code)
	}
	if after := p.tables(t); after != before || told.Load() != 0 {
		t.Errorf("an answer without the token, or with one others can read, was given:\n%s", after)
	}
	if err := os.Chmod(token, 0o640); err != nil {
		t.Fatal(err)
	}
	if w := post(tcp, "/v1/guards/t1", `{"answer": "confirm", "yes": true}`, "Bearer s3cret"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"done": true`) || told.Load() != 1 {
		t.Errorf("with the token: %d %s", w.Code, w.Body.String())
	}
	// The socket asks for no token: its mode says who may ask.
	if w := post(a.routes(), "/v1/guards/t1", `{"answer": "confirm", "yes": true}`, ""); w.Code != http.StatusOK {
		t.Errorf("over the socket: %d %s", w.Code, w.Body.String())
	}
}

// tokenPool is a guarded pool whose API takes TCP with a token file in a folder of its own, and the paths of both.
// named gives the path the configuration names for it, the token's own when nil.
func tokenPool(t *testing.T, named func(folder, token string) string) (p *guardedPool, a *apiServer, told *atomic.Int32, folder, token string) {
	t.Helper()
	p = newGuardedPool(t, clockBase())
	folder = filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(folder, 0o750); err != nil {
		t.Fatal(err)
	}
	token = filepath.Join(folder, "api.token")
	if err := os.WriteFile(token, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configured := token
	if named != nil {
		configured = named(folder, token)
	}
	text, err := os.ReadFile(p.configPath)
	if err != nil {
		t.Fatal(err)
	}
	text = append(text, []byte("[api]\nlisten = \"127.0.0.1:7341\"\ntoken_file = "+strconv.Quote(configured)+"\n")...)
	if err := os.WriteFile(p.configPath, text, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := load(p.configPath)
	if err != nil {
		t.Fatal(err)
	}
	a, told = answerAPI(t, c)
	return p, a, told, folder, token
}

// An answer over TCP takes a token file only root or the account the daemon runs as can change, and other accounts
// cannot read: any account that could write the file, or a folder that holds its name, could set the token it then
// sends. The 403 says why; questions and dry runs are answered as before (specs.md §25.1).
func TestTheAPIAnswerTakesATokenOnlyItsAccountsCanSet(t *testing.T) {
	root := os.Geteuid() == 0
	for _, tc := range []struct {
		name string
		// set changes the file or its folder; root says that only root can.
		set  func(folder, token string) error
		root bool
		// says is what the 403 says, "" for an answer taken.
		says string
	}{
		{name: "0600 in a folder 0750", set: func(string, string) error { return nil }},
		{name: "0640", set: func(_, token string) error { return os.Chmod(token, 0o640) }},
		{name: "0400", set: func(_, token string) error { return os.Chmod(token, 0o400) }},
		{name: "others may read it", set: func(_, token string) error { return os.Chmod(token, 0o604) }, says: "other accounts may read api.token_file"},
		{name: "others may write it", set: func(_, token string) error { return os.Chmod(token, 0o602) }, says: "other accounts may write api.token_file"},
		{name: "its group may write it", set: func(_, token string) error { return os.Chmod(token, 0o620) }, says: "its group may write api.token_file"},
		{name: "others may write its folder", set: func(folder, _ string) error { return os.Chmod(folder, 0o757) }, says: "other accounts may write the folder"},
		{name: "others may write its folder, sticky", set: func(folder, _ string) error { return os.Chmod(folder, os.ModeSticky|0o777) }, says: "other accounts may write the folder"},
		{name: "the daemon's group may write its folder", set: func(folder, _ string) error { return os.Chmod(folder, 0o770) }},
		{name: "another account's file", root: true, set: func(_, token string) error { return os.Chown(token, 4321, -1) }, says: "api.token_file belongs to the account 4321"},
		{name: "another group's file, 0640", root: true, set: func(_, token string) error {
			return errors.Join(os.Chown(token, -1, 4321), os.Chmod(token, 0o640))
		}},
		{name: "another account's folder", root: true, set: func(folder, _ string) error { return os.Chown(folder, 4321, -1) }, says: "belongs to the account 4321"},
		{name: "another group may write its folder", root: true, set: func(folder, _ string) error {
			return errors.Join(os.Chown(folder, -1, 4321), os.Chmod(folder, 0o770))
		}, says: "the group 4321 may write the folder"},
		{name: "another group may read its folder", root: true, set: func(folder, _ string) error { return os.Chown(folder, -1, 4321) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.root && !root {
				t.Skip("only root gives a file away")
			}
			p, a, told, folder, token := tokenPool(t, nil)
			tcp := a.withToken(a.routes())
			if err := tc.set(folder, token); err != nil {
				t.Fatal(err)
			}
			before := p.tables(t)
			if w := post(tcp, "/v1/guards/t1", `{"answer": "confirm", "dry_run": true}`, "Bearer s3cret"); w.Code != http.StatusOK {
				t.Errorf("a dry run: %d %s", w.Code, w.Body.String())
			}
			if w := ask(tcp, http.MethodGet, "/v1/guards", "Bearer s3cret"); w.Code != http.StatusOK {
				t.Errorf("a question: %d", w.Code)
			}
			w := post(tcp, "/v1/guards/t1", `{"answer": "confirm", "yes": true}`, "Bearer s3cret")
			if tc.says == "" {
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"done": true`) || told.Load() != 1 {
					t.Errorf("the answer: %d %s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), tc.says) {
				t.Errorf("the answer: %d %s, want 403 saying %q", w.Code, w.Body.String(), tc.says)
			}
			if after := p.tables(t); after != before || told.Load() != 0 {
				t.Errorf("an answer with a token another account could set was given:\n%s", after)
			}
			// The socket asks for no token: its mode says who may ask.
			if w := post(a.routes(), "/v1/guards/t1", `{"answer": "confirm", "yes": true}`, ""); w.Code != http.StatusOK {
				t.Errorf("over the socket: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// A token file reached through a symbolic link is judged by the folders of both: whoever may write the folder of the
// link's name may point it elsewhere (specs.md §25.1).
func TestTheAPIAnswerLooksAtTheFoldersOfALink(t *testing.T) {
	open := filepath.Join(t.TempDir(), "open")
	p, a, told, _, _ := tokenPool(t, func(_, token string) string {
		if err := os.Mkdir(open, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(token, filepath.Join(open, "api.token")); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(open, "api.token")
	})
	tcp := a.withToken(a.routes())
	if err := os.Chmod(open, 0o757); err != nil {
		t.Fatal(err)
	}
	before := p.tables(t)
	if w := post(tcp, "/v1/guards/t1", `{"answer": "confirm", "yes": true}`, "Bearer s3cret"); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "other accounts may write the folder "+open) {
		t.Errorf("a link in a folder others may write: %d %s", w.Code, w.Body.String())
	}
	if after := p.tables(t); after != before || told.Load() != 0 {
		t.Errorf("an answer was given:\n%s", after)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if w := post(tcp, "/v1/guards/t1", `{"answer": "confirm", "yes": true}`, "Bearer s3cret"); w.Code != http.StatusOK || told.Load() != 1 {
		t.Errorf("a link in a folder only its owner may write: %d %s", w.Code, w.Body.String())
	}
}

// What makes a token file one an answer cannot take, each reason in a fixed order: whose the file is and its mode,
// then each folder on the way to it (specs.md §25.1).
func TestTokenDoubts(t *testing.T) {
	const daemon, group = 998, 997
	file := func(uid, gid uint32, mode os.FileMode) fsx.Owner { return fsx.Owner{UID: uid, GID: gid, Mode: mode} }
	folder := func(path string, uid, gid uint32, mode os.FileMode) fsx.Folder {
		return fsx.Folder{Path: path, Owner: fsx.Owner{UID: uid, GID: gid, Mode: os.ModeDir | mode}}
	}
	etc := folder("/etc/atoll", 0, 0, 0o750)
	for _, tc := range []struct {
		name    string
		file    fsx.Owner
		folders []fsx.Folder
		want    []string
	}{
		{"root's, 0600", file(0, 0, 0o600), []fsx.Folder{etc}, nil},
		{"root's, 0640 to the daemon's group", file(0, group, 0o640), []fsx.Folder{etc}, nil},
		{"the daemon's, 0400", file(daemon, group, 0o400), []fsx.Folder{folder("/var/lib/atoll", daemon, group, 0o750)}, nil},
		{"another group may read it", file(0, 100, 0o640), []fsx.Folder{etc}, nil},
		{"another account's", file(1000, 1000, 0o600), []fsx.Folder{etc}, []string{"api.token_file belongs to the account 1000"}},
		{"its group may write it", file(0, group, 0o660), []fsx.Folder{etc}, []string{"its group may write api.token_file"}},
		{"others may read it", file(0, 0, 0o644), []fsx.Folder{etc}, []string{"other accounts may read api.token_file"}},
		{"others may write it", file(0, 0, 0o602), []fsx.Folder{etc}, []string{"other accounts may write api.token_file"}},
		{"everything at once", file(1000, 1000, 0o666), []fsx.Folder{etc}, []string{
			"api.token_file belongs to the account 1000", "its group may write api.token_file", "other accounts may read api.token_file",
			"other accounts may write api.token_file",
		}},
		{"another account's folder", file(0, 0, 0o600), []fsx.Folder{folder("/home/me", 1000, 1000, 0o700)}, []string{"the folder /home/me belongs to the account 1000"}},
		{"others may write its folder", file(0, 0, 0o600), []fsx.Folder{folder("/tmp", 0, 0, os.ModeSticky|0o777)}, []string{"other accounts may write the folder /tmp"}},
		{"root's group may write its folder", file(0, 0, 0o600), []fsx.Folder{folder("/etc/atoll", 0, 0, 0o770)}, nil},
		{"the daemon's group may write its folder", file(0, 0, 0o600), []fsx.Folder{folder("/etc/atoll", 0, group, 0o770)}, nil},
		{"another group may write its folder", file(0, 0, 0o600), []fsx.Folder{folder("/srv/keys", 0, 100, 0o770)}, []string{"the group 100 may write the folder /srv/keys"}},
		{"a link's folder, then the file's", file(0, 0, 0o600), []fsx.Folder{folder("/etc", 0, 0, 0o755), folder("/srv/open", 0, 0, 0o777), etc},
			[]string{"other accounts may write the folder /srv/open"}},
	} {
		got := tokenDoubts(tc.file, tc.folders, daemon, group)
		if len(got) != len(tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if !strings.HasPrefix(got[i], tc.want[i]) {
				t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
			}
		}
	}
}

// A daemon in --dry-run records no answer, as it writes nothing; it answers a dry run (specs.md §25.1).
func TestADaemonInDryRunRecordsNoAnswer(t *testing.T) {
	p := newGuardedPool(t, clockBase())
	a, told := answerAPI(t, p.c)
	a.apply = false
	h := a.routes()
	before := p.tables(t)
	if w := post(h, "/v1/guards/t1", `{"answer": "confirm", "yes": true}`, ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("an answer: %d %s", w.Code, w.Body.String())
	}
	if w := post(h, "/v1/guards/t1", `{"answer": "confirm", "dry_run": true}`, ""); w.Code != http.StatusOK {
		t.Errorf("a dry run: %d %s", w.Code, w.Body.String())
	}
	if after := p.tables(t); after != before || told.Load() != 0 {
		t.Errorf("a daemon in --dry-run recorded an answer:\n%s", after)
	}
}

// In the daemon, the answer goes through its loop, under the catalog's lock, and the loop is told as SIGHUP would
// tell it, so that a pass takes the answer at once; with the loop gone, an answer that cannot be given is 503 with
// nothing written (specs.md §15.6, §25.1).
func TestTheDaemonsLoopTakesTheAnswer(t *testing.T) {
	_, c := apiPool(t, "")
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	work, reload := make(chan *daemon.CatalogWork), make(chan struct{}, 1)
	a := newTestAPI(t, c)
	a.answerThrough(work, reload, true)
	passes := make(chan []string, 64)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Options{Config: c, Machine: machine, Apply: true, Log: slog.New(slog.DiscardHandler), CheckEvery: time.Hour,
			Reload: reload, Load: func() (*config.Config, error) { return c, nil }, Catalog: work,
			OnPass: func(because []string, _ *jobs.Outcome) { passes <- because }})
	}()
	waitPass := func(why string) {
		t.Helper()
		deadline := time.After(30 * time.Second)
		for {
			select {
			case because := <-passes:
				if slices.Contains(because, why) {
					return
				}
			case <-deadline:
				t.Fatalf("no pass because %s", why)
			}
		}
	}
	waitPass(daemon.BecauseStart)
	// A guard event, once the first pass is over: the loop then sleeps until something calls for a look.
	db, err := catalog.OpenPool(t.Context(), catalogPath(c), "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.WriteGuardEvent(t.Context(), db, "t1", 1_000); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	w := post(a.routes(), "/v1/guards/t1", `{"answer": "confirm", "yes": true}`, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"up_to": "1970-01-01T00:00:00.000001000Z"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	waitPass(daemon.BecauseReload)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// No loop takes the answer: the request gives up with its time, and nothing is answered.
	db, err = catalog.OpenPool(t.Context(), catalogPath(c), "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := catalog.WriteGuardEvent(t.Context(), db, "t1", 2_000); err != nil {
		t.Fatal(err)
	}
	gone, stop := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer stop()
	r := httptest.NewRequestWithContext(gone, http.MethodPost, "/v1/guards/t1", strings.NewReader(`{"answer": "reject", "yes": true}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("with no loop: %d %s", rec.Code, rec.Body.String())
	}
	var answers int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM events WHERE kind IN ('guard-confirm', 'guard-reject')").Scan(&answers); err != nil || answers != 1 {
		t.Errorf("%d answers, %v", answers, err)
	}
}
