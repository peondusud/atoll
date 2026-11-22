package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// What /proc/<pid>/status says of the account a process acts as: the second number of its Uid line, the effective one.
func TestEffectiveUID(t *testing.T) {
	for _, tc := range []struct {
		status string
		uid    uint32
		ok     bool
	}{
		{"Name:\tatoll\nUid:\t998\t998\t998\t998\nGid:\t998\t998\t998\t998\n", 998, true},
		{"Name:\tatoll\nUid:\t1000\t0\t0\t0\n", 0, true},
		{"Uid:\t1000\t1001\t1000\t1000", 1001, true},
		{"Name:\tatoll\nGid:\t998\t998\t998\t998\n", 0, false},
		{"Uid:\t1000\n", 0, false},
		{"Uid:\t1000\tx\t1000\t1000\n", 0, false},
		{"Uid:\t1000\t-1\t1000\t1000\n", 0, false},
		{"Uid:\t1000\t4294967296\t1000\t1000\n", 0, false},
		{"", 0, false},
	} {
		uid, ok := effectiveUID([]byte(tc.status))
		if uid != tc.uid || ok != tc.ok {
			t.Errorf("%q: %d, %v", tc.status, uid, ok)
		}
	}
}

// A process of this executable that runs daemon is told only when it acts as one of the accounts a daemon of the pool
// runs as, those Atoll takes as its own (specs.md §10, §15.13). Any account that may run the executable can start
// one: counted, it made a disk command say the daemon was told, exit 0 and no daemon-not-told, when none ran, and a
// guard's answer drop the line that says how long it holds (scrutineer F061, 2026-11-22).
func TestOnlyADaemonOfThePoolsAccountsIsTold(t *testing.T) {
	impostor := exec.CommandContext(t.Context(), os.Args[0], "daemon") //nolint:gosec // this test's own binary
	impostor.Args[0] = os.Args[0]
	impostor.Env = append(os.Environ(), impostorEnv+"=1")
	if err := impostor.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = impostor.Process.Kill()
		_ = impostor.Wait()
	})
	pid := strconv.Itoa(impostor.Process.Pid)
	for range 500 {
		if cmdline, err := os.ReadFile("/proc/" + pid + "/cmdline"); err == nil && runsForGood(cmdline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	own := uint32(os.Geteuid()) //nolint:gosec // an account's number, never negative
	if told, err := tellDaemonsOf([]uint32{own + 1}); told != 0 || err != nil {
		t.Errorf("a process of another account was told: %d, %v", told, err)
	}
	if err := impostor.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the process ended: %v", err)
	}
	told, err := tellDaemonsOf([]uint32{own})
	if told != 1 || err != nil {
		t.Errorf("the process of the pool's account: %d told, %v", told, err)
	}
	err = impostor.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGHUP {
		t.Errorf("not hung up: %v", err)
	}
}
