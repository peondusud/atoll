package fsx

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// The I/O priorities of Linux (ioprio_set(2)): who, and the best-effort class with its lowest level.
const (
	ioprioWhoProcess = 1
	ioprioClassShift = 13
	ioprioClassBE    = 2
	ioprioLowestBE   = ioprioClassBE<<ioprioClassShift | 7
)

// LowIOPriority runs what follows on this goroutine at the lowest level of the best-effort I/O class: a
// copy yields the disk to everything else and is never starved, as the idle class would be under a load
// that never stops (specs.md §14). An I/O priority belongs to a thread, so the goroutine keeps its thread
// until restore puts both back.
func LowIOPriority() (restore func(), err error) {
	runtime.LockOSThread()
	tid := uintptr(unix.Gettid())
	prev, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, tid, 0)
	if errno != 0 {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("ioprio_get: %w", errno)
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, tid, ioprioLowestBE); errno != 0 {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("ioprio_set: %w", errno)
	}
	return func() {
		// Back as it was: a thread that keeps a low priority would pass it on to whatever Go runs on it next.
		_, _, _ = unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, tid, prev)
		runtime.UnlockOSThread()
	}, nil
}
