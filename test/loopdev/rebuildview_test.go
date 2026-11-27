//go:build loopdev

package loopdev

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/names"
	"github.com/peondusud/atoll/internal/view/fusefs"
)

// TestARebuildRefusedWhileTheViewRuns: atoll mount serves Atoll's own view read only beside the pool, as its unit
// does, with FUSE; atoll catalog rebuild --yes is refused, exit 1, the catalog where it was; once the view is stopped,
// SIGTERM, exit 0, the rebuild goes (specs.md §9, §21.2, §25). The rebuild went through, and the view went on with the
// catalog the swap set aside (flaw 35 of conformance/models/README.md).
func TestARebuildRefusedWhileTheViewRuns(t *testing.T) {
	needRoot(t)
	if _, err := os.Stat(atollBinary); err != nil {
		t.Skipf("no %s: make qemu-build builds it, and test/qemu/run.sh puts it there", atollBinary)
	}
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBenchWith(t, v, at, "[defaults]\nsettle = \"0s\"\n")
	writeAt(t, b.keep, "Films/a.mkv", "a film")
	point := mountPoint(t)
	mustDo(t, os.MkdirAll(point, 0o755))
	file := filepath.Join(filepath.Dir(b.catalog), "atoll.toml")
	text := strings.Replace(b.configText, "[view]\n", "[view]\nreadonly_mount = \""+point+"\"\n", 1)
	mustDo(t, os.WriteFile(file, []byte(text), 0o600))
	if code, out := b.cli("scan", "--full"); code != 0 {
		t.Fatalf("atoll scan --full: exit %d\n%s", code, out)
	}
	before, err := os.Stat(b.catalog)
	mustDo(t, err)

	view := exec.CommandContext(t.Context(), atollBinary, "--config", file, "mount") //nolint:gosec // the guest's own binary
	var stderr bytes.Buffer
	view.Stderr = &stderr
	mustDo(t, view.Start())
	defer func() { _ = view.Process.Kill() }()
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(mountsAt(t, point), fusefs.Type) {
		if time.Now().After(deadline) {
			t.Fatalf("the view is not mounted at %s: %s", point, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, out := b.cli("catalog", "rebuild", "--yes"); code != 1 || !strings.Contains(out, "atoll mount") {
		t.Errorf("a rebuild while the view runs: exit %d\n%s", code, out)
	}
	if after, err := os.Stat(b.catalog); err != nil || !os.SameFile(before, after) {
		t.Errorf("the refused rebuild moved the catalog: %v", err)
	}
	mustDo(t, view.Process.Signal(syscall.SIGTERM))
	if err := view.Wait(); err != nil {
		t.Fatalf("the view stopped by SIGTERM: %v\n%s", err, stderr.String())
	}
	if code, out := b.cli("catalog", "rebuild", "--yes"); code != 0 {
		t.Errorf("a rebuild once the view is stopped: exit %d\n%s", code, out)
	}
	if after, err := os.Stat(b.catalog); err != nil || os.SameFile(before, after) {
		t.Errorf("the rebuild once the view is stopped left the old catalog in place: %v", err)
	}
}
