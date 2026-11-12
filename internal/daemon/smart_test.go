package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

// smartBench is a machine whose disks are each mounted on the device it names, and whose smartctl answers by
// device from a table a test changes between readings.
type smartBench struct {
	disks.Machine
	devices map[string]string
	outputs map[string]string
	ran     []string
	gone    bool
}

func (m *smartBench) Mount(mount string) disks.MountSeen {
	return disks.MountSeen{Path: disks.PathOK, Entry: &disks.MountEntry{MountPoint: mount, Root: "/", FSType: "ext4"}}
}

func (m *smartBench) PhysicalDisks(_ names.Filesystem, entry *disks.MountEntry, _ string) ([]string, error) {
	return []string{m.devices[entry.MountPoint]}, nil
}

func (m *smartBench) MountPointImmutable(string) (bool, error) { return false, nil }

func (m *smartBench) Smartctl(_ context.Context, args ...string) ([]byte, error) {
	m.ran = append(m.ran, args[len(args)-1])
	if m.gone {
		return nil, fmt.Errorf("smartctl: %w", &exec.Error{Name: "smartctl", Err: exec.ErrNotFound})
	}
	return []byte(m.outputs[args[len(args)-1]]), nil
}

const (
	smartMoved   = `{"smartctl":{"exit_status":0},"smart_status":{"passed":true},"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":8}}]},"temperature":{"current":41}}`
	smartFailing = `{"smartctl":{"exit_status":8},"smart_status":{"passed":false}}`
	smartAsleep  = `{"smartctl":{"exit_status":2,"messages":[{"string":"Device is in STANDBY mode, exit(2)"}]}}`
)

// smartLoop makes a loop over four disks: t1 online, t2 read-only, t3 failed and t4 missing; with ntfy set when
// alerting.
func smartLoop(t *testing.T, apply, alerting bool) (*loop, *smartBench) {
	t.Helper()
	more := ""
	for i, id := range []string{"t2", "t3", "t4"} {
		more += fmt.Sprintf("[[disk]]\nid = %q\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c4%d\"\nmount = \"/mnt/%s\"\nfs = \"ext4\"\n", id, i, id)
		if id == "t3" {
			more += "state = \"failed\"\n"
		}
	}
	if alerting {
		more += "[alerts]\nntfy = \"http://127.0.0.1:9/atoll\"\n"
	}
	c := bench(t, more)
	file, _ := c.Effective()["catalog"].(string)
	db, err := catalog.OpenPool(t.Context(), file, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	list := make([]catalog.Disk, 0, len(c.Disks))
	for i := range c.Disks {
		d := &c.Disks[i]
		list = append(list, catalog.Disk{ID: d.ID, UUID: d.UUID, FS: string(d.FS), Mount: d.Mount, Root: d.Root, Workdir: d.Workdir, Role: d.Role,
			Position: i, State: "online"})
	}
	if err := catalog.SyncDisks(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	real, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	m := &smartBench{Machine: real, devices: map[string]string{c.Disks[0].Mount: "sda", "/mnt/t2": "sdb", "/mnt/t3": "sdc", "/mnt/t4": "sdd"},
		outputs: map[string]string{"/dev/sda": smartMoved, "/dev/sdb": smartAsleep, "/dev/sdc": smartFailing, "/dev/sdd": smartFailing}}
	opt := &Options{Config: c, Machine: m, Apply: apply, Log: slog.New(slog.DiscardHandler)}
	set, err := readSettings(c, opt)
	if err != nil {
		t.Fatal(err)
	}
	return &loop{opt: opt, set: set, log: opt.Log, smartDue: smartFirst(m), disks: map[string]*diskState{
		"t1": {state: disks.Online}, "t2": {state: disks.Readonly}, "t3": {state: disks.Online}, "t4": {state: disks.Missing},
	}}, m
}

// recorded gives each disk's smart_json, by id.
func recorded(t *testing.T, l *loop) map[string]string {
	t.Helper()
	db, err := catalog.OpenPool(t.Context(), l.set.catalog, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	list, err := catalog.Disks(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range list {
		out[d.ID] = d.SmartJSON
	}
	return out
}

// The daemon reads the SMART of each disk the last check found mounted but a failed one, records what it read of a
// disk awake, brings a pass when a reading tells of more trouble than the one before, and stops reading when there
// is no smartctl (specs.md §23).
func TestReadSmartRecords(t *testing.T) {
	l, m := smartLoop(t, true, true)
	if l.smartDue == nil {
		t.Fatal("a machine that runs smartctl has no reading due")
	}
	before := l.opt.Machine.Now()
	l.readSmart(t.Context())
	if !slices.Equal(m.ran, []string{"/dev/sda", "/dev/sdb"}) {
		t.Errorf("read %q", m.ran)
	}
	if l.smartDue == nil || *l.smartDue < before+smartEvery.Nanoseconds() {
		t.Errorf("next reading %v", l.smartDue)
	}
	if l.passDue == nil || *l.passDue < before {
		t.Errorf("no pass after a first reading with sectors moved: %v", l.passDue)
	}
	got := recorded(t, l)
	var r1 struct {
		ReadNS      int64 `json:"read_ns"`
		Reallocated int64 `json:"reallocated"`
		Temperature int64 `json:"temperature"`
	}
	if err := json.Unmarshal([]byte(got["t1"]), &r1); err != nil || r1.ReadNS < before || r1.Reallocated != 8 || r1.Temperature != 41 {
		t.Errorf("t1 holds %q", got["t1"])
	}
	if got["t2"] != "" || got["t3"] != "" || got["t4"] != "" {
		t.Errorf("recorded %q", got)
	}
	// The same reading again brings no pass.
	l.passDue = nil
	l.readSmart(t.Context())
	if l.passDue != nil {
		t.Error("a pass for nothing new")
	}
	// The disk that was asleep wakes up failing: recorded, and a pass.
	m.outputs["/dev/sdb"] = smartFailing
	l.readSmart(t.Context())
	if got := recorded(t, l); l.passDue == nil || got["t2"] == "" || got["t1"] == "" {
		t.Errorf("%v, %q", l.passDue, got)
	}
	// No smartctl: nothing read until the configuration is read again.
	m.gone, m.ran = true, nil
	l.readSmart(t.Context())
	if l.smartDue != nil || len(m.ran) != 1 {
		t.Errorf("due %v, ran %q", l.smartDue, m.ran)
	}
}

// A daemon in shadow mode records no reading and brings no pass; one with no channel brings no pass (specs.md §23).
func TestReadSmartShadowAndSilent(t *testing.T) {
	l, _ := smartLoop(t, false, true)
	l.readSmart(t.Context())
	if got := recorded(t, l); l.passDue != nil || got["t1"] != "" {
		t.Errorf("shadow: %v, %q", l.passDue, got)
	}
	l, _ = smartLoop(t, true, false)
	l.readSmart(t.Context())
	if got := recorded(t, l); l.passDue != nil || got["t1"] == "" {
		t.Errorf("no channel: %v, %q", l.passDue, got)
	}
}

// A machine that runs no smartctl has no reading due, and gets none from a reload.
func TestSmartFirst(t *testing.T) {
	if due := smartFirst(struct{ disks.Machine }{}); due != nil {
		t.Errorf("%v", *due)
	}
}
