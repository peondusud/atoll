package disks

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/vectors"
)

type smartVector struct {
	Note    string          `json:"note"`
	Output  json.RawMessage `json:"output"`
	Reading *Smart          `json:"reading"`
	Error   bool            `json:"error"`
}

func TestSmartVectors(t *testing.T) {
	for i, v := range vectors.Load[smartVector](t, "smart.json") {
		if v.Output == nil || (v.Reading == nil) == !v.Error {
			t.Fatalf("vector %d: want output, and exactly one of reading and error", i)
		}
		got, err := ParseSmart(v.Output)
		switch {
		case v.Error && err == nil:
			t.Errorf("vector %d (%s): %+v, want an error", i, v.Note, got)
		case !v.Error && (err != nil || got != *v.Reading):
			t.Errorf("vector %d (%s): %+v, %v, want %+v", i, v.Note, got, err, *v.Reading)
		}
	}
}

// A disk on several devices reads as the worst of them, and asleep only when every device is; its bad sectors are
// those it moved, waits to move and could not read.
func TestWorstSmartAndSectors(t *testing.T) {
	w := WorstSmart([]Smart{{Reallocated: 3, Temperature: 40}, {Pending: 2, Temperature: 45}, {Asleep: true}})
	if w.Asleep || w.Reallocated != 3 || w.Pending != 2 || w.Temperature != 45 {
		t.Errorf("%+v", w)
	}
	if w := WorstSmart([]Smart{{Asleep: true}, {Asleep: true}}); !w.Asleep {
		t.Errorf("%+v", w)
	}
	if n := (&Smart{Reallocated: 8, Pending: 2, Uncorrectable: 1, Temperature: 70}).Sectors(); n != 11 {
		t.Errorf("%d sectors", n)
	}
}

// smartMachine has one disk mounted at /mnt/t1 on the devices it names, and answers smartctl from its table, by
// arguments; a call the table does not hold finds no smartctl.
type smartMachine struct {
	Machine
	devices []string
	outputs map[string]string
	ran     []string
}

func (m *smartMachine) Mount(mount string) MountSeen {
	if mount != "/mnt/t1" {
		return MountSeen{Path: PathOK}
	}
	return MountSeen{Path: PathOK, Entry: &MountEntry{MountPoint: mount, Root: "/", FSType: "ext4"}}
}

func (m *smartMachine) PhysicalDisks(names.Filesystem, *MountEntry, string) ([]string, error) {
	return m.devices, nil
}

func (m *smartMachine) MountPointImmutable(string) (bool, error) { return false, nil }

func (m *smartMachine) Smartctl(_ context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	m.ran = append(m.ran, key)
	out, ok := m.outputs[key]
	if !ok {
		return nil, exec.ErrNotFound
	}
	return []byte(out), nil
}

const (
	smartHealthy = `{"smartctl":{"exit_status":0},"smart_status":{"passed":true},"temperature":{"current":34}}`
	smartBridge  = `{"smartctl":{"exit_status":1,"messages":[{"string":"/dev/sdb: Unknown USB bridge [0x152d:0x0578 (0x214)]"},` +
		`{"string":"Please specify device type with the -d option."}]}}`
	smartMoved  = `{"smartctl":{"exit_status":0},"smart_status":{"passed":true},"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":8}}]}}`
	smartAsleep = `{"smartctl":{"exit_status":2,"messages":[{"string":"Device is in STANDBY mode, exit(2)"}]}}`
)

// ReadSmart reads each device below a disk's mount with -n standby, again with -d sat behind a USB bridge, and gives
// the worst of them (specs.md §23).
func TestReadSmart(t *testing.T) {
	t1 := &config.Disk{ID: "t1", UUID: "u1", Mount: "/mnt/t1", FS: names.Ext4}
	for _, tc := range []struct {
		note    string
		disk    *config.Disk
		devices []string
		outputs map[string]string
		want    Smart
		ran     []string
		fails   bool
		err     error
	}{
		{note: "one device", disk: t1, devices: []string{"sda"}, outputs: map[string]string{"-j -n standby /dev/sda": smartHealthy},
			want: Smart{Temperature: 34}, ran: []string{"-j -n standby /dev/sda"}},
		{note: "behind a USB bridge", disk: t1, devices: []string{"sdb"},
			outputs: map[string]string{"-j -n standby /dev/sdb": smartBridge, "-j -n standby -d sat /dev/sdb": smartMoved},
			want:    Smart{Reallocated: 8}, ran: []string{"-j -n standby /dev/sdb", "-j -n standby -d sat /dev/sdb"}},
		{note: "-d sat that still asks", disk: t1, devices: []string{"sdb"},
			outputs: map[string]string{"-j -n standby /dev/sdb": smartBridge, "-j -n standby -d sat /dev/sdb": smartBridge},
			ran:     []string{"-j -n standby /dev/sdb", "-j -n standby -d sat /dev/sdb"}, fails: true},
		{note: "two devices, one asleep: the other's reading", disk: t1, devices: []string{"sda", "sdb"},
			outputs: map[string]string{"-j -n standby /dev/sda": smartMoved, "-j -n standby /dev/sdb": smartAsleep},
			want:    Smart{Reallocated: 8}, ran: []string{"-j -n standby /dev/sda", "-j -n standby /dev/sdb"}},
		{note: "asleep: left so", disk: t1, devices: []string{"sda"}, outputs: map[string]string{"-j -n standby /dev/sda": smartAsleep},
			want: Smart{Asleep: true}, ran: []string{"-j -n standby /dev/sda"}},
		{note: "no smartctl", disk: t1, devices: []string{"sda"}, ran: []string{"-j -n standby /dev/sda"}, err: exec.ErrNotFound},
		{note: "not mounted", disk: &config.Disk{ID: "t2", Mount: "/mnt/t2", FS: names.Ext4}, devices: []string{"sda"}, fails: true},
		{note: "no device", disk: t1, fails: true},
	} {
		m := &smartMachine{devices: tc.devices, outputs: tc.outputs}
		got, err := ReadSmart(t.Context(), m, tc.disk)
		switch {
		case (tc.fails || tc.err != nil) && err == nil, tc.err != nil && !errors.Is(err, tc.err):
			t.Errorf("%s: %+v, %v, want an error %v", tc.note, got, err, tc.err)
		case !tc.fails && tc.err == nil && (err != nil || got != tc.want):
			t.Errorf("%s: %+v, %v, want %+v", tc.note, got, err, tc.want)
		}
		if !slices.Equal(m.ran, tc.ran) {
			t.Errorf("%s: ran %q, want %q", tc.note, m.ran, tc.ran)
		}
	}
	// A machine that runs no smartctl reads none.
	if _, err := ReadSmart(t.Context(), struct{ Machine }{}, t1); !errors.Is(err, ErrNoSmart) {
		t.Errorf("a machine with no smartctl: %v", err)
	}
}

// A reading is worse than an earlier one when its health check failed and the earlier passed, or when it counts more
// bad sectors.
func TestSmartWorse(t *testing.T) {
	for _, tc := range []struct {
		now     Smart
		earlier *Smart
		worse   bool
	}{
		{Smart{}, nil, false},
		{Smart{Pending: 1}, nil, true},
		{Smart{Pending: 1}, &Smart{}, true},
		{Smart{Pending: 1}, &Smart{Reallocated: 1}, false},
		{Smart{Pending: 1, Reallocated: 1}, &Smart{Reallocated: 1}, true},
		{Smart{Failing: true, Reallocated: 3}, &Smart{Reallocated: 3}, true},
		{Smart{Failing: true, Reallocated: 3}, &Smart{Failing: true, Reallocated: 3}, false},
		{Smart{Temperature: 70}, &Smart{Temperature: 30}, false},
		{Smart{Failing: true}, &Smart{}, true},
		{Smart{Reallocated: 2}, &Smart{Reallocated: 3}, false},
	} {
		if got := tc.now.Worse(tc.earlier); got != tc.worse {
			t.Errorf("%+v after %+v: %v", tc.now, tc.earlier, got)
		}
	}
}
