package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/alerts"
	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/trash"
)

func TestFstabHas(t *testing.T) {
	table := []byte("# /etc/fstab\nUUID=1 / ext4 defaults 0 1\n\n  UUID=2   /mnt/t1  ext4 defaults,nofail 0 2\n/dev/sdc1 /mnt/my\\040disk ntfs3 iocharset=utf8 0 0\n#UUID=3 /mnt/off ext4 defaults 0 0\nbroken\n")
	for mount, want := range map[string]bool{"/mnt/t1": true, "/mnt/t1/": true, "/mnt/my disk": true, "/mnt/off": false, "/mnt/t2": false, "/": true, "broken": false} {
		if got := fstabHas(table, mount); got != want {
			t.Errorf("%q: %v", mount, got)
		}
	}
	if got := fstabUnescape(`a\040b\134c\011d\0`); got != "a b\\c\td\\0" {
		t.Errorf("%q", got)
	}
}

func TestSdNotify(t *testing.T) {
	if err := sdNotify("READY=1"); err != nil {
		t.Errorf("with no socket: %v", err)
	}
	socket := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	t.Setenv("NOTIFY_SOCKET", socket)
	if err := sdNotify("READY=1"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "READY=1" {
		t.Errorf("%q, %v", buf[:n], err)
	}
}

// bench makes a configuration of disks that are not mounted, and its catalog.
func bench(t *testing.T, more string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n" +
		"[[disk]]\nid = \"t1\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b\"\nmount = " + strconv.Quote(filepath.Join(dir, "t1")) + "\nfs = \"ext4\"\n" + more
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	return c
}

// The loop on a disk that is not mounted: it tells the service manager it is ready, runs its first pass, checks
// again and again, takes no branch from a mergerfs that is not there, and stops with nil when asked, telling the
// service manager first (specs.md §15.13).
func TestRunStartsAndStops(t *testing.T) {
	c := bench(t, "")
	socket := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	t.Setenv("NOTIFY_SOCKET", socket)
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	passes := make(chan []string, 16)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: c, Machine: machine, Apply: true, Log: slog.New(slog.DiscardHandler), CheckEvery: 50 * time.Millisecond,
			OnPass: func(because []string, o *jobs.Outcome) {
				if len(o.Disks) != 1 || o.Disks[0].Verdict.State != disks.Missing {
					t.Errorf("the pass saw %+v", o.Disks)
				}
				passes <- because
			}})
	}()
	select {
	case because := <-passes:
		if !slices.Equal(because, []string{BecauseStart}) {
			t.Errorf("the first pass: %v", because)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no first pass")
	}
	// Checks go on; nothing calls for another pass.
	select {
	case because := <-passes:
		t.Errorf("another pass: %v", because)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stopped: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop")
	}
	var said []string
	buf := make([]byte, 64)
	for range 2 {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, err := conn.Read(buf)
		if err != nil {
			break
		}
		said = append(said, string(buf[:n]))
	}
	if !slices.Equal(said, []string{"READY=1", "STOPPING=1"}) {
		t.Errorf("told the service manager %v", said)
	}
}

// The daemon sends an alert when a condition begins, and not again at the next pass while it holds; an alert the
// server refused is recorded as not sent, and the next pass sends it (specs.md §23).
func TestRunSendsAlerts(t *testing.T) {
	var mu sync.Mutex
	var got []string
	status := http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r.Header.Get("Title")+" | "+r.Header.Get("Priority"))
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := bench(t, "[alerts]\nntfy = "+strconv.Quote(srv.URL+"/atoll")+"\n")
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reload := make(chan struct{})
	passes := make(chan []string, 16)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: c, Machine: machine, Apply: true, Log: slog.New(slog.DiscardHandler), CheckEvery: time.Hour,
			Reload: reload, Load: func() (*config.Config, error) { return c, nil },
			OnPass: func(because []string, _ *jobs.Outcome) { passes <- because }})
	}()
	sent := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
	pass := func(what string) {
		t.Helper()
		select {
		case <-passes:
		case <-time.After(10 * time.Second):
			t.Fatalf("no pass %s", what)
		}
	}
	pass("at the start")
	missing := "Atoll: disk t1 is missing | 4"
	if s := sent(); !slices.Equal(s, []string{missing}) {
		t.Fatalf("after the first pass: %q", s)
	}
	mu.Lock()
	status = http.StatusOK
	mu.Unlock()
	reload <- struct{}{}
	pass("after a reload")
	reload <- struct{}{}
	pass("after another reload")
	if s := sent(); !slices.Equal(s, []string{missing, missing}) {
		t.Errorf("sent: %q", s)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	file, _ := c.Effective()["catalog"].(string)
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := catalog.ReadAlerts(t.Context(), db)
	if err != nil || len(rows) != 1 || rows[0].Key != "disk-missing:t1" || rows[0].Count != 1 {
		t.Errorf("recorded %+v, %v", rows, err)
	}
}

// In shadow mode, the daemon sends no alert and records none (specs.md §15.13, §23).
func TestDryRunSendsNoAlert(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
	}))
	defer srv.Close()
	c := bench(t, "[alerts]\nntfy = "+strconv.Quote(srv.URL+"/atoll")+"\n")
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	passes := make(chan struct{}, 4)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: c, Machine: machine, Log: slog.New(slog.DiscardHandler), CheckEvery: time.Hour,
			OnPass: func([]string, *jobs.Outcome) { passes <- struct{}{} }})
	}()
	select {
	case <-passes:
	case <-time.After(10 * time.Second):
		t.Fatal("no pass")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Errorf("%d alerts sent", requests)
	}
	file, _ := c.Effective()["catalog"].(string)
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if rows, err := catalog.ReadAlerts(t.Context(), db); err != nil || len(rows) != 0 {
		t.Errorf("recorded %+v, %v", rows, err)
	}
}

// The sink counts decisions and operations, and keeps the names refused: a rename's new name, a copy's own.
func TestPassSink(t *testing.T) {
	s := &passSink{}
	for _, op := range []jobs.Operation{
		{Kind: jobs.KindRename, Path: "a", To: "b?", Outcome: trash.OutcomeNameRefused},
		{Kind: jobs.KindReplicate, Path: "c?", Outcome: trash.OutcomeNameRefused},
		{Kind: jobs.KindReplicate, Path: "d", Outcome: trash.OutcomeIOError},
	} {
		if err := s.Operation(&op); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Decision(&jobs.Decision{}); err != nil {
		t.Fatal(err)
	}
	if s.operations != 3 || s.decisions != 1 || s.refused.Count != 2 || !slices.Equal(s.refused.First, []string{"b?", "c?"}) {
		t.Errorf("%+v", s)
	}
}

// Inside maintenance.window, after its first pass, the daemon backs the catalog up, once for that stretch of the
// window (specs.md §9, §15.13).
func TestRunBacksUp(t *testing.T) {
	window := time.Now().Add(-time.Hour).Format("15:04") + "-" + time.Now().Add(time.Hour).Format("15:04")
	c := bench(t, "[maintenance]\nwindow = \""+window+"\"\n")
	file, _ := c.Effective()["catalog"].(string)
	backups := filepath.Join(filepath.Dir(file), catalog.BackupFolder)
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	reload := make(chan struct{})
	passes := make(chan struct{}, 8)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: c, Machine: machine, Apply: true, Log: slog.New(slog.DiscardHandler), CheckEvery: time.Hour,
			Reload: reload, Load: func() (*config.Config, error) { return c, nil },
			OnPass: func([]string, *jobs.Outcome) { passes <- struct{}{} }})
	}()
	count := func() int {
		entries, _ := os.ReadDir(backups)
		return len(entries)
	}
	<-passes
	for deadline := time.Now().Add(10 * time.Second); count() == 0 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	// Another pass in the same stretch makes no other backup.
	time.Sleep(1100 * time.Millisecond)
	reload <- struct{}{}
	<-passes
	time.Sleep(300 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Errorf("%d backups", n)
	}
}

// failing is a channel that cannot be reached.
type failing struct{}

func (failing) Name() string                                { return "alerts.ntfy" }
func (failing) Send(context.Context, *alerts.Message) error { return errors.New("unreachable") }

// An alert that did not go brings the next pass within alertRetry, and never puts off one due sooner.
func TestFailedAlertBringsAPass(t *testing.T) {
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	l := &loop{opt: &Options{Machine: machine}, set: &settings{channels: []alerts.Channel{failing{}}}, log: slog.New(slog.DiscardHandler)}
	due := &alerts.Due{Messages: []*alerts.Message{{Key: "disk-missing:t1"}}}
	before := machine.Now()
	l.sendAlerts(t.Context(), due)
	if l.passDue == nil || *l.passDue < before+alertRetry.Nanoseconds() || *l.passDue > machine.Now()+alertRetry.Nanoseconds() {
		t.Fatalf("the next pass at %v, now %d", l.passDue, before)
	}
	sooner := before + 1
	l.passDue = &sooner
	l.sendAlerts(t.Context(), due)
	if *l.passDue != sooner {
		t.Errorf("a pass due sooner was put off to %d", *l.passDue)
	}
	l.passDue = nil
	l.sendAlerts(t.Context(), &alerts.Due{})
	if l.passDue != nil {
		t.Error("nothing to send brought a pass")
	}
}

// With no catalog, or another pool's, the daemon does not start.
func TestRunNeedsItsCatalog(t *testing.T) {
	c := bench(t, "")
	file, _ := c.Effective()["catalog"].(string)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), Options{Config: c, Machine: machine, Log: slog.New(slog.DiscardHandler)})
	if !errors.Is(err, ErrStartup) {
		t.Errorf("%v", err)
	}
	if _, statErr := os.Stat(file); !os.IsNotExist(statErr) {
		t.Errorf("a catalog was made: %v", statErr)
	}
}

// On SIGHUP the configuration is read again: one that does not load is left aside, with no pass; one that loads
// takes effect, with a pass for reload that sees its disks (specs.md §15.13).
func TestRunReloads(t *testing.T) {
	c := bench(t, "")
	file, _ := c.Effective()["catalog"].(string)
	dir := filepath.Dir(file)
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n" +
		"[[disk]]\nid = \"t1\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b\"\nmount = " + strconv.Quote(filepath.Join(dir, "t1")) + "\nfs = \"ext4\"\n" +
		"[[disk]]\nid = \"t2\"\nuuid = \"7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d\"\nmount = " + strconv.Quote(filepath.Join(dir, "t2")) + "\nfs = \"ext4\"\n"
	bigger, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	loads := []func() (*config.Config, error){
		func() (*config.Config, error) { return nil, errors.New("a configuration that does not load") },
		func() (*config.Config, error) { return bigger, nil },
	}
	machine, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reload := make(chan struct{})
	passes := make(chan passLine, 16)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: c, Machine: machine, Apply: true, Log: slog.New(slog.DiscardHandler), CheckEvery: time.Hour,
			Reload: reload, Load: func() (*config.Config, error) { next := loads[0]; loads = loads[1:]; return next() },
			OnPass: func(because []string, o *jobs.Outcome) { passes <- passLine{because, len(o.Disks)} }})
	}()
	if p := <-passes; !slices.Equal(p.because, []string{BecauseStart}) || p.disks != 1 {
		t.Fatalf("the first pass: %+v", p)
	}
	reload <- struct{}{}
	select {
	case p := <-passes:
		t.Errorf("a pass for a configuration that did not load: %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
	reload <- struct{}{}
	select {
	case p := <-passes:
		if !slices.Equal(p.because, []string{BecauseReload}) || p.disks != 2 {
			t.Errorf("the pass after the reload: %+v", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no pass after the reload")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("stopped: %v", err)
	}
}

type passLine struct {
	because []string
	disks   int
}
