package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/jobs"
	"github.com/peondusud/atoll/internal/metrics"
)

// goneDisks is flushDisks whose disks can be taken away: a mount check then finds nothing mounted there.
type goneDisks struct {
	*flushDisks
	mu   sync.Mutex
	gone map[string]bool
}

func (m *goneDisks) Mount(mount string) disks.MountSeen {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gone[mount] {
		return disks.MountSeen{Path: disks.PathAbsent}
	}
	return m.flushDisks.Mount(mount)
}

func (m *goneDisks) take(mount string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gone[mount] = true
}

// runMetrics runs the daemon on a bench until the test ends, recording its metrics.
func runMetrics(t *testing.T, c *config.Config, m disks.Machine) *metrics.Recorder {
	t.Helper()
	rec := metrics.NewRecorder("test")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Config: c, Machine: m, Apply: true, Log: slog.New(slog.DiscardHandler), CheckEvery: 100 * time.Millisecond, Inotify: true,
			UPSEvery: 50 * time.Millisecond, UPSDead: 300 * time.Millisecond, Metrics: rec, OnPass: func([]string, *jobs.Outcome) {}})
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
	return rec
}

// waitMetrics waits until the metrics hold every line, and gives them.
func waitMetrics(t *testing.T, rec *metrics.Recorder, why string, lines ...string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		text, err := rec.Text()
		if err != nil {
			t.Fatal(err)
		}
		missing := ""
		for _, line := range lines {
			if !strings.Contains("\n"+string(text), "\n"+line+"\n") {
				missing = line
				break
			}
		}
		if missing == "" {
			return string(text)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: no line %q in\n%s", why, missing, text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// aroundNow is a window of two hours around now, in local time, so that what waits for a window runs at once.
func aroundNow() string {
	h := time.Now().Hour()
	return fmt.Sprintf("%02d:00-%02d:00", (h+23)%24, (h+1)%24)
}

// The metrics follow the daemon (specs.md §23.1): the disks as each check finds them, with their space; what each pass
// did and counted; what the scrub read; the conditions of the alerts, with no channel set; and the passes that fail.
func TestTheMetricsFollowTheDaemon(t *testing.T) {
	window := aroundNow()
	c, m, _ := upsBench(t, "[maintenance]\nwindow = \""+window+"\"\n[integrity]\nwindow = \""+window+"\"\n"+
		"[[rule]]\nlabel = \"all\"\npath = \"**\"\nmode = \"immediate\"\n")
	machine := &goneDisks{flushDisks: m, gone: map[string]bool{}}
	// Before the daemon: a copied on both disks, whose copies are then never verified, so that its scrub is due.
	file, _ := c.Effective()["catalog"].(string)
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := jobs.Run(t.Context(), db, c, machine, true); err != nil {
			t.Fatal(err)
		}
	}
	res, err := db.ExecContext(t.Context(), "UPDATE copies SET verified_ns = NULL")
	if n, _ := res.RowsAffected(); err != nil || n != 2 {
		t.Fatalf("%d copies of a, %v", n, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rec := runMetrics(t, c, machine)
	waitMetrics(t, rec, "the first pass, before the scrub", `atoll_scrub_due_copies{disk="t1"} 1`, `atoll_scrub_due_bytes{disk="t2"} 1`)
	waitMetrics(t, rec, "a scrubbed", `atoll_scrub_read_copies_total{disk="t1"} 1`, `atoll_scrub_read_bytes_total{disk="t1"} 1`,
		`atoll_scrub_read_copies_total{disk="t2"} 1`, `atoll_scrub_corrupt_total{disk="t1"} 0`)
	// b, written now, is copied by the pass it brings.
	if err := os.WriteFile(filepath.Join(c.Disks[0].Mount, "media", "b"), []byte("yy"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := waitMetrics(t, rec, "b copied",
		`atoll_build_info{version="test"} 1`,
		`atoll_disk_info{disk="t1",fs="ext4",role="data",mount="`+c.Disks[0].Mount+`"} 1`,
		`atoll_disk_state{disk="t1",state="online"} 1`, `atoll_disk_state{disk="t2",state="online"} 1`,
		`atoll_files{state="ok"} 2`, `atoll_file_bytes{state="ok"} 3`, `atoll_files{state="pending"} 0`,
		`atoll_disk_copies{disk="t1"} 2`, `atoll_disk_copies{disk="t2"} 2`, `atoll_disk_copy_bytes{disk="t2"} 3`,
		`atoll_operations_total{kind="replicate",outcome="replicated"} 1`, `atoll_copied_bytes_total 2`,
		`atoll_scrub_due_copies{disk="t1"} 0`, `atoll_disk_standby{disk="t1"} 0`, `atoll_pass_failures_total 0`, `atoll_last_pass_partial 0`)
	for _, want := range []string{`atoll_disk_capacity_bytes{disk="t1"} `, `atoll_disk_free_bytes{disk="t2"} `, "atoll_last_pass_end_timestamp_seconds "} {
		if !strings.Contains(text, "\n"+want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "atoll_alert{") || strings.Contains(text, "atoll_ups_") {
		t.Errorf("an alert, or a UPS, in\n%s", text)
	}
	// A disk that goes: the check says so, and the pass it brings works out its alert, which no channel is set to send.
	machine.take(c.Disks[1].Mount)
	waitMetrics(t, rec, "t2 gone", `atoll_disk_state{disk="t2",state="missing"} 1`, `atoll_alert{alert="disk-missing",disk="t2"} 1`)
	// A pass that fails is counted: its catalog cannot be opened.
	for _, name := range []string{file, file + "-wal", file + "-shm"} {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(file, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Disks[0].Mount, "media", "c"), []byte("zzz"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitMetrics(t, rec, "a pass that failed", "atoll_pass_failures_total 1")
}

// What the UPS runs on, and whether it went unheard, as the daemon reads it every 5 s; nothing without ups.enabled
// (specs.md §20, §23.1).
func TestTheMetricsTellOfTheUPS(t *testing.T) {
	u := newFakeUPS(t, "OL")
	c, m, _ := upsBench(t, u.config())
	rec := runMetrics(t, c, m)
	waitMetrics(t, rec, "on line", `atoll_ups_power{power="online"} 1`, `atoll_ups_power{power="battery"} 0`, "atoll_ups_unreachable 0")
	u.set("OB DISCHRG")
	waitMetrics(t, rec, "on battery", `atoll_ups_power{power="battery"} 1`, `atoll_ups_power{power="online"} 0`)
	u.stale()
	waitMetrics(t, rec, "unheard", "atoll_ups_unreachable 1")
}

// Each reading of SMART is in the metrics as it comes, also in a dry run, which records none in the catalog, and with
// no channel set; a disk asleep keeps what it had (specs.md §23).
func TestTheMetricsTakeEachReadingOfSmart(t *testing.T) {
	l, _ := smartLoop(t, false, false)
	rec := metrics.NewRecorder("test")
	l.opt.Metrics = rec
	l.recordConfig()
	before := l.opt.Machine.Now()
	l.readSmart(t.Context())
	rec.Update(func(s *metrics.Snapshot) {
		if r := s.DiskOf("t1").Smart; r == nil || r.ReadNS < before || r.Reallocated != 8 || r.Temperature != 41 || r.Failing {
			t.Errorf("t1 %+v", r)
		}
		if r := s.DiskOf("t2").Smart; r != nil {
			t.Errorf("t2, asleep, %+v", r)
		}
	})
}

// After a pass, each disk takes the reading of SMART the catalog holds, unless the daemon read a newer one since, which a
// dry run did not record; a failed disk's reading is no news of it (specs.md §23).
func TestAPassKeepsTheNewerReadingOfSmart(t *testing.T) {
	c := bench(t, "[[disk]]\nid = \"t2\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c40\"\nmount = \"/mnt/t2\"\nfs = \"ext4\"\nstate = \"failed\"\n"+
		"[[disk]]\nid = \"t3\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c41\"\nmount = \"/mnt/t3\"\nfs = \"ext4\"\n")
	rec := metrics.NewRecorder("test")
	l := &loop{opt: &Options{Config: c, Metrics: rec}}
	l.recordConfig()
	rec.Update(func(s *metrics.Snapshot) {
		s.DiskOf("t1").Smart = &metrics.Smart{ReadNS: 10, Temperature: 30}
		s.DiskOf("t2").Smart = &metrics.Smart{ReadNS: 10}
	})
	count := &passCount{counted: &catalog.Counted{Files: map[string]catalog.CountedFiles{"ok": {Files: 2, Bytes: 9}},
		Disks: map[string]*catalog.CountedDisk{"t1": {Copies: 2, Bytes: 9}}},
		smart: map[string]*metrics.Smart{"t1": {ReadNS: 5, Temperature: 20}, "t2": {ReadNS: 20}, "t3": {ReadNS: 7, Temperature: 33}}}
	sink := &passSink{ops: map[[2]string]int64{{"trash", "trashed"}: 2}}
	l.recordPass(100, 350, &jobs.Outcome{Partial: true, Waiting: 3, Copied: 9}, sink, count)
	l.recordPass(400, 450, &jobs.Outcome{Waiting: 1}, &passSink{ops: map[[2]string]int64{{"trash", "trashed"}: 1}}, nil)
	rec.Update(func(s *metrics.Snapshot) {
		if r := s.DiskOf("t1").Smart; r.ReadNS != 10 || r.Temperature != 30 {
			t.Errorf("t1 took the older reading: %+v", r)
		}
		if r := s.DiskOf("t2").Smart; r != nil {
			t.Errorf("failed t2 %+v", r)
		}
		if r := s.DiskOf("t3").Smart; r == nil || r.Temperature != 33 {
			t.Errorf("t3 %+v", r)
		}
		if d := s.DiskOf("t1").Catalog; d == nil || d.Copies != 2 || d.Bytes != 9 {
			t.Errorf("t1 %+v", d)
		}
		if d := s.DiskOf("t3").Catalog; d == nil || *d != (metrics.DiskCatalog{}) {
			t.Errorf("t3, with no copy, %+v", d)
		}
		// The second pass counted nothing: the counts stay as the first left them, and what it did adds up.
		if s.Passes != 2 || s.Files["ok"] != (metrics.Count{Files: 2, Bytes: 9}) || *s.Waiting != 1 || s.CopiedBytes != 9 ||
			len(s.Operations) != 1 || s.Operations[0].Count != 3 || s.LastPass.EndNS != 450 || s.LastPass.DurationNS != 50 || s.LastPass.Partial {
			t.Errorf("%+v, %+v, %+v", s, s.LastPass, s.Operations)
		}
	})
}

// What the scrub read is added up, disk by disk.
func TestTheMetricsCountTheScrub(t *testing.T) {
	c := bench(t, "")
	rec := metrics.NewRecorder("test")
	l := &loop{opt: &Options{Config: c, Metrics: rec}}
	l.recordConfig()
	for range 2 {
		l.recordScrub(&jobs.ScrubOutcome{Disks: []jobs.ScrubDisk{{ID: "t1", Read: 2, Bytes: 30, Corrupt: []string{"a"}}, {ID: "t9", Read: 1}}})
	}
	rec.Update(func(s *metrics.Snapshot) {
		if d := s.DiskOf("t1"); d.ScrubReadCopies != 4 || d.ScrubReadBytes != 60 || d.ScrubCorrupt != 2 {
			t.Errorf("%+v", d)
		}
	})
}

// The state of a disk, as the metrics give it.
func TestMetricState(t *testing.T) {
	for _, tc := range []struct {
		config  string
		found   disks.State
		standby bool
		want    string
	}{
		{"", disks.Online, false, "online"},
		{"", disks.Online, true, "standby"},
		{"", disks.Readonly, true, "readonly"},
		{"", disks.Missing, false, "missing"},
		{config.DiskDraining, disks.Online, true, "draining"},
		{config.DiskDraining, disks.Readonly, false, "readonly"},
		{config.DiskDraining, disks.Missing, false, "missing"},
		{config.DiskFailed, disks.Online, false, "failed"},
		{config.DiskFailed, disks.Missing, false, "failed"},
	} {
		if got := metricState(&config.Disk{State: tc.config}, &diskState{state: tc.found, standby: tc.standby}); got != tc.want {
			t.Errorf("%q, %s, standby %v: %s, want %s", tc.config, tc.found, tc.standby, got, tc.want)
		}
	}
}
