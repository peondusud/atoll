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
	conf := configFile(t, t.TempDir(), "atoll.toml")
	impostor := startImpostor(t, "", "--config", conf, "daemon")
	own := uint32(os.Geteuid()) //nolint:gosec // an account's number, never negative
	if told, err := tellDaemonsOf(conf, []uint32{own + 1}); told != 0 || err != nil {
		t.Errorf("a process of another account was told: %d, %v", told, err)
	}
	if err := impostor.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the process ended: %v", err)
	}
	told, err := tellDaemonsOf(conf, []uint32{own})
	if told != 1 || err != nil {
		t.Errorf("the process of the pool's account: %d told, %v", told, err)
	}
	err = impostor.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGHUP {
		t.Errorf("not hung up: %v", err)
	}
}

// configFile makes an empty file standing for a configuration.
func configFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// startImpostor starts this test binary as a process that only sleeps, with these arguments after its name, in dir
// when it is not "", and waits until /proc shows them.
func startImpostor(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	p := exec.CommandContext(t.Context(), os.Args[0], args...) //nolint:gosec // this test's own binary
	p.Env = append(os.Environ(), impostorEnv+"=1")
	p.Dir = dir
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.Process.Kill()
		_ = p.Wait()
	})
	pid := strconv.Itoa(p.Process.Pid)
	for range 500 {
		if cmdline, err := os.ReadFile("/proc/" + pid + "/cmdline"); err == nil && runsForGood(cmdline) {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the process never showed its arguments")
	return nil
}

// A daemon is told only when it runs with this command's configuration, the same file whatever path names it: on a
// machine with two pools of one account, a disk command of one told the other's daemon too, and counted it, so said
// the daemon was told when its own ran none (specs.md §15.13). Its path is read from its --config, relative to its
// own folder, or is the default.
func TestOnlyADaemonOfThisConfigurationIsTold(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	ours := configFile(t, dir, "atoll.toml")
	theirs := configFile(t, other, "atoll.toml")
	link := filepath.Join(other, "ours.toml")
	if err := os.Symlink(ours, link); err != nil {
		t.Fatal(err)
	}
	own := []uint32{uint32(os.Geteuid())} //nolint:gosec // an account's number, never negative
	a := startImpostor(t, "", "--config", ours, "daemon")
	b := startImpostor(t, "", "--config="+theirs, "daemon")
	c := startImpostor(t, dir, "--config", "atoll.toml", "daemon")
	d := startImpostor(t, "", "--config", link, "daemon")
	e := startImpostor(t, "", "daemon")
	told, err := tellDaemonsOf(ours, own)
	if told != 3 || err != nil {
		t.Errorf("%d told, %v", told, err)
	}
	for name, p := range map[string]*exec.Cmd{"its path": a, "a path relative to its folder": c, "a link to it": d} {
		err := p.Wait()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() {
			t.Errorf("%s: not hung up: %v", name, err)
		}
	}
	for name, p := range map[string]*exec.Cmd{"another configuration": b, "the default": e} {
		if err := p.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("%s: told: %v", name, err)
		}
	}
}
