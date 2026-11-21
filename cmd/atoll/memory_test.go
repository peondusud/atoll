package main

import (
	"runtime/debug"
	"testing"

	"golang.org/x/sys/unix"
)

// Each command sets its process up as specs.md §26 says, before anything else: a soft memory limit of 155 MiB unless
// GOMEMLIMIT says otherwise, and no transparent huge pages. At 200,000 files a disk the kernel kept up to 43 MiB
// resident in huge pages after the runtime gave it back, and 170 MiB of limit left no room for SQLite's own memory and
// the binary's pages under the 200 MB.
func TestLimitMemory(t *testing.T) {
	before := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(before) })

	t.Setenv("GOMEMLIMIT", "")
	limitMemory()
	if got := debug.SetMemoryLimit(-1); got != 155<<20 {
		t.Errorf("the soft limit is %d MiB", got>>20)
	}
	disabled, _, errno := unix.Syscall(unix.SYS_PRCTL, unix.PR_GET_THP_DISABLE, 0, 0)
	if errno != 0 || disabled != 1 {
		t.Errorf("transparent huge pages are on for the process: %d, %v", disabled, errno)
	}

	// A limit GOMEMLIMIT gave is left as the runtime read it.
	t.Setenv("GOMEMLIMIT", "300MiB")
	debug.SetMemoryLimit(300 << 20)
	limitMemory()
	if got := debug.SetMemoryLimit(-1); got != 300<<20 {
		t.Errorf("with GOMEMLIMIT, the soft limit is %d MiB", got>>20)
	}
}
