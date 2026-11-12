package fsx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A lock that waits says so once, takes the lock once it is free, stops waiting when asked, and gives up on
// anything but a lock held: a catalog gone is no reason to wait (specs.md §25).
func TestWaitLockFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "catalog.db")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := LockFile(file)
	if err != nil {
		t.Fatal(err)
	}
	said := 0
	got := make(chan error, 1)
	go func() {
		again, err := WaitLockFile(t.Context(), file, func() { said++ })
		if err == nil {
			again()
		}
		got <- err
	}()
	time.Sleep(3 * lockRetry)
	release()
	if err := <-got; err != nil || said != 1 {
		t.Errorf("once free: %v, said %d times", err, said)
	}

	release, err = LockFile(file)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		_, err := WaitLockFile(ctx, file, nil)
		got <- err
	}()
	time.Sleep(lockRetry)
	cancel()
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Errorf("asked to stop: %v", err)
	}

	go func() {
		_, err := WaitLockFile(t.Context(), file, nil)
		got <- err
	}()
	time.Sleep(lockRetry)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err == nil || errors.Is(err, ErrBusy) {
			t.Errorf("the file gone: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("still waiting for a file that is gone")
	}
	release()
}
