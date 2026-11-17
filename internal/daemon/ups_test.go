package daemon

import (
	"bufio"
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/ups"
	"github.com/peondusud/atoll/internal/value"
)

// fakeUPS is an upsd that answers each GET VAR with what the test last said.
type fakeUPS struct {
	mu     sync.Mutex
	answer string
	port   int
}

func newFakeUPS(t *testing.T, status string) *fakeUPS {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	u := &fakeUPS{port: l.Addr().(*net.TCPAddr).Port}
	u.set(status)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go u.serve(conn)
		}
	}()
	return u
}

func (u *fakeUPS) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil || line == "LOGOUT\n" {
			return
		}
		u.mu.Lock()
		answer := u.answer
		u.mu.Unlock()
		if _, err := conn.Write([]byte(answer)); err != nil {
			return
		}
	}
}

// set has the UPS say a status; stale has upsd say its driver gives nothing fresh.
func (u *fakeUPS) set(status string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.answer = "VAR ups ups.status \"" + status + "\"\n"
}

func (u *fakeUPS) stale() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.answer = "ERR DATA-STALE\n"
}

func (u *fakeUPS) nut() string {
	return "ups@127.0.0.1:" + strconv.Itoa(u.port)
}

func (u *fakeUPS) config() string {
	return "[ups]\nenabled = true\nnut = " + strconv.Quote(u.nut()) + "\n"
}

// flushDisks is folderDisks that counts, by mount, the times a disk was asked to write out what it holds.
type flushDisks struct {
	*folderDisks
	mu      sync.Mutex
	flushed map[string]int
}

func (m *flushDisks) OpenTop(mount string) (fsx.Dir, error) {
	top, err := m.folderDisks.OpenTop(mount)
	if err != nil {
		return nil, err
	}
	return &countedFlush{Dir: top, flushed: func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.flushed[mount]++
	}}, nil
}

func (m *flushDisks) times(mount string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flushed[mount]
}

type countedFlush struct {
	fsx.Dir
	flushed func()
}

func (d *countedFlush) SyncFS() error {
	d.flushed()
	return d.Dir.SyncFS()
}

// upsBench gives a configuration of two disks that are folders, the first holding a file, copies waiting for no
// settle, with more after the disks; and a machine whose checks pass on them and that counts their flushes.
func upsBench(t *testing.T, more string) (*config.Config, *flushDisks, string) {
	t.Helper()
	top := t.TempDir()
	file := filepath.Join(top, "catalog.db")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(top, "pool")) + "\n" +
		"[deletes]\ndebounce = \"1s\"\n[defaults]\nsettle = \"0s\"\n"
	for i, id := range []string{"t1", "t2"} {
		mount := filepath.Join(top, id)
		if err := os.MkdirAll(filepath.Join(mount, "media"), 0o755); err != nil {
			t.Fatal(err)
		}
		text += "[[disk]]\nid = \"" + id + "\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3" + strconv.Itoa(i) + "\"\nmount = " + strconv.Quote(mount) + "\nfs = \"ext4\"\n"
	}
	if err := os.WriteFile(filepath.Join(top, "t1", "media", "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse([]byte(text + more))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	real, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	return c, &flushDisks{folderDisks: &folderDisks{Machine: real, list: c.Disks}, flushed: map[string]int{}}, text + more
}

type upsRun struct {
	passes chan []string
	reload chan struct{}
	config chan *config.Config
}

// runUPS runs the daemon on a bench until the test ends, reading the UPS every 50 ms, unheard for 300 ms at most.
func runUPS(t *testing.T, c *config.Config, m disks.Machine) *upsRun {
	t.Helper()
	r := &upsRun{passes: make(chan []string, 64), reload: make(chan struct{}, 1), config: make(chan *config.Config, 1)}
	current := c
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: c, Machine: m, Apply: true, Log: slog.New(slog.DiscardHandler), CheckEvery: 100 * time.Millisecond, Inotify: true,
			UPSEvery: 50 * time.Millisecond, UPSDead: 300 * time.Millisecond, FlushWait: 5 * time.Second,
			Reload: r.reload, Load: func() (*config.Config, error) {
				select {
				case current = <-r.config:
				default:
				}
				return current, nil
			},
			OnPass: func(because []string, _ *jobs.Outcome) { r.passes <- because }})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the daemon stopped: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	return r
}

// waitPass waits for a pass for a reason.
func (r *upsRun) waitPass(t *testing.T, why string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case because := <-r.passes:
			if slices.Contains(because, why) {
				return
			}
		case <-deadline:
			t.Fatalf("no pass because %s", why)
		}
	}
}

// noPass says that no pass runs for a second.
func (r *upsRun) noPass(t *testing.T, what string) {
	t.Helper()
	select {
	case because := <-r.passes:
		t.Fatalf("%s: a pass because %v", what, because)
	case <-time.After(time.Second):
	}
}

func eventually(t *testing.T, what string, holds func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !holds() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// On battery, the daemon starts no copy; on line power again, the copy it held comes at once (specs.md §20).
func TestTheDaemonCopiesNothingOnBattery(t *testing.T) {
	u := newFakeUPS(t, "OB DISCHRG")
	c, m, _ := upsBench(t, u.config())
	r := runUPS(t, c, m)
	r.waitPass(t, BecauseStart)
	copied := filepath.Join(c.Disks[1].Mount, "media", "a")
	if exists(copied) {
		t.Fatal("a copy on battery")
	}
	r.noPass(t, "on battery, nothing to do")
	if exists(copied) {
		t.Fatal("a copy on battery")
	}
	u.set("OL CHRG")
	r.waitPass(t, BecauseDue)
	eventually(t, "no copy on line power", func() bool { return exists(copied) })
}

// The battery runs low: the daemon writes out every disk, then does nothing, not even for a change on a disk, until
// line power is back, when it passes as at a start (specs.md §20).
func TestALowBatteryParksTheDaemon(t *testing.T) {
	u := newFakeUPS(t, "OL")
	c, m, _ := upsBench(t, u.config())
	r := runUPS(t, c, m)
	r.waitPass(t, BecauseStart)
	u.set("OB DISCHRG LB")
	eventually(t, "the disks were not written out", func() bool {
		return m.times(c.Disks[0].Mount) == 1 && m.times(c.Disks[1].Mount) == 1
	})
	for len(r.passes) > 0 {
		<-r.passes
	}
	if err := os.WriteFile(filepath.Join(c.Disks[0].Mount, "media", "b"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.noPass(t, "parked")
	if exists(filepath.Join(c.Disks[1].Mount, "media", "b")) {
		t.Fatal("a copy while parked")
	}
	u.set("OL CHRG LB")
	r.waitPass(t, BecauseStart)
	eventually(t, "no copy once back", func() bool { return exists(filepath.Join(c.Disks[1].Mount, "media", "b")) })
}

// A UPS on battery that goes unheard counts as low, as upsmon counts it critical: the daemon parks.
func TestAUPSUnheardOnBatteryParksTheDaemon(t *testing.T) {
	u := newFakeUPS(t, "OB DISCHRG")
	c, m, _ := upsBench(t, u.config())
	r := runUPS(t, c, m)
	r.waitPass(t, BecauseStart)
	u.stale()
	eventually(t, "the disks were not written out", func() bool { return m.times(c.Disks[0].Mount) == 1 })
}

// A configuration read again that no longer reads the UPS ends the park: the UPS counts as on line power.
func TestAReloadWithNoUPSEndsThePark(t *testing.T) {
	u := newFakeUPS(t, "FSD OB LB")
	c, m, text := upsBench(t, u.config())
	r := runUPS(t, c, m)
	eventually(t, "the disks were not written out", func() bool { return m.times(c.Disks[0].Mount) == 1 })
	r.noPass(t, "parked from the start")
	off, err := config.Parse([]byte(strings.Replace(text, "enabled = true", "enabled = false", 1)))
	if err != nil {
		t.Fatal(err)
	}
	r.config <- off
	r.reload <- struct{}{}
	r.waitPass(t, BecauseStart)
}

// A UPS that goes unheard on line power is an alert, and the work goes on: the copy comes (specs.md §20, §23).
func TestAnUnheardUPSIsAnAlert(t *testing.T) {
	var mu sync.Mutex
	var titles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		titles = append(titles, r.Header.Get("Title"))
	}))
	defer srv.Close()
	alerted := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(titles, "Atoll: the UPS cannot be read")
	}
	u := newFakeUPS(t, "OL")
	c, m, _ := upsBench(t, u.config()+"[alerts]\nntfy = "+strconv.Quote(srv.URL+"/atoll")+"\n")
	r := runUPS(t, c, m)
	r.waitPass(t, BecauseStart)
	eventually(t, "no copy", func() bool { return exists(filepath.Join(c.Disks[1].Mount, "media", "a")) })
	// The pool quiet, with no pass to come, the UPS goes unheard: that alone brings the pass whose alert tells of it.
	r.quiet(t)
	u.stale()
	eventually(t, "no alert", alerted)
	// The work goes on: a new file is copied.
	if err := os.WriteFile(filepath.Join(c.Disks[0].Mount, "media", "b"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	eventually(t, "no copy with the UPS unheard", func() bool { return exists(filepath.Join(c.Disks[1].Mount, "media", "b")) })
}

// quiet waits until no pass has come for two seconds.
func (r *upsRun) quiet(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-r.passes:
			if time.Now().After(deadline) {
				t.Fatal("passes come on and on")
			}
		case <-time.After(2 * time.Second):
			return
		}
	}
}

// What the daemon makes of the UPS, reading after reading; and the battery running low ends the step under way.
func TestPowerFollowsTheUPS(t *testing.T) {
	now := int64(0)
	var answer *string
	p := newPower(slog.New(slog.DiscardHandler), func() int64 { return now }, time.Second, 15*time.Second)
	p.read = func(context.Context, value.NUT) (string, error) {
		if answer == nil {
			return "", errors.New("no answer")
		}
		return *answer, nil
	}
	c, err := config.Parse([]byte("pool_id = \"p\"\n[view]\nmount = \"/srv/pool\"\n[ups]\nenabled = true\nnut = \"ups@localhost\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	p.configure(c)
	say := func(status string) {
		answer = &status
		p.poll(t.Context())
	}
	say("OB DISCHRG")
	if r := p.get(); r != (ups.Reading{Power: ups.Battery}) || !p.onBattery() {
		t.Fatalf("%+v", r)
	}
	turn, done := p.turn(t.Context())
	defer done()
	say("OB DISCHRG LB")
	if turn.Err() == nil {
		t.Error("the step under way goes on with the battery low")
	}
	say("OL")
	if p.onBattery() {
		t.Error("on battery on line power")
	}
	answer = nil
	now = 14_999_999_999
	p.poll(t.Context())
	if r := p.get(); r != (ups.Reading{Power: ups.Online}) {
		t.Errorf("within dead time: %+v", r)
	}
	now = 15_000_000_000
	p.poll(t.Context())
	if r := p.get(); r != (ups.Reading{Power: ups.Online, Unreachable: true}) {
		t.Errorf("past dead time: %+v", r)
	}
	off, err := config.Parse([]byte("pool_id = \"p\"\n[view]\nmount = \"/srv/pool\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	p.configure(off)
	if r := p.get(); r != (ups.Reading{Power: ups.Online}) {
		t.Errorf("no UPS: %+v", r)
	}
}

// On battery, the new files' hashes wait, inside the window too; on line power, they are read (specs.md §20).
func TestTheDaemonReadsNoHashOnBattery(t *testing.T) {
	u := newFakeUPS(t, "OB")
	// The window around now, in the machine's time zone, which timezone takes when left out.
	now := time.Now()
	window := "[maintenance]\nwindow = \"" + now.Add(-time.Hour).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04") + "\"\n"
	c, m, _ := upsBench(t, u.config()+window+"[[rule]]\nlabel = \"solo\"\npath = \"**\"\nreplicas = 0\n")
	r := runUPS(t, c, m)
	r.waitPass(t, BecauseStart)
	file, _ := c.Effective()["catalog"].(string)
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	hashed := func() bool {
		var n int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM files WHERE blake3 IS NOT NULL").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	time.Sleep(time.Second)
	if hashed() {
		t.Fatal("a hash read on battery")
	}
	u.set("OL")
	eventually(t, "no hash on line power", hashed)
}
