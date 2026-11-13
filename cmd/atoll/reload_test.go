package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The daemon that runs from this executable is told also once the executable was replaced on disk, as an upgrade
// leaves it: the kernel then names what the daemon runs "<path> (deleted)". Compared as it was, no daemon was found,
// the command said so and exited with 0, and the daemon kept the configuration it had (specs.md §15.13).
func TestSameExecutable(t *testing.T) {
	for _, tc := range []struct {
		exe, self string
		want      bool
	}{
		{"/usr/local/bin/atoll", "/usr/local/bin/atoll", true},
		{"/usr/local/bin/atoll (deleted)", "/usr/local/bin/atoll", true},
		{"/usr/local/bin/atoll", "/usr/local/bin/atoll (deleted)", true},
		{"/usr/local/bin/atoll (deleted)", "/usr/local/bin/atoll (deleted)", true},
		{"/usr/local/bin/atoll2", "/usr/local/bin/atoll", false},
		{"/usr/bin/sleep", "/usr/local/bin/atoll", false},
		{"/usr/bin/sleep (deleted)", "/usr/local/bin/atoll", false},
		{"", "/usr/local/bin/atoll", false},
	} {
		if got := sameExecutable(tc.exe, tc.self); got != tc.want {
			t.Errorf("%q and %q: %v", tc.exe, tc.self, got)
		}
	}
}

// A hangup that comes while the daemon reads its configuration finds it listening: unheard, it ended a daemon that was
// starting, as a disk command run then, or systemctl reload, sends one (specs.md §15.13). The daemon runs as a process
// of its own here, since the signal that is not heard ends the process it reaches.
func TestADaemonThatStartsHearsAHangup(t *testing.T) {
	if path := os.Getenv("ATOLL_TEST_DAEMON"); path != "" {
		os.Exit(runDaemon(path, nil, io.Discard, os.Stderr))
	}
	fifo := filepath.Join(t.TempDir(), "atoll.toml")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	daemon := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestADaemonThatStartsHearsAHangup$") //nolint:gosec // this test's own binary
	daemon.Env = append(os.Environ(), "ATOLL_TEST_DAEMON="+fifo)
	var stderr bytes.Buffer
	daemon.Stderr = &stderr
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	// The open returns once the daemon has opened its configuration to read it: it waits there for what is written.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	// Time for the signal to reach it, before its configuration ends.
	time.Sleep(300 * time.Millisecond)
	// A daemon the hangup ended reads nothing more: what the write says then is told with how it ended.
	_, written := w.WriteString("not a configuration\n")
	_ = w.Close()
	err = daemon.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitError {
		t.Fatalf("the daemon ended with %v, want exit 1 for a configuration that does not load (its configuration: %v): %s", err, written, stderr.String())
	}
}
