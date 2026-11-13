package watch

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// What a process is stays known by its number for a short time only. The kernel gives the number of a process that
// ended to another: an answer kept for good would count a user's changes as Atoll's own, which bring no pass, once a
// command of Atoll that wrote to a disk has ended (specs.md §15.1).
func TestOriginIsAskedAgain(t *testing.T) {
	child := exec.CommandContext(t.Context(), "sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	pid := child.Process.Pid
	exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Hour.Nanoseconds()
	w := NewWatcher(WatcherOptions{Now: func() int64 { return now }})
	defer w.Close()
	w.exe = exe
	if o := w.origin(pid); o != OriginAtoll {
		t.Fatalf("a process of Atoll's own program: %q", o)
	}
	// The number now names another program, as after the kernel gave it again.
	w.exe = "/nowhere/atoll"
	if o := w.origin(pid); o != OriginAtoll {
		t.Errorf("right after, %q: a process is not asked about for each of its events", o)
	}
	now += time.Minute.Nanoseconds()
	if o := w.origin(pid); o != OriginUser {
		t.Errorf("a minute later, %q: the answer was kept past the life of a process", o)
	}
}
