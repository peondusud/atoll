package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
)

// A mount that never ends, as on a disk that died halfway, must not hold the daemon's only loop: the remount gives it
// its time, then goes on without it, whatever a helper it started still holds open (specs.md §15.13).
func TestRemountGivesUp(t *testing.T) {
	bin := t.TempDir()
	for name, script := range map[string]string{
		"umount": "#!/bin/sh\nexit 0\n",
		// The child keeps the output open, as a mount helper does.
		"mount": "#!/bin/sh\nsleep 600 &\nsleep 600\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fstab := filepath.Join(t.TempDir(), "fstab")
	if err := os.WriteFile(fstab, []byte("UUID=x /mnt/u1 ext4 defaults 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &loop{
		opt: &Options{Apply: true}, log: slog.New(slog.DiscardHandler),
		set: &settings{remount: true, fstab: fstab, remountWait: 200 * time.Millisecond},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.remount(t.Context(), &config.Disk{ID: "u1", Mount: "/mnt/u1"})
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the remount still waits for a mount that never ends")
	}
}

// slowCheck is a machine on which looking at a mount takes a minute.
type slowCheck struct {
	disks.Machine
	now int64
}

func (m *slowCheck) Now() int64 { return m.now }

func (m *slowCheck) Mount(string) disks.MountSeen {
	m.now += time.Minute.Nanoseconds()
	return disks.MountSeen{Path: disks.PathAbsent}
}

// A check that takes longer than the time between two checks leaves that time before the next. The check comes before
// everything else: one due again as soon as it ends, as when each waits for the remount of a dying disk, would keep
// every pass waiting for good (specs.md §15.13).
func TestACheckLeavesItsTimeBeforeTheNext(t *testing.T) {
	m := &slowCheck{now: time.Hour.Nanoseconds()}
	every := 30 * time.Second
	l := &loop{
		opt: &Options{Machine: m, Config: &config.Config{Disks: []config.Disk{{ID: "u1", Mount: "/mnt/u1"}}}},
		set: &settings{checkEvery: every}, log: slog.New(slog.DiscardHandler), disks: map[string]*diskState{},
	}
	l.check(t.Context())
	if l.checkDue < m.now+every.Nanoseconds() {
		t.Errorf("the next check is due %v after this one ended, not %v", time.Duration(l.checkDue-m.now), every)
	}
}
