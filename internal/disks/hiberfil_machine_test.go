//go:build !conformance

package disks

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTheMachineReadsHiberfilOncePerMount: the delivered machine reads hiberfil.sys through a handle whose mount is the
// one the check found, keeps it for that mount, and forgets it once a check finds no mount of its own at the path
// (specs.md §10). A folder of a test is no mount: its entry is that of the mount it is on, which is what makes the
// machine forget it.
func TestTheMachineReadsHiberfilOncePerMount(t *testing.T) {
	m := machine{hiberfil: &hiberfilCache{}, logFile: &logFileCache{}}
	top := t.TempDir()
	at := filepath.Join(top, HiberfilName)
	if err := os.WriteFile(at, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	seen := lookAtMount(top)
	if seen.Path != PathOK || seen.Entry == nil {
		t.Skipf("this machine's mount table does not answer for %s: %+v", top, seen)
	}
	entry := seen.Entry
	if got := m.Hiberfil(top, entry); got.File != HiberfilFile || got.Size != 10 || !got.Hibernated() {
		t.Fatalf("%+v", got)
	}
	if err := os.Remove(at); err != nil {
		t.Fatal(err)
	}
	if got := m.Hiberfil(top, entry); got.File != HiberfilFile || got.Size != 10 {
		t.Errorf("read again on the same mount: %+v", got)
	}
	// Another mount id than the folder's: what is read through the handle would not be that mount's.
	other := *entry
	other.ID++
	if got := m.Hiberfil(top, &other); got.File != HiberfilUnreadable || judgeHiberfil(got).Reason != Hibernated {
		t.Errorf("through a handle on another mount: %+v", got)
	}
	// The folder is not the mount the table names: a check finds no mount of its own there.
	if got := m.Mount(top); got.Entry == nil || got.Entry.MountPoint == top {
		t.Fatalf("%+v", got)
	}
	if got := m.Hiberfil(top, entry); got.File != HiberfilAbsent {
		t.Errorf("once forgotten: %+v", got)
	}
	if err := os.WriteFile(at, make([]byte, 2*HiberfilHeader), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.Hiberfil(top, entry); got.File != HiberfilAbsent {
		t.Errorf("kept: %+v", got)
	}
	gone := filepath.Join(top, "gone")
	m.hiberfil.seen[gone] = keptReading[HiberfilSeen]{seen: HiberfilSeen{File: HiberfilAbsent}}
	if got := m.Mount(gone); got.Path != PathAbsent {
		t.Fatalf("%+v", got)
	}
	if _, kept := m.hiberfil.seen[gone]; kept {
		t.Error("a path that is gone keeps what was read there")
	}
}

// TestTheMachineReadsLogFileOncePerMount: $LogFile likewise, through a handle whose mount is the one the check found,
// kept for that mount and forgotten once a check finds no mount of its own at the path (specs.md §10).
func TestTheMachineReadsLogFileOncePerMount(t *testing.T) {
	m := machine{hiberfil: &hiberfilCache{}, logFile: &logFileCache{}}
	top := t.TempDir()
	at := filepath.Join(top, LogFileName)
	writeJournal(t, at, "unclean 1.1")
	seen := lookAtMount(top)
	if seen.Path != PathOK || seen.Entry == nil {
		t.Skipf("this machine's mount table does not answer for %s: %+v", top, seen)
	}
	entry := seen.Entry
	if got := m.LogFile(top, entry); got.File != LogFileFile || got.Journal != JournalUnclean {
		t.Fatalf("%+v", got)
	}
	writeJournal(t, at, "clean 1.1")
	if got := m.LogFile(top, entry); got.Journal != JournalUnclean {
		t.Errorf("read again on the same mount: %+v", got)
	}
	other := *entry
	other.ID++
	if got := m.LogFile(top, &other); got.File != LogFileUnreadable || judgeLogFile(got).Reason != Hibernated {
		t.Errorf("through a handle on another mount: %+v", got)
	}
	if got := m.Mount(top); got.Entry == nil || got.Entry.MountPoint == top {
		t.Fatalf("%+v", got)
	}
	if got := m.LogFile(top, entry); got.Journal != JournalClean {
		t.Errorf("once forgotten: %+v", got)
	}
	if err := os.Remove(at); err != nil {
		t.Fatal(err)
	}
	if got := m.LogFile(top, entry); got.Journal != JournalClean {
		t.Errorf("kept: %+v", got)
	}
	gone := filepath.Join(top, "gone")
	m.logFile.seen[gone] = keptReading[LogFileSeen]{seen: LogFileSeen{File: LogFileAbsent}}
	if got := m.Mount(gone); got.Path != PathAbsent {
		t.Fatalf("%+v", got)
	}
	if _, kept := m.logFile.seen[gone]; kept {
		t.Error("a path that is gone keeps what was read there")
	}
}
