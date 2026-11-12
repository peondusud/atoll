//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/names"
)

// TestImmediateCopiesOnClose: a file written through the merged view, in immediate mode, is copied to the other
// disk once the daemon's watcher saw it closed and quiet for deletes.debounce, a second here, long before its settle
// of an hour (specs.md §12.3, §14.1). A file in deferred mode beside it waits for its settle.
func TestImmediateCopiesOnClose(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBenchWith(t, v, at, "[defaults]\nmode = \"immediate\"\nsettle = \"1h\"\n"+
		"[[rule]]\nlabel = \"later\"\npath = \"later/**\"\nmode = \"deferred\"\n")
	pool := viewOf(t, b)
	passes, stop := runDaemon(t, b, filepath.Join(t.TempDir(), "fstab"))
	waitPass(t, passes, daemon.BecauseStart)

	start := time.Now()
	for _, name := range []string{"now.mkv", "later/wait.mkv"} {
		mustDo(t, os.MkdirAll(filepath.Dir(filepath.Join(pool, name)), 0o755))
		mustDo(t, os.WriteFile(filepath.Join(pool, name), []byte("a film: "+name), 0o644))
	}
	// Each lands on one branch, mergerfs choosing which: the copy goes to the other.
	both := func(name string) bool {
		_, a := os.Stat(filepath.Join(b.keep, "media", name))
		_, c := os.Stat(filepath.Join(at, "media", name))
		return a == nil && c == nil
	}
	waitFor(t, "a copy of the file in immediate mode", func() bool { return both("now.mkv") })
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("copied after %v", took)
	}
	if both("later/wait.mkv") {
		t.Error("the file in deferred mode was copied before its settle")
	}
	stop()
}
