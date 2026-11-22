//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// atoll is the binary the tests drive: the conformance build ATOLL_BIN names, as make test-integration builds it, or
// one TestMain builds of this checkout. As the conformance runner does, they talk to it through its command line, its
// signals, its socket and the filesystem alone, and import nothing from internal/ (specs.md §29.4).
var atoll string

func TestMain(m *testing.M) {
	os.Exit(runAll(m))
}

func runAll(m *testing.M) int {
	if atoll = os.Getenv("ATOLL_BIN"); atoll != "" {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "atoll-integration-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	atoll = filepath.Join(dir, "atoll")
	build := exec.CommandContext(context.Background(), "go", "build", "-tags", "conformance", "-o", atoll, "./cmd/atoll")
	build.Dir = filepath.Join("..", "..")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "a conformance build of ./cmd/atoll:", err)
		return 1
	}
	return m.Run()
}

// A disk of a pool: a folder, which a conformance build takes for a mounted disk of that identifier.
type disk struct{ id, uuid string }

var poolDisks = []disk{{"t1", "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b"}, {"t2", "7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d"}}

// pool is a pool of two disks that are folders, in a folder of the test, with its configuration, its control file and
// its catalog there.
type pool struct {
	t                            *testing.T
	dir, config, control, socket string
	// text is the configuration as written: what follows the disks is added to it.
	text string
}

// newPool makes a pool whose first disk holds files, each a logical path and its text, then runs atoll init and a full
// scan on it. The clock of the control file starts at the machine's an hour ahead, and runs ("running"), as a daemon
// that runs for good needs time to pass: for its debounce, its checks and its waits. Every file the test writes is
// older, so that no settle holds it back. The maintenance window opens an hour after that clock, so that no catalog
// backup and no scrub runs meanwhile.
func newPool(t *testing.T, files map[string]string) *pool {
	t.Helper()
	// Not t.TempDir, whose name holds the test's: a socket's path holds 107 bytes at most.
	dir, err := os.MkdirTemp("", "atoll-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	now := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	p := &pool{t: t, dir: dir, config: filepath.Join(dir, "atoll.toml"), control: filepath.Join(dir, "conformance.json"), socket: filepath.Join(dir, "atoll.sock")}
	p.text = fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n[api]\nsocket = %q\n[defaults]\nsettle = \"0s\"\n[deletes]\ndebounce = \"1s\"\n"+
		"[deletes.mass_guard]\npercent = \"100%%\"\n[maintenance]\nwindow = \"%s-%s\"\n",
		filepath.Join(dir, "catalog.db"), filepath.Join(dir, "pool"), p.socket, now.Add(time.Hour).Format("15:04"), now.Add(2*time.Hour).Format("15:04"))
	type controlDisk struct {
		ID       string `json:"id"`
		UUID     string `json:"uuid"`
		FS       string `json:"fs"`
		Path     string `json:"path"`
		Capacity int64  `json:"capacity"`
	}
	control := struct {
		Now     string        `json:"now"`
		Running bool          `json:"running"`
		Disks   []controlDisk `json:"disks"`
	}{Now: now.Format(time.RFC3339), Running: true}
	for _, d := range poolDisks {
		mount := p.mount(d.id)
		must(t, os.MkdirAll(filepath.Join(mount, "media"), 0o755))
		p.text += fmt.Sprintf("[[disk]]\nid = %q\nuuid = %q\nmount = %q\nfs = \"ext4\"\n", d.id, d.uuid, mount)
		control.Disks = append(control.Disks, controlDisk{d.id, d.uuid, "ext4", mount, 1 << 30})
	}
	data, err := json.Marshal(control)
	must(t, err)
	must(t, os.WriteFile(p.control, data, 0o644))
	must(t, os.WriteFile(p.config, []byte(p.text), 0o644))
	for path, text := range files {
		p.write("t1", path, text)
	}
	p.run(0, "init")
	p.run(0, "scan", "--full")
	return p
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (p *pool) mount(id string) string {
	return filepath.Join(p.dir, "disks", id)
}

// write writes a file below a disk's media/, its folders made.
func (p *pool) write(id, path, text string) {
	p.t.Helper()
	file := filepath.Join(p.mount(id), "media", filepath.FromSlash(path))
	must(p.t, os.MkdirAll(filepath.Dir(file), 0o755))
	must(p.t, os.WriteFile(file, []byte(text), 0o644))
}

// env is the environment of every run of the binary: the control file, and the time zone of the scenarios.
func (p *pool) env() []string {
	return append(os.Environ(), "ATOLL_CONFORMANCE="+p.control, "TZ=UTC", "LC_ALL=C", "LANG=C")
}

// run runs a command that ends, and gives its standard output; another exit code than want fails the test.
func (p *pool) run(want int, args ...string) string {
	p.t.Helper()
	cmd := exec.CommandContext(p.t.Context(), atoll, append([]string{"--config", p.config}, args...)...)
	cmd.Env = p.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		p.t.Fatalf("atoll %v: %v", args, err)
	}
	if code != want {
		p.t.Fatalf("atoll %v: exit %d, want %d\n%s", args, code, want, stderr.String())
	}
	return stdout.String()
}

// lockedBuffer gathers what a process writes while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data) //nolint:wrapcheck // a buffer's own
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// daemon is atoll daemon, running for good.
type daemon struct {
	p      *pool
	cmd    *exec.Cmd
	stderr *lockedBuffer
	done   chan struct{}
	err    error
}

// start starts the daemon that runs for good and waits until its socket answers. A daemon the test leaves running is
// killed at its end, as its context ends.
func (p *pool) start() *daemon {
	p.t.Helper()
	d := &daemon{p: p, cmd: exec.CommandContext(p.t.Context(), atoll, "--config", p.config, "daemon"), stderr: &lockedBuffer{}, done: make(chan struct{})}
	d.cmd.Env = p.env()
	d.cmd.Stdout, d.cmd.Stderr = io.Discard, d.stderr
	must(p.t, d.cmd.Start())
	go func() {
		d.err = d.cmd.Wait()
		close(d.done)
	}()
	p.t.Cleanup(func() { <-d.done })
	p.waitFor("the socket to answer", func() bool {
		select {
		case <-d.done:
			p.t.Fatalf("the daemon stopped at its start: %v\n%s", d.err, d.stderr.String())
		default:
		}
		code, _ := p.get("/v1/health")
		return code == http.StatusOK
	})
	return d
}

// stop sends the daemon a signal and gives its exit code once it has stopped.
func (d *daemon) stop(sig syscall.Signal) int {
	d.p.t.Helper()
	must(d.p.t, d.cmd.Process.Signal(sig))
	select {
	case <-d.done:
	case <-time.After(30 * time.Second):
		d.p.t.Fatalf("the daemon did not stop on %v\n%s", sig, d.stderr.String())
	}
	var exit *exec.ExitError
	switch {
	case d.err == nil:
		return 0
	case errors.As(d.err, &exit):
		return exit.ExitCode()
	}
	d.p.t.Fatalf("the daemon: %v", d.err)
	return -1
}

// logIsJSONLines checks that what the daemon wrote on standard error is its log alone, in JSON lines (specs.md §23.2).
func (d *daemon) logIsJSONLines() {
	d.p.t.Helper()
	text := d.stderr.String()
	if text == "" {
		d.p.t.Error("the daemon logged nothing")
	}
	for line := range strings.Lines(text) {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			d.p.t.Errorf("a line of standard error that is no JSON object: %q", line)
		}
	}
}

// get asks the daemon over its socket, and gives the status and the body.
func (p *pool) get(path string) (int, string) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", p.socket)
		},
	}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://atoll"+path, nil)
	must(p.t, err)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err.Error()
	}
	return resp.StatusCode, string(body)
}

// waitFor waits until cond holds, 30 s at most.
func (p *pool) waitFor(what string, cond func() bool) {
	p.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			p.t.Fatalf("waited 30 s for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// holds says whether a disk holds a file below its media/ with that text.
func (p *pool) holds(id, path, text string) bool {
	data, err := os.ReadFile(filepath.Join(p.mount(id), "media", filepath.FromSlash(path)))
	return err == nil && string(data) == text
}

// noHiddenFile checks that no name below a disk begins with a dot (invariant 5).
func (p *pool) noHiddenFile() {
	p.t.Helper()
	for _, d := range poolDisks {
		err := filepath.WalkDir(p.mount(d.id), func(path string, e fs.DirEntry, err error) error {
			if err == nil && strings.HasPrefix(e.Name(), ".") {
				p.t.Errorf("a hidden file: %s", path)
			}
			return err
		})
		must(p.t, err)
	}
}

// sameTimes checks that a copy keeps its source's modification time (invariant 6).
func (p *pool) sameTimes(path string) {
	p.t.Helper()
	var times []time.Time
	for _, d := range poolDisks {
		st, err := os.Stat(filepath.Join(p.mount(d.id), "media", filepath.FromSlash(path)))
		must(p.t, err)
		times = append(times, st.ModTime())
	}
	if !times[0].Equal(times[1]) {
		p.t.Errorf("%s: modified at %v on t1, %v on its copy", path, times[0], times[1])
	}
}

// status gives atoll status --json, read.
func (p *pool) status() (total, atLevel int64) {
	p.t.Helper()
	var out struct {
		FilesTotal   int64 `json:"files_total"`
		FilesAtLevel int64 `json:"files_at_level"`
	}
	must(p.t, json.Unmarshal([]byte(p.run(0, "status", "--json")), &out))
	return out.FilesTotal, out.FilesAtLevel
}

// inTrash says whether a disk's trash holds a copy of a logical path with that text: trash/<date>/<op-id>/<path>.
func (p *pool) inTrash(id, path, text string) bool {
	found, _ := filepath.Glob(filepath.Join(p.mount(id), "Atoll", "trash", "*", "*", filepath.FromSlash(path)))
	for _, f := range found {
		if data, err := os.ReadFile(f); err == nil && string(data) == text {
			return true
		}
	}
	return false
}
