package daemon

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/alerts"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

// dirtyBench reads one NTFS volume by its by-uuid name, when plugged, and counts the reads.
type dirtyBench struct {
	disks.Machine
	plugged bool
	dirty   bool
	reads   int
}

func (m *dirtyBench) ReadDevice(path string, off int64, n int) ([]byte, error) {
	if !m.plugged || path != "/dev/disk/by-uuid/4A3F1C2B9D8E7F60" {
		return nil, fmt.Errorf("open: %w", fs.ErrNotExist)
	}
	m.reads++
	return ntfsImage(m.dirty, off, n), nil
}

// ntfsImage is a volume of 512-byte clusters whose MFT starts at cluster 4, its records 1024 bytes.
func ntfsImage(dirty bool, off int64, n int) []byte {
	img := make([]byte, 4*512+4*1024)
	copy(img[3:], "NTFS    ")
	img[0x0B], img[0x0C], img[0x0D], img[0x30], img[0x40] = 0x00, 0x02, 1, 4, 0xF6
	rec := img[4*512+3*1024:]
	copy(rec, "FILE")
	rec[4], rec[6], rec[0x14] = 0x30, 3, 0x38
	attr := rec[0x38:]
	attr[0], attr[4], attr[0x10], attr[0x14] = 0x70, 0x28, 12, 0x18
	if dirty {
		attr[0x18+0x0A] = 1
	}
	copy(rec[0x38+0x28:], []byte{0xff, 0xff, 0xff, 0xff})
	// The update sequence: number 1, each block's last two bytes kept in the array.
	rec[0x30] = 1
	for i := range 2 {
		end := (i+1)*512 - 2
		copy(rec[0x32+2*i:], rec[end:end+2])
		rec[end], rec[end+1] = 1, 0
	}
	if off >= int64(len(img)) {
		return nil
	}
	return img[off:min(int64(len(img)), off+int64(n))]
}

// The daemon reads a missing NTFS disk's volume once it is plugged, once while it stays missing, alerts when it is
// dirty and never remounts it then; a disk back forgets it (specs.md §10, §22).
func TestProbeDirty(t *testing.T) {
	m := &dirtyBench{dirty: true}
	d := &config.Disk{ID: "n1", UUID: "4A3F1C2B9D8E7F60", FS: names.NTFS}
	l := &loop{opt: &Options{Machine: m, Config: &config.Config{Disks: []config.Disk{*d}}}, log: slog.New(slog.DiscardHandler)}
	st := &diskState{state: disks.Missing}
	l.disks = map[string]*diskState{"n1": st}
	l.probeDirty(d, st)
	if st.probed || m.reads != 0 {
		t.Errorf("unplugged: probed %v, %d reads", st.probed, m.reads)
	}
	m.plugged = true
	l.probeDirty(d, st)
	l.probeDirty(d, st)
	if !st.probed || !st.dirty || m.reads != 2 {
		t.Errorf("plugged: probed %v, dirty %v, %d reads", st.probed, st.dirty, m.reads)
	}
	if got := l.dirtyDisks(); !slices.Equal(got, []string{"n1"}) {
		t.Errorf("dirty %q", got)
	}
	st.state = disks.Online
	l.probeDirty(d, st)
	if st.probed || st.dirty || l.dirtyDisks() != nil {
		t.Errorf("back: probed %v, dirty %v", st.probed, st.dirty)
	}
	// Clean, it is read once and not said dirty.
	m.dirty, m.reads, st.state = false, 0, disks.Missing
	l.probeDirty(d, st)
	if !st.probed || st.dirty || m.reads != 2 {
		t.Errorf("clean: probed %v, dirty %v, %d reads", st.probed, st.dirty, m.reads)
	}
	// An ext4 disk is never read.
	e := &config.Disk{ID: "e1", UUID: "x", FS: names.Ext4}
	est := &diskState{state: disks.Missing}
	l.probeDirty(e, est)
	if est.probed {
		t.Error("an ext4 disk read as NTFS")
	}
}

// A disk a USB reset took is remounted, but not one failed, nor one whose volume is marked dirty (specs.md §10, §22).
func TestRemountWanted(t *testing.T) {
	d := &config.Disk{ID: "n1", FS: names.NTFS}
	for _, tc := range []struct {
		st    diskState
		state string
		want  bool
	}{
		{diskState{state: disks.Missing, reset: true}, "", true},
		{diskState{state: disks.Missing, reset: true, dirty: true}, "", false},
		{diskState{state: disks.Missing, reset: true}, config.DiskFailed, false},
		{diskState{state: disks.Missing}, "", false},
		{diskState{state: disks.Online, reset: true}, "", false},
	} {
		d.State = tc.state
		if got := remountWanted(d, &tc.st); got != tc.want {
			t.Errorf("%+v, %q: %v", tc.st, tc.state, got)
		}
	}
}

func (m *dirtyBench) Now() int64 { return 42 }

// silent is a channel that takes every alert.
type silent struct{}

func (silent) Name() string                                { return "alerts.test" }
func (silent) Send(context.Context, *alerts.Message) error { return nil }

// A volume found dirty brings a pass at once, which works its alert out: nothing else brings one for a disk that stays
// away, and the alert waited for a pass that had nothing to do with it. A reminder is due a day after an alert was
// last sent, and brings a pass then, unless one is due sooner (specs.md §22, §23).
func TestWhatBringsAPassForAnAlert(t *testing.T) {
	m := &dirtyBench{dirty: true, plugged: true}
	d := &config.Disk{ID: "n1", UUID: "4A3F1C2B9D8E7F60", FS: names.NTFS}
	for _, tc := range []struct {
		name     string
		apply    bool
		channels []alerts.Channel
		want     bool
	}{
		{"a channel set", true, []alerts.Channel{silent{}}, true},
		{"no channel", true, nil, false},
		{"a dry run", false, []alerts.Channel{silent{}}, false},
	} {
		l := &loop{opt: &Options{Machine: m, Apply: tc.apply, Config: &config.Config{Disks: []config.Disk{*d}}}, log: slog.New(slog.DiscardHandler),
			set: &settings{channels: tc.channels}}
		st := &diskState{state: disks.Missing}
		l.disks = map[string]*diskState{"n1": st}
		l.probeDirty(d, st)
		if !st.dirty || (l.passDue != nil) != tc.want || (tc.want && *l.passDue != 42) {
			t.Errorf("%s: dirty %v, a pass due %v", tc.name, st.dirty, l.passDue)
		}
	}
	l := &loop{}
	l.remindAt(nil)
	l.remindAt(&alerts.Due{})
	if l.passDue != nil {
		t.Errorf("with nothing to remind of, a pass is due at %d", *l.passDue)
	}
	l.remindAt(&alerts.Due{RemindNS: 900})
	if l.passDue == nil || *l.passDue != 900 {
		t.Fatalf("a reminder at 900: %v", l.passDue)
	}
	// A pass due sooner stays, and one due later gives way.
	l.remindAt(&alerts.Due{RemindNS: 1200})
	sooner := int64(500)
	if *l.passDue != 900 {
		t.Errorf("a later reminder moved the pass to %d", *l.passDue)
	}
	l.passDue = &sooner
	l.remindAt(&alerts.Due{RemindNS: 900})
	if *l.passDue != 500 {
		t.Errorf("a pass due sooner moved to %d", *l.passDue)
	}
}
