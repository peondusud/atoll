//go:build !conformance

package disks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peondusud/atoll/internal/layout"
)

// TestTheMachineReadsTheMarkerOncePerMount: the delivered machine reads a disk's marker as it reads hiberfil.sys,
// through a handle whose mount is the one the check found, keeps it for that mount, and forgets it once a check finds
// no mount of its own at the path; but an absence is read again when asked to, as atoll disk mark may have written the
// marker since on a disk whose own marker was seen (specs.md §10).
func TestTheMachineReadsTheMarkerOncePerMount(t *testing.T) {
	m := machine{hiberfil: &hiberfilCache{}, logFile: &logFileCache{}, marker: &markerCache{}}
	top := t.TempDir()
	seen := lookAtMount(top)
	if seen.Path != PathOK || seen.Entry == nil {
		t.Skipf("this machine's mount table does not answer for %s: %+v", top, seen)
	}
	entry := seen.Entry
	if got := m.Marker(top, "Atoll", entry, false); got.File != MarkerAbsent {
		t.Fatalf("none yet: %+v", got)
	}
	at := filepath.Join(top, "Atoll", layout.MarkerName)
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(at, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.Marker(top, "Atoll", entry, false); got.File != MarkerAbsent {
		t.Errorf("its absence kept on the same mount: %+v", got)
	}
	if got := m.Marker(top, "Atoll", entry, true); got.File != MarkerFile || string(got.Text) != "one" {
		t.Errorf("its absence read again: %+v", got)
	}
	if err := os.WriteFile(at, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.Marker(top, "Atoll", entry, true); got.File != MarkerFile || string(got.Text) != "one" {
		t.Errorf("a file kept on the same mount: %+v", got)
	}
	other := *entry
	other.ID++
	if got := m.Marker(top, "Atoll", &other, false); got.File != MarkerUnreadable {
		t.Errorf("through a handle on another mount: %+v", got)
	}
	if got := m.Mount(top); got.Entry == nil || got.Entry.MountPoint == top {
		t.Fatalf("%+v", got)
	}
	if got := m.Marker(top, "Atoll", entry, false); got.File != MarkerFile || string(got.Text) != "two" {
		t.Errorf("once forgotten: %+v", got)
	}
	// Something there that is no regular file, never followed.
	link := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(link, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.marker.forget(top)
	if err := os.Remove(at); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(link, at); err != nil {
		t.Fatal(err)
	}
	if got := m.Marker(top, "Atoll", entry, false); got.File != MarkerNotAFile {
		t.Errorf("a symbolic link: %+v", got)
	}
}
