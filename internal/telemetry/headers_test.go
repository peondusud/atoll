package telemetry

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The headers file is read with its limit, never whole first, and what is no regular file, a FIFO among them, is
// refused without waiting for a writer, which would hold an export for good (specs.md §23.2).
func TestTheHeadersFileIsNoFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "otlp.headers")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readHeaders(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, errHeaders) {
			t.Errorf("a FIFO: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO held the reader")
	}
}
