package main

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/peondusud/atoll/internal/fsx"
)

// tellDaemon sends SIGHUP to each daemon that runs for good from this executable, so that it reads the
// configuration again, as systemctl reload atoll would (specs.md §15.13). It gives how many it told; a daemon it
// may not signal is an error, with the others still told.
func tellDaemon() (int, error) {
	self, err := fsx.Readlink("/proc/self/exe")
	if err != nil {
		return 0, fmt.Errorf("this executable: %w", err)
	}
	proc, err := fsx.OpenDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("/proc: %w", err)
	}
	names, err := proc.Names()
	_ = proc.Close()
	if err != nil {
		return 0, fmt.Errorf("/proc: %w", err)
	}
	own := unix.Getpid()
	told := 0
	var failed []error
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid == own {
			continue
		}
		// A process that ended meanwhile, or that this user may not look at, is none of ours.
		exe, err := fsx.Readlink("/proc/" + name + "/exe")
		if err != nil || !sameExecutable(exe, self) {
			continue
		}
		cmdline, err := fsx.ReadFile("/proc/" + name + "/cmdline")
		if err != nil || !runsForGood(cmdline) {
			continue
		}
		if err := unix.Kill(pid, unix.SIGHUP); err != nil {
			failed = append(failed, fmt.Errorf("process %d: %w", pid, err))
			continue
		}
		told++
	}
	return told, errors.Join(failed...)
}

// deleted is what the kernel adds to the name of an executable that was removed or replaced on disk since the process
// that runs it started, as an upgrade leaves the daemon's.
const deleted = " (deleted)"

// sameExecutable says whether a process runs this executable, from what /proc/<pid>/exe says of each: the same path,
// whether or not the file it named was replaced since. A daemon started before an upgrade is still the one to tell.
func sameExecutable(exe, self string) bool {
	exe, self = strings.TrimSuffix(exe, deleted), strings.TrimSuffix(self, deleted)
	return exe != "" && exe == self
}

// runsForGood says whether a command line is atoll daemon without --until-idle: the daemon that runs for good.
func runsForGood(cmdline []byte) bool {
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	if len(args) < 2 {
		return false
	}
	args = args[1:]
	switch {
	case args[0] == "--config" && len(args) > 2:
		args = args[2:]
	case strings.HasPrefix(args[0], "--config="):
		args = args[1:]
	}
	return len(args) > 0 && args[0] == "daemon" && !bytes.Contains(cmdline, []byte("\x00--until-idle"))
}
