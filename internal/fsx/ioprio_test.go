package fsx

import (
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func ioprioOfThisThread(t *testing.T) uintptr {
	t.Helper()
	got, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, uintptr(unix.Gettid()), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	return got
}

// A copy runs at the lowest best-effort level, and its thread gets back what it had (specs.md §14).
func TestLowIOPriority(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	before := ioprioOfThisThread(t)
	restore, err := LowIOPriority()
	if err != nil {
		t.Fatal(err)
	}
	if got := ioprioOfThisThread(t); got != ioprioLowestBE {
		t.Errorf("during the copy: %#x, want %#x", got, ioprioLowestBE)
	}
	restore()
	if got := ioprioOfThisThread(t); got != before {
		t.Errorf("after: %#x, want %#x", got, before)
	}
}
